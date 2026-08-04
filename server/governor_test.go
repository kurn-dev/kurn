package server_test

// Per-tenant rate limits (429 + Retry-After, not 503) and the
// two-level memory governor (a tenant saturating its own scratch slice
// cannot starve another tenant).

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func TestRateLimit429(t *testing.T) {
	// rate 1/s, burst 2: two immediate queries pass, the third is 429 with
	// Retry-After — and it is a 429, never a 503 (throttling is not
	// admission failure).
	ts := quotaTS(t, server.TenantQuotas{RatePerSec: 1})
	if resp, _ := doAuth(t, ts, "PUT", "/v1/lists/codes", exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("create failed")
	}
	q := `{"q":"x","lists":["codes"]}`
	for i := 0; i < 2; i++ {
		if resp, body := doAuth(t, ts, "POST", "/v1/query", q, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
			t.Fatalf("burst query %d: %d %s", i, resp.StatusCode, body)
		}
	}
	resp, body := doAuth(t, ts, "POST", "/v1/query", q, "X-API-Key", "key-acme")
	if resp.StatusCode != 429 {
		t.Fatalf("over-rate query: %d %s, want 429", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if !strings.Contains(string(body), "rate limit") {
		t.Errorf("429 body does not name the limit: %s", body)
	}

	// The 429 shows up in the tenant's metric.
	_, mbody := doAuth(t, ts, "GET", "/metrics", "", "", "")
	if !strings.Contains(string(mbody), `kurn_tenant_429s_total{tenant="acme"} 1`) {
		t.Fatalf("429 counter missing:\n%s", mbody)
	}

	// Mutations are not rate-limited (only queries spend tokens).
	if resp, _ := doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"a","keys":["k"]}]`, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("mutation hit the query rate limit")
	}
}

func TestRateLimitBatchInline(t *testing.T) {
	// A batch spends one token per check: burst 2 admits the first two
	// checks, the rest fail INLINE with the batch still 200.
	ts := quotaTS(t, server.TenantQuotas{RatePerSec: 1})
	if resp, _ := doAuth(t, ts, "PUT", "/v1/lists/codes", exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("create failed")
	}
	var checks []string
	for i := 0; i < 4; i++ {
		checks = append(checks, `{"q":"x","lists":["codes"]}`)
	}
	body := `{"checks":[` + strings.Join(checks, ",") + `]}`
	resp, rbody := doAuth(t, ts, "POST", "/v1/batch-query", body, "X-API-Key", "key-acme")
	if resp.StatusCode != 200 {
		t.Fatalf("batch: %d %s", resp.StatusCode, rbody)
	}
	if got := strings.Count(string(rbody), "rate limit"); got != 2 {
		t.Fatalf("inline rate errors = %d, want 2 (burst admits 2 of 4): %s", got, rbody)
	}
}

// Two tenants, each with a one-query-sized scratch slice and a big ngram
// list. A floods no-floor scans until its slice saturates (proven by a
// tenant-budget 503); B's queries keep succeeding throughout — the
// governor makes A's storm A's problem.
func TestTenantScratchIsolation(t *testing.T) {
	dir := t.TempDir()
	// Queue timeout 1ms: far below one no-floor scan's runtime even after the
	// 2026-08 query-path optimizations (which made the original 30ms window
	// admit the whole storm and flake this test) — the first storm query is
	// admitted, the rest deterministically time out against the TENANT slice.
	web := server.NewServer(nil, server.Config{QueryMemBudget: 1 << 30, QueryQueueTimeout: time.Millisecond})
	rts := map[string]server.TenantRuntime{}
	stores := map[string]*engine.Store{}
	for _, name := range []string{"aaa", "bbb"} {
		st, err := engine.Open(filepath.Join(dir, "tenants", name))
		if err != nil {
			t.Fatal(err)
		}
		stores[name] = st
		if _, err := st.CreateList("people", engine.ListConfig{
			Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
			Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
		}); err != nil {
			t.Fatal(err)
		}
		entries := make([]engine.Entry, 120000)
		for i := range entries {
			entries[i] = engine.Entry{ID: fmt.Sprintf("p%06d", i),
				Keys: []string{fmt.Sprintf("veko rima nasol %04d", i%9973)}}
		}
		if err := st.Replace("people", entries); err != nil {
			t.Fatal(err)
		}
	}
	// Each tenant's slice fits exactly one query on its own list.
	l, _ := stores["aaa"].List("people")
	sliceMB := (l.ScratchBytes() + (1 << 20) - 1) >> 20
	for name, st := range stores {
		rts[name] = server.TenantRuntime{
			Spec: server.TenantSpec{
				KeyDigests: []string{digestOf("key-" + name)},
				Quotas:     server.TenantQuotas{ScratchBudgetMB: sliceMB},
			},
			Store: st,
		}
	}
	if err := web.SetTenants(rts); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(web)
	t.Cleanup(ts.Close)

	// A: 8 concurrent no-floor scans (the maximal-work shape) against a
	// one-scan slice and a 30ms queue timeout — overflow 503s must name
	// the TENANT budget.
	noFloor := `{"q":"veko rima nasol 1234","lists":["people"],"threshold":0}`
	var wg sync.WaitGroup
	var mu sync.Mutex
	saw503, saw200 := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, body := doAuth(t, ts, "POST", "/v1/query", noFloor, "X-API-Key", "key-aaa")
			mu.Lock()
			defer mu.Unlock()
			switch resp.StatusCode {
			case 200:
				saw200++
			case 503:
				saw503++
				if !strings.Contains(string(body), "tenant scratch budget") {
					t.Errorf("503 does not name the tenant budget: %s", body)
				}
			default:
				t.Errorf("unexpected status %d: %s", resp.StatusCode, body)
			}
		}()
	}
	// B concurrently: every query must succeed — A's storm queues against
	// A's slice, and the global budget (1 GiB) has room for B.
	bDone := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			resp, body := doAuth(t, ts, "POST", "/v1/query",
				`{"q":"veko rima nasol 0007","lists":["people"]}`, "X-API-Key", "key-bbb")
			if resp.StatusCode != 200 {
				bDone <- fmt.Errorf("tenant B query failed during A's storm: %d %s", resp.StatusCode, body)
				return
			}
			bDone <- nil
		}()
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		if err := <-bDone; err != nil {
			t.Fatal(err)
		}
	}
	if saw200 == 0 || saw503 == 0 {
		t.Fatalf("storm shape off: %d ok, %d rejected — want both (some serialized in, overflow rejected)", saw200, saw503)
	}
}
