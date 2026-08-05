// Package bench generates synthetic corpora and measures kurn recall,
// latency, and memory against pinned record runs. Right-entity methodology:
// every query carries the ID of the entry that generated it; only finding
// THAT entry counts as recall (any-hit metrics inflate ~26-29 points).
package bench

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/kurn-dev/kurn/engine"
)

// syll is the token vocabulary, built deterministically: 16 consonants ×
// 5 vowels gives 80 CV syllables, plus one CVC variant per CV pair (final
// consonant by rotation) — 160 syllables of 2-3 letters over 21 letters of
// the alphabet. Gram density matters more than name count: the previous 20
// syllables spanned ~18 letters, so 10M keys shared only a few thousand
// distinct 2/3-grams — pathologically dense postings (p50 ~11ms at 800k,
// extrapolating ~140ms at 10M: candidate-set size, not engine cleverness,
// was the bottleneck). A larger vocabulary over more letters spreads keys
// across a realistic slice of the gram space.
var syll = buildSyllables()

// alphabet is the 21 letters the vocabulary spans (16 consonants + 5
// vowels). typo() draws substitutes from it — every typo letter is then a
// letter the indexed corpus can actually contain, so its grams count in the
// score denominator. Drawing from all 26 letters instead meant ~20% of typo
// letters (c/q/w/x/y) were unknown to the index, and unknown grams are
// excluded from the denominator: typos with out-of-alphabet letters barely
// lowered the score, inflating TYPO recall (pinned by alphabet_test.go).
const alphabet = "bdfghjklmnprstvzaeiou"

func buildSyllables() []string {
	cons := []string{"b", "d", "f", "g", "h", "j", "k", "l",
		"m", "n", "p", "r", "s", "t", "v", "z"}
	vow := []string{"a", "e", "i", "o", "u"}
	out := make([]string, 0, 2*len(cons)*len(vow))
	for ci, c := range cons {
		for vi, v := range vow {
			out = append(out, c+v)
			out = append(out, c+v+cons[(ci+vi+1)%len(cons)])
		}
	}
	return out
}

func name(r *rand.Rand) string {
	tokens := 2 + r.Intn(2) // 2..3
	parts := make([]string, tokens)
	for i := range parts {
		// 2..3 syllables of 2-3 letters → 4-9 letter tokens (realistic name
		// token lengths). Duplicate math at 160 syllables: the hot stratum —
		// two tokens of 2 syllables each — spans 160^4 ≈ 655M distinct forms;
		// at 10M entries ~1.25M land in it (P(2 tokens)=1/2 × P(2,2 sylls)=
		// 1/4), expecting ~n²/2N ≈ 1.2k collisions — dupes < 0.1% of the
		// corpus.
		n := 2 + r.Intn(2)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteString(syll[r.Intn(len(syll))])
		}
		parts[i] = b.String()
	}
	return strings.Join(parts, " ")
}

// Generate returns n deterministic person-shaped entries.
func Generate(seed int64, n int) []engine.Entry {
	r := rand.New(rand.NewSource(seed))
	out := make([]engine.Entry, n)
	for i := range out {
		out[i] = engine.Entry{ID: fmt.Sprintf("e%08d", i), Keys: []string{name(r)}}
	}
	return out
}

// Case is one benchmark query with its ground truth.
type Case struct {
	Query    string
	TruthID  string
	Category string
}

var categories = []string{"EXACT", "TYPO_1", "TYPO_2", "TRANSPOSE_NONADJ",
	"SPACE_INSERT", "FUSED", "DOUBLE_TOKEN", "REMOVE_TOKEN"}

// Categories returns the perturbation categories in generation order (a copy;
// callers may not reorder the canonical list).
func Categories() []string {
	return append([]string(nil), categories...)
}

// Corpus samples `sample` entries and emits one Case per category each.
//
// REMOVE_TOKEN rejection-samples its own 3-token entry: on a 2-token name
// the perturbation would return the key unchanged — a mislabeled EXACT that
// inflates the category's recall ~50 points (about half of generated names
// have 2 tokens). Requires entries to contain at least one 3-token name,
// which Generate guarantees in practice for any non-trivial n.
func Corpus(seed int64, entries []engine.Entry, sample int) []Case {
	r := rand.New(rand.NewSource(seed + 1))
	out := make([]Case, 0, sample*len(categories))
	for i := 0; i < sample; i++ {
		e := entries[r.Intn(len(entries))]
		for _, cat := range categories {
			src := e
			if cat == "REMOVE_TOKEN" {
				// Bounded rejection sampling: ~half of generated names have 3
				// tokens, so 10k misses means the input has (statistically) no
				// 3-token entries at all — bail instead of spinning forever on
				// such degenerate input. Panic rather than error: the signature
				// is pinned, and this is a bench-harness precondition failure.
				for tries := 0; strings.Count(src.Keys[0], " ") < 2; tries++ {
					if tries == 10000 {
						panic("bench: Corpus: no 3-token entry found in 10000 samples; REMOVE_TOKEN requires one")
					}
					src = entries[r.Intn(len(entries))]
				}
			}
			out = append(out, Case{Query: perturb(r, src.Keys[0], cat), TruthID: src.ID, Category: cat})
		}
	}
	return out
}

