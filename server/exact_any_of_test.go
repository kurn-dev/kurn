package server_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type anyOfResp struct {
	Candidates []struct {
		EntryID string  `json:"entry_id"`
		Score   float64 `json:"score"`
		Key     string  `json:"key"`
	} `json:"candidates"`
	Filter      json.RawMessage            `json:"filter"`
	FilterStats map[string]json.RawMessage `json:"filter_stats"`
}

func decodeAnyOf(t *testing.T, body []byte) anyOfResp {
	t.Helper()
	var r anyOfResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r
}

func TestExactAnyOfViaAPI(t *testing.T) {
	ts := newTS(t)
	mustDo(t, "PUT", ts.URL+"/v1/lists/parts",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"ngram","grams":[2,3],"exact_companion":true},"filterable":[{"name":"tenant","path":"tenant"}]}`)
	mustDo(t, "POST", ts.URL+"/v1/lists/parts/entries",
		`[{"id":"p1","keys":["ACME-042"],"payload":{"tenant":"a"}},{"id":"p2","keys":["ACME-024","acme-042"],"payload":{"tenant":"b"}},{"id":"p3","keys":["ZZ-9"],"payload":{"tenant":"a"}},{"id":"p4","keys":["  acme-402 "],"payload":{"tenant":"a"}}]`)

	resp, body := do(t, "POST", ts.URL+"/v1/query", `{"exact_any_of":["ACME-042","acme-402"],"lists":["parts"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("any-of: %d %s", resp.StatusCode, body)
	}
	r := decodeAnyOf(t, body)
	if len(r.Candidates) != 3 || r.Candidates[0].EntryID != "p1" || r.Candidates[1].EntryID != "p2" || r.Candidates[2].EntryID != "p4" ||
		r.Candidates[0].Score != 100 || r.Candidates[1].Key != "acme-042" || r.Candidates[2].Key != "  acme-402 " || r.Filter != nil || r.FilterStats != nil {
		t.Fatalf("any-of: %s", body)
	}
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"exact_any_of":["ACME-042","acme-402"],"lists":["parts"],"topk":2}`)
	if r = decodeAnyOf(t, body); resp.StatusCode != 200 || len(r.Candidates) != 2 {
		t.Fatalf("topk 2: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"exact_any_of":["ACME-042","acme-402"],"lists":["parts"],"filter":{"tenant":"a"}}`)
	if r = decodeAnyOf(t, body); resp.StatusCode != 200 || len(r.Candidates) != 2 || r.Candidates[0].EntryID != "p1" || r.Candidates[1].EntryID != "p4" ||
		string(r.Filter) != `{"tenant":"a"}` || string(r.FilterStats["parts"]) != `{"evaluated":3,"rejected":1}` {
		t.Fatalf("filtered: %d %s", resp.StatusCode, body)
	}

	// Batch: an any-of check beside an ordinary check on the same list.
	resp, body = do(t, "POST", ts.URL+"/v1/batch-query", `{"checks":[{"exact_any_of":["zz-9","nope"],"lists":["parts"]},{"q":"acme 042","lists":["parts"]}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("batch: %d %s", resp.StatusCode, body)
	}
	var batch struct {
		Results []anyOfResp `json:"results"`
	}
	if err := json.Unmarshal(body, &batch); err != nil || len(batch.Results) != 2 {
		t.Fatalf("batch: %v %s", err, body)
	}
	if len(batch.Results[0].Candidates) != 1 || batch.Results[0].Candidates[0].EntryID != "p3" || len(batch.Results[1].Candidates) == 0 {
		t.Fatalf("batch results: %s", body)
	}

	// Request errors.
	for _, tc := range []struct{ name, req, want string }{
		{"q and any-of", `{"q":"x","exact_any_of":["x"],"lists":["parts"]}`, "mutually exclusive"},
		{"threshold", `{"exact_any_of":["x"],"lists":["parts"],"threshold":0.5}`, "threshold does not apply"},
		{"empty term", `{"exact_any_of":["x",""],"lists":["parts"]}`, "exact_any_of[1] must be non-empty"},
		{"too many", `{"exact_any_of":[` + strings.Repeat(`"x",`, 64) + `"x"],"lists":["parts"]}`, "exceeds 64 terms"},
		{"long term", fmt.Sprintf(`{"exact_any_of":[%q],"lists":["parts"]}`, strings.Repeat("x", 513)), "exceeds"},
		{"no q at all", `{"lists":["parts"]}`, "q must be non-empty"},
	} {
		resp, body := do(t, "POST", ts.URL+"/v1/query", tc.req)
		if resp.StatusCode != 400 || !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: %d %s, want 400 containing %q", tc.name, resp.StatusCode, body, tc.want)
		}
	}

	// A list without an exact index refuses; an exact-mode list answers.
	mustCreate(t, ts, "people")
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"exact_any_of":["x"],"lists":["parts","people"]}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), `has no exact index`) || !strings.Contains(string(body), `people`) {
		t.Fatalf("plain ngram list: %d %s", resp.StatusCode, body)
	}
	mustDo(t, "PUT", ts.URL+"/v1/lists/codes", `{"analyzer":{"preset":"identifier"},"match":{"mode":"exact"}}`)
	mustDo(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"c1","keys":["ACME-042"]}]`)
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"exact_any_of":["acme-042"],"lists":["codes","parts"]}`)
	if r = decodeAnyOf(t, body); resp.StatusCode != 200 || len(r.Candidates) != 3 {
		t.Fatalf("exact-mode and companion lists together: %d %s", resp.StatusCode, body)
	}

	// exact_companion is an ngram-mode setting.
	resp, body = do(t, "PUT", ts.URL+"/v1/lists/bad", `{"analyzer":{"preset":"identifier"},"match":{"mode":"exact","exact_companion":true}}`)
	if resp.StatusCode != 400 {
		t.Fatalf("exact mode with exact_companion: %d %s", resp.StatusCode, body)
	}
}
