package analyzer_test

import (
	"testing"

	"github.com/kurn-dev/kurn/engine/analyzer"
)

func TestSteps(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
		in    string
		want  string
	}{
		{"lowercase", []string{"lowercase"}, "AbC", "abc"},
		{"fold_diacritics", []string{"fold_diacritics"}, "Zürich Ñoño", "Zurich Nono"},
		{"strip_punctuation keeps inner hyphen", []string{"strip_punctuation"}, "O'Neil, Vega-Ruiz!", "ONeil Vega-Ruiz"},
		{"strip_punctuation trims edge hyphen", []string{"strip_punctuation"}, "-abc- d", "abc d"},
		{"strip_words", []string{"lowercase", "strip_words:mr,dr"}, "Mr John dr smith", "john smith"},
		{"sort_tokens", []string{"sort_tokens"}, "c a b", "a b c"},
		{"trim collapses spaces", []string{"trim"}, "  a   b  ", "a b"},
		{"pipeline order", []string{"lowercase", "strip_punctuation", "sort_tokens"}, "Vasquez, Elena", "elena vasquez"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := analyzer.New(c.steps)
			if err != nil {
				t.Fatalf("New(%v): %v", c.steps, err)
			}
			if got := a.Normalize(c.in); got != c.want {
				t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestUnknownStep(t *testing.T) {
	if _, err := analyzer.New([]string{"bogus"}); err == nil {
		t.Fatal("want error for unknown step")
	}
}

// The iron rule: normalization must be idempotent, or the build and query
// paths can key differently.
func TestIdempotent(t *testing.T) {
	a, _ := analyzer.New([]string{"lowercase", "fold_diacritics", "strip_punctuation", "sort_tokens", "trim"})
	for _, s := range []string{"Mr. José-María O'Connor", "  A  B ", "Ñ"} {
		once := a.Normalize(s)
		if twice := a.Normalize(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", s, once, twice)
		}
	}
}
