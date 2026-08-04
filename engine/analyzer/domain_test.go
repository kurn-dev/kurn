package analyzer_test

import (
	"testing"

	"github.com/kurn-dev/kurn/engine/analyzer"
)

func TestIdnaStep(t *testing.T) {
	a, err := analyzer.New([]string{"lowercase", "trim", "idna"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, want string }{
		{"MAIL.TempMail.COM.", "mail.tempmail.com"},          // case + trailing dot
		{"münchen.example", "xn--mnchen-3ya.example"},        // IDN -> punycode
		{"xn--mnchen-3ya.example", "xn--mnchen-3ya.example"}, // already punycode: stable
		{"plain.example.com", "plain.example.com"},           // ASCII passthrough
		{"", ""},
		{"...", ""}, // pathological: no labels survive
		// Regression: ToASCII preserves a remaining root dot with nil error,
		// so ALL trailing dots must be trimmed up front — otherwise
		// "example.com.." -> "example.com." -> "example.com" is not idempotent.
		{"example.com..", "example.com"},
		{"münchen.example..", "xn--mnchen-3ya.example"},
		{"foo_bar.com..", "foo_bar.com"}, // underscore label: mapped, not rejected
		// Regression: leading dots are as meaningless as trailing ones and must
		// be stripped too — otherwise ".tempmail.com" keeps its empty leading
		// label and parent_domain fallback scores the listed domain 90 (a
		// "subdomain" one level up) instead of 100 (the domain itself).
		{".tempmail.com", "tempmail.com"},
		{"..example.com..", "example.com"},
		// Real-world sloppy shapes (underscored service labels, wildcards) must
		// convert, not degrade: blocklists are full of them, and Task 2's parent
		// suffix probing never re-analyzes — Unicode and punycode spellings of
		// the same domain have to converge to one indexed form.
		{"smtp_1.münchen.example", "smtp_1.xn--mnchen-3ya.example"},
		{"*.tempmail.com", "*.tempmail.com"},
		{"_dmarc.example.com", "_dmarc.example.com"},
	}
	for _, c := range cases {
		if got := a.Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Idempotence — the iron rule for index/query symmetry.
	for _, c := range cases {
		once := a.Normalize(c.in)
		if twice := a.Normalize(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", c.in, once, twice)
		}
	}
	// Convergence: the Unicode and punycode spellings of the same sloppy
	// domain must normalize identically, or one spelling on the list is
	// unreachable from the other at query time.
	u, p := a.Normalize("smtp_1.münchen.example"), a.Normalize("smtp_1.xn--mnchen-3ya.example")
	if u != p {
		t.Errorf("divergent spellings: unicode %q vs punycode %q", u, p)
	}
	// Invalid input must degrade, never error: Normalize is string -> string,
	// and the degraded form must be non-empty and stable (idempotent).
	if got := a.Normalize("exa mple .com"); got == "" {
		t.Error("invalid domain degraded to empty")
	} else if again := a.Normalize(got); again != got {
		t.Errorf("degraded form not idempotent: %q -> %q", got, again)
	}
}

func TestDomainPreset(t *testing.T) {
	a, err := analyzer.Preset("domain")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Normalize("  Sub.MÜNCHEN.example.  "); got != "sub.xn--mnchen-3ya.example" {
		t.Errorf("domain preset: %q", got)
	}
}
