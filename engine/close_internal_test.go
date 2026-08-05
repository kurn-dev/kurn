package engine

// Deterministic counterpart to the racing test in close_test.go: it pins the
// exact interleaving rather than hoping to hit it.

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloseWaitsForAnAlreadyAdmittedMutation(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := ListConfig{
		Analyzer: AnalyzerConfig{Preset: "person-name"},
		Match:    MatchConfig{Mode: "ngram"},
	}
	if _, err := st.CreateList("people", cfg); err != nil {
		t.Fatal(err)
	}

	admitted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	testHookAdmitted = func() {
		once.Do(func() {
			close(admitted)
			<-release // hold the mutation between admission and its list lock
		})
	}
	defer func() { testHookAdmitted = nil }()

	var wrote atomic.Bool
	go func() {
		if err := st.Upsert("people", []Entry{{ID: "p1", Keys: []string{"Anna Smith"}}}); err != nil {
			t.Error(err)
		}
		wrote.Store(true)
	}()
	<-admitted // the mutation is past the guard and holds no list lock

	closeReturned := make(chan struct{})
	go func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
		close(closeReturned)
	}()

	// This is the whole finding: sweeping the per-list locks here sees them
	// all free, because the mutation has not reached one yet.
	select {
	case <-closeReturned:
		t.Fatal("Close returned while an admitted mutation had not yet written")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the mutation was released")
	}
	if !wrote.Load() {
		t.Fatal("Close returned before the mutation finished writing")
	}
}

// TestCloseStopsTheGroupCommitter: an interval-mode store's group-commit
// goroutine must exit when the store closes. It used to live for the
// process lifetime, so every tenant remove/re-add cycle of an
// interval-fsync store retired one store and leaked one goroutine —
// indefinitely on a long-lived kurnd. The deterministic pin is the
// committer's stopped channel, which Close waits on; the goroutine count
// is the original leak reproduction shape, kept as a secondary check.
func TestCloseStopsTheGroupCommitter(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		st, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		st.JournalFsync = FsyncInterval
		if _, err := st.CreateList("people", internalPersonCfg()); err != nil {
			t.Fatal(err)
		}
		// The first interval-fsynced append starts the committer.
		if err := st.Upsert("people", []Entry{{ID: "p1", Keys: []string{"Marcus Chen"}}}); err != nil {
			t.Fatal(err)
		}
		if st.fsyncCh == nil {
			t.Fatal("committer never started; the test exercises nothing")
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		// Close waited for the committer's exit: the channel must already
		// be closed, with no sleeping or polling involved.
		select {
		case <-st.fsyncStopped:
		default:
			t.Fatal("committer still running after Close returned")
		}
	}
	// Secondary: the goroutine population returns to baseline (exited
	// goroutines unwind asynchronously, hence the bounded retry).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := runtime.NumGoroutine(); n <= before+2 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("goroutines before=%d after=%d — committers leaked", before, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
