package server_test

// Optional API-key middleware — /v1/* gated, probes/metrics
// keyless, 401 JSON envelopes, both header forms accepted.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func authedTS(t *testing.T, keys ...string) *httptest.Server {
	t.Helper()
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.NewWith(st, server.Config{APIKeys: keys}))
	t.Cleanup(ts.Close)
	return ts
}

func doAuth(t *testing.T, ts *httptest.Server, method, path, body, header, key string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if header != "" {
		if header == "Authorization" {
			req.Header.Set(header, "Bearer "+key)
		} else {
			req.Header.Set(header, key)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	b := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	return resp, []byte(buf.String())
}

func TestAPIKeyGate(t *testing.T) {
	ts := authedTS(t, "sekret-1", "sekret-2")

	// No key → 401 JSON envelope with WWW-Authenticate.
	resp, body := doAuth(t, ts, "GET", "/v1/lists", "", "", "")
	if resp.StatusCode != 401 {
		t.Fatalf("keyless /v1: %d, want 401", resp.StatusCode)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error == "" {
		t.Fatalf("401 body not an envelope: %s", body)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("WWW-Authenticate missing on 401")
	}

	// Wrong key → 401. Right keys → 200, both header forms, both keys.
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "Authorization", "nope"); resp.StatusCode != 401 {
		t.Fatalf("wrong key: %d, want 401", resp.StatusCode)
	}
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "Authorization", "sekret-1"); resp.StatusCode != 200 {
		t.Fatalf("bearer sekret-1: %d, want 200", resp.StatusCode)
	}
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "X-API-Key", "sekret-2"); resp.StatusCode != 200 {
		t.Fatalf("x-api-key sekret-2: %d, want 200", resp.StatusCode)
	}

	// Unrouted paths under auth answer 401 before 404: no route probing.
	if resp, _ := doAuth(t, ts, "GET", "/v1/anything-else", "", "", ""); resp.StatusCode != 401 {
		t.Fatalf("unrouted keyless: %d, want 401", resp.StatusCode)
	}

	// Probe and metric endpoints stay keyless.
	for _, p := range []string{"/healthz", "/livez", "/readyz", "/metrics"} {
		resp, _ := doAuth(t, ts, "GET", p, "", "", "")
		if resp.StatusCode == 401 {
			t.Errorf("%s gated behind auth", p)
		}
	}

	// End-to-end with a key: create + upsert + query all work.
	if resp, body := doAuth(t, ts, "PUT", "/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`, "X-API-Key", "sekret-1"); resp.StatusCode != 200 {
		t.Fatalf("authed create: %d %s", resp.StatusCode, body)
	}
	if resp, _ := doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"c1","keys":["AA-1"]}]`, "Authorization", "sekret-2"); resp.StatusCode != 200 {
		t.Fatalf("authed upsert: %d", resp.StatusCode)
	}
	resp, body = doAuth(t, ts, "POST", "/v1/query", `{"q":"aa-1","lists":["codes"]}`, "X-API-Key", "sekret-1")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"c1"`) {
		t.Fatalf("authed query: %d %s", resp.StatusCode, body)
	}
}
