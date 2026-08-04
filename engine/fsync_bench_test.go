package engine_test

// Measures the journal-fsync trade-off (the accept bar:
// "bench the cost of `every`"). Single-writer sequential upserts — the
// worst case for FsyncEvery, the case FsyncInterval's group commit cannot
// help (no concurrency to batch).

import (
	"fmt"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func BenchmarkJournalAppend(b *testing.B) {
	for _, mode := range []engine.FsyncMode{engine.FsyncNone, engine.FsyncEvery, engine.FsyncInterval} {
		name := string(mode)
		if name == "" {
			name = "none"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			st, err := engine.Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			st.JournalFsync = mode
			if _, err := st.CreateList("codes", engine.ListConfig{
				Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
				Match:    engine.MatchConfig{Mode: "exact"},
			}); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e := engine.Entry{ID: fmt.Sprintf("c%08d", i), Keys: []string{fmt.Sprintf("KEY-%08d", i)}}
				if err := st.Upsert("codes", []engine.Entry{e}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
