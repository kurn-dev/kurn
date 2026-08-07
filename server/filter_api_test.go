package server_test

// The HTTP filter surface (iteration-6 step 2): declaration enforcement
// before admission, duplicate-name rejection with batch error locality,
// the exact fail-closed response echo, empty-filter identity, the
// old-node compatibility rule, and the malformed-stored-payload error
// classification.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

// newFilterTS builds a server over a store the test keeps a handle to
// (the malformed-payload case needs engine-level access the HTTP surface
// rightly refuses).
func newFilterTS(t *testing.T) (*httptest.Server, *engine.Store) {
	t.Helper()
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	t.Cleanup(ts.Close)
	return ts, st
}

func putFilterableList(t *testing.T, base, name string) {
	t.Helper()
	do(t, "PUT", base+"/v1/lists/"+name,
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"ngram","grams":[2,3],"threshold":0.5},
		  "filterable":[{"name":"program","path":"program"},{"name":"country","path":"meta.country"}]}`)
	do(t, "POST", base+"/v1/lists/"+name+"/entries", `[
		{"id":"e1","keys":["dana kovak"],"payload":{"program":"SDN","meta":{"country":"EE"}}},
		{"id":"e2","keys":["dana kovak"],"payload":{"program":"EU","meta":{"country":"FR"}}}]`)
}

func TestFilterDeclarationEnforcedAcrossLists(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")
	// A second list WITHOUT declarations: any filtered query naming both
	// lists must 400 before anything executes.
	do(t, "PUT", ts.URL+"/v1/lists/plain", `{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"ngram","grams":[2,3],"threshold":0.5}}`)
	do(t, "POST", ts.URL+"/v1/lists/plain/entries", `[{"id":"p1","keys":["dana kovak"],"payload":{"program":"SDN"}}]`)

	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions","plain"],"filter":{"program":"SDN"}}`)
	if resp.StatusCode != 400 {
		t.Fatalf("undeclared name on the second list: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "plain") || !strings.Contains(string(body), "program") {
		t.Fatalf("400 must name the list and the field: %s", body)
	}
	// The valid single-list version still works.
	resp, body = do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("declared filter: %d %s", resp.StatusCode, body)
	}
}

func TestFilterDuplicateNameLocality(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	// Single query: raw duplicate name -> top-level 400.
	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN","program":"EU"}}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "duplicate filter name") {
		t.Fatalf("duplicate name: %d %s", resp.StatusCode, body)
	}

	// A repeated top-level filter MEMBER: the typed decode keeps the LAST
	// value, so {"filter":{...},"filter":{}} would silently erase the
	// filter and answer unfiltered with no echo — must be a 400, and the
	// unfiltered execution path must never be reached.
	for _, dup := range []string{
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"},"filter":{}}`,
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"},"filter":null}`,
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"},"filter":{"program":"EU"}}`,
	} {
		resp, body := do(t, "POST", ts.URL+"/v1/query", dup)
		if resp.StatusCode != 400 || !strings.Contains(string(body), "duplicate filter member") {
			t.Fatalf("duplicate member %s: %d %s", dup, resp.StatusCode, body)
		}
		if strings.Contains(string(body), `"candidates"`) {
			t.Fatalf("duplicate member reached execution: %s", body)
		}
	}

	// Batch: a duplicate MEMBER stays inline too; the good sibling answers.
	resp, body = do(t, "POST", ts.URL+"/v1/batch-query",
		`{"checks":[
		   {"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"},"filter":{}},
		   {"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}}]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "duplicate filter member") {
		t.Fatalf("batch duplicate member: %d %s", resp.StatusCode, body)
	}
	if c := strings.Count(string(body), `"entry_id"`); c != 1 {
		t.Fatalf("good sibling beside a duplicate-member check should yield exactly one candidate: %s", body)
	}

	// Batch: the duplicate stays INLINE; the good sibling still answers.
	resp, body = do(t, "POST", ts.URL+"/v1/batch-query",
		`{"checks":[
		   {"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN","program":"EU"}},
		   {"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("batch with one bad check must stay 200: %d %s", resp.StatusCode, body)
	}
	var br struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &br); err != nil || len(br.Results) != 2 {
		t.Fatalf("batch shape: %v %s", err, body)
	}
	if !strings.Contains(string(br.Results[0]), "duplicate filter name") {
		t.Fatalf("bad check not inline-errored: %s", br.Results[0])
	}
	var good struct {
		Candidates []struct {
			EntryID string `json:"entry_id"`
		} `json:"candidates"`
		Filter map[string]string `json:"filter"`
	}
	if err := json.Unmarshal(br.Results[1], &good); err != nil {
		t.Fatalf("good check: %v %s", err, br.Results[1])
	}
	if len(good.Candidates) != 1 || good.Candidates[0].EntryID != "e1" {
		t.Fatalf("good sibling wrong answer: %s", br.Results[1])
	}
	if good.Filter["program"] != "SDN" || len(good.Filter) != 1 {
		t.Fatalf("good sibling echo: %v", good.Filter)
	}
}

