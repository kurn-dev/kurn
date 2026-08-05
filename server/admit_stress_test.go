package server_test

// Integration stress for the query admitter: concurrent queries against a
// budget that admits only one at a time all still succeed (serialized
// backpressure, not errors), and healthz surfaces the queue metric.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func TestQueryAdmissionSerializesUnderBudget(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	}); err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, 5000)
	for i := range entries {
		entries[i] = engine.Entry{ID: fmt.Sprintf("p%05d", i), Keys: []string{fmt.Sprintf("veko rima %04d", i)}}
	}
	if err := st.Replace("people", entries); err != nil {
		t.Fatal(err)
	}
	l, _ := st.List("people")
	// Budget = one query's cost — as the server charges it: these requests
	// omit topk, so the effective per-list collection bound is the global
	// default cut of 100. Concurrency 1, everything else queues.
	ts := httptest.NewServer(server.NewWith(st, server.Config{
		QueryMemBudget:    l.ScratchBytesFor(100),
		QueryQueueTimeout: 10 * time.Second,
	}))
	t.Cleanup(ts.Close)

	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, body := do(t, "POST", ts.URL+"/v1/query",
				fmt.Sprintf(`{"q":"veko rima %04d","lists":["people"]}`, i))
			if resp.StatusCode != 200 {
				errs <- fmt.Sprintf("query %d: %d %s", i, resp.StatusCode, body)
				return
			}
			if !strings.Contains(string(body), fmt.Sprintf(`"p%05d"`, i)) {
				errs <- fmt.Sprintf("query %d: wrong result %s", i, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// Metric surfaced (idle now: depth 0, bytes 0).
	_, body := do(t, "GET", ts.URL+"/healthz", "")
	var h struct {
		Status     string `json:"status"`
		QueueDepth *int64 `json:"query_queue_depth"`
		Inflight   *int64 `json:"query_inflight_bytes"`
	}
	if err := json.Unmarshal(body, &h); err != nil || h.Status != "ok" {
		t.Fatalf("healthz: %v %s", err, body)
	}
	if h.QueueDepth == nil || h.Inflight == nil {
		t.Fatalf("healthz missing admission metrics: %s", body)
	}
	if *h.QueueDepth != 0 || *h.Inflight != 0 {
		t.Fatalf("idle metrics = %d, %d, want 0, 0", *h.QueueDepth, *h.Inflight)
	}
}
