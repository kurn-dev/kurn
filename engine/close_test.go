package engine_test

// Store.Close under concurrency. These are STRESS tests: they exercise the
// contract from many call shapes at once and are worth running under -race,
// but they did NOT catch the ordering bug they were written for — a racing
// test only fails when it wins the race. TestCloseWaitsForAnAlreadyAdmitted-
// Mutation in close_internal_test.go pins that interleaving deterministically
// and is the real proof.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestCloseAdmitsNoWriteAfterItReturns(t *testing.T) {
	for attempt := 0; attempt < 300; attempt++ {
		dir := t.TempDir()
		st, err := engine.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateList("people", robustList()); err != nil {
			t.Fatal(err)
		}
		var returned atomic.Bool
		var late atomic.Int64
		var wg sync.WaitGroup
		// Writers race Close from every mutation entry point that touches
		// disk, so the window is hit from several call shapes at once.
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				e := []engine.Entry{{ID: fmt.Sprint(i), Keys: []string{"Anna Smith"}}}
				for j := 0; j < 40; j++ {
					var err error
					switch j % 4 {
					case 0:
						err = st.Upsert("people", e)
					case 1:
						err = st.Replace("people", e)
					case 2:
						err = st.Delete("people", "0")
					case 3:
						err = st.Compact("people")
					}
					if err == nil && returned.Load() {
						late.Add(1)
					}
				}
			}(i)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		returned.Store(true)
		wg.Wait()
		if n := late.Load(); n > 0 {
			t.Fatalf("attempt %d: %d mutations succeeded after Close returned", attempt, n)
		}
	}
}

// A second caller must not read "someone is closing" as "the directory is
// released": every Close return has to mean the same thing.
func TestConcurrentCloseAllWaitForTheDrain(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", robustList()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if err := st.Upsert("people", []engine.Entry{{ID: fmt.Sprint(i), Keys: []string{"Anna Smith"}}}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var done atomic.Int64
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Close(); err != nil {
				t.Error(err)
			}
			// Every returning caller must see a fully drained store.
			if err := st.Upsert("people", []engine.Entry{{ID: "x", Keys: []string{"X"}}}); err == nil {
				t.Error("a mutation was admitted after Close returned")
			}
			done.Add(1)
		}()
	}
	wg.Wait()
	if done.Load() != 6 {
		t.Fatalf("only %d of 6 Close callers completed", done.Load())
	}
}
