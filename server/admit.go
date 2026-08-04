// Query admission control: each concurrent ngram lookup
// allocates ~4 B × numOrds of scratch per list touched, so N concurrent
// queries on a 50M-ordinal list would pin ~200 MB each — unbounded
// concurrency is an OOM, not a slowdown. Admission charges each query its
// scratch cost (engine List.ScratchBytes) against a configured budget:
// within budget queries run immediately, over budget they queue (FIFO,
// backpressure) until the request context or the queue timeout ends the
// wait with a 503. Memory is the cost model; the same bound also caps how
// many maximal-CPU scans (explicit no-floor queries — every gram a
// generator) run at once.
package server

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// admitter is a weighted FIFO gate over a scratch-memory budget. A nil
// admitter admits everything (budget disabled).
type admitter struct {
	budget   int64
	sem      *semaphore.Weighted
	queued   atomic.Int64 // waiters currently blocked in acquire
	inflight atomic.Int64 // bytes currently admitted
}

func newAdmitter(budgetBytes int64) *admitter {
	if budgetBytes <= 0 {
		return nil
	}
	return &admitter{budget: budgetBytes, sem: semaphore.NewWeighted(budgetBytes)}
}

// acquire admits cost bytes, blocking while the budget is exhausted; ctx
// bounds the wait (request context + queue timeout). A cost larger than the
// whole budget is clamped — the query runs alone rather than never. The
// returned release must be called exactly once; it is non-nil even on the
// zero-cost fast path.
func (a *admitter) acquire(ctx context.Context, cost int64) (release func(), err error) {
	if a == nil || cost <= 0 {
		return func() {}, nil
	}
	if cost > a.budget {
		cost = a.budget
	}
	a.queued.Add(1)
	err = a.sem.Acquire(ctx, cost)
	a.queued.Add(-1)
	if err != nil {
		return nil, err
	}
	a.inflight.Add(cost)
	return func() {
		a.inflight.Add(-cost)
		a.sem.Release(cost)
	}, nil
}

// depth reports (queued waiters, admitted bytes) for the health metric.
func (a *admitter) depth() (int64, int64) {
	if a == nil {
		return 0, 0
	}
	return a.queued.Load(), a.inflight.Load()
}

// rateCheck applies the tenant's token bucket: reject-if-not-
// immediate (Reserve + Cancel — the request never waits on tokens; waiting
// is the admitter's job, throttling is a fast no). Returns the Retry-After
// seconds and the 429 message, or (0, "") when admitted. Counted per
// tenant in the metrics registry. Legacy and unlimited tenants pass.
func (s *srv) rateCheck(ctx context.Context) (retryAfter int, msg string) {
	t := tenantFrom(ctx)
	if t == nil || t.limiter == nil {
		return 0, ""
	}
	res := t.limiter.Reserve()
	d := res.Delay()
	if d <= 0 {
		return 0, "" // token consumed, admitted
	}
	res.Cancel() // not waiting: hand the reservation back
	s.reg.tenant429(t.name).Add(1)
	ra := int(math.Ceil(d.Seconds()))
	if ra < 1 {
		ra = 1
	}
	return ra, fmt.Sprintf("rate limit: %g queries/s exceeded, retry in ~%ds", t.spec.Quotas.RatePerSec, ra)
}
