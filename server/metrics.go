// Per-list metrics with a hand-written Prometheus text exposition
// (the format is trivial and
// stable, and a page of code beats a dependency tree). Query-path
// instrumentation is atomics only — no locks, no allocation on the hot
// path once a list's slot exists. Label values are list names, whose
// charset (^[a-z0-9][a-z0-9_-]*$) needs no Prometheus escaping.
package server

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

// latBuckets are the histogram upper bounds in seconds: 100 µs to 10 s,
// log-spaced — exact-mode hits land in the first bucket, 10M-key no-floor
// ngram scans in the last few. Fixed at compile time; +Inf is implicit.
var latBuckets = [numBuckets]float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

const numBuckets = 16

// histogram is a fixed-bucket Prometheus histogram: counts per bucket
// (cumulative rendered at exposition, stored per-bucket), total count, and
// the sum in nanoseconds (integer atomics — no float CAS loops).
type histogram struct {
	buckets [numBuckets + 1]atomic.Uint64 // last slot = +Inf overflow
	count   atomic.Uint64
	sumNs   atomic.Int64
}

func (h *histogram) observe(d time.Duration) {
	s := d.Seconds()
	i := sort.SearchFloat64s(latBuckets[:], s) // first bucket with bound >= s
	h.buckets[i].Add(1)
	h.count.Add(1)
	h.sumNs.Add(int64(d))
}

// listMetrics is one list's query-path counters.
type listMetrics struct {
	queries atomic.Uint64
	hits    atomic.Uint64 // queries with >= 1 candidate from this list
	lat     histogram
}

// mkey identifies a metric series: tenant is "" in single-tenant mode
// (the label is omitted from the exposition then).
type mkey struct{ tenant, list string }

// metricsRegistry holds per-series slots and the mutation counters.
type metricsRegistry struct {
	mu    sync.RWMutex
	lists map[mkey]*listMetrics
	t429s map[string]*atomic.Uint64 // per-tenant 429 counters (survive reloads)
	tMuts map[string]*atomic.Uint64 // per-tenant acknowledged mutations (metering)

	upserts, deletes, replaces, compacts, creates atomic.Uint64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{
		lists: map[mkey]*listMetrics{},
		t429s: map[string]*atomic.Uint64{},
		tMuts: map[string]*atomic.Uint64{},
	}
}

// tenant429 returns the tenant's 429 counter, creating it on first use.
// Lives in the registry (not the tenant object) so it survives reloads.
func (m *metricsRegistry) tenant429(tenant string) *atomic.Uint64 {
	return m.tenantCounter(&m.t429s, tenant)
}

// tenantMutations returns the tenant's acknowledged-mutation counter (the
// metering tick auditMut bumps).
func (m *metricsRegistry) tenantMutations(tenant string) *atomic.Uint64 {
	return m.tenantCounter(&m.tMuts, tenant)
}

