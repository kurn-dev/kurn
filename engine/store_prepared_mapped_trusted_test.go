//go:build unix

package engine_test

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
)

// The mapped request/admission matrix under the trusted policy, through
// OpenPrepared, in child processes (one admission per process). The
// registry-level matrix and races are also covered on fresh registries in
// engine/prepared/mapped_trusted_test.go.

// TestPreparedMappedTrustedMatrix writes three artifacts of one size:
// A, A2 (another valid artifact whose header digest differs) and A3 (A
// with one payload byte altered and every recorded digest untouched), and
// runs each scenario in its own child.
func TestPreparedMappedTrustedMatrix(t *testing.T) {
	c := prepCaseNamed(t, "ngram-filtered")
	a := prepWriteArtifact(t, c)
	twin, _, err := builder.Encode(prepListName, c.cfg, c.entries(), builder.Options{Source: "tesT"})
	if err != nil {
		t.Fatal(err)
	}
	a2 := prepWriteBytes(t, twin)
	altered, target := prepAlterPayload(t, c)
	a3 := prepWriteBytes(t, altered)
	for _, p := range []string{a2, a3} {
		fa, _ := os.Stat(a)
		fp, _ := os.Stat(p)
		if fa.Size() != fp.Size() {
			t.Fatalf("fixture sizes differ: %d vs %d", fa.Size(), fp.Size())
		}
	}
	env := map[string]string{"A": a, "A2": a2, "A3": a3, "TARGET": target.ID}
	for _, mode := range []string{"tm-none", "tm-full", "tm-trusted", "tm-nofallback"} {
		t.Run(mode, func(t *testing.T) {
			env["A"] = prepWriteArtifact(t, c) // a fresh inode per child
			mustMappedChild(t, mode, env)
		})
	}
	t.Run("races", func(t *testing.T) {
		env["A"] = prepWriteArtifact(t, c)
		for i := 0; i < 3; i++ {
			mustMappedChild(t, "tm-race-mixed", env)
		}
		for i := 0; i < 5; i++ {
			mustMappedChild(t, "tm-race-first", env)
		}
	})
}

func mappedOpen(level prepared.Level, path string) (*engine.Store, error) {
	st, err := engine.OpenPrepared(path, engine.PreparedOptions{Loader: engine.LoaderMapped, Verify: level})
	if (st == nil) == (err == nil) {
		panic("store and error both set or both unset")
	}
	return st, err
}

func mappedMust(t *testing.T, level prepared.Level, path string, wantLevel prepared.Level) *engine.Store {
	t.Helper()
	st, err := mappedOpen(level, path)
	if err != nil {
		t.Fatalf("%v open: %v", level, err)
	}
	info := st.Prepared()
	basis := engine.StampBasisDeclared
	if wantLevel == prepared.LevelFull {
		basis = engine.StampBasisVerified
	}
	if info.Level != wantLevel || info.Loader != engine.LoaderMapped || info.StampBasis != basis ||
		info.ContentVerified != (wantLevel == prepared.LevelFull) || !info.HeaderDigestVerified ||
		(info.BaseID == "") != (wantLevel != prepared.LevelFull) || (info.ContainerSHA256 == "") != (wantLevel != prepared.LevelFull) {
		t.Fatalf("%v open: info %+v, want level %v", level, info, wantLevel)
	}
	return st
}

func mappedRefused(t *testing.T, level prepared.Level, path string, is error, msg string) {
	t.Helper()
	if st, err := mappedOpen(level, path); st != nil || !errors.Is(err, is) || !strings.Contains(err.Error(), msg) {
		t.Fatalf("%v open: %v, want %v naming %q", level, err, is, msg)
	}
}

// rewriteFrom overwrites dst in place (same inode) with src's bytes.
func rewriteFrom(t *testing.T, dst, src string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o444); err != nil {
		t.Fatal(err)
	}
}