// requireExactEcho is the documented client rule: a non-empty filter is
// only honored if the response echoes EXACTLY the map that was sent —
// missing, altered, or extra entries all fail closed.
func requireExactEcho(sent map[string]string, resp map[string]string) error {
	if len(resp) != len(sent) {
		return fmt.Errorf("echo has %d entries, sent %d", len(resp), len(sent))
	}
	for k, v := range sent {
		got, ok := resp[k]
		if !ok {
			return fmt.Errorf("echo missing %q", k)
		}
		if got != v {
			return fmt.Errorf("echo altered %q: %q != %q", k, got, v)
		}
	}
	return nil
}

func TestFilterEchoExactAndEmptySuccess(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	sent := map[string]string{"program": "SDN", "country": "EE"}
	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN","country":"EE"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("filtered hit: %d %s", resp.StatusCode, body)
	}
	var qr struct {
		Candidates []struct {
			EntryID string `json:"entry_id"`
		} `json:"candidates"`
		Filter map[string]string `json:"filter"`
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) != 1 || qr.Candidates[0].EntryID != "e1" {
		t.Fatalf("filtered AND result: %s", body)
	}
	if err := requireExactEcho(sent, qr.Filter); err != nil {
		t.Fatalf("hit echo: %v", err)
	}

	// Empty SUCCESS still echoes: zero candidates is a filtered answer,
	// not an unfiltered shape.
	resp, body = do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"NEVER"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("filtered empty: %d %s", resp.StatusCode, body)
	}
	var er struct {
		Candidates []json.RawMessage `json:"candidates"`
		Filter     map[string]string `json:"filter"`
	}
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatal(err)
	}
	if len(er.Candidates) != 0 {
		t.Fatalf("expected empty candidates: %s", body)
	}
	if err := requireExactEcho(map[string]string{"program": "NEVER"}, er.Filter); err != nil {
		t.Fatalf("empty-success echo: %v", err)
	}

	// Old-node compatibility fixture: a node that ignored the filter
	// member answers with the UNFILTERED shape — no echo. The client rule
	// must reject it, and altered/extra echoes with it.
	oldNode := map[string]string(nil)
	if err := requireExactEcho(sent, oldNode); err == nil {
		t.Fatal("missing echo must fail the client rule")
	}
	if err := requireExactEcho(sent, map[string]string{"program": "SDN", "country": "FR"}); err == nil {
		t.Fatal("altered echo must fail the client rule")
	}
	if err := requireExactEcho(sent, map[string]string{"program": "SDN", "country": "EE", "x": "y"}); err == nil {
		t.Fatal("extra echo entry must fail the client rule")
	}
}

