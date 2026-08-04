package bench

// Pins the alphabet const to the letters the syllable vocabulary actually
// produces: a substitute letter outside the corpus alphabet
// produces grams unknown to the index, which the score denominator excludes
// — silently easing TYPO cases. The first version of the const included 'c'
// (a letter no syllable contains), reintroducing exactly that inflation for
// ~1/22 of substitutions.

import (
	"strings"
	"testing"
)

func TestAlphabetMatchesVocabulary(t *testing.T) {
	vocab := map[rune]bool{}
	for _, s := range syll {
		for _, r := range s {
			vocab[r] = true
		}
	}
	for _, r := range alphabet {
		if !vocab[r] {
			t.Errorf("alphabet letter %q never appears in the syllable vocabulary — typos using it hit unknown grams", r)
		}
	}
	for r := range vocab {
		if !strings.ContainsRune(alphabet, r) {
			t.Errorf("vocabulary letter %q missing from alphabet — substitutions can never produce it", r)
		}
	}
}

// Every typo query must stay inside the corpus alphabet end to end.
func TestTypoQueriesStayInAlphabet(t *testing.T) {
	entries := Generate(42, 20000)
	for _, c := range Corpus(42, entries, 500) {
		if c.Category != "TYPO_1" && c.Category != "TYPO_2" {
			continue
		}
		for _, r := range c.Query {
			if r == ' ' {
				continue
			}
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("typo query %q contains out-of-alphabet letter %q", c.Query, r)
			}
		}
	}
}
