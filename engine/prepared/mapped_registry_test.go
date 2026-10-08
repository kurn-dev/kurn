//go:build unix

package prepared

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"unsafe"
)

// These tests drive fresh mappedRegistry values, so each starts empty and
// they can run in one process; the process-wide registry (procMapped) is
// the same type. Mappings they admit are never unmapped, as in production.

func writeArtifact(t testing.TB, dir, name string, buf []byte, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

// sourceOffset returns the file offset of the first letter of the build
// section's source string.
func sourceOffset(t testing.TB, buf []byte) int {
	t.Helper()
	s := rawFind(buf, KindBuild)
	i := bytes.Index(buf[s.off:s.end()], []byte(`"source":"`))
	if i < 0 {
		t.Fatalf("no source in the build section: %s", buf[s.off:s.end()])
	}
	return int(s.off) + i + len(`"source":"`)
}

// openB is open reporting only whether this call admitted.
func (r *mappedRegistry) openB(path string, level Level, accept func(*View, *Verified) error) (*View, *Verified, bool, error) {
	v, ver, o, err := r.open(path, level, accept)
	return v, ver, o == outAdmitted, err
}

func bufAddr(b []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(b))) }

// patchInPlace rewrites one byte of the file at off through a fresh
// read-write descriptor (same inode, same length), restoring mode 0444.
func patchInPlace(t *testing.T, path string, off int64, b byte) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{b}, off); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
}

func TestMappedAdmitAndReuse(t *testing.T) {
	dir := t.TempDir()
	a := writeArtifact(t, dir, "a.kart", artifactT(t, "ngram", 200), 0o444)
	b := writeArtifact(t, dir, "b.kart", artifactT(t, "exact", 200), 0o444)
	var r mappedRegistry

	v1, ver1, admitted, err := r.openB(a, LevelFull, nil)
	if err != nil || !admitted {
		t.Fatalf("admission: admitted %v err %v", admitted, err)
	}
	if !isAligned(v1.buf) || bufAddr(v1.buf)%uintptr(os.Getpagesize()) != 0 {
		t.Fatalf("mapping at %#x is not page-aligned", bufAddr(v1.buf))
	}
	if ver1.Level != LevelFull || ver1.ContainerSHA256 == "" || ver1.ContainerSHA256 != r.adm.sum {
		t.Fatalf("admitted identity %+v, registry %q", ver1, r.adm.sum)
	}
	want, err := Verify(a, LevelFull)
	if err != nil || want.ContainerSHA256 != ver1.ContainerSHA256 || want.BaseID != ver1.BaseID {
		t.Fatalf("heap verification %+v %v, mapped %+v", want, err, ver1)
	}

	v2, ver2, admitted, err := r.openB(a, LevelFull, nil)
	if err != nil || admitted {
		t.Fatalf("reuse: admitted %v err %v", admitted, err)
	}
	if v2 == v1 || bufAddr(v2.buf) != bufAddr(v1.buf) || len(v2.buf) != len(v1.buf) {
		t.Fatalf("reuse did not serve a new View over the admitted mapping: %#x vs %#x", bufAddr(v2.buf), bufAddr(v1.buf))
	}
	if *ver2 != *ver1 {
		t.Fatalf("reuse verified %+v, admission %+v", ver2, ver1)
	}

	// A structure-level open is served from the admitted mapping, and
	// reports the full verification the reuse check ran.
	v3, ver3, _, err := r.openB(a, LevelStructure, nil)
	if err != nil || bufAddr(v3.buf) != bufAddr(v1.buf) || ver3.Level != LevelFull || ver3.ContainerSHA256 != ver1.ContainerSHA256 {
		t.Fatalf("structure-level reuse: %+v %v", ver3, err)
	}

	// Any other artifact is refused, however valid, and nothing changes.
	for _, lvl := range []Level{LevelFull, LevelStructure} {
		if _, _, _, err := r.openB(b, lvl, nil); !errors.Is(err, ErrMappedInUse) {
			t.Fatalf("different artifact at %v: %v, want ErrMappedInUse", lvl, err)
		}
	}
	if bufAddr(r.adm.buf) != bufAddr(v1.buf) {
		t.Fatal("admission changed")
	}
	// A hard link is the same inode: the same artifact.
	link := filepath.Join(dir, "link.kart")
	if err := os.Link(a, link); err != nil {
		t.Fatal(err)
	}
	if v, _, _, err := r.openB(link, LevelFull, nil); err != nil || bufAddr(v.buf) != bufAddr(v1.buf) {
		t.Fatalf("hard link to the admitted inode: %v", err)
	}
	// A copy with identical bytes is another inode: refused.
	c := writeArtifact(t, dir, "copy.kart", artifactT(t, "ngram", 200), 0o444)
	if _, _, _, err := r.openB(c, LevelFull, nil); !errors.Is(err, ErrMappedInUse) {
		t.Fatalf("byte-identical copy: %v, want ErrMappedInUse", err)
	}
	// Even a refused open with write bits on another file is ErrMappedInUse
	// once something is admitted.
	w := writeArtifact(t, dir, "w.kart", artifactT(t, "ngram", 200), 0o644)
	if _, _, _, err := r.openB(w, LevelFull, nil); !errors.Is(err, ErrMappedInUse) {
		t.Fatalf("writable other file: %v, want ErrMappedInUse", err)
	}
}

