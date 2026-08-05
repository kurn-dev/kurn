package engine_test

// Regression tests: Keys that analyze to "" are dropped
// silently, and an entry whose keys ALL collapse still occupies an ordinal
// and counts in Stats() while being unfindable — BuildStats makes the loss
// visible.

import (
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/exact"
)

func TestBuildStatsCountsAnalyzedAwayKeys(t *testing.T) {
	l, err := engine.NewList("codes", engine.ListConfig{
		// strip_punctuation + trim collapse punctuation-only keys to "".
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "strip_punctuation", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		{ID: "e1", Keys: []string{"...", "ok"}}, // one key dropped, one survives
		{ID: "e2", Keys: []string{"!!!"}},       // all keys collapse: keyless
		{ID: "e3", Keys: []string{"fine"}},      // clean
	}); err != nil {
		t.Fatal(err)
	}

	dropped, keyless := l.BuildStats()
	if dropped != 2 || keyless != 1 {
		t.Fatalf("BuildStats = (%d, %d), want (2, 1)", dropped, keyless)
	}
	// The keyless entry is exactly the invisible-loss case: counted in
	// Stats() entries, unfindable by any query.
	if e, _, _ := l.Stats(); e != 3 {
		t.Fatalf("Stats entries = %d, want 3 (keyless entry still occupies an ordinal)", e)
	}
	if c := l.Query("!!!", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("keyless entry matched: %+v", c)
	}

	// Overlay builds count too (summed with base).
	if err := l.Upsert([]engine.Entry{{ID: "e4", Keys: []string{"???", "---"}}}); err != nil {
		t.Fatal(err)
	}
	dropped, keyless = l.BuildStats()
	if dropped != 4 || keyless != 2 {
		t.Fatalf("BuildStats after keyless upsert = (%d, %d), want (4, 2)", dropped, keyless)
	}

	// Compact folds everything into one base; counters recompute over the
	// fold, not accumulate across builds.
	if err := l.Compact(); err != nil {
		t.Fatal(err)
	}
	dropped, keyless = l.BuildStats()
	if dropped != 4 || keyless != 2 {
		t.Fatalf("BuildStats after compact = (%d, %d), want (4, 2)", dropped, keyless)
	}
}

// ReplaceWithIndex deliberately tolerates an index with fewer ordinals than
// entries, so a stale-index crash window stays recoverable instead of
// fatal. The entries past the index's reach are then live, counted in
// Stats(), and findable by nothing — the same invisible loss a keyless
// entry is, with a different cause.
//
// Which cause it is cannot be read off the index: an entry whose keys all
// analyzed away contributes no postings and lowers the highest ordinal
// exactly as a missing entry does. The count therefore travels WITH the
// index (IndexBuildInfo) instead of being inferred from it.
func TestUnindexedEntriesAreCounted(t *testing.T) {
	entries := []engine.Entry{
		{ID: "p1", Keys: []string{"Marcus Chen"}},
		{ID: "p2", Keys: []string{"Dana Kovak"}},
		{ID: "p3", Keys: []string{"Clara Diaz"}},
	}
	// An index over the first two only: a base.idx one publish behind its
	// base.jsonl, and it says so.
	idx := buildIdx(t, "marcus chen", "dana kovak")
	stale := &engine.IndexBuildInfo{Entries: 2}

	l := personList(t)
	if err := l.ReplaceWithIndexInfo(entries, idx, stale); err != nil {
		t.Fatal(err)
	}
	if got := l.UnindexedEntries(); got != 1 {
		t.Fatalf("UnindexedEntries = %d, want 1", got)
	}
	// The count must describe something real: p3 is live in Stats and
	// findable by nothing.
	if n, _, _ := l.Stats(); n != 3 {
		t.Fatalf("Stats entries = %d, want 3 — the entry IS live, that is the point", n)
	}
	// Its own key must not reach it. (Other entries may still surface: on a
	// corpus this small nearly every query gram is unknown to the index and
	// excluded from the score denominator, so one shared gram reads as 100.
	// The claim here is about p3, not about what else matches.)
	for _, c := range l.Query("clara diaz", engine.QueryOpts{}) {
		if c.EntryID == "p3" {
			t.Fatalf("the unindexed entry was findable after all: %+v", c)
		}
	}

	// An index that covers every entry reports zero even though the same
	// arithmetic on ordinals would not: this is the case the old inference
	// got wrong.
	full := personList(t)
	if err := full.ReplaceWithIndexInfo(entries[:2], idx, &engine.IndexBuildInfo{Entries: 2}); err != nil {
		t.Fatal(err)
	}
	if got := full.UnindexedEntries(); got != 0 {
		t.Fatalf("fully indexed list reports %d unindexed entries, want 0", got)
	}

	// With no build info nothing is claimed. Reporting zero for an unknown
	// is wrong, but inferring it from the index names the wrong repair.
	quiet := personList(t)
	if err := quiet.ReplaceWithIndex(entries, idx); err != nil {
		t.Fatal(err)
	}
	if got := quiet.UnindexedEntries(); got != 0 {
		t.Fatalf("a list given no build info claimed %d unindexed entries", got)
	}
}

