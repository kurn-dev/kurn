package engine_test

// Regression tests: Keys that analyze to "" are dropped
// silently, and an entry whose keys ALL collapse still occupies an ordinal
// and counts in Stats() while being unfindable — BuildStats makes the loss
// visible.

import (
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestBuildStatsCountsAnalyzedAwayKeys(t *testing.T) {
	l, err := engine.NewList("codes", engine.ListConfig{
		// strip_punctuation + trim collapse punctuation-only keys to "".
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "strip_punctuation", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		{ID: "e1", Keys: []string{"...", "ok"}}, // one key dropped, one survives
		{ID: "e2", Keys: []string{"!!!"}},       // all keys collapse: keyless
		{ID: "e3", Keys: []string{"fine"}},      // clean
	}); err != nil {
		t.Fatal(err)
	}

	dropped, keyless := l.BuildStats()
	if dropped != 2 || keyless != 1 {
		t.Fatalf("BuildStats = (%d, %d), want (2, 1)", dropped, keyless)
	}
	// The keyless entry is exactly the invisible-loss case: counted in
	// Stats() entries, unfindable by any query.
	if e, _, _ := l.Stats(); e != 3 {
		t.Fatalf("Stats entries = %d, want 3 (keyless entry still occupies an ordinal)", e)
	}
	if c := l.Query("!!!", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("keyless entry matched: %+v", c)
	}

	// Overlay builds count too (summed with base).
	if err := l.Upsert([]engine.Entry{{ID: "e4", Keys: []string{"???", "---"}}}); err != nil {
		t.Fatal(err)
	}
	dropped, keyless = l.BuildStats()
	if dropped != 4 || keyless != 2 {
		t.Fatalf("BuildStats after keyless upsert = (%d, %d), want (4, 2)", dropped, keyless)
	}

	// Compact folds everything into one base; counters recompute over the
	// fold, not accumulate across builds.
	if err := l.Compact(); err != nil {
		t.Fatal(err)
	}
	dropped, keyless = l.BuildStats()
	if dropped != 4 || keyless != 2 {
		t.Fatalf("BuildStats after compact = (%d, %d), want (4, 2)", dropped, keyless)
	}
}