// Refusals before admission leave the registry empty, so a later valid
// open still admits.
func TestMappedRefusalsLeaveRegistryEmpty(t *testing.T) {
	dir := t.TempDir()
	good := artifactT(t, "ngram", 100)
	stale := clone(good)
	stale[sourceOffset(t, stale)] = 'X' // build section changed, digests stale
	var r mappedRegistry
	for _, tc := range []struct {
		name  string
		path  string
		level Level
		is    error // nil: a plain error, none of the sentinels
	}{
		{"write bits 0644", writeArtifact(t, dir, "w644.kart", good, 0o644), LevelFull, ErrNotMappable},
		{"group write bit 0464", writeArtifact(t, dir, "w464.kart", good, 0o464), LevelFull, ErrNotMappable},
		{"other write bit 0446", writeArtifact(t, dir, "w446.kart", good, 0o446), LevelFull, ErrNotMappable},
		{"directory", dir, LevelFull, ErrNotMappable},
		{"character device", os.DevNull, LevelFull, ErrNotMappable},
		{"zero length", writeArtifact(t, dir, "empty.kart", nil, 0o444), LevelFull, ErrMalformed},
		{"stale digest", writeArtifact(t, dir, "stale.kart", stale, 0o444), LevelFull, ErrIntegrity},
		{"truncated", writeArtifact(t, dir, "trunc.kart", good[:len(good)/2], 0o444), LevelFull, ErrMalformed},
		{"structure-level admission", writeArtifact(t, dir, "struct.kart", good, 0o444), LevelStructure, ErrNotMappable},
		{"unknown level", writeArtifact(t, dir, "lvl.kart", good, 0o444), Level(9), nil},
	} {
		v, ver, admitted, err := r.openB(tc.path, tc.level, nil)
		if err == nil || v != nil || ver != nil || admitted {
			t.Fatalf("%s: view %v verified %v admitted %v err %v, want a refusal", tc.name, v, ver, admitted, err)
		}
		if errors.Is(err, ErrMappedInUse) {
			t.Fatalf("%s: %v wraps ErrMappedInUse with nothing admitted", tc.name, err)
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Fatalf("%s: %v, want %v", tc.name, err, tc.is)
		}
		if tc.is == nil && (errors.Is(err, ErrMalformed) || errors.Is(err, ErrIntegrity) || errors.Is(err, ErrIncompatible) || errors.Is(err, ErrNotMappable)) {
			t.Fatalf("%s: %v, want a plain error", tc.name, err)
		}
		if r.adm != nil {
			t.Fatalf("%s: refused open left an admission", tc.name)
		}
		t.Logf("%s: %v", tc.name, err)
	}
	p := writeArtifact(t, dir, "good.kart", good, 0o444)
	if _, _, admitted, err := r.openB(p, LevelFull, nil); err != nil || !admitted {
		t.Fatalf("valid open after refusals: admitted %v err %v", admitted, err)
	}
}