func (m *metricsRegistry) tenantCounter(mp *map[string]*atomic.Uint64, tenant string) *atomic.Uint64 {
	m.mu.RLock()
	c := (*mp)[tenant]
	m.mu.RUnlock()
	if c != nil {
		return c
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c = (*mp)[tenant]; c == nil {
		c = &atomic.Uint64{}
		(*mp)[tenant] = c
	}
	return c
}

// list returns the series' slot, creating it on first use. Steady state
// is one RLock + map read.
func (m *metricsRegistry) list(k mkey) *listMetrics {
	m.mu.RLock()
	lm := m.lists[k]
	m.mu.RUnlock()
	if lm != nil {
		return lm
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if lm = m.lists[k]; lm == nil {
		lm = &listMetrics{}
		m.lists[k] = lm
	}
	return lm
}

// observeQuery records one per-list query: its duration and whether this
// list contributed candidates.
func (m *metricsRegistry) observeQuery(tenant, list string, d time.Duration, hit bool) {
	lm := m.list(mkey{tenant, list})
	lm.queries.Add(1)
	if hit {
		lm.hits.Add(1)
	}
	lm.lat.observe(d)
}

// labels renders the label set for a series (tenant omitted when "").
func (k mkey) labels() string {
	if k.tenant == "" {
		return fmt.Sprintf("{list=%q}", k.list)
	}
	return fmt.Sprintf("{tenant=%q,list=%q}", k.tenant, k.list)
}

// gaugeLabels renders labels for store-derived gauges.
func gaugeLabels(tenant, list string) string {
	return mkey{tenant, list}.labels()
}

// metrics renders the exposition. Gauges come fresh from the store (they
// ARE the current state); counters/histograms from the registry.
func (s *srv) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Stable order: tenants sorted (tenantStores is name-sorted), lists
	// sorted within each; gauges then counters then histogram.
	type unit struct {
		tenant string
		l      *engine.List
	}
	var units []unit
	for _, ts := range s.tenantStores() {
		lists := ts.st.Lists()
		sort.Slice(lists, func(a, b int) bool { return lists[a].Name() < lists[b].Name() })
		for _, l := range lists {
			units = append(units, unit{ts.name, l})
		}
	}

	fmt.Fprintf(w, "# HELP kurn_list_entries Live entries in the list (base minus tombstones, plus overlay).\n# TYPE kurn_list_entries gauge\n")
	for _, u := range units {
		e, _, _ := u.l.Stats()
		fmt.Fprintf(w, "kurn_list_entries%s %d\n", gaugeLabels(u.tenant, u.l.Name()), e)
	}
	fmt.Fprintf(w, "# HELP kurn_list_overlay Overlay entries awaiting compaction.\n# TYPE kurn_list_overlay gauge\n")
	for _, u := range units {
		_, ov, _ := u.l.Stats()
		fmt.Fprintf(w, "kurn_list_overlay%s %d\n", gaugeLabels(u.tenant, u.l.Name()), ov)
	}
	fmt.Fprintf(w, "# HELP kurn_list_tombstones Tombstoned base entries.\n# TYPE kurn_list_tombstones gauge\n")
	for _, u := range units {
		_, _, tomb := u.l.Stats()
		fmt.Fprintf(w, "kurn_list_tombstones%s %d\n", gaugeLabels(u.tenant, u.l.Name()), tomb)
	}
	fmt.Fprintf(w, "# HELP kurn_list_dropped_keys Keys the analyzer collapsed to empty (never indexed).\n# TYPE kurn_list_dropped_keys gauge\n")
	for _, u := range units {
		d, _ := u.l.BuildStats()
		fmt.Fprintf(w, "kurn_list_dropped_keys%s %d\n", gaugeLabels(u.tenant, u.l.Name()), d)
	}
	fmt.Fprintf(w, "# HELP kurn_list_keyless_entries Entries with no indexable key (counted in entries, unfindable).\n# TYPE kurn_list_keyless_entries gauge\n")
	for _, u := range units {
		_, k := u.l.BuildStats()
		fmt.Fprintf(w, "kurn_list_keyless_entries%s %d\n", gaugeLabels(u.tenant, u.l.Name()), k)
	}
	fmt.Fprintf(w, "# HELP kurn_list_unindexed_entries Entries the installed index does not reach (stale base.idx; counted in entries, unfindable).\n# TYPE kurn_list_unindexed_entries gauge\n")
	for _, u := range units {
		fmt.Fprintf(w, "kurn_list_unindexed_entries%s %d\n", gaugeLabels(u.tenant, u.l.Name()), u.l.UnindexedEntries())
	}

	m := s.reg
	m.mu.RLock()
	keys := make([]mkey, 0, len(m.lists))
	for k := range m.lists {
		keys = append(keys, k)
	}
	m.mu.RUnlock()
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].tenant != keys[b].tenant {
			return keys[a].tenant < keys[b].tenant
		}
		return keys[a].list < keys[b].list
	})

	fmt.Fprintf(w, "# HELP kurn_queries_total Queries served, per list.\n# TYPE kurn_queries_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(w, "kurn_queries_total%s %d\n", k.labels(), m.list(k).queries.Load())
	}
	fmt.Fprintf(w, "# HELP kurn_query_hits_total Queries with at least one candidate, per list.\n# TYPE kurn_query_hits_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(w, "kurn_query_hits_total%s %d\n", k.labels(), m.list(k).hits.Load())
	}
	fmt.Fprintf(w, "# HELP kurn_query_duration_seconds Per-list lookup latency.\n# TYPE kurn_query_duration_seconds histogram\n")
	for _, k := range keys {
		h := &m.list(k).lat
		lb := k.labels()
		inner := lb[1 : len(lb)-1] // strip braces to append le
		var cum uint64
		for i, ub := range latBuckets[:] {
			cum += h.buckets[i].Load()
			fmt.Fprintf(w, "kurn_query_duration_seconds_bucket{%s,le=%q} %d\n", inner, trimFloat(ub), cum)
		}
		cum += h.buckets[numBuckets].Load()
		fmt.Fprintf(w, "kurn_query_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", inner, cum)
		fmt.Fprintf(w, "kurn_query_duration_seconds_sum%s %g\n", lb, float64(h.sumNs.Load())/1e9)
		fmt.Fprintf(w, "kurn_query_duration_seconds_count%s %d\n", lb, h.count.Load())
	}

	fmt.Fprintf(w, "# HELP kurn_mutations_total Acknowledged mutations by operation.\n# TYPE kurn_mutations_total counter\n")
	for _, mc := range []struct {
		op string
		c  *atomic.Uint64
	}{
		{"create", &m.creates}, {"upsert", &m.upserts}, {"delete", &m.deletes},
		{"replace", &m.replaces}, {"compact", &m.compacts},
	} {
		fmt.Fprintf(w, "kurn_mutations_total{op=%q} %d\n", mc.op, mc.c.Load())
	}

	if s.admit != nil {
		queued, inflight := s.admit.depth()
		fmt.Fprintf(w, "# HELP kurn_query_queue_depth Queries waiting for scratch-budget admission.\n# TYPE kurn_query_queue_depth gauge\nkurn_query_queue_depth %d\n", queued)
		fmt.Fprintf(w, "# HELP kurn_query_inflight_bytes Scratch bytes admitted and in flight.\n# TYPE kurn_query_inflight_bytes gauge\nkurn_query_inflight_bytes %d\n", inflight)
	}

	// Per-tenant metering rollups + governor state (multi-tenant mode
	// only). These four families ARE the billing contract: the control
	// plane scrapes them and aggregates into its usage store — kurnd
	// persists nothing.
	if reg := s.tenants.Load(); reg != nil {
		fmt.Fprintf(w, "# HELP kurn_tenant_queries_total Queries served per tenant (all lists).\n# TYPE kurn_tenant_queries_total counter\n")
		for _, t := range reg.ordered {
			var sum uint64
			m.mu.RLock()
			for k, lm := range m.lists {
				if k.tenant == t.name {
					sum += lm.queries.Load()
				}
			}
			m.mu.RUnlock()
			fmt.Fprintf(w, "kurn_tenant_queries_total{tenant=%q} %d\n", t.name, sum)
		}
		fmt.Fprintf(w, "# HELP kurn_tenant_mutations_total Acknowledged mutations per tenant.\n# TYPE kurn_tenant_mutations_total counter\n")
		for _, t := range reg.ordered {
			fmt.Fprintf(w, "kurn_tenant_mutations_total{tenant=%q} %d\n", t.name, m.tenantMutations(t.name).Load())
		}
		fmt.Fprintf(w, "# HELP kurn_tenant_keys Live raw keys stored per tenant (max_total_keys usage).\n# TYPE kurn_tenant_keys gauge\n")
		for _, t := range reg.ordered {
			fmt.Fprintf(w, "kurn_tenant_keys{tenant=%q} %d\n", t.name, tenantKeyUsage(t.st))
		}
		fmt.Fprintf(w, "# HELP kurn_tenant_lists Lists per tenant (max_lists usage).\n# TYPE kurn_tenant_lists gauge\n")
		for _, t := range reg.ordered {
			fmt.Fprintf(w, "kurn_tenant_lists{tenant=%q} %d\n", t.name, len(t.st.Lists()))
		}
		fmt.Fprintf(w, "# HELP kurn_tenant_query_queue_depth Queries waiting for the tenant's scratch slice.\n# TYPE kurn_tenant_query_queue_depth gauge\n")
		for _, t := range reg.ordered {
			q, _ := t.admit.depth()
			fmt.Fprintf(w, "kurn_tenant_query_queue_depth{tenant=%q} %d\n", t.name, q)
		}
		fmt.Fprintf(w, "# HELP kurn_tenant_inflight_bytes Scratch bytes the tenant has in flight.\n# TYPE kurn_tenant_inflight_bytes gauge\n")
		for _, t := range reg.ordered {
			_, in := t.admit.depth()
			fmt.Fprintf(w, "kurn_tenant_inflight_bytes{tenant=%q} %d\n", t.name, in)
		}
		m.mu.RLock()
		tnames := make([]string, 0, len(m.t429s))
		for n := range m.t429s {
			tnames = append(tnames, n)
		}
		m.mu.RUnlock()
		sort.Strings(tnames)
		fmt.Fprintf(w, "# HELP kurn_tenant_429s_total Rate-limited queries per tenant.\n# TYPE kurn_tenant_429s_total counter\n")
		for _, n := range tnames {
			fmt.Fprintf(w, "kurn_tenant_429s_total{tenant=%q} %d\n", n, m.tenant429(n).Load())
		}
	}
}

// trimFloat renders a bucket bound the way Prometheus expects (no
// exponent, no trailing zeros): 0.0001, 0.25, 1, 10.
func trimFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}
