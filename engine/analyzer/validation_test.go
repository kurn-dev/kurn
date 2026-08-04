package analyzer_test

import (
	"testing"

	"github.com/kurn-dev/kurn/engine/analyzer"
)

// Arg-less steps must reject arguments: "lowercase:junk" silently parsing as
// "lowercase" would let two behaviorally identical analyzers carry different
// canonical step specs — and therefore different config digests.
func TestArglessStepRejectsArg(t *testing.T) {
	for _, spec := range []string{
		"lowercase:junk",
		"fold_diacritics:x",
		"strip_punctuation:x",
		"sort_tokens:x",
		"trim:x",
		"lowercase:", // even an empty arg is a malformed spec
	} {
		if _, err := analyzer.New([]string{spec}); err == nil {
			t.Errorf("New(%q): want error, got nil", spec)
		}
	}
}

// strip_words with an empty resulting word set is a misconfiguration, not a
// silent no-op step.
func TestStripWordsEmptyWordSet(t *testing.T) {
	for _, spec := range []string{"strip_words:", "strip_words: , ,"} {
		if _, err := analyzer.New([]string{spec}); err == nil {
			t.Errorf("New(%q): want error, got nil", spec)
		}
	}
	// Sanity: a non-empty word set still works.
	if _, err := analyzer.New([]string{"strip_words:mr,dr"}); err != nil {
		t.Errorf("New(strip_words:mr,dr): %v", err)
	}
}

// Steps must return a defensive copy: mutating the returned slice must not
// change the analyzer's canonical spec.
func TestStepsDefensiveCopy(t *testing.T) {
	a, err := analyzer.New([]string{"lowercase", "trim"})
	if err != nil {
		t.Fatal(err)
	}
	s := a.Steps()
	s[0] = "mutated"
	if got := a.Steps(); got[0] != "lowercase" {
		t.Errorf("Steps aliases internal state: got %v", got)
	}
}

// PresetSteps must return a defensive copy: mutating the returned slice must
// not corrupt the global preset for every future Preset/PresetSteps call.
func TestPresetStepsDefensiveCopy(t *testing.T) {
	s, ok := analyzer.PresetSteps("identifier")
	if !ok {
		t.Fatal("PresetSteps(identifier): not found")
	}
	orig := s[0]
	s[0] = "bogus-step"
	s2, _ := analyzer.PresetSteps("identifier")
	if s2[0] != orig {
		t.Fatalf("PresetSteps aliases the global presets map: got %v", s2)
	}
	if _, err := analyzer.Preset("identifier"); err != nil {
		t.Fatalf("Preset(identifier) after caller mutation: %v", err)
	}
}

// Pin invalid-UTF-8 behavior: invalid bytes fold to U+FFFD in lowercase, then
// strip_punctuation drops them (not letter/digit/hyphen). Deterministic and
// idempotent — the build and query paths must key identically for any input.
func TestInvalidUTF8Pinned(t *testing.T) {
	a, err := analyzer.Preset("person-name")
	if err != nil {
		t.Fatal(err)
	}
	in := "\xffJos\xe9 Smith"
	once := a.Normalize(in)
	if want := "jos smith"; once != want {
		t.Errorf("Normalize(%q) = %q, want %q", in, once, want)
	}
	if twice := a.Normalize(once); twice != once {
		t.Errorf("not idempotent: %q -> %q", once, twice)
	}
}

// Pin empty/whitespace-only input → "".
func TestEmptyAndWhitespaceInput(t *testing.T) {
	a, err := analyzer.Preset("person-name")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"", "   ", " \t\n ", " "} {
		if got := a.Normalize(in); got != "" {
			t.Errorf("Normalize(%q) = %q, want empty", in, got)
		}
	}
}
