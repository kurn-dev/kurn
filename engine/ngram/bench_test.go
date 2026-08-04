package ngram_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine/ngram"
)

// benchKeys returns n deterministic synthetic person-shaped keys (seeded
// generator; 2-3 tokens of 4-9 letters — same shape the bench corpus uses).
func benchKeys(n int) []string {
	rng := rand.New(rand.NewSource(7))
	consonants := "bcdfghjklmnpqrstvz"
	vowels := "aeiou"
	word := func() string {
		var sb strings.Builder
		for s := 2 + rng.Intn(2); s > 0; s-- {
			sb.WriteByte(consonants[rng.Intn(len(consonants))])
			sb.WriteByte(vowels[rng.Intn(len(vowels))])
			if rng.Intn(3) == 0 {
				sb.WriteByte(consonants[rng.Intn(len(consonants))])
			}
		}
		return sb.String()
	}
	keys := make([]string, n)
	for i := range keys {
		toks := make([]string, 2+rng.Intn(2))
		for j := range toks {
			toks[j] = word()
		}
		keys[i] = strings.Join(toks, " ")
	}
	return keys
}

// BenchmarkAdd measures Builder.Add per entry: keys=1 exercises the single-key
// fast path (the 10M-build hot path — one Add per generated entry), keys=3 the
// multi-key union path.
func BenchmarkAdd(b *testing.B) {
	keys := benchKeys(4096)
	for _, nk := range []int{1, 3} {
		b.Run(fmt.Sprintf("keys=%d", nk), func(b *testing.B) {
			bld := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
			ks := make([]string, nk)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := 0; j < nk; j++ {
					ks[j] = keys[(i*nk+j)%len(keys)]
				}
				bld.Add(uint32(i), ks)
			}
		})
	}
}

// BenchmarkLookup measures the pigeonhole scan against a 50k-key index with a
// mixed query set (exact keys, one-char typos, space-fused keys).
func BenchmarkLookup(b *testing.B) {
	keys := benchKeys(50000)
	bld := ngram.NewBuilder(ngram.Config{Grams: []int{2, 3}, StripSpaces: true})
	for i, k := range keys {
		bld.Add(uint32(i), []string{k})
	}
	idx := bld.Finish()

	queries := make([]string, 0, 300)
	for i := 0; i < 100; i++ {
		k := keys[(i*137)%len(keys)]
		queries = append(queries, k) // exact
		typo := []byte(k)
		typo[(i*7)%len(typo)] = 'x'
		queries = append(queries, string(typo))                   // typo
		queries = append(queries, strings.ReplaceAll(k, " ", "")) // fused
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.Lookup(queries[i%len(queries)], 0.6, 100)
	}
}
