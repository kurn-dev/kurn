package engine_test

import (
	"fmt"
	"runtime"
	"runtime/debug"
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

// TestFloodChargeCoversAllocation: admission control admits queries against
// ScratchBytesFor, so that model must never charge LESS than a query
// actually allocates — an undercharge multiplied by admitted concurrency is
// an OOM, the exact defect this pins: hit collection used to materialize
// every qualifying ordinal (~16 B each) while the model charged only the
// 4 B/ordinal accumulator, a sixfold undercharge on flood shapes. The
// corpus shares one hot gram across every entry and the query asks for
// topK=1: every ordinal qualifies, and everything the query allocates must
// still fit under the charge.
func TestFloodChargeCoversAllocation(t *testing.T) {
	if raceEnabled {
		// The model targets production allocation. Race instrumentation
		// deliberately defeats sync.Pool reuse, so the pooled per-ordinal
		// scratch re-allocates every query and its growth chain double-
		// counts — the measurement stops describing what the governor
		// bounds.
		t.Skip("allocation measurement is meaningless under the race detector")
	}
	const n = 100_000
	l, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, n)
	for i := range entries {
		entries[i] = engine.Entry{ID: fmt.Sprintf("c%06d", i), Keys: []string{fmt.Sprintf("zq%06d", i)}}
	}
	if err := l.Replace(entries); err != nil {
		t.Fatal(err)
	}

	// The query "zq" grams to the one posting every entry shares: a full
	// flood. Warm once so the pooled per-ordinal scratch (counts + touched)
	// is allocated and, with GC disabled, stays pooled — the measured run
	// then shows exactly the per-QUERY allocations the model's non-pooled
	// terms must cover. (With a cold pool the scratch itself is allocated
	// too, still under the model's 8 B/ordinal term.)
	// GC off BEFORE the warm-up: a collection between warm-up and
	// measurement could empty the scratch pool and bill the re-allocation
	// (plus its growth chain) to the measured query.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	opts := engine.QueryOpts{TopK: 1}
	if c := l.Query("zq", opts); len(c) != 1 {
		t.Fatalf("flood query returned %d candidates, want 1", len(c))
	}

	charge := l.ScratchBytesFor(1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if c := l.Query("zq", opts); len(c) != 1 {
		t.Fatal("flood query lost its hit")
	}
	runtime.ReadMemStats(&after)
	measured := int64(after.TotalAlloc - before.TotalAlloc)

	if measured > charge {
		t.Fatalf("flood query allocated %d bytes but admission charges only %d — concurrency at the budget can exceed the budget", measured, charge)
	}
	// Validity guard: the flood really was corpus-wide (the charge's
	// per-ordinal terms exist for a reason).
	if charge < 8*n {
		t.Fatalf("charge %d below 8 B x %d ordinals; the model lost its per-ordinal terms", charge, n)
	}
}
