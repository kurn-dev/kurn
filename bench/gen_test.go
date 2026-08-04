package bench_test

import (
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/bench"
)

func TestGenerateDeterministic(t *testing.T) {
	a := bench.Generate(42, 100)
	b := bench.Generate(42, 100)
	if len(a) != 100 || len(a[0].Keys) == 0 {
		t.Fatalf("bad entries: %+v", a[:1])
	}
	if a[0].Keys[0] != b[0].Keys[0] || a[99].ID != b[99].ID {
		t.Error("not deterministic")
	}
	if bench.Generate(43, 100)[0].Keys[0] == a[0].Keys[0] {
		t.Error("seed ignored")
	}
}

// TestGenerateNoDuplicateKeys probes 20k entries for duplicate names. The hot
// (2-syll, 2-syll two-token) stratum spans ~655M forms at 160 syllables, so
// even at 10M entries dupes stay < 0.1%; at 20k a single collision would
// signal the vocabulary shrank.
func TestGenerateNoDuplicateKeys(t *testing.T) {
	entries := bench.Generate(42, 20000)
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if _, dup := seen[e.Keys[0]]; dup {
			t.Fatalf("duplicate key %q", e.Keys[0])
		}
		seen[e.Keys[0]] = struct{}{}
	}
}

func TestPerturbations(t *testing.T) {
	entries := bench.Generate(42, 1000)
	corpus := bench.Corpus(42, entries, 100) // 100 sampled entries × 8 categories
	if len(corpus) != 800 {
		t.Fatalf("corpus size %d, want 800", len(corpus))
	}
	keyByID := map[string]string{}
	for _, e := range entries {
		keyByID[e.ID] = e.Keys[0]
	}
	byCat := map[string]int{}
	for _, q := range corpus {
		byCat[q.Category]++
		if q.TruthID == "" || q.Query == "" {
			t.Fatalf("bad case: %+v", q)
		}
		key, ok := keyByID[q.TruthID]
		if !ok {
			t.Fatalf("truth ID not in entries: %+v", q)
		}
		switch q.Category {
		case "FUSED":
			if strings.Contains(q.Query, " ") {
				t.Errorf("FUSED has space: %q", q.Query)
			}
		case "EXACT":
			// query must equal one of the truth entry's keys
			if q.Query != key {
				t.Errorf("EXACT query %q != truth key %q", q.Query, key)
			}
		}
		// Every non-EXACT perturbation must actually perturb: a query equal
		// to the source key (after space collapsing, which the analyzer does)
		// is a mislabeled EXACT that inflates its category's recall.
		// REMOVE_TOKEN included — Corpus rejection-samples a 3-token source.
		collapsed := strings.Join(strings.Fields(q.Query), " ")
		if q.Category != "EXACT" && collapsed == key {
			t.Errorf("%s is a no-op: query %q == truth key", q.Category, q.Query)
		}
	}
	for _, c := range []string{"EXACT", "TYPO_1", "TYPO_2", "TRANSPOSE_NONADJ", "SPACE_INSERT", "FUSED", "DOUBLE_TOKEN", "REMOVE_TOKEN"} {
		if byCat[c] != 100 {
			t.Errorf("category %s: %d cases, want 100", c, byCat[c])
		}
	}
}
