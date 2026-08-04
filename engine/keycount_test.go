package engine_test

// List.KeyCount() — live raw-key count (base − tombstoned +
// overlay), the engine-side basis for the max_total_keys quota.

import (
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestKeyCount(t *testing.T) {
	l, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := l.KeyCount(); n != 0 {
		t.Fatalf("empty list KeyCount = %d, want 0", n)
	}

	// Base: 3 entries, 4 keys.
	if err := l.Replace([]engine.Entry{
		{ID: "a", Keys: []string{"k1", "k2"}},
		{ID: "b", Keys: []string{"k3"}},
		{ID: "c", Keys: []string{"k4"}},
	}); err != nil {
		t.Fatal(err)
	}
	if n := l.KeyCount(); n != 4 {
		t.Fatalf("base KeyCount = %d, want 4", n)
	}

	// Overlay add: +2.
	if err := l.Upsert([]engine.Entry{{ID: "d", Keys: []string{"k5", "k6"}}}); err != nil {
		t.Fatal(err)
	}
	if n := l.KeyCount(); n != 6 {
		t.Fatalf("after add KeyCount = %d, want 6", n)
	}

	// Upsert superseding a base entry: a's 2 keys credited, 3 new counted.
	if err := l.Upsert([]engine.Entry{{ID: "a", Keys: []string{"n1", "n2", "n3"}}}); err != nil {
		t.Fatal(err)
	}
	if n := l.KeyCount(); n != 7 { // 4 - 2 + 2 + 3
		t.Fatalf("after supersede KeyCount = %d, want 7", n)
	}

	// Delete a base entry: −1.
	if err := l.Delete("b"); err != nil {
		t.Fatal(err)
	}
	if n := l.KeyCount(); n != 6 {
		t.Fatalf("after delete KeyCount = %d, want 6", n)
	}

	// Compact folds everything; count is invariant.
	if err := l.Compact(); err != nil {
		t.Fatal(err)
	}
	if n := l.KeyCount(); n != 6 {
		t.Fatalf("after compact KeyCount = %d, want 6", n)
	}
}

// The artifact fast path constructs segments without buildSegment — the
// counter must survive a store reopen through it.
func TestKeyCountSurvivesArtifactReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("codes", exactCfg()); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("codes", []engine.Entry{
		{ID: "a", Keys: []string{"k1", "k2"}},
		{ID: "b", Keys: []string{"k3"}},
	}); err != nil {
		t.Fatal(err)
	}
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := st2.List("codes")
	if n := l.KeyCount(); n != 3 {
		t.Fatalf("KeyCount after artifact reopen = %d, want 3", n)
	}
}