// Build info is a claim about the index, and impossible claims must be
// refused at the boundary: negative counts would surface as negative
// metrics, and Entries past the slice fabricates an unindexed count.
func TestImpossibleBuildInfoIsRefused(t *testing.T) {
	entries := []engine.Entry{
		{ID: "p1", Keys: []string{"Marcus Chen"}},
		{ID: "p2", Keys: []string{"Dana Kovak"}},
	}
	idx := buildIdx(t, "marcus chen", "dana kovak")
	for name, bi := range map[string]*engine.IndexBuildInfo{
		"negative everything":  {Entries: -1, DroppedKeys: -2, KeylessEntries: -3},
		"keyless past entries": {Entries: 2, KeylessEntries: 3},
		"entries past slice":   {Entries: 5},
		// Internally contradictory: the index carries ordinals 0 and 1,
		// so it cannot have come from a one-entry build. Accepting it
		// reported a reachable entry as unindexed.
		"ordinals past claimed entries": {Entries: 1},
	} {
		// Install real content first: a rejected call must leave the
		// serving snapshot and its metrics untouched, not half-applied.
		l := personList(t, engine.Entry{ID: "keep", Keys: []string{"Elena Vasquez"}})
		if err := l.ReplaceWithIndexInfo(entries, idx, bi); err == nil {
			t.Errorf("%s: accepted %+v", name, bi)
			continue
		}
		if n, _, _ := l.Stats(); n != 1 {
			t.Errorf("%s: rejected call disturbed the snapshot (entries=%d)", name, n)
		}
		if c := l.Query("elena vasquez", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "keep" {
			t.Errorf("%s: prior content stopped serving after a rejected install: %+v", name, c)
		}
		if u := l.UnindexedEntries(); u != 0 {
			t.Errorf("%s: rejected call left a loss metric behind (unindexed=%d)", name, u)
		}
	}

	// Exact mode enforces the same relationship, and — symmetrically with
	// the ngram half above — rejection must leave an existing snapshot
	// untouched, not merely return an error from an empty list.
	eb := exact.NewBuilder()
	eb.Add(0, []string{"aa"})
	eb.Add(1, []string{"bb"})
	eidx, err := eb.Finish()
	if err != nil {
		t.Fatal(err)
	}
	el, err := engine.NewList("codes", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := el.Replace([]engine.Entry{{ID: "keep", Keys: []string{"zz"}}}); err != nil {
		t.Fatal(err)
	}
	beforeE, beforeO, beforeT := el.Stats()
	beforeD, beforeK := el.BuildStats()

	if err := el.ReplaceWithExactIndexInfo(entries, eidx, &engine.IndexBuildInfo{Entries: 1}); err == nil {
		t.Fatal("exact: accepted an index with ordinals past its claimed entry count")
	}
	if c := el.Query("zz", engine.QueryOpts{}); len(c) != 1 || c[0].EntryID != "keep" {
		t.Errorf("exact: rejected install disturbed prior content: %+v", c)
	}
	if e, o, tb := el.Stats(); e != beforeE || o != beforeO || tb != beforeT {
		t.Errorf("exact: rejected install changed Stats: (%d,%d,%d) -> (%d,%d,%d)",
			beforeE, beforeO, beforeT, e, o, tb)
	}
	if d, k := el.BuildStats(); d != beforeD || k != beforeK || el.UnindexedEntries() != 0 {
		t.Errorf("exact: rejected install changed loss metrics: dropped %d->%d keyless %d->%d unindexed %d",
			beforeD, d, beforeK, k, el.UnindexedEntries())
	}
}
