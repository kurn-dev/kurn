package main

import "testing"

// The -filter flag contract: first-'=' split (empty values and values
// containing '=' stay representable), missing separator rejected,
// repeated logical names rejected before anything executes.
func TestFilterFlagParsing(t *testing.T) {
	var f filterFlags
	for _, ok := range []struct{ arg, name, want string }{
		{"program=SDN", "program", "SDN"},
		{"note=", "note", ""},       // empty value
		{"expr=a=b", "expr", "a=b"}, // value containing '='
		{"país=Español", "país", "Español"} /* non-ASCII survives */} {
		if err := f.Set(ok.arg); err != nil {
			t.Fatalf("Set(%q): %v", ok.arg, err)
		}
		if got := f.m[ok.name]; got != ok.want {
			t.Fatalf("Set(%q): m[%q] = %q, want %q", ok.arg, ok.name, got, ok.want)
		}
	}
	if err := f.Set("noseparator"); err == nil {
		t.Fatal("missing separator accepted")
	}
	if err := f.Set("=value"); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := f.Set("program=EU"); err == nil {
		t.Fatal("repeated logical name accepted (silent last-wins)")
	}
	if f.m["program"] != "SDN" {
		t.Fatalf("rejected repeat mutated the map: %q", f.m["program"])
	}
}
