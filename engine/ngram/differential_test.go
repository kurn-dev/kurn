package ngram_test

import (
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring"

	"github.com/kurn-dev/kurn/engine/artifact"
	"github.com/kurn-dev/kurn/engine/ngram"
)

// Differential recall test: Lookup's pigeonhole scan (generators + probes
// with early exit) must return the same ordinals as a brute-force reference
// scorer, with the same rounded scores. This pins the recall-completeness
// property before any refactor calcifies. Caveat on "same": Lookup
// accumulates generator IDF in float32 while the reference sums float64
// throughout, so a score sitting EXACTLY on the acceptance floor could in
// principle diverge between the two; the seeded corpus doesn't produce such
// a knife-edge case, and the rounded-score comparison absorbs sub-rounding
// drift, but this is an empirical pin, not an arithmetic guarantee. Also
// asserted against a Save/Load round-tripped index.

// synthKeys returns a deterministic synthetic corpus (seeded generator, no
// wall-clock randomness): a few hundred keys varying in token count and length.
func synthKeys() []string {
	rng := rand.New(rand.NewSource(42))
	consonants := "bcdfghjklmnpqrstvwz"
	vowels := "aeiou"
	word := func() string {
		var sb strings.Builder
		for s := 1 + rng.Intn(4); s > 0; s-- {
			sb.WriteByte(consonants[rng.Intn(len(consonants))])
			sb.WriteByte(vowels[rng.Intn(len(vowels))])
			if rng.Intn(3) == 0 {
				sb.WriteByte(consonants[rng.Intn(len(consonants))])
			}
		}
		return sb.String()
	}
	keys := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		toks := make([]string, 1+rng.Intn(3))
		for j := range toks {
			toks[j] = word()
		}
		keys = append(keys, strings.Join(toks, " "))
	}
	return keys
}

// refLookup is the brute-force reference: for every ordinal, the IDF-weighted
// coverage over the query's index-known grams, computed directly from
// Postings(); accepted iff score >= threshold*maxScore. Returns ord -> rounded
// 0..100 score, mirroring Hit.Score.
func refLookup(idx *ngram.Index, q string, threshold float64) map[uint32]float64 {
	cfg := idx.Cfg()
	s := q
	if cfg.StripSpaces {
		s = strings.ReplaceAll(s, " ", "")
	}
	if s == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var grams []string
	for _, n := range cfg.Grams {
		for _, g := range ngram.CharGrams(s, n) {
			if _, dup := seen[g]; dup {
				continue
			}
			seen[g] = struct{}{}
			grams = append(grams, g)
		}
	}
	post := idx.Postings()
	total := float64(idx.NumOrds())
	type known struct {
		bm  *roaring.Bitmap
		idf float64
	}
	var kn []known
	var maxScore float64
	for _, g := range grams {
		bm := post[g]
		if bm == nil || bm.GetCardinality() == 0 {
			continue
		}
		idf := math.Log(1 + total/float64(bm.GetCardinality()))
		kn = append(kn, known{bm, idf})
		maxScore += idf
	}
	if maxScore == 0 {
		return nil
	}
	floor := threshold * maxScore
	accepted := map[uint32]float64{}
	for ord := uint32(0); ord < idx.NumOrds(); ord++ {
		var score float64
		for _, k := range kn {
			if k.bm.Contains(ord) {
				score += k.idf
			}
		}
		if score >= floor {
			accepted[ord] = math.Round(score / maxScore * 100)
		}
	}
	return accepted
}

// synthQueries derives a deterministic query mix from the corpus: exact keys,
// single-char typos, space-fused keys, and junk.
func synthQueries(keys []string) []string {
	var qs []string
	for i := 0; i < len(keys); i += 17 { // exact
		qs = append(qs, keys[i])
	}
	for i := 5; i < len(keys); i += 23 { // typo: substitute one char
		k := []byte(keys[i])
		pos := (i * 7) % len(k)
		if k[pos] == ' ' {
			pos = (pos + 1) % len(k)
		}
		k[pos] = 'x'
		qs = append(qs, string(k))
	}
	for i := 11; i < len(keys); i += 29 { // fused
		qs = append(qs, strings.ReplaceAll(keys[i], " ", ""))
	}
	qs = append(qs, "zzzzzz", "xq", "the quick brown fox", "")
	return qs
}

func TestDifferentialRecall(t *testing.T) {
	keys := synthKeys()
	b := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	for ord, k := range keys {
		b.Add(uint32(ord), []string{k})
	}
	idx := b.Finish()

	// Round-trip through the artifact so the same differential also pins the
	// Load()ed index.
	path := filepath.Join(t.TempDir(), "diff.idx")
	if err := artifact.Save(path, idx, "differential-test-digest"); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := artifact.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	queries := synthQueries(keys)
	for _, threshold := range []float64{0.4, 0.6, 0.8} {
		for _, q := range queries {
			want := refLookup(idx, q, threshold)
			for name, ix := range map[string]*ngram.Index{"built": idx, "loaded": loaded} {
				got := map[uint32]float64{}
				for _, h := range ix.Lookup(q, threshold, 0) {
					got[h.Ord] = h.Score
				}
				if len(got) != len(want) {
					t.Errorf("%s Lookup(%q, %v): got %d hits, reference %d\n%s", name, q, threshold, len(got), len(want), diffSets(got, want))
					continue
				}
				for ord, ws := range want {
					gs, ok := got[ord]
					if !ok {
						t.Errorf("%s Lookup(%q, %v): missing ord %d (ref score %v)", name, q, threshold, ord, ws)
					} else if gs != ws {
						t.Errorf("%s Lookup(%q, %v): ord %d score %v, reference %v", name, q, threshold, ord, gs, ws)
					}
				}
			}
		}
	}
}

func diffSets(got, want map[uint32]float64) string {
	var sb strings.Builder
	for ord := range want {
		if _, ok := got[ord]; !ok {
			fmt.Fprintf(&sb, "  missing ord %d\n", ord)
		}
	}
	for ord := range got {
		if _, ok := want[ord]; !ok {
			fmt.Fprintf(&sb, "  extra ord %d\n", ord)
		}
	}
	return sb.String()
}
