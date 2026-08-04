package analyzer_test

import (
	"testing"

	"github.com/kurn-dev/kurn/engine/analyzer"
)

func TestPresets(t *testing.T) {
	cases := []struct{ preset, in, want string }{
		{"person-name", "Dr. José-María O'Connor", "jose-maria oconnor"},
		{"person-name", "Vasquez, Elena", "elena vasquez"}, // token sort ⇒ order-stable
		{"person-name", "Elena Vasquez", "elena vasquez"},
		{"identifier", "  ACME-042  ", "acme-042"},
		{"free-text", "Café «Métro» #12", "cafe metro 12"},
	}
	for _, c := range cases {
		a, err := analyzer.Preset(c.preset)
		if err != nil {
			t.Fatalf("Preset(%q): %v", c.preset, err)
		}
		if got := a.Normalize(c.in); got != c.want {
			t.Errorf("%s: Normalize(%q) = %q, want %q", c.preset, c.in, got, c.want)
		}
	}
	if _, err := analyzer.Preset("nope"); err == nil {
		t.Fatal("want error for unknown preset")
	}
}
