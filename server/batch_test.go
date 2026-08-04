package server_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBatchQuery(t *testing.T) {
	ts := newTS(t)
	mustDoLocal := func(method, url, body string) []byte { // reuse do(); assert 200
		resp, b := do(t, method, ts.URL+url, body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, b)
		}
		return b
	}
	mustDoLocal("PUT", "/v1/lists/emails", `{"analyzer":{"preset":"identifier"},"match":{"mode":"exact"}}`)
	mustDoLocal("POST", "/v1/lists/emails/entries", `[{"id":"e1","keys":["bad@example.com"]}]`)

	body := mustDoLocal("POST", "/v1/batch-query", `{"checks":[
		{"q":"bad@example.com","lists":["emails"]},
		{"q":"good@example.com","lists":["emails"]},
		{"q":"x","lists":["nope"]},
		{"q":"","lists":["emails"]}
	]}`)
	var out struct {
		Results []json.RawMessage `json:"results"`
		TookUs  int64             `json:"took_us"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(out.Results))
	}
	if !strings.Contains(string(out.Results[0]), `"entry_id":"e1"`) {
		t.Errorf("check 0: %s", out.Results[0])
	}
	if !strings.Contains(string(out.Results[1]), `"candidates":[]`) {
		t.Errorf("check 1: %s", out.Results[1])
	}
	if !strings.Contains(string(out.Results[2]), `"error"`) || !strings.Contains(string(out.Results[2]), "nope") {
		t.Errorf("check 2: %s", out.Results[2])
	}
	if !strings.Contains(string(out.Results[3]), `"error"`) {
		t.Errorf("check 3: %s", out.Results[3])
	}

	// bounds
	if resp, _ := do(t, "POST", ts.URL+"/v1/batch-query", `{"checks":[]}`); resp.StatusCode != 400 {
		t.Errorf("empty checks: %d", resp.StatusCode)
	}
	var many strings.Builder
	many.WriteString(`{"checks":[`)
	for i := 0; i < 101; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"q":"a","lists":["emails"]}`)
	}
	many.WriteString(`]}`)
	if resp, _ := do(t, "POST", ts.URL+"/v1/batch-query", many.String()); resp.StatusCode != 400 {
		t.Errorf("101 checks: %d", resp.StatusCode)
	}
}
