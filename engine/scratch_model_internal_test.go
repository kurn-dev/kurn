package engine

// White-box: the scratch model's per-gram term stays at or above what the
// pathological query shape really allocates. Admission control divides a
// fixed budget by these charges, so a term that slips below reality turns
// concurrency into an OOM — "every term errs upward" is the model's
// contract, and this pins the gram term to the recorded measurement as
// model arithmetic (a runtime heap assertion would be platform-sensitive:
// map buckets and allocator rounding shift between Go releases).
//
// Measurement of record (method in the scratchQueryPerGramBytes comment):
// all grams distinct (dense CJK runes) and all known to the index, so every
// gram pays one dedup-map entry plus one 24-byte gramInfo. Heap delta of
// exactly those two structures with GC off, growth transients included:
// worst observed 194 B/gram (4096 runes, sizes {2,3}); the 512-rune,
// sizes-{2,3} shape allocated 168 376 B over 1021 distinct grams.

import "testing"

const (
	measuredPerGramWorstBytes   = 194    // worst B/gram over all measured shapes
	measured512x2GramShapeBytes = 168376 // 512 runes, sizes {2,3}, 1021 grams
	measured512x2GramShapeRunes = 512
	measured512x2GramShapeSizes = 2
)

func TestGramChargeCoversMeasuredBound(t *testing.T) {
	if scratchQueryPerGramBytes < 72 {
		t.Fatalf("per-gram charge %d fell below the 72 B agreed floor", scratchQueryPerGramBytes)
	}
	if scratchQueryPerGramBytes < measuredPerGramWorstBytes {
		t.Fatalf("per-gram charge %d is below the measured %d B/gram worst case — admission undercharges flood-of-grams queries",
			scratchQueryPerGramBytes, measuredPerGramWorstBytes)
	}

	// The model's gram term alone must cover the measured allocation of the
	// pathological 512-rune, two-gram-size shape.
	gramTerm := int64(measured512x2GramShapeRunes) * measured512x2GramShapeSizes * scratchQueryPerGramBytes
	if gramTerm < measured512x2GramShapeBytes {
		t.Fatalf("gram term %d < measured %d for the 512-rune two-size shape", gramTerm, measured512x2GramShapeBytes)
	}

	// And the wired-through charge on a real list: everything the model
	// charges before the corpus (8 B/ordinal) and hit (192 B/hit) terms must
	// be at least fixed + rune + MEASURED gram bytes.
	l, err := NewList("pin", ListConfig{
		Analyzer: AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    MatchConfig{Mode: "ngram", Grams: []int{2, 3}, Threshold: 0.6, TopK: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]Entry{
		{ID: "a", Keys: []string{"alpha"}},
		{ID: "b", Keys: []string{"beta"}},
	}); err != nil {
		t.Fatal(err)
	}
	s := l.snap.Load()
	ords := int64(s.base.ng.NumOrds())
	const topK = 1
	charge := scratchBytesSnap(s, topK, measured512x2GramShapeRunes, len(l.cfg.Match.Grams))
	preCorpus := charge - 8*ords - scratchPerHitBytes*topK // masked = 0: no tombstones
	want := int64(scratchQueryFixedBytes) + measured512x2GramShapeRunes*scratchQueryPerRuneBytes + measured512x2GramShapeBytes
	if preCorpus < want {
		t.Fatalf("pre-corpus charge %d < fixed+rune+measured %d", preCorpus, want)
	}
}