// Reuse must match the admitted identity, not merely verify.
func TestMappedReuseRequiresAdmittedIdentity(t *testing.T) {
	dir := t.TempDir()
	buf := artifactT(t, "ngram", 200)
	p := writeArtifact(t, dir, "a.kart", buf, 0o444)
	var r mappedRegistry
	v1, _, _, err := r.openB(p, LevelFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	reuse := func() error { _, _, _, err := r.openB(p, LevelFull, nil); return err }

	// Write bits on the admitted inode.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reuse(); !errors.Is(err, ErrMappedInUse) {
		t.Fatalf("admitted file with write bits: %v", err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := reuse(); err != nil {
		t.Fatalf("write bits cleared again: %v", err)
	}

	// Same-inode, same-length change: visible through MAP_SHARED, and the
	// mapping no longer verifies to the admitted identity.
	// A letter of the build section's source string: the section stays
	// canonical, so only digests can tell.
	off := int64(sourceOffset(t, buf))
	flip := buf[off] ^ 0x20
	patchInPlace(t, p, off, flip)
	if v1.buf[off] != flip {
		t.Fatal("in-place change not visible through the shared mapping")
	}
	err = reuse()
	if !errors.Is(err, ErrMappedInUse) || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("same-inode change: %v, want ErrMappedInUse wrapping ErrIntegrity", err)
	}
	t.Logf("same-inode change: %v", err)

	// Changed and rehashed so it verifies on its own: still refused,
	// because it is not the admitted identity.
	patched := clone(buf)
	patched[off] = flip
	rehash(patched)
	if _, _, err := VerifyBytes(patched, LevelFull); err != nil {
		t.Fatalf("rehashed change does not verify on its own: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, patched, 0); err != nil { // O_TRUNC then write: same inode, same final length
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	err = reuse()
	if !errors.Is(err, ErrMappedInUse) || errors.Is(err, ErrIntegrity) {
		t.Fatalf("verifying but different content: %v, want ErrMappedInUse without ErrIntegrity", err)
	}
	t.Logf("rehashed same-inode change: %v", err)

	// Restored to the admitted bytes: the identity matches again.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := reuse(); err != nil {
		t.Fatalf("restored content: %v", err)
	}

	// Grown: refused on size, before the mapping is read.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := reuse(); !errors.Is(err, ErrMappedInUse) {
		t.Fatalf("grown file: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, int64(len(buf))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := reuse(); err != nil {
		t.Fatalf("restored length: %v", err)
	}

	// Replaced by rename (new inode, same bytes): refused.
	tmp := writeArtifact(t, dir, "next.kart", buf, 0o444)
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if err := reuse(); !errors.Is(err, ErrMappedInUse) {
		t.Fatalf("replaced file: %v", err)
	}
	// The admitted mapping still reads the unlinked inode.
	if _, _, err := VerifyBytes(r.adm.buf, LevelFull); err != nil {
		t.Fatalf("admitted mapping after its path was replaced: %v", err)
	}
}

func TestMappedConcurrentAdmission(t *testing.T) {
	dir := t.TempDir()
	a := writeArtifact(t, dir, "a.kart", artifactT(t, "ngram", 150), 0o444)
	b := writeArtifact(t, dir, "b.kart", artifactT(t, "exact", 150), 0o444)
	const workers = 16
	type res struct {
		path     string
		v        *View
		admitted bool
		err      error
	}
	race := func(r *mappedRegistry, pathOf func(int) string) []res {
		out := make([]res, workers)
		var start, wg sync.WaitGroup
		start.Add(1)
		for i := range out {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				p := pathOf(i)
				v, _, adm, err := r.openB(p, LevelFull, nil)
				out[i] = res{p, v, adm, err}
			}()
		}
		start.Done()
		wg.Wait()
		return out
	}
	for iter := 0; iter < 10; iter++ {
		var r mappedRegistry
		admissions := 0
		for _, x := range race(&r, func(int) string { return a }) {
			if x.err != nil {
				t.Fatalf("same file: %v", x.err)
			}
			if x.admitted {
				admissions++
			}
			if bufAddr(x.v.buf) != bufAddr(r.adm.buf) {
				t.Fatal("an open was served from another mapping")
			}
		}
		if admissions != 1 {
			t.Fatalf("same file: %d admissions", admissions)
		}

		var r2 mappedRegistry
		out := race(&r2, func(i int) string { return []string{a, b}[i%2] })
		admissions = 0
		winner := r2.adm.path
		for _, x := range out {
			if x.admitted {
				admissions++
			}
			switch {
			case x.path == winner && x.err != nil:
				t.Fatalf("winning file %s refused: %v", x.path, x.err)
			case x.path != winner && !errors.Is(x.err, ErrMappedInUse):
				t.Fatalf("losing file %s: %v, want ErrMappedInUse", x.path, x.err)
			case x.path == winner && bufAddr(x.v.buf) != bufAddr(r2.adm.buf):
				t.Fatal("winning file served from another mapping")
			}
		}
		if admissions != 1 {
			t.Fatalf("two files: %d admissions", admissions)
		}
	}
}

// The mapping is read-only: a write into a payload faults. SetPanicOnFault
// turns the fault into a recoverable panic here; the engine's subprocess
// test lets it crash the process.
func TestMappedPayloadWriteFaults(t *testing.T) {
	p := writeArtifact(t, t.TempDir(), "a.kart", artifactT(t, "ngram", 50), 0o444)
	var r mappedRegistry
	v, _, _, err := r.openB(p, LevelFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	faulted := 0
	for i := 0; i < v.Len(); i++ {
		pl := v.Payload(i)
		if len(pl) == 0 {
			continue
		}
		func() {
			defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
			defer func() {
				e := recover()
				re, ok := e.(runtime.Error)
				if !ok {
					t.Fatalf("record %d: write into a mapped payload did not fault (recovered %v)", i, e)
				}
				ad, ok := e.(interface{ Addr() uintptr })
				if !ok || ad.Addr() != bufAddr(pl) {
					t.Fatalf("record %d: fault %v not at the payload", i, re)
				}
				faulted++
			}()
			pl[0] = 'X'
		}()
	}
	if faulted == 0 {
		t.Fatal("no payload to write")
	}
	if _, _, err := VerifyBytes(v.buf, LevelFull); err != nil {
		t.Fatalf("mapping changed: %v", err)
	}
}

// accept runs after verification and before admission: an error from it
// is returned unchanged, the mapping is not admitted, and a later open
// admits. On reuse it runs after the identity checks.
func TestMappedAcceptBeforeAdmit(t *testing.T) {
	dir := t.TempDir()
	a := writeArtifact(t, dir, "a.kart", artifactT(t, "ngram", 100), 0o444)
	var r mappedRegistry
	refuse := fmt.Errorf("%w: engine refuses", ErrIncompatible)
	var seen *View
	for i := 0; i < 2; i++ {
		v, ver, admitted, err := r.openB(a, LevelFull, func(v *View, ver *Verified) error {
			if ver.Level != LevelFull || ver.ContainerSHA256 == "" || r.adm != nil {
				t.Errorf("accept ran with %+v, admitted %v", ver, r.adm != nil)
			}
			seen = v
			return refuse
		})
		if err != refuse || v != nil || ver != nil || admitted || r.adm != nil {
			t.Fatalf("refused acceptance: err %v (want it unchanged) admitted %v registry %v", err, admitted, r.adm != nil)
		}
	}
	if seen == nil {
		t.Fatal("accept was not called")
	}
	seen = nil
	calls := 0
	accept := func(v *View, ver *Verified) error {
		calls++
		if v.Len() != 100 || ver.ContainerSHA256 == "" {
			t.Errorf("accept got %d records, %+v", v.Len(), ver)
		}
		return nil
	}
	v1, _, admitted, err := r.openB(a, LevelFull, accept)
	if err != nil || !admitted || calls != 1 {
		t.Fatalf("admission after refused acceptances: %v admitted %v calls %d", err, admitted, calls)
	}
	// Reuse: the identity checks, then accept.
	v2, _, _, err := r.openB(a, LevelStructure, accept)
	if err != nil || calls != 2 || bufAddr(v2.buf) != bufAddr(v1.buf) {
		t.Fatalf("reuse: %v calls %d", err, calls)
	}
	if _, _, _, err := r.openB(a, LevelFull, func(*View, *Verified) error { return refuse }); err != refuse || r.adm == nil {
		t.Fatalf("refused reuse: %v, admitted %v", err, r.adm != nil)
	}
	// Identity checks come first: a different file never reaches accept.
	b := writeArtifact(t, dir, "b.kart", artifactT(t, "exact", 100), 0o444)
	if _, _, _, err := r.openB(b, LevelFull, func(*View, *Verified) error { t.Error("accept ran for a refused file"); return nil }); !errors.Is(err, ErrMappedInUse) {
		t.Fatalf("different file: %v", err)
	}
}
