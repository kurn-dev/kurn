package server_test

// Regression tests (server side): Threshold/topk are
// range-validated with 400s (pre-fix threshold 7.5 and topk 100000000 were
// accepted silently), threshold 0 is expressible as an explicit no-floor
// (item 9), and a hot-key exact list stays bounded at the effective top-K.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestQueryBounds(t *testing.T) {
	ts := newTS(t)
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/people",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"ngram","grams":[2,3],"strip_spaces":true,"threshold":0.9}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/people/entries",
		`[{"id":"p1","keys":["elena vasquez"]},{"id":"p2","keys":["elena marlow"]}]`)
	if resp.StatusCode != 200 {
		t.Fatalf("seed: %d", resp.StatusCode)
	}

	for _, bad := range []string{
		`{"q":"x","lists":["people"],"threshold":-0.1}`,
		`{"q":"x","lists":["people"],"threshold":7.5}`,
		`{"q":"x","lists":["people"],"topk":0}`,
		`{"q":"x","lists":["people"],"topk":-5}`,
		`{"q":"x","lists":["people"],"topk":100000000}`,
	} {
		resp, body := do(t, "POST", ts.URL+"/v1/query", bad)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d (%s), want 400", bad, resp.StatusCode, body)
		}
	}

	count := func(body []byte) int {
		var r struct {
			Candidates []json.RawMessage `json:"candidates"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("bad response: %v (%s)", err, body)
		}
		return len(r.Candidates)
	}
	// Default threshold (0.9) hides the half-match; explicit threshold 0 is
	// a real no-floor, not "use default".
	_, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"elena vasquez","lists":["people"]}`)
	if got := count(body); got != 1 {
		t.Fatalf("default threshold: %d candidates, want 1", got)
	}
	_, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"elena vasquez","lists":["people"],"threshold":0}`)
	if got := count(body); got != 2 {
		t.Fatalf("threshold 0 (explicit no-floor): %d candidates, want 2", got)
	}
}

func TestQueryHotKeyExactBounded(t *testing.T) {
	ts := newTS(t)
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 300; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"e%03d","keys":["hot"]}`, i)
	}
	b.WriteString("]")
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/codes/entries", b.String())
	if resp.StatusCode != 200 {
		t.Fatalf("seed: %d", resp.StatusCode)
	}

	// No topk: the exact list's own default is unlimited, but the global cut
	// is 100 — the response must carry exactly 100, and the per-list
	// collection is bounded at the same K (asserted behaviorally; the
	// engine-side cap has its own unit test).
	_, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"hot","lists":["codes"]}`)
	var r struct {
		Candidates []json.RawMessage `json:"candidates"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Candidates) != 100 {
		t.Fatalf("hot-key exact: %d candidates (err %v), want 100", len(r.Candidates), err)
	}
	// Explicit topk within bounds applies.
	_, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"hot","lists":["codes"],"topk":7}`)
	if err := json.Unmarshal(body, &r); err != nil || len(r.Candidates) != 7 {
		t.Fatalf("topk 7: %d candidates (err %v), want 7", len(r.Candidates), err)
	}
}
