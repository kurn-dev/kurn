//go:build unix

package prepared

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// The mapped request/admission matrix with LevelTrusted, on fresh
// registries (the process-wide registry is the same type): three requests
// (full, trusted, structure) against three states (nothing admitted, a
// full admission, a trusted admission), the upgrade's outcomes, header
// mismatches on every reuse row, accept failures, and the concurrency
// rules. Mappings admitted here are never unmapped, as in production.

// twinArtifacts returns two valid artifacts of exactly the same size
// whose header and table digests differ (their build sections' source
// strings differ in one byte).
func twinArtifacts(t *testing.T) (a, b []byte) {
	t.Helper()
	ca := testContents("ngram", testRecords(120, 1))
	cb := testContents("ngram", testRecords(120, 1))
	ca.Source, cb.Source = "twin-a", "twin-b"
	a, _ = encodeT(t, ca)
	b, _ = encodeT(t, cb)
	if len(a) != len(b) || bytes.Equal(a, b) {
		t.Fatal("twins differ in size or not at all")
	}
	va, _ := NewView(a)
	vb, _ := NewView(b)
	if va.tab.digest == vb.tab.digest {
		t.Fatal("twins share a header digest")
	}
	return a, b
}

// rewriteInPlace overwrites the file's bytes through a fresh descriptor:
// same inode, same size when data is the same length. Mode 0444 restored.
func rewriteInPlace(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
}

func mustOpen(t *testing.T, r *mappedRegistry, path string, level Level, want outcome) (*View, *Verified) {
	t.Helper()
	v, ver, o, err := r.open(path, level, nil)
	if err != nil || o != want {
		t.Fatalf("%v open: outcome %d err %v, want outcome %d", level, o, err, want)
	}
	return v, ver
}

func mustRefuse(t *testing.T, r *mappedRegistry, path string, level Level, is error, msg string) {
	t.Helper()
	before := r.adm
	v, ver, o, err := r.open(path, level, func(*View, *Verified) error {
		t.Errorf("accept ran for a refused %v open", level)
		return nil
	})
	if v != nil || ver != nil || o != outRefused || !errors.Is(err, is) || !strings.Contains(err.Error(), msg) {
		t.Fatalf("%v open: view %v outcome %d err %v, want %v naming %q", level, v != nil, o, err, is, msg)
	}
	if r.adm != before {
		t.Fatalf("%v refusal changed the admission", level)
	}
}

func checkTrustedVerified(t *testing.T, ver *Verified) {
	t.Helper()
	if ver.Level != LevelTrusted || ver.ContentVerified || !ver.HeaderDigestVerified || ver.BaseID != "" || ver.ContainerSHA256 != "" || ver.DeclaredBaseID == "" {
		t.Fatalf("trusted result %+v", ver)
	}
}

func checkFullVerified(t *testing.T, ver *Verified) {
	t.Helper()
	if ver.Level != LevelFull || !ver.ContentVerified || !ver.HeaderDigestVerified || ver.BaseID == "" || ver.ContainerSHA256 == "" {
		t.Fatalf("full result %+v", ver)
	}
}

// Row "nothing admitted": full admits as full, trusted admits as trusted,
// structure is ErrNotMappable.
func TestMappedMatrixNothingAdmitted(t *testing.T) {
	a, _ := twinArtifacts(t)
	p := writeArtifact(t, t.TempDir(), "a.kart", a, 0o444)

	var rs mappedRegistry
	mustRefuse(t, &rs, p, LevelStructure, ErrNotMappable, "requires full or trusted verification")
	if rs.adm != nil {
		t.Fatal("structure admitted")
	}

	var rf mappedRegistry
	v, ver := mustOpen(t, &rf, p, LevelFull, outAdmitted)
	checkFullVerified(t, ver)
	if rf.adm.level != LevelFull || rf.adm.sum != ver.ContainerSHA256 || rf.adm.header != v.tab.digest {
		t.Fatalf("full admission record %+v", rf.adm)
	}

	var rt mappedRegistry
	v, ver = mustOpen(t, &rt, p, LevelTrusted, outAdmitted)
	checkTrustedVerified(t, ver)
	if rt.adm.level != LevelTrusted || rt.adm.sum != "" || rt.adm.header != v.tab.digest {
		t.Fatalf("trusted admission record %+v", rt.adm)
	}
}

