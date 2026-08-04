package server_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

// Three lists, three presets, one engine:
//
//	people  (person-name / ngram)  — fuzzy person matching, token-order-proof
//	brands  (free-text / ngram)    — merchant-style fuzzy text
//	domains (identifier / exact)   — exact blocklist membership
func TestGeneralityAcrossPresets(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	defer ts.Close()

	mustDo(t, "PUT", ts.URL+"/v1/lists/people", `{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram"}}`)
	mustDo(t, "PUT", ts.URL+"/v1/lists/brands", `{"analyzer":{"preset":"free-text"},"match":{"mode":"ngram","threshold":0.5}}`)
	mustDo(t, "PUT", ts.URL+"/v1/lists/domains", `{"analyzer":{"preset":"identifier"},"match":{"mode":"exact"}}`)

	mustDo(t, "POST", ts.URL+"/v1/lists/people/entries", `[{"id":"p1","keys":["Elena Vasquez","Vasquez, Elena M."]}]`)
	mustDo(t, "POST", ts.URL+"/v1/lists/brands/entries", `[{"id":"b1","keys":["Café Metro GmbH"]},{"id":"b2","keys":["Acme Corporation"]}]`)
	mustDo(t, "POST", ts.URL+"/v1/lists/domains/entries", `[{"id":"d1","keys":["evil-corp.example.com"]}]`)

	cases := []struct{ q, list, wantID string }{
		{"vasquez elena", "people", "p1"},          // token reorder
		{"Elenavasquez", "people", "p1"},           // fused
		{"cafe metro gmbh", "brands", "b1"},        // diacritics folded
		{"Acme Corp0ration", "brands", "b2"},       // typo
		{"EVIL-CORP.example.com", "domains", "d1"}, // case-folded exact
	}
	for _, c := range cases {
		_, body := do(t, "POST", ts.URL+"/v1/query", fmt.Sprintf(`{"q":%q,"lists":[%q]}`, c.q, c.list))
		if !strings.Contains(string(body), fmt.Sprintf(`"entry_id":%q`, c.wantID)) {
			t.Errorf("query %q on %s: want %s, got %s", c.q, c.list, c.wantID, body)
		}
	}

	// multi-list query merges + tags
	_, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"acme corporation","lists":["people","brands"]}`)
	if !strings.Contains(string(body), `"list":"brands"`) {
		t.Errorf("multi-list: %s", body)
	}

	// restart the whole service on the same dir: everything survives
	ts.Close()
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(server.New(st2))
	defer ts2.Close()
	_, body = do(t, "POST", ts2.URL+"/v1/query", `{"q":"vasquez elena","lists":["people"]}`)
	if !strings.Contains(string(body), `"entry_id":"p1"`) {
		t.Errorf("after restart: %s", body)
	}

	// ---- extensions beyond the pinned generality body ----

	// Strict ?replace= contract through the e2e stack: anything other than
	// "true"/"false" is a 400, and nothing is applied.
	resp, body := do(t, "POST", ts2.URL+"/v1/lists/domains/entries?replace=1",
		`[{"id":"dX","keys":["should-not-land.example.com"]}]`)
	if resp.StatusCode != 400 {
		t.Errorf("replace=1: got %d %s, want 400", resp.StatusCode, body)
	}
	_, body = do(t, "POST", ts2.URL+"/v1/query", `{"q":"should-not-land.example.com","lists":["domains"]}`)
	if strings.Contains(string(body), `"entry_id":"dX"`) {
		t.Errorf("rejected replace=1 upsert was applied: %s", body)
	}

	// NDJSON bulk append into domains, then compact, then restart AGAIN:
	// the reopened list must be served from the compacted artifact (fast
	// path) and answer identically to the pre-compact journal state.
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, `{"id":"nd%d","keys":["host-%d.blocked.example.net"]}`+"\n", i, i)
	}
	if code, resp := doNDJSON(t, ts2.URL+"/v1/lists/domains/entries", b.String()); code != 200 {
		t.Fatalf("ndjson load: %d %s", code, resp)
	}

	domainQueries := []string{
		"evil-corp.example.com",      // original JSON-array entry
		"HOST-0.blocked.example.net", // first NDJSON entry, case-folded
		"host-499.blocked.example.net",
		"host-777.blocked.example.net", // absent: must stay a miss
	}
	before := make([]string, len(domainQueries))
	for i, q := range domainQueries {
		before[i] = candidatesOf(t, ts2, q, "domains")
	}

	if resp, body := do(t, "POST", ts2.URL+"/v1/lists/domains/compact", ""); resp.StatusCode != 200 {
		t.Fatalf("compact: %d %s", resp.StatusCode, body)
	}
	for i, q := range domainQueries {
		if got := candidatesOf(t, ts2, q, "domains"); got != before[i] {
			t.Errorf("post-compact %q: got %s, want %s", q, got, before[i])
		}
	}

	ts2.Close()
	st3, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts3 := httptest.NewServer(server.New(st3))
	defer ts3.Close()
	for i, q := range domainQueries {
		if got := candidatesOf(t, ts3, q, "domains"); got != before[i] {
			t.Errorf("post-compact restart %q: got %s, want %s", q, got, before[i])
		}
	}
	// the other two lists survived the second restart too
	_, body = do(t, "POST", ts3.URL+"/v1/query", `{"q":"cafe metro","lists":["brands","people"]}`)
	if !strings.Contains(string(body), `"entry_id":"b1"`) {
		t.Errorf("brands after second restart: %s", body)
	}
}

// mustDo is do plus a 200-status check, so setup failures localize to the
// call that broke instead of surfacing as downstream query misses.
func mustDo(t *testing.T, method, url, body string) {
	t.Helper()
	resp, respBody := do(t, method, url, body)
	if resp.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, respBody)
	}
}

// candidatesOf runs a single-list query and returns the raw candidates array,
// which is deterministic across compaction and restarts (unlike took_us and
// versions, so we can't compare whole response bodies).
func candidatesOf(t *testing.T, ts *httptest.Server, q, list string) string {
	t.Helper()
	resp, body := do(t, "POST", ts.URL+"/v1/query", fmt.Sprintf(`{"q":%q,"lists":[%q]}`, q, list))
	if resp.StatusCode != 200 {
		t.Fatalf("query %q: %d %s", q, resp.StatusCode, body)
	}
	var qr struct {
		Candidates json.RawMessage `json:"candidates"`
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatalf("query %q: %v: %s", q, err, body)
	}
	return string(qr.Candidates)
}
