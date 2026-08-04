package ngram_test

import (
	"math"
	"reflect"
	"testing"

	"github.com/RoaringBitmap/roaring"

	"github.com/kurn-dev/kurn/engine/ngram"
)

func TestCharGrams(t *testing.T) {
	got := ngram.CharGrams("abcd", 2)
	want := []string{"ab", "bc", "cd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CharGrams(abcd,2) = %v, want %v", got, want)
	}
	// shorter than n: the string itself
	if got := ngram.CharGrams("ab", 3); !reflect.DeepEqual(got, []string{"ab"}) {
		t.Errorf("short: %v", got)
	}
	// distinct only
	if got := ngram.CharGrams("aaa", 2); !reflect.DeepEqual(got, []string{"aa"}) {
		t.Errorf("dedup: %v", got)
	}
	// rune-safe
	if got := ngram.CharGrams("žür", 2); !reflect.DeepEqual(got, []string{"žü", "ür"}) {
		t.Errorf("runes: %v", got)
	}
}

func TestBuilderStats(t *testing.T) {
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	b.Add(0, []string{"elena vasquez"})
	b.Add(1, []string{"elena sandoval", "e sandoval"})
	idx := b.Finish()
	if ords, grams := idx.Stats(); ords != 2 || grams == 0 {
		t.Errorf("Stats() = %d ords %d grams, want 2 ords, >0 grams", ords, grams)
	}
}

// NewBuilder must reject gram sizes < 1 at construction time: CharGrams with
// n < 1 would misbehave at every Add/Lookup, so the panic has to fire here,
// once, with a clear message.
func TestNewBuilderRejectsInvalidGram(t *testing.T) {
	for _, grams := range [][]int{{0}, {-1}, {2, 0, 3}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewBuilder(Grams=%v): want panic, got none", grams)
				}
			}()
			ngram.NewBuilder(ngram.Config{Grams: grams})
		}()
	}
}

// The multi-key union path must dedupe grams shared between keys (one posting
// per gram) and its reused scratch must not leak grams across Add calls.
func TestAddMultiKeyUnion(t *testing.T) {
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	b.Add(0, []string{"elena vasquez", "elena v"}) // overlapping grams
	b.Add(1, []string{"marcus chen", "m chen"})
	idx := b.Finish()
	hits := idx.Lookup("elena vasquez", 0.6, 10)
	if len(hits) == 0 || hits[0].Ord != 0 || hits[0].Score != 100 {
		t.Fatalf("multi-key self match: %+v, want ord 0 score 100", hits)
	}
	// Cross-Add leak check: ord 1 must not match grams only key 0 had.
	if hits := idx.Lookup("elena vasquez", 0.6, 10); len(hits) > 1 {
		t.Errorf("grams leaked across Add calls: %+v", hits)
	}
}

// Regression: a crafted artifact with ordinal MaxUint32 must be rejected for
// any numOrds — the old check `numOrds < m+1` overflowed (m+1 wraps to 0),
// accepted everything, and Lookup then panicked with index-out-of-range.
func TestRestoreRejectsMaxUint32Ordinal(t *testing.T) {
	bm := roaring.New()
	bm.Add(math.MaxUint32)
	post := map[string]*roaring.Bitmap{"ab": bm}
	cfg := ngram.Config{Grams: []int{2}, StripSpaces: true}
	for _, numOrds := range []uint32{0, 1, 1000, math.MaxUint32} {
		if _, err := ngram.Restore(cfg, post, numOrds); err == nil {
			t.Errorf("Restore with ordinal MaxUint32, numOrds=%d: want error, got nil", numOrds)
		}
	}
}
