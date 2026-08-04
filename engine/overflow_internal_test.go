package engine

// Internal regression tests for the exact-mode hot-key overflow,
// Compact path: a poisoned overlay is unreachable through the
// public API post-fix (Upsert/Delete reject the poison before it lands in
// overlaySrc), so these poison overlaySrc directly to prove Compact (and a
// Delete on poisoned state) fail as clean no-ops instead of panicking.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kurn-dev/kurn/engine/exact"
)

// poisonOverlay fills l's overlay source with more than maxRun entries
// sharing one analyzed key — simulating the legacy state a pre-fix journal
// could have left behind (post-fix, the public API rejects such batches
// before they ever reach overlaySrc).
func poisonOverlay(t *testing.T, l *List) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i < 1<<20; i++ {
		l.overlaySrc[fmt.Sprintf("e%07d", i)] = Entry{ID: fmt.Sprintf("e%07d", i), Keys: []string{"hot"}}
	}
}

func TestCompactOnPoisonedOverlayFailsClean(t *testing.T) {
	l, err := NewList("codes", ListConfig{
		Analyzer: AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]Entry{{ID: "g1", Keys: []string{"good"}}}); err != nil {
		t.Fatal(err)
	}
	poisonOverlay(t, l)
	version := l.Version()

	err = l.Compact()
	var ko *exact.KeyOverflowError
	if !errors.As(err, &ko) {
		t.Fatalf("Compact error = %v, want *exact.KeyOverflowError", err)
	}
	// Nothing mutated: version unchanged, the good base still served.
	if l.Version() != version {
		t.Fatalf("failed Compact mutated state: %q -> %q", version, l.Version())
	}
	if c := l.Query("good", QueryOpts{}); len(c) != 1 || c[0].EntryID != "g1" {
		t.Fatalf("base entry missing after failed Compact: %+v", c)
	}

	// A Delete on the poisoned state also fails clean (its overlay rebuild
	// hits the same overflow) — an error, not a panic.
	if err := l.Delete("g1"); !errors.As(err, &ko) {
		t.Fatalf("Delete on poisoned overlay = %v, want *exact.KeyOverflowError", err)
	}
	if l.Version() != version {
		t.Fatal("failed Delete mutated state")
	}
}
