package server_test

// The shared free-node shape: shared_reads tenants read the node's OWN
// store — platform-published public lists — under their own keys, rate
// limits, and metering, and every list mutation is refused. Private
// tenants on the same node keep their isolated stores.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func sharedNode(t *testing.T) (*server.Server, *httptest.Server, *engine.Store) {
	t.Helper()
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The platform-published public list.
	if _, err := st.CreateList("leie", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram", Threshold: 0.6, TopK: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Replace("leie", []engine.Entry{
		{ID: "e1", Keys: []string{"elena vasquez"}},
		{ID: "e2", Keys: []string{"iris bell"}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(st, server.Config{})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts, st
}

func doReq(t *testing.T, ts *httptest.Server, method, path, body, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestSharedReadsTenants(t *testing.T) {
	srv, ts, _ := sharedNode(t)

	priv, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.SetTenants(map[string]server.TenantRuntime{
		"free1": {Spec: server.TenantSpec{
			KeyDigests: []string{digestOf("fk-1")}, SharedReads: true,
			Quotas: server.TenantQuotas{RatePerSec: 1},
		}},
		"payer": {Spec: server.TenantSpec{
			KeyDigests: []string{digestOf("pk-1")},
		}, Store: priv},
	}); err != nil {
		t.Fatal(err)
	}

	// The shared tenant reads the node's public list.
	code, body := doReq(t, ts, "POST", "/v1/query", `{"q":"elena vasqez","lists":["leie"]}`, "fk-1")
	if code != 200 || !strings.Contains(body, `"e1"`) {
		t.Fatalf("shared read: %d %s", code, body)
	}

	// Every mutation shape is refused with 403 — not 404, not 401.
	for _, m := range []struct{ method, path, body string }{
		{"PUT", "/v1/lists/mine", `{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram"}}`},
		{"POST", "/v1/lists/leie/entries", `{"entries":[{"id":"x","keys":["toni brook"]}]}`},
		{"DELETE", "/v1/lists/leie/entries/e1", ""},
		{"POST", "/v1/lists/leie/compact", ""},
		{"POST", "/v1/lists/leie/reload", ""},
	} {
		if code, body := doReq(t, ts, m.method, m.path, m.body, "fk-1"); code != 403 || !strings.Contains(body, "read-only") {
			t.Fatalf("%s %s: %d %s, want 403 read-only", m.method, m.path, code, body)
		}
	}
	// Reads under /v1/lists stay open (GET is never a mutation).
	if code, _ := doReq(t, ts, "GET", "/v1/lists/leie", "", "fk-1"); code != 200 {
		t.Fatal("shared tenant list stats refused")
	}

	// Isolation holds in both directions: the private tenant cannot see
	// the shared store's list, and its own writes work.
	if code, _ := doReq(t, ts, "POST", "/v1/query", `{"q":"elena vasquez","lists":["leie"]}`, "pk-1"); code != 404 {
		t.Fatalf("private tenant reached the shared store: %d", code)
	}
	if code, _ := doReq(t, ts, "PUT", "/v1/lists/own", `{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram"}}`, "pk-1"); code != 200 {
		t.Fatal("private tenant mutation refused")
	}

	// Rate limits stay per-tenant on the shared store (rate 1, burst 2:
	// a quick volley throttles).
	throttled := 0
	for i := 0; i < 8; i++ {
		if code, _ := doReq(t, ts, "POST", "/v1/query", `{"q":"iris bell","lists":["leie"]}`, "fk-1"); code == 429 {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatal("shared tenant never throttled: quotas not applied")
	}
}

func TestSharedReadsRegistryValidation(t *testing.T) {
	// A shared_reads tenant declaring its own store is refused.
	srv, _, _ := sharedNode(t)
	own, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = srv.SetTenants(map[string]server.TenantRuntime{
		"free1": {Spec: server.TenantSpec{
			KeyDigests: []string{digestOf("fk-1")}, SharedReads: true,
		}, Store: own},
	})
	if err == nil || !strings.Contains(err.Error(), "must not declare a store") {
		t.Fatalf("shared tenant with own store accepted: %v", err)
	}

	// shared_reads on a node with no shared store is refused (the
	// registry-only NewServer(nil) shape).
	headless := server.NewServer(nil, server.Config{})
	err = headless.SetTenants(map[string]server.TenantRuntime{
		"free1": {Spec: server.TenantSpec{
			KeyDigests: []string{digestOf("fk-1")}, SharedReads: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "requires the node's shared store") {
		t.Fatalf("shared tenant without shared store accepted: %v", err)
	}

	// ParseTenants round-trips the field.
	specs, err := server.ParseTenants([]byte(`{"free1":{"key_digests":["` + digestOf("fk-1") + `"],"shared_reads":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !specs["free1"].SharedReads {
		t.Fatal("shared_reads lost in parse")
	}
}