func perturb(r *rand.Rand, s, cat string) string {
	rs := []rune(s)
	switch cat {
	case "EXACT":
		return s
	case "TYPO_1":
		return typo(r, rs, 1)
	case "TYPO_2":
		return typo(r, rs, 2)
	case "TRANSPOSE_NONADJ":
		// Swap two non-adjacent letters. The partner must differ from rs[i]:
		// swapping equal runes is a no-op that would silently turn this case
		// into EXACT and inflate the category's recall.
		i := letterPos(r, rs)
		if i < 0 {
			return s
		}
		var cand []int
		for j := range rs {
			if rs[j] != ' ' && rs[j] != rs[i] && j != i+1 && i != j+1 {
				cand = append(cand, j)
			}
		}
		if len(cand) == 0 {
			// Unreachable with this vocabulary (every name has >= 8 letters
			// and >= 2 distinct rune values); kept so perturb can't panic.
			return s
		}
		j := cand[r.Intn(len(cand))]
		rs[i], rs[j] = rs[j], rs[i]
		return string(rs)
	case "SPACE_INSERT":
		// Split inside a token: a position adjacent to an existing space
		// normalizes back to the original key (no perturbation at all).
		// Reject-sample as before — the draw sequence is part of what a seed
		// reproduces — but bounded: Corpus is exported and takes arbitrary
		// input, and "a b c" offers no such position at all, which spun the
		// sampler forever. Only inputs that never terminated reach the
		// fallback, so every corpus that generated before generates
		// identically.
		i, ok := 0, false
		for att := 0; att < maxSampleAttempts; att++ {
			j := 1 + r.Intn(len(rs)-1)
			if rs[j] != ' ' && rs[j-1] != ' ' {
				i, ok = j, true
				break
			}
		}
		if !ok {
			return s // no split position exists; an unperturbed case
		}
		return string(rs[:i]) + " " + string(rs[i:])
	case "FUSED":
		return strings.ReplaceAll(s, " ", "")
	case "DOUBLE_TOKEN":
		parts := strings.Fields(s)
		return parts[0] + " " + s
	case "REMOVE_TOKEN":
		parts := strings.Fields(s)
		if len(parts) < 3 {
			// Defensive only: Corpus rejection-samples a 3-token entry for
			// this category, so direct callers alone can reach this.
			return s
		}
		return strings.Join(parts[1:], " ")
	}
	panic("unknown category " + cat)
}

// typo applies n single-letter substitutions at distinct positions, each
// substitute differing from the letter it replaces AND drawn from the corpus
// alphabet (see the alphabet const). All three constraints keep the edit
// honest: a self-substitution is a zero-edit query, a repeated position
// collapses TYPO_2 to one effective edit (or reverts to the original), and
// an out-of-alphabet substitute produces grams unknown to the index — which
// the score denominator excludes, silently easing the case.
func typo(r *rand.Rand, rs []rune, n int) string {
	out := append([]rune(nil), rs...)
	used := make(map[int]bool, n)
	for k := 0; k < n; k++ {
		i := letterPos(r, out)
		if i < 0 {
			return string(out) // no letters to perturb
		}
		for att := 0; used[i] && att < maxSampleAttempts; att++ {
			i = letterPos(r, out)
		}
		if used[i] {
			return string(out) // fewer distinct letters than typos requested
		}
		used[i] = true
		c := rune(alphabet[r.Intn(len(alphabet))])
		for c == out[i] {
			c = rune(alphabet[r.Intn(len(alphabet))])
		}
		out[i] = c
	}
	return string(out)
}

// maxSampleAttempts bounds every rejection sampler here. Retrying is what
// preserves the draw sequence a seed reproduces; the bound only decides
// when to stop, and it is reached only by inputs that never terminated at
// all (an all-space key, a key with fewer letters than the typo count).
const maxSampleAttempts = 64

// letterPos returns a random non-space position, or -1 when there is none.
func letterPos(r *rand.Rand, rs []rune) int {
	for att := 0; att < maxSampleAttempts; att++ {
		i := r.Intn(len(rs))
		if rs[i] != ' ' {
			return i
		}
	}
	for i := range rs { // exhausted: settle it exactly rather than spin
		if rs[i] != ' ' {
			return i
		}
	}
	return -1
}
