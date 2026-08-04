package ngram

import (
	"math/rand"
	"testing"
)

// EachGram must emit exactly the sequence a consumer of grams()/CharGrams
// with a cross-size dedup set saw before streaming existed: sizes in order,
// windows left-to-right, short strings contributing themselves once, first
// occurrence wins. The reference below is that original consumption pattern.
func TestEachGramMatchesCharGrams(t *testing.T) {
	ref := func(s string, sizes []int) []string {
		seen := map[string]struct{}{}
		var out []string
		for _, n := range sizes {
			for _, g := range CharGrams(s, n) {
				if _, ok := seen[g]; ok {
					continue
				}
				seen[g] = struct{}{}
				out = append(out, g)
			}
		}
		return out
	}

	cases := []string{
		"", "a", "ab", "abc", "jane smith", "smith, jane",
		"õnne läänemaa", "мария иванова", "🦊fox🦊", "aa", "aaa aaa",
		"ab cd ef", "x", "日本語テキスト",
	}
	sizeSets := [][]int{{2, 3}, {3}, {1}, {2, 3, 4}, {5}}

	// deterministic random strings, mixed-width runes
	alphabet := []rune("abco ĕжщ🦊日")
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		n := rng.Intn(12)
		r := make([]rune, n)
		for j := range r {
			r[j] = alphabet[rng.Intn(len(alphabet))]
		}
		cases = append(cases, string(r))
	}

	for _, sizes := range sizeSets {
		var offs []int
		seen := map[string]struct{}{}
		for _, s := range cases {
			want := ref(s, sizes)
			var got []string
			clear(seen)
			offs = EachGram(s, sizes, seen, offs, func(g string) {
				got = append(got, g)
			})
			if len(got) != len(want) {
				t.Fatalf("sizes %v s %q: got %d grams %v, want %d %v",
					sizes, s, len(got), got, len(want), want)
			}
			for j := range got {
				if got[j] != want[j] {
					t.Fatalf("sizes %v s %q: gram %d = %q, want %q", sizes, s, j, got[j], want[j])
				}
			}
		}
	}
}

// The seen map is a cross-call union when not cleared — Builder-style usage.
func TestEachGramSharedSeenUnions(t *testing.T) {
	seen := map[string]struct{}{}
	var offs []int
	var first, second []string
	offs = EachGram("abc", []int{2}, seen, offs, func(g string) { first = append(first, g) })
	offs = EachGram("bcd", []int{2}, seen, offs, func(g string) { second = append(second, g) })
	if len(first) != 2 || first[0] != "ab" || first[1] != "bc" {
		t.Fatalf("first = %v", first)
	}
	// "bc" already seen from the first call
	if len(second) != 1 || second[0] != "cd" {
		t.Fatalf("second = %v", second)
	}
}
