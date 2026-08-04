package engine_test

// Functional coverage: Every JournalFsync mode produces
// identical logical results (appends acked, replayed after reopen), and the
// group-commit path releases concurrent waiters. Durability against actual
// power loss is not testable here — these pin the plumbing.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

func TestJournalFsyncModes(t *testing.T) {
	for _, mode := range []engine.FsyncMode{engine.FsyncNone, engine.FsyncEvery, engine.FsyncInterval} {
		t.Run(string(mode)+"-mode", func(t *testing.T) {
			dir := t.TempDir()
			st, err := engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			st.JournalFsync = mode
			st.JournalFsyncInterval = time.Millisecond
			if _, err := st.CreateList("codes", exactCfg()); err != nil {
				t.Fatal(err)
			}
			// Concurrent upserts: in interval mode these group-commit
			// through one committer; all must ack and all must persist.
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					e := engine.Entry{ID: fmt.Sprintf("c%d", i), Keys: []string{fmt.Sprintf("KEY-%d", i)}}
					if err := st.Upsert("codes", []engine.Entry{e}); err != nil {
						t.Errorf("upsert %d: %v", i, err)
					}
				}(i)
			}
			wg.Wait()

			st2, err := engine.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			l, ok := st2.List("codes")
			if !ok {
				t.Fatal("list missing after reopen")
			}
			for i := 0; i < 8; i++ {
				q := fmt.Sprintf("key-%d", i)
				if c := l.Query(q, engine.QueryOpts{}); len(c) != 1 {
					t.Fatalf("mode %q: entry %d not replayed: %+v", mode, i, c)
				}
			}
		})
	}
}
