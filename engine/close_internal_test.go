package engine

// Deterministic counterpart to the racing test in close_test.go: it pins the
// exact interleaving rather than hoping to hit it.

import (
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