func mappedChildTrusted(t *testing.T, mode string) {
	c := prepCaseNamed(t, "ngram-filtered")
	a, a2, a3 := mappedEnv("A"), mappedEnv("A2"), mappedEnv("A3")
	full, trusted, structure := prepared.LevelFull, prepared.LevelTrusted, prepared.LevelStructure
	var target engine.Entry
	for _, e := range c.entries() {
		if e.ID == mappedEnv("TARGET") {
			target = e
		}
	}
	keep := func(path string) string { // a copy of the original bytes, to restore from
		p := path + ".orig"
		prepCopyFile(t, path, p)
		return p
	}

	switch mode {
	case "tm-none":
		// Nothing admitted: structure is ErrNotMappable; trusted admits and
		// answers exactly as a full heap open.
		mappedRefused(t, structure, a, engine.ErrNotMappable, "requires full or trusted verification")
		st := mappedMust(t, trusted, a, trusted)
		heap, err := engine.OpenPrepared(a, engine.PreparedOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := prepAnswers(t, prepList(t, heap), c)
		if got := prepAnswers(t, prepList(t, st), c); !bytes.Equal(got, want) {
			t.Fatalf("mapped trusted answers differ from heap full:\n%s", prepFirstDiff(got, want))
		}
		// Read-only and process-lifetime retention hold under trusted.
		l := prepList(t, st)
		errCalls, _ := prepMutators(st, l, prepListName)
		for what, call := range errCalls {
			if err := call(); !errors.Is(err, engine.ErrReadOnly) {
				t.Fatalf("%s: %v", what, err)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if got := prepAnswers(t, l, c); !bytes.Equal(got, want) {
			t.Fatal("answers changed after Close")
		}
		mappedMust(t, trusted, a, trusted) // the mapping is retained after Close

	case "tm-full":
		orig := keep(a)
		f := mappedMust(t, full, a, full)
		tr := mappedMust(t, trusted, a, trusted) // served and reported as trusted
		ta, fa := payloadAddrs(prepList(t, tr)), payloadAddrs(prepList(t, f))
		if len(ta) == 0 || len(fa) != len(ta) || fa[0] != ta[0] {
			t.Fatal("trusted reuse is not served from the admitted mapping")
		}
		mappedMust(t, structure, a, full) // structure against full reports LevelFull
		mappedMust(t, full, a, full)
		rewriteFrom(t, a, a2) // same inode and size, another valid artifact
		for _, lvl := range []prepared.Level{full, trusted, structure} {
			mappedRefused(t, lvl, a, engine.ErrMappedInUse, "header and table digest")
		}
		rewriteFrom(t, a, orig)
		for _, lvl := range []prepared.Level{full, trusted} {
			mappedMust(t, lvl, a, lvl)
		}
		mappedMust(t, structure, a, full)

	case "tm-trusted":
		orig := keep(a)
		s1 := mappedMust(t, trusted, a, trusted)
		info1 := *s1.Prepared()
		want := prepAnswers(t, prepList(t, s1), c)
		mappedRefused(t, structure, a, engine.ErrMappedInUse, "trusted admission")
		mappedMust(t, trusted, a, trusted)

		// Header mismatch: full (whose verification of the rewritten
		// mapping passes) and trusted are refused; nothing is upgraded, and
		// later requests still compare against the original header.
		rewriteFrom(t, a, a2)
		mappedRefused(t, full, a, engine.ErrMappedInUse, "header and table digest")
		mappedRefused(t, trusted, a, engine.ErrMappedInUse, "header and table digest")
		mappedRefused(t, structure, a, engine.ErrMappedInUse, "trusted admission")
		rewriteFrom(t, a, orig)
		mappedMust(t, trusted, a, trusted)
		mappedRefused(t, structure, a, engine.ErrMappedInUse, "trusted admission") // still trusted

		// A structurally valid content change with the recorded digests
		// untouched: trusted reuse cannot see it and serves it; the full
		// request's verification fails, so it is refused and nothing is
		// upgraded.
		rewriteFrom(t, a, a3)
		if st := mappedMust(t, trusted, a, trusted); !prepServesAltered(prepList(t, st), target) {
			t.Fatal("trusted reuse does not serve the mapping's current bytes")
		}
		if st, err := mappedOpen(full, a); st != nil || !errors.Is(err, engine.ErrMappedInUse) || !errors.Is(err, prepared.ErrIntegrity) {
			t.Fatalf("full against an altered trusted mapping: %v", err)
		}
		mappedRefused(t, structure, a, engine.ErrMappedInUse, "trusted admission")

		// Restored: the full request upgrades the admission.
		rewriteFrom(t, a, orig)
		up := mappedMust(t, full, a, full)
		if got := prepAnswers(t, prepList(t, up), c); !bytes.Equal(got, want) {
			t.Fatal("upgraded store answers differ")
		}
		if *s1.Prepared() != info1 {
			t.Fatal("an earlier store's info changed after the upgrade")
		}
		mappedMust(t, structure, a, full)
		mappedMust(t, trusted, a, trusted)
		mappedMust(t, full, a, full)

	case "tm-nofallback":
		// A full open of a content-altered file fails and admits nothing;
		// nothing is retried as trusted.
		if st, err := mappedOpen(full, a3); st != nil || !errors.Is(err, prepared.ErrIntegrity) || errors.Is(err, engine.ErrMappedInUse) {
			t.Fatalf("full on altered content: %v", err)
		}
		mappedMust(t, full, a, full) // the registry was left empty
		mappedRefused(t, trusted, a3, engine.ErrMappedInUse, "not the admitted artifact")

	case "tm-race-mixed":
		mappedMust(t, trusted, a, trusted)
		type res struct {
			level prepared.Level
			st    *engine.Store
			info  engine.PreparedInfo
			err   error
		}
		var levels []prepared.Level
		for i := 0; i < 6; i++ {
			levels = append(levels, full, trusted, structure)
		}
		out := make([]res, len(levels))
		var start, wg sync.WaitGroup
		start.Add(1)
		for i, lvl := range levels {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				st, err := mappedOpen(lvl, a)
				out[i] = res{level: lvl, st: st, err: err}
				if st != nil {
					out[i].info = *st.Prepared()
				}
			}()
		}
		start.Done()
		wg.Wait()
		served := 0
		for _, r := range out {
			switch {
			case r.level == full && (r.err != nil || r.info.Level != full || r.info.StampBasis != engine.StampBasisVerified):
				t.Fatalf("full: %v %+v", r.err, r.info)
			case r.level == trusted && (r.err != nil || r.info.Level != trusted || r.info.StampBasis != engine.StampBasisDeclared):
				t.Fatalf("trusted: %v %+v", r.err, r.info)
			case r.level == structure && r.err == nil:
				if r.info.Level != full {
					t.Fatalf("structure served as %v", r.info.Level)
				}
				served++
			case r.level == structure && !errors.Is(r.err, engine.ErrMappedInUse):
				t.Fatalf("structure: %v", r.err)
			}
		}
		for _, r := range out {
			if r.st != nil && *r.st.Prepared() != r.info {
				t.Fatal("a store's info changed after it was returned")
			}
		}
		mappedMust(t, structure, a, full) // the full requests upgraded the admission
		t.Logf("structure requests served after the upgrade: %d of 6", served)

	case "tm-race-first":
		var fs, ts *engine.Store
		var fe, te error
		var start, wg sync.WaitGroup
		start.Add(1)
		wg.Add(2)
		go func() { defer wg.Done(); start.Wait(); fs, fe = mappedOpen(full, a) }()
		go func() { defer wg.Done(); start.Wait(); ts, te = mappedOpen(trusted, a) }()
		start.Done()
		wg.Wait()
		if fe != nil || te != nil || fs.Prepared().Level != full || ts.Prepared().Level != trusted {
			t.Fatalf("full %v, trusted %v", fe, te)
		}
		mappedMust(t, structure, a, full) // whichever order, the admission ends full
	}
}
