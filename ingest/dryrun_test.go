package ingest_test

// The dry-run gate's report fields, and the all-collapsing
// mapping the empty-key rate exists to catch.

import (
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/ingest"
)

func TestDryRunReport(t *testing.T) {
	// strip_punctuation+trim: "..." collapses to "" — one bad key, one
	// keyless entry among good ones.
	input := `{"id":"a","keys":["Anna Smith","..."],"note":"x"}
{"id":"b","keys":["!!!"]}
{"id":"c","keys":["Bo Chan"]}
{"id":"a","keys":["Anna Again"]}`
	m := &ingest.Mapping{
		Format: "ndjson",
		ID:     "id",
		Keys:   []ingest.KeyRule{{Path: "keys"}},
		List: engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "strip_punctuation", "trim"}},
			Match:    engine.MatchConfig{Mode: "exact"},
		},
	}
	// Instance semantics make a bare array path multi-key: with path
	// "keys", the instance set IS the array elements, so each element
	// becomes its own key (remainder-path resolves the element itself).
	rep, err := ingest.DryRun(m, strings.NewReader(input), ingest.Options{}, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Records != 4 || rep.Sampled != 4 {
		t.Fatalf("records = %+v", rep)
	}
	if rep.DistinctIDs != 3 || rep.DuplicateIDs != 1 {
		t.Fatalf("id accounting: %+v", rep)
	}
	if rep.KeylessEntries != 1 { // "!!!" collapses entirely
		t.Fatalf("keyless = %d, want 1 (%+v)", rep.KeylessEntries, rep)
	}
	if rep.EmptyKeys == 0 || rep.EmptyKeyRate <= 0 {
		t.Fatalf("empty-key accounting missing: %+v", rep)
	}
	if len(rep.Samples) != 2 || rep.Samples[0].ID != "a" || rep.Samples[0].AnalyzedKeys[0] != "anna smith" {
		t.Fatalf("samples: %+v", rep.Samples)
	}
	if rep.EstMemoryBytes != int64(float64(rep.AnalyzedKeys)*133.0) {
		t.Fatalf("estimate off: %+v", rep)
	}
}

func TestDryRunSampleBound(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(`{"id":"x` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `","k":"key"}` + "\n")
	}
	m := &ingest.Mapping{Format: "ndjson", ID: "id", Keys: []ingest.KeyRule{{Path: "k"}}, List: exactList()}
	rep, err := ingest.DryRun(m, strings.NewReader(b.String()), ingest.Options{}, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Records != 10 || len(rep.Samples) != 3 {
		t.Fatalf("sample bound: %+v", rep)
	}
}
