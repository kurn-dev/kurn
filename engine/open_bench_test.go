package engine_test

// Cold-open RTO benchmark: recovery time IS Open time — the
// crash-only contract's number. Two shapes: the artifact fast path (normal
// restart) and the full rebuild (artifact missing/rejected — e.g. after an
// analyzer change). The committed replacement for a previously ad-hoc
// cold-open harness.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
)

func seedOpenBench(b *testing.B, n int) string {
	b.Helper()
	dir := b.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	}
	if _, err := st.CreateList("people", cfg); err != nil {
		b.Fatal(err)
	}
	if err := st.Replace("people", bench.Generate(42, n)); err != nil {
		b.Fatal(err)
	}
	return dir
}

func BenchmarkStoreOpen(b *testing.B) {
	const n = 1_000_000
	b.Run("artifact", func(b *testing.B) {
		dir := seedOpenBench(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			st, err := engine.Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			if _, ok := st.List("people"); !ok {
				b.Fatal("list missing")
			}
		}
	})
	b.Run("rebuild", func(b *testing.B) {
		dir := seedOpenBench(b, n)
		if err := os.Remove(filepath.Join(dir, "people", "base.idx")); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// The rebuild path never re-saves the artifact, so every
			// iteration measures the same full build from base.jsonl.
			st, err := engine.Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			if _, ok := st.List("people"); !ok {
				b.Fatal("list missing")
			}
		}
	})
}
