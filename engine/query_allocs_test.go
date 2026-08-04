package engine_test

import (
	"fmt"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

// Hard allocation ceilings for the query hot path. The gram-streaming rework
// (profiled at ~1 MB and hundreds of allocations per query before it) brought
// the path to a few dozen small allocations; these bounds are set with ~2×
// headroom so incidental churn passes but a reintroduced per-gram or
// per-candidate-key allocation pattern fails loudly.
func TestQueryAllocs(t *testing.T) {
	build := func(t *testing.T, keysPerEntry int) *engine.List {
		l, err := engine.NewList("people", engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
			Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
		})
		if err != nil {
			t.Fatal(err)
		}
		first := []string{"elena", "marcus", "john", "jane", "dana", "omar", "nils", "mira", "tomas", "kiran"}
		last := []string{"vasquez", "chen", "smith", "doe", "kovak", "reyes", "berg", "solano", "okafor", "lindqvist"}
		var entries []engine.Entry
		for i, f := range first {
			for j, ln := range last {
				keys := []string{f + " " + ln}
				for k := 1; k < keysPerEntry; k++ {
					keys = append(keys, fmt.Sprintf("%s %s alias%d", ln, f, k))
				}
				entries = append(entries, engine.Entry{
					ID:   fmt.Sprintf("p%d-%d", i, j),
					Keys: keys,
				})
			}
		}
		if err := l.Replace(entries); err != nil {
			t.Fatal(err)
		}
		return l
	}

	t.Run("single-key", func(t *testing.T) {
		l := build(t, 1)
		allocs := testing.AllocsPerRun(200, func() {
			if c := l.Query("elena vasquze", engine.QueryOpts{}); len(c) == 0 {
				t.Fatal("no hits")
			}
		})
		const ceiling = 60
		if allocs > ceiling {
			t.Fatalf("query path allocates %.0f/op, ceiling %d", allocs, ceiling)
		}
	})

	// Attribution-heavy: low threshold + aliases makes every candidate pay the
	// per-key attribution walk — the profiled hot spot.
	t.Run("attribution-heavy", func(t *testing.T) {
		l := build(t, 3)
		allocs := testing.AllocsPerRun(200, func() {
			if c := l.Query("ana vasquez", engine.QueryOpts{Threshold: 0.3}); len(c) == 0 {
				t.Fatal("no hits")
			}
		})
		// Normalize + StripSpaces still allocate per candidate key by design
		// (multi-key entries only — single-key entries skip attribution);
		// the ceiling scales with candidates × keys, not grams. Measured 431.
		const ceiling = 500
		if allocs > ceiling {
			t.Fatalf("attribution-heavy query allocates %.0f/op, ceiling %d", allocs, ceiling)
		}
	})
}
