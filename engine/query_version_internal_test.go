package engine

import (
	"context"
	"testing"
)

// A query's version is its evidence: candidates and version must come from
// the SAME snapshot. This drives a mutation deterministically into the
// window between snapshot acquisition and query execution — the answer must
// keep the acquired snapshot's version and content, not the mutation's.
func TestQueryVersionComesFromTheAnsweringSnapshot(t *testing.T) {
	l := newPersonListInternal(t)
	if err := l.Replace([]Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	vBefore := l.Version()

	mutated := false
	testHookQuerySnapshot = func() {
		if mutated {
			return
		}
		mutated = true
		if err := l.Upsert([]Entry{{ID: "p2", Keys: []string{"Dana Kovak"}}}); err != nil {
			t.Errorf("hook upsert: %v", err)
		}
	}
	defer func() { testHookQuerySnapshot = nil }()

	// The query races the hook's mutation by construction. Querying for the
	// MUTATION's key makes any leakage visible in the candidates too: the
	// acquired snapshot cannot contain it.
	cands, ver := l.QueryVersioned(context.Background(), "dana kovak", QueryOpts{})
	if ver != vBefore {
		t.Errorf("answer stamped version %q, want the acquired snapshot's %q", ver, vBefore)
	}
	if len(cands) != 0 {
		t.Errorf("answer contains the concurrent mutation's entry: %+v", cands)
	}

	// Validity guard: the mutation really did land and change the version —
	// otherwise the assertions above pass vacuously.
	if !mutated {
		t.Fatal("hook never ran")
	}
	if now := l.Version(); now == vBefore {
		t.Fatalf("mutation did not change the version (%q); the test proved nothing", now)
	}
	if c := l.Query("dana kovak", QueryOpts{}); len(c) != 1 || c[0].EntryID != "p2" {
		t.Fatalf("mutation not visible to a later query: %+v", c)
	}
}
