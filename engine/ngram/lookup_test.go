package ngram_test

import (
	"sync"
	"testing"

	"github.com/kurn-dev/kurn/engine/ngram"
)

func build(t *testing.T, keys ...string) *ngram.Index {
	t.Helper()
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	for i, k := range keys {
		b.Add(uint32(i), []string{k})
	}
	return b.Finish()
}

func TestLookupExactString(t *testing.T) {
	idx := build(t, "elena vasquez", "elena sandoval", "marcus chen")
	hits := idx.Lookup("elena vasquez", 0.6, 10)
	if len(hits) == 0 || hits[0].Ord != 0 {
		t.Fatalf("hits = %+v, want ord 0 first", hits)
	}
	if hits[0].Score != 100 {
		t.Errorf("self-match score = %v, want 100", hits[0].Score)
	}
}

// Space-insensitivity: the token-boundary win (strip_spaces).
func TestLookupFusedAndSplit(t *testing.T) {
	idx := build(t, "elena vasquez", "marcus chen")
	for _, q := range []string{"elenavasquez", "ele na vasquez"} {
		hits := idx.Lookup(q, 0.6, 10)
		if len(hits) == 0 || hits[0].Ord != 0 {
			t.Errorf("Lookup(%q) = %+v, want ord 0 first", q, hits)
		}
	}
}

func TestLookupTypo(t *testing.T) {
	idx := build(t, "elena vasquez", "marcus chen")
	hits := idx.Lookup("elena vasquze", 0.5, 10) // transposed
	if len(hits) == 0 || hits[0].Ord != 0 {
		t.Errorf("typo: hits = %+v, want ord 0", hits)
	}
}

func TestThresholdFiltersJunk(t *testing.T) {
	idx := build(t, "elena vasquez", "marcus chen")
	if hits := idx.Lookup("zzzz qqqq", 0.6, 10); len(hits) != 0 {
		t.Errorf("junk query hits = %+v, want none", hits)
	}
}

func TestTopK(t *testing.T) {
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	for i := 0; i < 50; i++ {
		b.Add(uint32(i), []string{"john smith"}) // 50 identical keys
	}
	idx := b.Finish()
	hits := idx.Lookup("john smith", 0.6, 5)
	if len(hits) != 5 {
		t.Fatalf("topK: got %d hits, want 5", len(hits))
	}
	// deterministic order: ties broken by ordinal
	for i, h := range hits {
		if h.Ord != uint32(i) {
			t.Errorf("hit %d ord = %d, want %d", i, h.Ord, i)
		}
	}
}

func TestEmptyQuery(t *testing.T) {
	idx := build(t, "elena vasquez")
	if hits := idx.Lookup("", 0.6, 10); hits != nil {
		t.Errorf("empty query: %+v, want nil", hits)
	}
}

func TestConcurrentLookups(t *testing.T) {
	idx := build(t, "elena vasquez", "marcus chen", "john smith", "jane doe")
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				hits := idx.Lookup("elena vasquez", 0.6, 10)
				if len(hits) == 0 || hits[0].Ord != 0 || hits[0].Score != 100 {
					t.Error("concurrent lookup gave wrong result")
					return
				}
			}
		}()
	}
	wg.Wait()
}