// Column "full admitted": full re-verifies, trusted is served and reported
// as trusted, structure re-verifies fully and reports LevelFull; a header
// mismatch refuses every row and leaves the admission unchanged.
func TestMappedMatrixFullAdmitted(t *testing.T) {
	a, b := twinArtifacts(t)
	p := writeArtifact(t, t.TempDir(), "a.kart", a, 0o444)
	var r mappedRegistry
	v0, _ := mustOpen(t, &r, p, LevelFull, outAdmitted)
	adm := r.adm

	_, ver := mustOpen(t, &r, p, LevelFull, outReused)
	checkFullVerified(t, ver)
	vt, ver := mustOpen(t, &r, p, LevelTrusted, outReused)
	checkTrustedVerified(t, ver)
	if bufAddr(vt.buf) != bufAddr(v0.buf) {
		t.Fatal("trusted reuse served another mapping")
	}
	_, ver = mustOpen(t, &r, p, LevelStructure, outReused)
	checkFullVerified(t, ver) // structure against full reports LevelFull
	if r.adm != adm {
		t.Fatal("reuse changed the admission")
	}

	rewriteInPlace(t, p, b) // same inode and size, another valid artifact
	for _, lvl := range []Level{LevelFull, LevelTrusted, LevelStructure} {
		mustRefuse(t, &r, p, lvl, ErrMappedInUse, "header and table digest")
	}
	rewriteInPlace(t, p, a)
	for _, lvl := range []Level{LevelFull, LevelTrusted, LevelStructure} {
		mustOpen(t, &r, p, lvl, outReused)
	}
	if r.adm != adm || r.adm.level != LevelFull {
		t.Fatal("admission changed")
	}
}

// Column "trusted admitted": trusted is served as trusted; structure is
// ErrMappedInUse; full upgrades only if real full verification passes
// with the admitted header digest.
func TestMappedMatrixTrustedAdmitted(t *testing.T) {
	a, b := twinArtifacts(t)
	p := writeArtifact(t, t.TempDir(), "a.kart", a, 0o444)
	var r mappedRegistry
	v0, first := mustOpen(t, &r, p, LevelTrusted, outAdmitted)
	firstCopy := *first
	adm := r.adm

	_, ver := mustOpen(t, &r, p, LevelTrusted, outReused)
	checkTrustedVerified(t, ver)
	mustRefuse(t, &r, p, LevelStructure, ErrMappedInUse, "cannot be served from a trusted admission")

	// Header mismatch on the trusted and full rows. The full request's
	// verification of the rewritten mapping PASSES, and it is still
	// refused, not served and not upgraded.
	rewriteInPlace(t, p, b)
	if _, _, err := VerifyBytes(v0.buf, LevelFull); err != nil {
		t.Fatalf("the rewritten mapping should pass full verification: %v", err)
	}
	mustRefuse(t, &r, p, LevelFull, ErrMappedInUse, "header and table digest")
	mustRefuse(t, &r, p, LevelTrusted, ErrMappedInUse, "header and table digest")
	mustRefuse(t, &r, p, LevelStructure, ErrMappedInUse, "trusted admission")
	if r.adm != adm || r.adm.level != LevelTrusted {
		t.Fatal("a header mismatch upgraded or changed the admission")
	}
	rewriteInPlace(t, p, a) // later requests compare against the original header
	mustOpen(t, &r, p, LevelTrusted, outReused)

	// A content change that stays structurally valid and leaves the
	// recorded digests alone: trusted reuse cannot see it (documented) and
	// serves the altered bytes; the full request's verification fails, so
	// it refuses without upgrading.
	altered := patchRecords(t, a, `"SDGT"`, `"SDGX"`)
	rewriteInPlace(t, p, altered)
	vt, _ := mustOpen(t, &r, p, LevelTrusted, outReused)
	seen := false
	for i := 0; i < vt.Len(); i++ {
		seen = seen || bytes.Contains(vt.Payload(i), []byte(`"SDGX"`))
	}
	if !seen {
		t.Fatal("trusted reuse does not serve the mapping's current bytes")
	}
	v, ver, o, err := r.open(p, LevelFull, nil)
	if v != nil || ver != nil || o != outRefused || !errors.Is(err, ErrMappedInUse) || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("full against an altered trusted mapping: %v", err)
	}
	if r.adm != adm {
		t.Fatal("failed full verification changed the admission")
	}
	mustRefuse(t, &r, p, LevelStructure, ErrMappedInUse, "trusted admission")

	// Restored: the upgrade passes.
	rewriteInPlace(t, p, a)
	vf, ver := mustOpen(t, &r, p, LevelFull, outUpgraded)
	checkFullVerified(t, ver)
	if r.adm == adm || r.adm.level != LevelFull || r.adm.sum != ver.ContainerSHA256 || r.adm.header != adm.header || bufAddr(r.adm.buf) != bufAddr(adm.buf) || bufAddr(vf.buf) != bufAddr(v0.buf) {
		t.Fatalf("upgrade record %+v", r.adm)
	}
	if adm.level != LevelTrusted || adm.sum != "" {
		t.Fatal("the old admission record was mutated")
	}
	if *first != firstCopy {
		t.Fatal("an earlier returned Verified changed")
	}
	// After the upgrade the full column applies.
	_, ver = mustOpen(t, &r, p, LevelStructure, outReused)
	checkFullVerified(t, ver)
	_, ver = mustOpen(t, &r, p, LevelTrusted, outReused)
	checkTrustedVerified(t, ver)
	mustOpen(t, &r, p, LevelFull, outReused)
}

