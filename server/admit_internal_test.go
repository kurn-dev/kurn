package server

// Deterministic unit tests for the query admitter.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdmitterWithinBudget(t *testing.T) {
	a := newAdmitter(100)
	r1, err := a.acquire(context.Background(), 60)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.acquire(context.Background(), 40)
	if err != nil {
		t.Fatal(err)
	}
	if q, in := a.depth(); q != 0 || in != 100 {
		t.Fatalf("depth = (%d, %d), want (0, 100)", q, in)
	}
	r1()
	r2()
	if q, in := a.depth(); q != 0 || in != 0 {
		t.Fatalf("depth after release = (%d, %d), want (0, 0)", q, in)
	}
}

func TestAdmitterBlocksAndTimesOut(t *testing.T) {
	a := newAdmitter(100)
	r1, err := a.acquire(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	// Budget exhausted: the next acquire must wait, then fail at deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.acquire(ctx, 1); err == nil {
		t.Fatal("acquire succeeded with an exhausted budget")
	}
	r1()
	// Released: the same acquire now succeeds immediately.
	r2, err := a.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("post-release acquire: %v", err)
	}
	r2()
}

func TestAdmitterBlocksThenAdmitsOnRelease(t *testing.T) {
	a := newAdmitter(100)
	r1, err := a.acquire(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	admitted := make(chan struct{})
	go func() {
		r2, err := a.acquire(context.Background(), 50)
		if err != nil {
			t.Errorf("queued acquire: %v", err)
			close(admitted)
			return
		}
		r2()
		close(admitted)
	}()
	// The waiter must be queued, not admitted.
	deadline := time.Now().Add(time.Second)
	for {
		if q, _ := a.depth(); q == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter never queued")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-admitted:
		t.Fatal("waiter admitted while budget exhausted")
	default:
	}
	r1()
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("waiter not admitted after release")
	}
}

func TestAdmitterRefusesOversizeCost(t *testing.T) {
	a := newAdmitter(100)
	// The budget is a CEILING. A single query bigger than the whole budget
	// used to be clamped and run alone — quietly exceeding the bound the
	// setting promises — so it must now be refused with the typed error
	// the server turns into an actionable 503.
	r, err := a.acquire(context.Background(), 1000)
	var over *budgetExceededError
	if !errors.As(err, &over) {
		if r != nil {
			r()
		}
		t.Fatalf("over-budget cost admitted (err=%v), want budgetExceededError", err)
	}
	if _, in := a.depth(); in != 0 {
		t.Fatalf("inflight = %d after a refusal, want 0", in)
	}
	// A cost exactly at the budget still runs.
	r, err = a.acquire(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	r()
}

func TestAdmitterNilAndZeroCost(t *testing.T) {
	var a *admitter // disabled
	r, err := a.acquire(context.Background(), 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	r()
	b := newAdmitter(10)
	r, err = b.acquire(context.Background(), 0) // exact-only query
	if err != nil {
		t.Fatal(err)
	}
	r()
}
