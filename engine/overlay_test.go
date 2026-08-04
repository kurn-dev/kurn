package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestUpsertVisibleImmediately(t *testing.T) {
	l := personList(t, engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}})
	if c := l.Query("dana kovak", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("pre-upsert: %+v", c)
	}
	l.Upsert([]engine.Entry{{ID: "p2", Keys: []string{"Dana Kovak"}}})
	c := l.Query("dana kovak", engine.QueryOpts{})
	if len(c) != 1 || c[0].EntryID != "p2" || c[0].Score != 100 {
		t.Fatalf("post-upsert: %+v", c)
	}
}

func TestUpsertSupersedesBase(t *testing.T) {
	l := personList(t, engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}})
	l.Upsert([]engine.Entry{{ID: "p1", Keys: []string{"Dana Kovak"}}}) // same ID, new keys
	if c := l.Query("marcus chen", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("old key still matches: %+v", c)
	}
	c := l.Query("dana kovak", engine.QueryOpts{})
	if len(c) != 1 || c[0].EntryID != "p1" {
		t.Fatalf("new key: %+v", c)
	}
}

func TestDeleteMasks(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}},
		engine.Entry{ID: "p2", Keys: []string{"Dana Kovak"}},
	)
	l.Delete("p1")
	if c := l.Query("marcus chen", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("deleted still matches: %+v", c)
	}
	// delete an overlay entry too
	l.Upsert([]engine.Entry{{ID: "p3", Keys: []string{"Omar Reyes"}}})
	l.Delete("p3")
	if c := l.Query("omar reyes", engine.QueryOpts{}); len(c) != 0 {
		t.Fatalf("deleted overlay entry still matches: %+v", c)
	}
}

// Key attribution must resolve through the overlay: both for an overlay-only
// hit and for a superseded ID present in BOTH byID maps (overlay must win).
func TestAttributionThroughOverlay(t *testing.T) {
	l := personList(t, engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}})
	l.Upsert([]engine.Entry{
		{ID: "p1", Keys: []string{"Marcus Chao"}}, // supersedes base: in both byID maps
		{ID: "p2", Keys: []string{"Dana Kovak"}},  // overlay-only
	})
	c := l.Query("dana kovak", engine.QueryOpts{})
	if len(c) != 1 || c[0].EntryID != "p2" || c[0].Key != "Dana Kovak" {
		t.Errorf("overlay-only attribution: %+v", c)
	}
	c = l.Query("marcus chao", engine.QueryOpts{})
	if len(c) != 1 || c[0].EntryID != "p1" || c[0].Key != "Marcus Chao" {
		t.Errorf("superseded-ID attribution (overlay must win): %+v", c)
	}
}

// Stats across an upsert that supersedes a base entry (live count unchanged)
// and a delete of an overlay-only entry (no tombstone).
func TestStatsSupersedeAndOverlayDelete(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}},
		engine.Entry{ID: "p2", Keys: []string{"Dana Kovak"}},
	)
	l.Upsert([]engine.Entry{{ID: "p1", Keys: []string{"Marcus Chao"}}})
	if e, o, tb := l.Stats(); e != 2 || o != 1 || tb != 1 {
		t.Errorf("after supersede: Stats() = %d,%d,%d want 2,1,1", e, o, tb)
	}
	l.Upsert([]engine.Entry{{ID: "p3", Keys: []string{"Omar Reyes"}}})
	l.Delete("p3") // overlay-only: must not create a tombstone
	if e, o, tb := l.Stats(); e != 2 || o != 1 || tb != 1 {
		t.Errorf("after overlay delete: Stats() = %d,%d,%d want 2,1,1", e, o, tb)
	}
}

// Supersede with partially-overlapping keys near the threshold: the old base
// copy ("Marcus Chen", payload v1) must never surface for a query matching
// the OLD key. We don't assert emptiness — the overlay copy may legitimately
// clear the threshold (overlay-segment IDF denominators exclude grams unknown
// to that segment) — but any p1 hit must carry the NEW key and payload, and
// p1 must never appear twice (base copy un-masked alongside overlay copy).
func TestUpsertSupersedePartialKeyOverlap(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}, Payload: json.RawMessage(`{"v":1}`)},
		engine.Entry{ID: "p2", Keys: []string{"Dana Kovak"}},
	)
	l.Upsert([]engine.Entry{{ID: "p1", Keys: []string{"Marcus Chao"}, Payload: json.RawMessage(`{"v":2}`)}})
	c := l.Query("marcus chen", engine.QueryOpts{})
	p1Hits := 0
	for _, cand := range c {
		if cand.EntryID != "p1" {
			continue
		}
		p1Hits++
		if string(cand.Payload) != `{"v":2}` || cand.Key != "Marcus Chao" {
			t.Errorf("superseded base copy leaked: %+v", cand)
		}
	}
	if p1Hits > 1 {
		t.Errorf("p1 returned %d times (base copy not masked): %+v", p1Hits, c)
	}
	// The new key must match at full score.
	c = l.Query("marcus chao", engine.QueryOpts{})
	if len(c) != 1 || c[0].EntryID != "p1" || c[0].Score != 100 {
		t.Errorf("new key query: %+v", c)
	}
}

func TestStats(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}},
		engine.Entry{ID: "p2", Keys: []string{"Dana Kovak"}},
	)
	l.Upsert([]engine.Entry{{ID: "p3", Keys: []string{"Omar Reyes"}}})
	l.Delete("p2")
	entries, overlay, tombs := l.Stats()
	if entries != 2 || overlay != 1 || tombs != 1 {
		t.Errorf("Stats() = %d,%d,%d want 2,1,1", entries, overlay, tombs)
	}
}