func TestFilterEmptyIdentity(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	for _, body := range []string{
		`{"q":"dana kovak","lists":["sanctions"]}`,
		`{"q":"dana kovak","lists":["sanctions"],"filter":{}}`,
	} {
		resp, raw := do(t, "POST", ts.URL+"/v1/query", body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), `"filter"`) {
			t.Fatalf("unfiltered response must omit the filter member (%s): %s", body, raw)
		}
		var qr struct {
			Candidates []json.RawMessage `json:"candidates"`
		}
		if err := json.Unmarshal(raw, &qr); err != nil || len(qr.Candidates) != 2 {
			t.Fatalf("unfiltered answer changed (%s): %s", body, raw)
		}
	}
}

func TestFilterMalformedStoredPayload(t *testing.T) {
	ts, st := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	// A malformed payload cannot enter through HTTP or the Store (both
	// marshal); install it via the in-memory library path on the served
	// list — the query must fail, never read the candidate drop as clear.
	l, ok := st.List("sanctions")
	if !ok {
		t.Fatal("list missing")
	}
	if err := l.Replace([]engine.Entry{
		{ID: "bad", Keys: []string{"dana kovak"}, Payload: json.RawMessage(`{"program":`)},
	}); err != nil {
		t.Fatal(err)
	}

	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}}`)
	if resp.StatusCode != 500 {
		t.Fatalf("malformed stored payload must be a node failure: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), `"candidates"`) {
		t.Fatalf("no partial answer may escape: %s", body)
	}

	// Batch: inline error for that check; the batch itself stays 200.
	resp, body = do(t, "POST", ts.URL+"/v1/batch-query",
		`{"checks":[{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}}]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("batch malformed-payload check: %d %s", resp.StatusCode, body)
	}
}

