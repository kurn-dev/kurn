package engine_test

// Regression tests: Store-managed list versions are
// content-addressed (base.jsonl hash + journal byte position) and therefore
// restart-stable — pre-fix they were process-local gen counters that
// collided across restarts while saying nothing about disk state.

import (
	"regexp"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

var storeVersionRE = regexp.MustCompile(`^([0-9a-f]{12}|empty)@\d+\+j\d+$`)

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
