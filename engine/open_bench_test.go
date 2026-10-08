package engine_test

// Cold-open RTO benchmark: recovery time IS Open time — the
// crash-only contract's number. Two shapes: the artifact fast path (normal
// restart) and the full rebuild (artifact missing/rejected — e.g. after an
// analyzer change). The committed replacement for a previously ad-hoc
// cold-open harness.
//
// Both sub-benchmarks are single-process and run against a warm filesystem
// cache: the seeded files were just written (or just read by the previous
// iteration), so these numbers say nothing about a cold-cache open. The
// fresh-process measurement lives in bench/loadbench.

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
)

// seedOpenList writes one ngram list of n generated entries into a fresh
// temp dir and closes the seeding store before returning, so every measured
// Open is the only store attached to the directory.
func seedOpenList(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		tb.Fatal(err)
	}
	cfg := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	}
	if _, err := st.CreateList("people", cfg); err != nil {
		tb.Fatal(err)
	}
	if err := st.Replace("people", bench.Generate(42, n)); err != nil {
		tb.Fatal(err)
	}
	if err := st.Close(); err != nil {
		tb.Fatal(err)
	}
	return dir
}

func seedOpenBench(b *testing.B, n int) string {
	b.Helper()
	return seedOpenList(b, n)
}

// openAndCheck opens dir and asserts the seeded list loaded.
func openAndCheck(tb testing.TB, dir string) *engine.Store {
	tb.Helper()
	st, err := engine.Open(dir)
	if err != nil {
		tb.Fatal(err)
	}
	if _, ok := st.List("people"); !ok {
		tb.Fatal("list missing")
	}
	return st
}

// openBenchN sizes BenchmarkStoreOpen's list. The default is the recorded
// 1M baseline; a smaller value (e.g. -args -openbench.n=100000) exercises
// the same two paths on constrained machines.
var openBenchN = flag.Int("openbench.n", 1_000_000, "entries seeded by BenchmarkStoreOpen")

func BenchmarkStoreOpen(b *testing.B) {
	n := *openBenchN
	// artifact: base.idx is present and valid, so Open installs the saved
	// index. Single process, warm filesystem cache. Closing each store is
	// untimed.
	b.Run("artifact", func(b *testing.B) {
		dir := seedOpenBench(b, n)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			st := openAndCheck(b, dir)
			b.StopTimer()
			if err := st.Close(); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	})
	// rebuild-and-heal: base.idx is removed (untimed) before EVERY
	// iteration, so each Open does the full build from base.jsonl. Open then
	// heals the cache by re-saving base.idx before it returns, and that heal
	// write is INSIDE the timed interval — this is the cost of a restart
	// with a missing or rejected artifact, not of the build alone. Without
	// the per-iteration removal, iterations after the first would load the
	// healed artifact and measure the artifact path. Single process, warm
	// filesystem cache; closing each store is untimed.
	b.Run("rebuild-and-heal", func(b *testing.B) {
		dir := seedOpenBench(b, n)
		idx := filepath.Join(dir, "people", "base.idx")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			if err := os.Remove(idx); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			st := openAndCheck(b, dir)
			b.StopTimer()
			if err := st.Close(); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	})
}

// TestStoreOpenRebuildHealsEachTime pins the precondition the
// rebuild-and-heal benchmark depends on: removing base.idx forces the
// rebuild path, and that path re-saves base.idx before Open returns. The
// artifact path never writes base.idx, so its reappearance after each Open
// proves the rebuild-and-heal path ran — twice, because the benchmark's
// defect was exactly that only the first iteration rebuilt.
func TestStoreOpenRebuildHealsEachTime(t *testing.T) {
	dir := seedOpenList(t, 2000)
	idx := filepath.Join(dir, "people", "base.idx")
	if _, err := os.Stat(idx); err != nil {
		t.Fatalf("seed did not write base.idx: %v", err)
	}
	for round := 1; round <= 2; round++ {
		if err := os.Remove(idx); err != nil {
			t.Fatalf("round %d: remove base.idx: %v", round, err)
		}
		if _, err := os.Stat(idx); !os.IsNotExist(err) {
			t.Fatalf("round %d: base.idx still present after remove (stat err %v)", round, err)
		}
		st := openAndCheck(t, dir)
		if err := st.Close(); err != nil {
			t.Fatalf("round %d: close: %v", round, err)
		}
		if _, err := os.Stat(idx); err != nil {
			t.Fatalf("round %d: base.idx not re-saved by the rebuild path: %v", round, err)
		}
	}
}
