package engine

import (
	"reflect"
	"strings"
	"testing"
)

func newPersonListInternal(t *testing.T) *List {
	t.Helper()
	l, err := NewList("people", ListConfig{
		Analyzer: AnalyzerConfig{Preset: "person-name"},
		Match:    MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true, Threshold: 0.6, TopK: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestApplyJournalMatchesSequential replays a journal with the tricky
// interleavings (upsert of a base ID, upsert-then-delete of a new ID,
// delete-then-reupsert of a base ID, last-wins re-upsert) in one batch and
// checks the end state is exactly what sequential Upsert/Delete produce.
func TestApplyJournalMatchesSequential(t *testing.T) {
	// Names in the ABSOLUTE assertions below are pairwise gram-disjoint
	// (fused 2+3-grams): with segment-local scoring, one shared gram makes
	// the known-gram denominator that gram alone and scores a spurious 100.
	base := []Entry{
		{ID: "p1", Keys: []string{"Marcus Chen"}},
		{ID: "p2", Keys: []string{"Dana Kovak"}},
		{ID: "p4", Keys: []string{"Piotr Wexler"}},
	}
	ops := []journalRec{
		{Op: "upsert", Entry: &Entry{ID: "p1", Keys: []string{"Marcus Chao"}}}, // upsert base ID
		{Op: "delete", ID: "p2"}, // delete base ID
		{Op: "upsert", Entry: &Entry{ID: "p3", Keys: []string{"Rosa Almeida"}}}, // new ID
		{Op: "delete", ID: "p3"}, // upsert-then-delete of new ID
		{Op: "upsert", Entry: &Entry{ID: "p3", Keys: []string{"Rosa Whitfield"}}}, // re-upsert, last wins
		{Op: "delete", ID: "p4"}, // delete base ID...
		{Op: "upsert", Entry: &Entry{ID: "p4", Keys: []string{"Piotr Wexley"}}}, // ...then re-upsert it
		{Op: "delete", ID: "nope"}, // delete of absent ID: no-op
	}

	batch := newPersonListInternal(t)
	if err := batch.Replace(append([]Entry(nil), base...)); err != nil {
		t.Fatal(err)
	}
	seq := newPersonListInternal(t)
	if err := seq.Replace(append([]Entry(nil), base...)); err != nil {
		t.Fatal(err)
	}

	batch.mu.Lock()
	genBefore := batch.gen
	batch.applyJournalLocked(ops, 0)
	if batch.gen != genBefore+1 {
		t.Errorf("batch apply bumped gen %d times, want exactly 1", batch.gen-genBefore)
	}
	batch.mu.Unlock()

	for _, op := range ops {
		switch op.Op {
		case "upsert":
			seq.Upsert([]Entry{*op.Entry})
		case "delete":
			seq.Delete(op.ID)
		}
	}

	// End states must match exactly.
	be, bo, bt := batch.Stats()
	se, so, st := seq.Stats()
	if be != se || bo != so || bt != st {
		t.Fatalf("stats: batch %d,%d,%d vs sequential %d,%d,%d", be, bo, bt, se, so, st)
	}
	if got, want := batch.LiveEntries(), seq.LiveEntries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("live entries: batch %+v vs sequential %+v", got, want)
	}
	for _, q := range []string{"marcus chao", "marcus chen", "dana kovak", "rosa whitfield", "rosa almeida", "piotr wexley", "piotr wexler"} {
		got := batch.Query(q, QueryOpts{})
		want := seq.Query(q, QueryOpts{})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("query %q: batch %+v vs sequential %+v", q, got, want)
		}
	}

	// Spot-check the expected end state directly.
	if c := batch.Query("marcus chao", QueryOpts{}); len(c) != 1 || c[0].EntryID != "p1" {
		t.Errorf("upserted base ID: %+v", c)
	}
	if c := batch.Query("dana kovak", QueryOpts{}); len(c) != 0 {
		t.Errorf("deleted base ID still matches: %+v", c)
	}
	if c := batch.Query("rosa whitfield", QueryOpts{}); len(c) != 1 || c[0].EntryID != "p3" {
		t.Errorf("re-upserted new ID: %+v", c)
	}
	if c := batch.Query("piotr wexley", QueryOpts{}); len(c) != 1 || c[0].EntryID != "p4" {
		t.Errorf("delete-then-reupsert of base ID: %+v", c)
	}
	// Invariant sweep on the batch snapshot: tombstones ⊆ base.byID, no live
	// ID both in overlay and un-tombstoned base, tombstones don't hit overlay.
	s := batch.snap.Load()
	for id := range s.tombstones {
		if _, ok := s.base.byID[id]; !ok {
			t.Errorf("tombstone %q not in base", id)
		}
	}
	if s.overlay != nil {
		for id := range s.overlay.byID {
			if _, inBase := s.base.byID[id]; inBase {
				if _, dead := s.tombstones[id]; !dead {
					t.Errorf("ID %q live in both overlay and base", id)
				}
			}
		}
	}
}

// TestApplyJournalEmptyAndMalformed: empty batch is a no-op (no gen bump);
// unknown ops and upserts without an entry are skipped, not fatal.
func TestApplyJournalMalformedRecords(t *testing.T) {
	l := newPersonListInternal(t)
	if err := l.Replace([]Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	gen := l.gen
	l.applyJournalLocked(nil, 0)
	if l.gen != gen {
		t.Errorf("empty batch bumped gen")
	}
	l.applyJournalLocked([]journalRec{
		{Op: "upsert", Entry: nil},
		{Op: "frobnicate", ID: "p1"},
		{Op: "upsert", Entry: &Entry{ID: "p2", Keys: []string{"Dana Kovak"}}},
	}, 0)
	l.mu.Unlock()
	if c := l.Query("dana kovak", QueryOpts{}); len(c) != 1 {
		t.Fatalf("valid record after malformed ones not applied: %+v", c)
	}
	if c := l.Query("marcus chen", QueryOpts{}); len(c) != 1 {
		t.Fatalf("malformed records damaged base: %+v", c)
	}
	if !strings.Contains(l.Version(), "gen") {
		t.Fatal("version stamp missing")
	}
}
