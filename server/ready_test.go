package server_test

// /livez (process up) vs /readyz (lists loaded AND golden
// probes passing). The readiness body names every failure — it is the
// diagnosis, not just a bit.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

type readyResp struct {
	Ready    bool `json:"ready"`
	Failures []struct {
		List   string `json:"list"`
		Q      string `json:"q,omitempty"`
		Reason string `json:"reason"`
	} `json:"failures,omitempty"`
}

func TestLivezAlwaysUp(t *testing.T) {
	ts := newTS(t)
	resp, body := do(t, "GET", ts.URL+"/livez", "")
	if resp.StatusCode != 200 {
		t.Fatalf("/livez: %d (%s)", resp.StatusCode, body)
	}
}

func TestReadyzGoldenLifecycle(t *testing.T) {
	ts := newTS(t)
	resp, body := do(t, "PUT", ts.URL+"/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"},
		  "golden":[{"q":"AA-1","expect_id":"c1"},{"q":"never-listed","absent":true}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d (%s)", resp.StatusCode, body)
	}

	// Golden expects c1, which doesn't exist yet: not ready, and the body
	// says exactly why.
	resp, body = do(t, "GET", ts.URL+"/readyz", "")
	var r readyResp
	if resp.StatusCode != 503 || json.Unmarshal(body, &r) != nil || r.Ready {
		t.Fatalf("empty list with expecting golden: %d %s, want 503 not-ready", resp.StatusCode, body)
	}
	if len(r.Failures) != 1 || r.Failures[0].List != "codes" || r.Failures[0].Q != "AA-1" {
		t.Fatalf("failure detail wrong: %s", body)
	}

	// Load the expected entry: ready. (No sleeps — the readiness cache is
	// keyed by list versions, so the mutation invalidates it immediately.)
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"c1","keys":["AA-1"]}]`)
	if resp.StatusCode != 200 {
		t.Fatalf("upsert: %d", resp.StatusCode)
	}
	resp, body = do(t, "GET", ts.URL+"/readyz", "")
	if resp.StatusCode != 200 || json.Unmarshal(body, &r) != nil || !r.Ready {
		t.Fatalf("loaded list: %d %s, want ready", resp.StatusCode, body)
	}

	// Delete it again: readiness flips back with the same diagnosis.
	resp, _ = do(t, "DELETE", ts.URL+"/v1/lists/codes/entries/c1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, body = do(t, "GET", ts.URL+"/readyz", "")
	if resp.StatusCode != 503 {
		t.Fatalf("after delete: %d %s, want 503", resp.StatusCode, body)
	}

	// Violate the absent probe instead: an entry that makes it hit.
	do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"c1","keys":["AA-1"]},{"id":"x","keys":["never-listed"]}]`)
	resp, body = do(t, "GET", ts.URL+"/readyz", "")
	if resp.StatusCode != 503 || json.Unmarshal(body, &r) != nil {
		t.Fatalf("absent probe violated: %d %s, want 503", resp.StatusCode, body)
	}
	if len(r.Failures) != 1 || r.Failures[0].Q != "never-listed" || !strings.Contains(r.Failures[0].Reason, "candidate") {
		t.Fatalf("absent failure detail wrong: %s", body)
	}
}

func TestReadyzMinScore(t *testing.T) {
	ts := newTS(t)
	// Parent-domain matches score 90: a min_score 95 golden must fail, 90 pass.
	resp, body := do(t, "PUT", ts.URL+"/v1/lists/domains",
		`{"analyzer":{"preset":"domain"},"match":{"mode":"exact","fallback":"parent_domain"},
		  "golden":[{"q":"smtp.tempmail.com","expect_id":"d1","min_score":95}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d (%s)", resp.StatusCode, body)
	}
	do(t, "POST", ts.URL+"/v1/lists/domains/entries", `[{"id":"d1","keys":["tempmail.com"]}]`)
	resp, body = do(t, "GET", ts.URL+"/readyz", "")
	if resp.StatusCode != 503 || !strings.Contains(string(body), "score") {
		t.Fatalf("min_score 95 vs parent match 90: %d %s, want 503 naming the score", resp.StatusCode, body)
	}
	// Recreate with min_score 90: passes.
	do(t, "PUT", ts.URL+"/v1/lists/domains",
		`{"analyzer":{"preset":"domain"},"match":{"mode":"exact","fallback":"parent_domain"},
		  "golden":[{"q":"smtp.tempmail.com","expect_id":"d1","min_score":90}]}`)
	do(t, "POST", ts.URL+"/v1/lists/domains/entries", `[{"id":"d1","keys":["tempmail.com"]}]`)
	resp, body = do(t, "GET", ts.URL+"/readyz", "")
	if resp.StatusCode != 200 {
		t.Fatalf("min_score 90: %d %s, want ready", resp.StatusCode, body)
	}
}

func TestReadyzDegradedOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	t.Cleanup(ts.Close)

	resp, body := do(t, "GET", ts.URL+"/readyz", "")
	var r readyResp
	if resp.StatusCode != 503 || json.Unmarshal(body, &r) != nil || r.Ready {
		t.Fatalf("skipped dir: %d %s, want 503", resp.StatusCode, body)
	}
	if len(r.Failures) != 1 || r.Failures[0].List != "stray" || !strings.Contains(r.Failures[0].Reason, "skipped") {
		t.Fatalf("degraded detail wrong: %s", body)
	}
	// Liveness is unaffected by degraded lists.
	resp, _ = do(t, "GET", ts.URL+"/livez", "")
	if resp.StatusCode != 200 {
		t.Fatalf("/livez during degraded state: %d, want 200", resp.StatusCode)
	}
}

func TestGoldenValidationAtCreate(t *testing.T) {
	ts := newTS(t)
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/bad",
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"exact"},
		  "golden":[{"q":"x"}]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid golden accepted: %d, want 400", resp.StatusCode)
	}
}
