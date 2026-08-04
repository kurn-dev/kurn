package server_test

// Regression test: The package promises "all responses
// are JSON", but the mux's default 404/405 responses were plain text.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnroutedResponsesAreJSON(t *testing.T) {
	ts := newTS(t)

	// Unrouted path → 404 with a JSON envelope.
	resp, body := do(t, "GET", ts.URL+"/nope", "")
	if resp.StatusCode != 404 {
		t.Fatalf("unrouted path: status %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("unrouted path: Content-Type %q, want application/json (body %q)", ct, body)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error == "" {
		t.Fatalf("unrouted path: body %q is not an error envelope (%v)", body, err)
	}

	// Wrong method on a routed path → 405, JSON, Allow header preserved.
	resp, body = do(t, "POST", ts.URL+"/healthz", "")
	if resp.StatusCode != 405 {
		t.Fatalf("wrong method: status %d, want 405", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("wrong method: Content-Type %q, want application/json (body %q)", ct, body)
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error == "" {
		t.Fatalf("wrong method: body %q is not an error envelope (%v)", body, err)
	}
	if resp.Header.Get("Allow") == "" {
		t.Fatal("wrong method: Allow header lost in the JSON rewrite")
	}

	// Handler-written JSON errors must pass through untouched (no double
	// interception): unknown list is the handlers' own 404.
	resp, body = do(t, "GET", ts.URL+"/v1/lists/ghost", "")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown list: status %d, want 404", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error == "" {
		t.Fatalf("unknown list: body %q is not an error envelope (%v)", body, err)
	}
}
