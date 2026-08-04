package bench_test

import (
	"testing"

	"github.com/kurn-dev/kurn/bench"
	"github.com/kurn-dev/kurn/engine"
)

func TestRunReportsRecall(t *testing.T) {
	entries := bench.Generate(42, 2000)
	corpus := bench.Corpus(42, entries, 50)
	l, err := engine.NewList("bench", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.5, TopK: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	l.Replace(entries)
	rep := bench.Run(l, corpus, engine.QueryOpts{})
	if rep.Cases != 400 {
		t.Errorf("cases = %d, want 400", rep.Cases)
	}
	// EXACT must be ~perfect; overall must be well above zero
	if rep.ByCategory["EXACT"].Recall < 0.99 {
		t.Errorf("EXACT recall = %v, want ~1.0", rep.ByCategory["EXACT"].Recall)
	}
	if rep.Recall < 0.5 {
		t.Errorf("overall right-entity recall = %v, suspiciously low", rep.Recall)
	}
	if rep.P99Us <= 0 || rep.QPS <= 0 {
		t.Errorf("missing perf numbers: %+v", rep)
	}
	// Empty corpus must not panic (percentile indexing) or report NaN recall.
	if empty := bench.Run(l, nil, engine.QueryOpts{}); empty.Cases != 0 || empty.Recall != 0 {
		t.Errorf("empty corpus report = %+v, want zero-valued", empty)
	}
}