// M2: the duplicated HTTP bounds must have server-level bite, exercised
// as RUNE contracts (non-ASCII input), and the sorted-first-offender rule
// must hold under repetition.
func TestFilterRequestBounds(t *testing.T) {
	ts, _ := newFilterTS(t)
	// A list declaring exactly 8 names: the maximum is usable, 9 is not.
	decls := make([]string, 0, 8)
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8"} {
		decls = append(decls, fmt.Sprintf(`{"name":%q,"path":%q}`, n, n))
	}
	do(t, "PUT", ts.URL+"/v1/lists/wide", `{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"ngram","grams":[2,3],"threshold":0.5},"filterable":[`+strings.Join(decls, ",")+`]}`)
	do(t, "POST", ts.URL+"/v1/lists/wide/entries", `[{"id":"e1","keys":["dana kovak"],"payload":{"a1":"x"}}]`)

	filt := func(pairs ...string) string {
		return `{"q":"dana kovak","lists":["wide"],"filter":{` + strings.Join(pairs, ",") + `}}`
	}
	// Exactly 8 declared names: accepted (200).
	eight := make([]string, 0, 8)
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8"} {
		eight = append(eight, fmt.Sprintf(`%q:"x"`, n))
	}
	resp, body := do(t, "POST", ts.URL+"/v1/query", filt(eight...))
	if resp.StatusCode != 200 {
		t.Fatalf("8 declared names must work: %d %s", resp.StatusCode, body)
	}
	// 9 names: refused on count before anything else.
	nine := append(append([]string{}, eight...), `"a9":"x"`)
	resp, body = do(t, "POST", ts.URL+"/v1/query", filt(nine...))
	if resp.StatusCode != 400 || !strings.Contains(string(body), "max 8") {
		t.Fatalf("9 names: %d %s", resp.StatusCode, body)
	}

	// Name rune bound, non-ASCII: 129 é-runes (258 bytes) trips the RUNE
	// bound; 128 é-runes passes it and fails DECLARATION instead — the
	// distinct errors pin the boundary exactly.
	name129 := strings.Repeat("é", 129)
	resp, body = do(t, "POST", ts.URL+"/v1/query", filt(fmt.Sprintf(`%q:"x"`, name129)))
	if resp.StatusCode != 400 || !strings.Contains(string(body), "exceeds 128") {
		t.Fatalf("129-rune name: %d %s", resp.StatusCode, body)
	}
	name128 := strings.Repeat("é", 128)
	resp, body = do(t, "POST", ts.URL+"/v1/query", filt(fmt.Sprintf(`%q:"x"`, name128)))
	if resp.StatusCode != 400 || !strings.Contains(string(body), "not declared") {
		t.Fatalf("128-rune name must pass the bound and fail declaration: %d %s", resp.StatusCode, body)
	}

	// Value rune bound, non-ASCII: 513 é-runes trips it; 512 passes and
	// the query executes (empty result — no such value).
	resp, body = do(t, "POST", ts.URL+"/v1/query", filt(fmt.Sprintf(`"a1":%q`, strings.Repeat("é", 513))))
	if resp.StatusCode != 400 || !strings.Contains(string(body), "exceeds 512") {
		t.Fatalf("513-rune value: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, "POST", ts.URL+"/v1/query", filt(fmt.Sprintf(`"a1":%q`, strings.Repeat("é", 512))))
	if resp.StatusCode != 200 {
		t.Fatalf("512-rune value must pass the bound: %d %s", resp.StatusCode, body)
	}

	// Two independently invalid names: the sorted-first offender is named
	// on every repetition (map order must never leak).
	twoBad := filt(fmt.Sprintf(`%q:"x"`, "zz"+name129), fmt.Sprintf(`%q:"x"`, "aa"+name129))
	for i := 0; i < 50; i++ {
		_, body = do(t, "POST", ts.URL+"/v1/query", twoBad)
		if !strings.Contains(string(body), `"aa`) {
			t.Fatalf("run %d: offender not sorted-first: %s", i, body)
		}
	}
}

// M2: validation and filtered preparation must strictly precede admission
// — under a budget too small to admit ANYTHING, an invalid filter still
// gets its 400 (never the 503 a valid query would get), and no list's
// query metrics move.
func TestFilterValidationPrecedesAdmission(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.NewWith(st, server.Config{QueryMemBudget: 1})) // 1 byte: every query over-budget
	t.Cleanup(ts.Close)
	putFilterableList(t, ts.URL, "sanctions")
	do(t, "PUT", ts.URL+"/v1/lists/plain", `{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"ngram","grams":[2,3],"threshold":0.5}}`)
	do(t, "POST", ts.URL+"/v1/lists/plain/entries", `[{"id":"p1","keys":["dana kovak"],"payload":{}}]`)

	// Sanity: a VALID query is refused by admission (503) under this budget.
	resp, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}}`)
	if resp.StatusCode != 503 {
		t.Fatalf("budget sanity: %d %s", resp.StatusCode, body)
	}

	metrics := func() string {
		_, b := do(t, "GET", ts.URL+"/metrics", "")
		return string(b)
	}
	before := metrics()

	// The invalid filter (undeclared on the LATER list) must 400 — proof
	// that declaration checking runs before admission could 503 it.
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"dana kovak","lists":["sanctions","plain"],"filter":{"program":"SDN"}}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "plain") {
		t.Fatalf("undeclared under tiny budget must 400 before admission: %d %s", resp.StatusCode, body)
	}
	if after := metrics(); after != before {
		t.Fatalf("query metrics moved for a request that never executed:\nbefore: %.200s\nafter: %.200s", before, after)
	}
}

// The fast-path gate in dupFilterName must stay byte-conservative: an
// ESCAPED spelling of the member name ("filter" decodes to
// "filter") contains a backslash, so it must still reach the token walk
// and collide with a literal member.
func TestFilterDuplicateMemberEscapedSpelling(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")
	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filte\u0072":{"program":"SDN"},"filte\u0072":{}}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "duplicate filter member") {
		t.Fatalf("escaped member spelling escaped the guard: %d %s", resp.StatusCode, body)
	}
}