// Accept failures on a trusted admission publish nothing, change nothing,
// unmap nothing, and return the callback's error as is.
func TestMappedTrustedAcceptFailure(t *testing.T) {
	a, _ := twinArtifacts(t)
	p := writeArtifact(t, t.TempDir(), "a.kart", a, 0o444)
	var r mappedRegistry
	refuse := fmt.Errorf("%w: caller refuses", ErrIncompatible)
	// First admission, refused: unmapped, registry empty.
	if _, _, o, err := r.open(p, LevelTrusted, func(*View, *Verified) error { return refuse }); err != refuse || o != outRefused || r.adm != nil {
		t.Fatalf("refused trusted admission: %v", err)
	}
	v0, _ := mustOpen(t, &r, p, LevelTrusted, outAdmitted)
	adm := r.adm
	for _, lvl := range []Level{LevelFull, LevelTrusted} {
		called := false
		v, ver, o, err := r.open(p, lvl, func(v *View, ver *Verified) error {
			called = true
			if lvl == LevelFull {
				checkFullVerified(t, ver)
			}
			return refuse
		})
		if !called || err != refuse || v != nil || ver != nil || o != outRefused {
			t.Fatalf("%v: accept failure returned %v (want the callback error unwrapped)", lvl, err)
		}
		if r.adm != adm || r.adm.level != LevelTrusted || r.adm.sum != "" {
			t.Fatalf("%v: accept failure changed the admission", lvl)
		}
	}
	// Nothing was unmapped: the mapping still verifies and serves.
	if _, _, err := VerifyBytes(v0.buf, LevelTrusted); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, &r, p, LevelFull, outUpgraded)
}

