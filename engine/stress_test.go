package engine_test

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

// Stress test for the lock-free query path against the full mutation surface:
// query goroutines race one goroutine doing Upsert/Delete churn and one doing
// Compact. Entry "keep" stays alive through every mutation, so every query
// must see an internally consistent snapshot: "keep" found exactly once at
// score 100 (identical-string queries always score 100 in their segment) and
// no EntryID duplicated (an ID surfacing from both base and overlay would
// mean a broken tombstone invariant). Run under -race.
func TestConcurrentMutateCompactQuery(t *testing.T) {
	keep := engine.Entry{ID: "keep", Keys: []string{"Elena Vasquez"}}
	l := personList(t, keep)
	stressList(t, l, keep, "Elena Vasquez", 100)
}

// Exact-mode twin: the exact index (packed postings + key arena) is rebuilt
// by every Upsert/Delete/Compact while queries run lock-free against old
// snapshots — with parent_domain fallback so the per-level probe loop is
// exercised too ("keep" is found via its parent suffix).
func TestConcurrentMutateCompactQueryExact(t *testing.T) {
	keep := engine.Entry{ID: "keep", Keys: []string{"tempmail.com"}}
	l, err := engine.NewList("domains", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "domain"},
		Match:    engine.MatchConfig{Mode: "exact", Fallback: "parent_domain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{keep}); err != nil {
		t.Fatal(err)
	}
	stressList(t, l, keep, "smtp.tempmail.com", engine.ParentMatchScore)
}

// stressList races query goroutines against upsert/delete churn and Compact;
// every query must see keep exactly once at wantScore and no duplicate IDs.
func stressList(t *testing.T, l *engine.List, keep engine.Entry, query string, wantScore float64) {

	churnDone := make(chan struct{})
	stop := make(chan struct{})
	var mut sync.WaitGroup

	mut.Add(1)
	go func() { // upsert/delete churn; every batch re-upserts keep
		defer mut.Done()
		defer close(churnDone)
		for i := 0; i < 150; i++ {
			id := fmt.Sprintf("churn%d", i%8)
			l.Upsert([]engine.Entry{
				keep,
				{ID: id, Keys: []string{fmt.Sprintf("churn-person-%d.example", i%8)}},
			})
			l.Delete(id)
		}
	}()
	mut.Add(1)
	go func() { // compactor races Compact against the churn
		defer mut.Done()
		for {
			select {
			case <-churnDone:
				return
			default:
			}
			l.Compact()
			runtime.Gosched()
		}
	}()
	go func() {
		mut.Wait()
		close(stop)
	}()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := l.Query(query, engine.QueryOpts{})
				seen := map[string]int{}
				for _, cand := range c {
					seen[cand.EntryID]++
					if cand.EntryID == "keep" && cand.Score != wantScore {
						t.Errorf("keep score = %v, want %v: %+v", cand.Score, wantScore, c)
						return
					}
				}
				for id, n := range seen {
					if n > 1 {
						t.Errorf("duplicate candidate %q (x%d): %+v", id, n, c)
						return
					}
				}
				if seen["keep"] != 1 {
					t.Errorf("keep not found exactly once: %+v", c)
					return
				}
			}
		}()
	}
	mut.Wait()
	wg.Wait()
}
