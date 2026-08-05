package exact_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine/exact"
)

// mustFinish builds the index, failing the test on a build error.
func mustFinish(t *testing.T, b *exact.Builder) *exact.Index {
	t.Helper()
	idx, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return idx
}

func TestExact(t *testing.T) {
	b := exact.NewBuilder()
	b.Add(0, []string{"acme-042", "acme-42"})
	b.Add(1, []string{"globex-1"})
	b.Add(2, []string{"acme-042"}) // two entries may share a key
	idx := mustFinish(t, b)

	hits := idx.Lookup("acme-042")
	if len(hits) != 2 || hits[0] != 0 || hits[1] != 2 {
		t.Fatalf("Lookup(acme-042) = %v, want ords [0 2]", hits)
	}
	if got := idx.Lookup("nope"); got != nil {
		t.Errorf("Lookup(nope) = %+v, want nil", got)
	}
}

func TestAddDedupeAndEdgeCases(t *testing.T) {
	b := exact.NewBuilder()
	b.Add(0, []string{"x", "x"})         // within-one-Add duplicate
	b.Add(1, []string{"", "y", "", "y"}) // empty keys skipped, dup deduped
	b.Add(2, []string{"x"})
	idx := mustFinish(t, b)

	if hits := idx.Lookup("x"); len(hits) != 2 || hits[0] != 0 || hits[1] != 2 {
		t.Errorf("Lookup(x) = %v, want single posting per entry, ords [0 2]", hits)
	}
	if hits := idx.Lookup("y"); len(hits) != 1 || hits[0] != 1 {
		t.Errorf("Lookup(y) = %v, want ords [1]", hits)
	}
	if got := idx.Keys(); got != 2 {
		t.Errorf("Keys() = %d, want 2 (empty key must not be indexed)", got)
	}
	if got := idx.Lookup(""); got != nil {
		t.Errorf("Lookup(\"\") = %+v, want nil", got)
	}
}

// A key held by more than maxRun entries is a data bug: Finish must reject it
// with a typed *KeyOverflowError — never a panic, so daemon-serving builds on
// untrusted data can reject the input and carry on.
func TestFinishRejectsOverlongRun(t *testing.T) {
	b := exact.NewBuilder()
	keys := []string{"hot"}
	for ord := uint32(0); ord < 1<<20; ord++ { // one over the 2^20-1 cap
		b.Add(ord, keys)
	}
	idx, err := b.Finish()
	if err == nil {
		t.Fatalf("Finish accepted a >maxRun postings run (idx %p)", idx)
	}
	var ko *exact.KeyOverflowError
	if !errors.As(err, &ko) {
		t.Fatalf("error is %T (%v), want *KeyOverflowError", err, err)
	}
	if ko.Key != "hot" || ko.Count != 1<<20 || ko.Cap != 1<<20-1 {
		t.Errorf("KeyOverflowError = %+v, want Key=hot Count=%d Cap=%d", ko, 1<<20, 1<<20-1)
	}
}

// Restore rejects the maximum ordinal because ord+1 wraps numOrds to 0;
// Finish did not, though Add takes an arbitrary ordinal and only documents
// the ascending contract. The result was an index reporting NumOrds 0 while
// Lookup still returned that ordinal, so a caller's NumOrds-vs-entry-count
// check passed and then indexed out of bounds.
func TestFinishRejectsSuccessorlessOrdinal(t *testing.T) {
	b := exact.NewBuilder()
	b.Add(^uint32(0), []string{"anna"})
	idx, err := b.Finish()
	if err == nil {
		t.Fatalf("Finish accepted the maximum ordinal: NumOrds = %d, Lookup = %v",
			idx.NumOrds(), idx.Lookup("anna"))
	}
	if !strings.Contains(err.Error(), "successor") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
	// One below the maximum is legal and must stay legal.
	b2 := exact.NewBuilder()
	b2.Add(^uint32(0)-1, []string{"anna"})
	if _, err := b2.Finish(); err != nil {
		t.Fatalf("Finish refused a representable ordinal: %v", err)
	}
}