// G-T2 races, under -race.
func TestMappedTrustedConcurrency(t *testing.T) {
	a, _ := twinArtifacts(t)
	p := writeArtifact(t, t.TempDir(), "a.kart", a, 0o444)
	type res struct {
		level Level
		ver   *Verified
		copy  Verified
		o     outcome
		err   error
	}
	race := func(r *mappedRegistry, levels []Level, accept func(Level) func(*View, *Verified) error) []res {
		out := make([]res, len(levels))
		var start, wg sync.WaitGroup
		start.Add(1)
		for i, lvl := range levels {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				var acc func(*View, *Verified) error
				if accept != nil {
					acc = accept(lvl)
				}
				_, ver, o, err := r.open(p, lvl, acc)
				out[i] = res{level: lvl, ver: ver, o: o, err: err}
				if ver != nil {
					out[i].copy = *ver
				}
			}()
		}
		start.Done()
		wg.Wait()
		return out
	}
	repeat := func(n int, l ...Level) []Level {
		var out []Level
		for i := 0; i < n; i++ {
			out = append(out, l...)
		}
		return out
	}

	t.Run("upgrade", func(t *testing.T) {
		for iter := 0; iter < 5; iter++ {
			var r mappedRegistry
			mustOpen(t, &r, p, LevelTrusted, outAdmitted)
			up := 0
			for _, x := range race(&r, repeat(16, LevelFull), nil) {
				if x.err != nil {
					t.Fatal(x.err)
				}
				checkFullVerified(t, x.ver)
				if x.o == outUpgraded {
					up++
				} else if x.o != outReused {
					t.Fatalf("outcome %d", x.o)
				}
			}
			if up != 1 || r.adm.level != LevelFull {
				t.Fatalf("%d upgrades", up)
			}
		}
	})

	t.Run("accept failure during upgrade", func(t *testing.T) {
		var r mappedRegistry
		mustOpen(t, &r, p, LevelTrusted, outAdmitted)
		adm := r.adm
		refuse := errors.New("caller refuses")
		out := race(&r, repeat(8, LevelFull, LevelTrusted), func(l Level) func(*View, *Verified) error {
			return func(*View, *Verified) error {
				if l == LevelFull {
					return refuse
				}
				return nil
			}
		})
		for _, x := range out {
			switch x.level {
			case LevelFull:
				if x.err != refuse || x.o != outRefused {
					t.Fatalf("full: %v", x.err)
				}
			case LevelTrusted:
				if x.err != nil || x.o != outReused {
					t.Fatalf("trusted: %v", x.err)
				}
				checkTrustedVerified(t, x.ver)
			}
		}
		if r.adm != adm || r.adm.level != LevelTrusted {
			t.Fatal("a refused upgrade changed the admission")
		}
	})

	t.Run("mixed requests", func(t *testing.T) {
		for iter := 0; iter < 10; iter++ {
			var r mappedRegistry
			mustOpen(t, &r, p, LevelTrusted, outAdmitted)
			old := r.adm
			out := race(&r, repeat(6, LevelFull, LevelTrusted, LevelStructure), nil)
			up, structOK, structRefused := 0, 0, 0
			for _, x := range out {
				switch x.level {
				case LevelFull:
					if x.err != nil {
						t.Fatal(x.err)
					}
					checkFullVerified(t, x.ver)
					if x.o == outUpgraded {
						up++
					}
				case LevelTrusted:
					if x.err != nil || x.o != outReused {
						t.Fatal(x.err)
					}
					checkTrustedVerified(t, x.ver) // never reported as full
				case LevelStructure:
					switch {
					case x.err == nil:
						checkFullVerified(t, x.ver) // served only after the upgrade
						structOK++
					case errors.Is(x.err, ErrMappedInUse) && x.o == outRefused:
						structRefused++
					default:
						t.Fatal(x.err)
					}
				}
			}
			if up > 1 || (structOK > 0 && up != 1) || structOK+structRefused != 6 {
				t.Fatalf("%d upgrades, structure %d served %d refused", up, structOK, structRefused)
			}
			// No retroactive change to returned values or the old record.
			for _, x := range out {
				if x.ver != nil && *x.ver != x.copy {
					t.Fatal("a returned Verified changed after it was returned")
				}
			}
			if old.level != LevelTrusted || old.sum != "" || (up == 1) != (r.adm.level == LevelFull) {
				t.Fatal("admission records inconsistent")
			}
		}
	})

	t.Run("first admission full versus trusted", func(t *testing.T) {
		orders := map[string]int{}
		for iter := 0; iter < 50; iter++ {
			var r mappedRegistry
			out := race(&r, []Level{LevelFull, LevelTrusted}, nil)
			full, tr := out[0], out[1]
			if full.err != nil || tr.err != nil {
				t.Fatalf("full %v, trusted %v", full.err, tr.err)
			}
			checkFullVerified(t, full.ver)
			checkTrustedVerified(t, tr.ver)
			switch {
			case tr.o == outAdmitted && full.o == outUpgraded:
				orders["trusted first, then upgrade"]++
			case full.o == outAdmitted && tr.o == outReused:
				orders["full first, then trusted reuse"]++
			default:
				t.Fatalf("outcomes full %d trusted %d", full.o, tr.o)
			}
			if r.adm.level != LevelFull {
				t.Fatal("final admission is not full")
			}
		}
		t.Logf("orders seen: %v", orders)
	})
}
