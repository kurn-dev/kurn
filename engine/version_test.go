package engine_test

// Regression tests: Store-managed list versions are
// content-addressed (base.jsonl hash + journal byte position) and therefore
// restart-stable — pre-fix they were process-local gen counters that
// collided across restarts while saying nothing about disk state.

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

// Store version stamps: "<baseID>@<entries>+j0" for an empty journal, or
// "+j<bytes>.<contentHash>" once mutations are journaled — the byte position
// is replay depth, the hash is the identity.
var storeVersionRE = regexp.MustCompile(`^([0-9a-f]{12}|empty)@\d+\+j(0|[1-9]\d*\.[0-9a-f]{12})$`)

func openVersion(t *testing.T, dir, list string) string {
	t.Helper()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Skipped) != 0 || len(st.Quarantined) != 0 {
		t.Fatalf("degraded open: skipped %+v quarantined %+v", st.Skipped, st.Quarantined)
	}
	l, ok := st.List(list)
	if !ok {
		t.Fatalf("list %s missing", list)
	}
	return l.Version()
}

func TestStoreVersionsAreContentAddressedAndRestartStable(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}

	// Replace: hash@N+j0.
	if err := st.Replace("codes", []engine.Entry{
		{ID: "c1", Keys: []string{"AA-1"}}, {ID: "c2", Keys: []string{"BB-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	l, _ := st.List("codes")
	v1 := l.Version()
	if !storeVersionRE.MatchString(v1) {
		t.Fatalf("version %q is not content-addressed", v1)
	}
	if openVersion(t, dir, "codes") != v1 {
		t.Fatalf("version changed across restart: %q", v1)
	}

	// Upsert: same base hash, journal position > 0; still restart-stable.
	if err := st.Upsert("codes", []engine.Entry{{ID: "c3", Keys: []string{"CC-3"}}}); err != nil {
		t.Fatal(err)
	}
	v2 := l.Version()
	if !storeVersionRE.MatchString(v2) || v2 == v1 {
		t.Fatalf("post-upsert version %q: want new content-addressed stamp (was %q)", v2, v1)
	}
	if openVersion(t, dir, "codes") != v2 {
		t.Fatalf("post-upsert version not restart-stable: %q", v2)
	}

	// Compact folds to a new base: new hash, j0; restart-stable.
	if err := st.Compact("codes"); err != nil {
		t.Fatal(err)
	}
	v3 := l.Version()
	if !storeVersionRE.MatchString(v3) || v3 == v2 {
		t.Fatalf("post-compact version %q (was %q)", v3, v2)
	}
	if openVersion(t, dir, "codes") != v3 {
		t.Fatalf("post-compact version not restart-stable: %q", v3)
	}

	// Content-addressed means content-determined: replacing with the SAME
	// two entries reproduces v1 exactly, on a list that has since diverged.
	if err := st.Replace("codes", []engine.Entry{
		{ID: "c1", Keys: []string{"AA-1"}}, {ID: "c2", Keys: []string{"BB-2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := l.Version(); got != v1 {
		t.Fatalf("same content produced version %q, want %q", got, v1)
	}

	// A fresh empty store list stamps "empty@0" (NewList) and journaled
	// mutations extend it with the journal position.
	if _, err := st.CreateList("fresh", exactCfg()); err != nil {
		t.Fatal(err)
	}
	lf, _ := st.List("fresh")
	if err := st.Upsert("fresh", []engine.Entry{{ID: "x", Keys: []string{"y"}}}); err != nil {
		t.Fatal(err)
	}
	vf := lf.Version()
	if !storeVersionRE.MatchString(vf) {
		t.Fatalf("fresh-list version %q not content-addressed", vf)
	}
	if openVersion(t, dir, "fresh") != vf {
		t.Fatalf("fresh-list version not restart-stable: %q", vf)
	}
}

// A version is evidence of WHICH data answered, so two journals holding
// different mutations must never share a version — even when their encoded
// byte lengths are identical. Pre-fix the overlay component was the journal
// byte position alone, and these two stores stamped the same version while
// answering the same query differently.
func TestEqualLengthJournalsGetDistinctVersions(t *testing.T) {
	mkStore := func(key string) (*engine.List, string) {
		t.Helper()
		dir := t.TempDir()
		st, err := engine.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateList("codes", exactCfg()); err != nil {
			t.Fatal(err)
		}
		if err := st.Upsert("codes", []engine.Entry{{ID: "e1", Keys: []string{key}}}); err != nil {
			t.Fatal(err)
		}
		l, _ := st.List("codes")
		return l, dir
	}
	la, dirA := mkStore("alpha")
	lb, dirB := mkStore("bravo")

	// Validity guard: the scenario only demonstrates anything if the two
	// journals really are the same length. If the record encoding changes,
	// fail here with the sizes rather than passing vacuously.
	sa := journalSize(t, dirA)
	sb := journalSize(t, dirB)
	if sa != sb {
		t.Fatalf("test setup broke: journal sizes %d vs %d must be equal for the scenario to mean anything", sa, sb)
	}

	va, vb := la.Version(), lb.Version()
	if !storeVersionRE.MatchString(va) || !storeVersionRE.MatchString(vb) {
		t.Fatalf("versions %q / %q not content-addressed", va, vb)
	}
	if va == vb {
		t.Fatalf("different journal content, same version %q — the version is not evidence of the data", va)
	}
	// The data really does differ: same query, different answers.
	if got := len(la.Query("alpha", engine.QueryOpts{})); got != 1 {
		t.Fatalf("store A alpha hits = %d, want 1", got)
	}
	if got := len(lb.Query("alpha", engine.QueryOpts{})); got != 0 {
		t.Fatalf("store B alpha hits = %d, want 0", got)
	}
	// Both stamps are restart-stable: replay reproduces the same identity.
	if got := openVersion(t, dirA, "codes"); got != va {
		t.Fatalf("store A version %q -> %q across restart", va, got)
	}
	if got := openVersion(t, dirB, "codes"); got != vb {
		t.Fatalf("store B version %q -> %q across restart", vb, got)
	}
}

// The same claim for the nastier pair: an upsert and a delete encoded to the
// same byte length, on top of identical prior journals. One store gained an
// entry, the other lost one; equal versions here would equate opposite
// states.
func TestEqualLengthUpsertAndDeleteGetDistinctVersions(t *testing.T) {
	// victimID and the pad entry are tuned so the delete record and the
	// upsert record serialize to the same length; the size guard below keeps
	// the test honest if the encoding ever changes.
	victimID := "vvvvvvvvvvvvvvvvvvvvvvvvv" // 25 chars
	mkStore := func(mutate func(st *engine.Store) error) (*engine.List, string) {
		t.Helper()
		dir := t.TempDir()
		st, err := engine.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateList("codes", exactCfg()); err != nil {
			t.Fatal(err)
		}
		if err := st.Upsert("codes", []engine.Entry{{ID: victimID, Keys: []string{"match-me"}}}); err != nil {
			t.Fatal(err)
		}
		if err := mutate(st); err != nil {
			t.Fatal(err)
		}
		l, _ := st.List("codes")
		return l, dir
	}
	la, dirA := mkStore(func(st *engine.Store) error { return st.Delete("codes", victimID) })
	lb, dirB := mkStore(func(st *engine.Store) error {
		return st.Upsert("codes", []engine.Entry{{ID: "a", Keys: []string{"kk"}}})
	})

	sa := journalSize(t, dirA)
	sb := journalSize(t, dirB)
	if sa != sb {
		t.Fatalf("test setup broke: journal sizes %d vs %d must be equal for the scenario to mean anything", sa, sb)
	}

	va, vb := la.Version(), lb.Version()
	if va == vb {
		t.Fatalf("a delete and an upsert produced the same version %q", va)
	}
	if got := len(la.Query("match-me", engine.QueryOpts{})); got != 0 {
		t.Fatalf("store A: deleted entry still answers (%d hits)", got)
	}
	if got := len(lb.Query("match-me", engine.QueryOpts{})); got != 1 {
		t.Fatalf("store B: match-me hits = %d, want 1", got)
	}
	if got := openVersion(t, dirA, "codes"); got != va {
		t.Fatalf("store A version %q -> %q across restart", va, got)
	}
	if got := openVersion(t, dirB, "codes"); got != vb {
		t.Fatalf("store B version %q -> %q across restart", vb, got)
	}
}

func journalSize(t *testing.T, dir string) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, "codes", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
