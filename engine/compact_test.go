package engine_test

import (
	"reflect"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestCompactPreservesResults(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}},
		engine.Entry{ID: "p2", Keys: []string{"Dana Kovak"}},
	)
	l.Upsert([]engine.Entry{{ID: "p3", Keys: []string{"Omar Reyes"}}, {ID: "p1", Keys: []string{"Marcus Chao"}}})
	l.Delete("p2")

	queries := []string{"marcus chao", "omar reyes", "dana kovak", "marcus chen"}
	before := map[string][]engine.Candidate{}
	for _, q := range queries {
		before[q] = l.Query(q, engine.QueryOpts{})
	}

	l.Compact()

	entries, overlay, tombs := l.Stats()
	if overlay != 0 || tombs != 0 || entries != 2 {
		t.Fatalf("post-compact stats = %d,%d,%d want 2,0,0", entries, overlay, tombs)
	}
	for _, q := range queries {
		if got := l.Query(q, engine.QueryOpts{}); !reflect.DeepEqual(got, before[q]) {
			t.Errorf("Query(%q) changed after compact:\nbefore %+v\nafter  %+v", q, before[q], got)
		}
	}
}

// Compacting a fully-deleted list yields an empty, queryable list; a second
// Compact is safe and changes nothing.
func TestCompactFullyDeleted(t *testing.T) {
	l := personList(t, engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}})
	l.Delete("p1")
	for i := 0; i < 2; i++ {
		l.Compact()
		if e, o, tb := l.Stats(); e != 0 || o != 0 || tb != 0 {
			t.Fatalf("compact #%d: Stats() = %d,%d,%d want 0,0,0", i+1, e, o, tb)
		}
		if c := l.Query("marcus chen", engine.QueryOpts{}); len(c) != 0 {
			t.Fatalf("compact #%d: query on emptied list: %+v", i+1, c)
		}
	}
}

// Compacting an already-compacted list is idempotent: stats and query results
// are unchanged (the base is already ID-sorted, so even ord order holds).
func TestCompactTwiceIdempotent(t *testing.T) {
	l := personList(t,
		engine.Entry{ID: "p2", Keys: []string{"Dana Kovak"}},
		engine.Entry{ID: "p1", Keys: []string{"Marcus Chen"}},
	)
	l.Upsert([]engine.Entry{{ID: "p3", Keys: []string{"Omar Reyes"}}})
	l.Compact()
	queries := []string{"marcus chen", "dana kovak", "omar reyes"}
	before := map[string][]engine.Candidate{}
	for _, q := range queries {
		before[q] = l.Query(q, engine.QueryOpts{})
	}
	e1, o1, t1 := l.Stats()

	l.Compact()

	if e2, o2, t2 := l.Stats(); e2 != e1 || o2 != o1 || t2 != t1 {
		t.Errorf("stats changed: %d,%d,%d -> %d,%d,%d", e1, o1, t1, e2, o2, t2)
	}
	for _, q := range queries {
		if got := l.Query(q, engine.QueryOpts{}); !reflect.DeepEqual(got, before[q]) {
			t.Errorf("Query(%q) changed on second compact:\nbefore %+v\nafter  %+v", q, before[q], got)
		}
	}
}
