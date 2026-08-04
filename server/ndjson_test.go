package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNDJSONUpsertAndReplace(t *testing.T) {
	ts := newTS(t)
	do(t, "PUT", ts.URL+"/v1/lists/people",
		`{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram"}}`)

	var b strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&b, `{"id":"p%d","keys":["Person Number%d"]}`+"\n", i, i)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/v1/lists/people/entries", strings.NewReader(b.String()))
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ndjson upsert: %d", resp.StatusCode)
	}
	if r, body := do(t, "GET", ts.URL+"/v1/lists/people", ""); r.StatusCode != 200 || !strings.Contains(string(body), `"entries":1000`) {
		t.Fatalf("stats: %s", body)
	}

	// replace=true swaps the whole list
	req, _ = http.NewRequest("POST", ts.URL+"/v1/lists/people/entries?replace=true",
		strings.NewReader(`{"id":"only","keys":["Solo Entry"]}`+"\n"))
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("replace: %d", resp.StatusCode)
	}
	if r, body := do(t, "GET", ts.URL+"/v1/lists/people", ""); r.StatusCode != 200 || !strings.Contains(string(body), `"entries":1`) {
		t.Fatalf("post-replace stats: %s", body)
	}

	// malformed line -> 400 with line number
	req, _ = http.NewRequest("POST", ts.URL+"/v1/lists/people/entries", strings.NewReader("{bad json\n"))
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("malformed: %d, want 400", resp.StatusCode)
	}
}

// doNDJSON posts body as application/x-ndjson and returns status + response
// body.
func doNDJSON(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if _, err := io.Copy(&b, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, b.String()
}

// entryCount fetches the list's stats and returns its live-entry count.
func entryCount(t *testing.T, ts *httptest.Server, list string) int {
	t.Helper()
	resp, body := do(t, "GET", ts.URL+"/v1/lists/"+list, "")
	if resp.StatusCode != 200 {
		t.Fatalf("stats %s: %d %s", list, resp.StatusCode, body)
	}
	var st struct {
		Entries int `json:"entries"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	return st.Entries
}

// Append mode is streaming with partial success: a failure mid-stream keeps
// batches already applied (they were journaled and acknowledged) and the
// error response reports both the failing line and the applied count.
func TestNDJSONAppendPartialFailure(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")

	// 10001 good lines fill and flush one full batch (10000); line 10002 is
	// over the 1 MiB line bound, so the stream fails with exactly one batch
	// applied and the 10001st entry (pending, unflushed) discarded.
	var b strings.Builder
	for i := 0; i < 10001; i++ {
		fmt.Fprintf(&b, `{"id":"p%d","keys":["Person Number%d"]}`+"\n", i, i)
	}
	b.WriteString(`{"id":"big","keys":["` + strings.Repeat("a", 1<<20) + `"]}` + "\n")
	status, body := doNDJSON(t, ts.URL+"/v1/lists/people/entries", b.String())
	if status != 400 {
		t.Fatalf("oversize mid-stream: %d %.200s, want 400", status, body)
	}
	if !strings.Contains(body, `"applied":10000`) {
		t.Errorf("response must report the applied count: %.200s", body)
	}
	if !strings.Contains(body, "line 10002") {
		t.Errorf("response must name the failing line: %.200s", body)
	}
	if n := entryCount(t, ts, "people"); n != 10000 {
		t.Errorf("entries = %d, want the 10000 applied before the failure", n)
	}

	// Malformed line mid-stream, all in the first (never-flushed) batch:
	// nothing applied, applied count says so.
	mustCreate(t, ts, "people2")
	status, body = doNDJSON(t, ts.URL+"/v1/lists/people2/entries",
		`{"id":"a","keys":["Ann"]}`+"\n"+`{nope`+"\n")
	if status != 400 || !strings.Contains(body, "line 2") || !strings.Contains(body, `"applied":0`) {
		t.Fatalf("malformed line 2: %d %s, want 400 naming line 2 with applied:0", status, body)
	}
	if n := entryCount(t, ts, "people2"); n != 0 {
		t.Errorf("entries = %d, want 0 (pending batch discarded)", n)
	}
}

// Replace mode is all-or-nothing: any bad line means the list is unchanged.
func TestNDJSONReplaceMalformedLeavesListUnchanged(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people", `[{"id":"p1","keys":["Ann"]},{"id":"p2","keys":["Bob"]}]`)

	status, body := doNDJSON(t, ts.URL+"/v1/lists/people/entries?replace=true",
		`{"id":"n1","keys":["New One"]}`+"\n"+`{bad`+"\n")
	if status != 400 || !strings.Contains(body, "line 2") {
		t.Fatalf("replace with bad line: %d %s, want 400 naming line 2", status, body)
	}
	if n := entryCount(t, ts, "people"); n != 2 {
		t.Errorf("entries = %d, want the original 2 (replace applied nothing)", n)
	}
	// The staged entry from line 1 must not be visible either.
	resp, qbody := do(t, "POST", ts.URL+"/v1/query", `{"q":"New One","lists":["people"]}`)
	if resp.StatusCode != 200 || strings.Contains(string(qbody), `"entry_id":"n1"`) {
		t.Errorf("staged replace entry leaked: %d %s", resp.StatusCode, qbody)
	}
}

// Empty NDJSON bodies: append is a no-op ({"upserted":0}); replace with an
// empty body is an explicit, documented "clear the list" ({"replaced":0}).
func TestNDJSONEmptyBody(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people", `[{"id":"p1","keys":["Ann"]}]`)

	status, body := doNDJSON(t, ts.URL+"/v1/lists/people/entries", "")
	if status != 200 || !strings.Contains(body, `"upserted":0`) {
		t.Fatalf("empty append: %d %s, want 200 upserted:0", status, body)
	}
	if n := entryCount(t, ts, "people"); n != 1 {
		t.Errorf("entries = %d after empty append, want 1", n)
	}

	status, body = doNDJSON(t, ts.URL+"/v1/lists/people/entries?replace=true", "")
	if status != 200 || !strings.Contains(body, `"replaced":0`) {
		t.Fatalf("empty replace: %d %s, want 200 replaced:0", status, body)
	}
	if n := entryCount(t, ts, "people"); n != 0 {
		t.Errorf("entries = %d after empty replace, want 0 (cleared)", n)
	}
}

// Duplicate IDs across lines: last wins, consistent with the array path.
// Blank lines are tolerated and still counted for line numbers.
func TestNDJSONDuplicateIDsAndBlankLines(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")

	status, body := doNDJSON(t, ts.URL+"/v1/lists/people/entries",
		`{"id":"p1","keys":["First Version"]}`+"\n\n"+`{"id":"p1","keys":["Second Version"]}`+"\n")
	if status != 200 {
		t.Fatalf("upsert: %d %s", status, body)
	}
	if n := entryCount(t, ts, "people"); n != 1 {
		t.Fatalf("entries = %d, want 1 (duplicate id collapsed)", n)
	}
	resp, qbody := do(t, "POST", ts.URL+"/v1/query", `{"q":"Second Version","lists":["people"]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(qbody), `"entry_id":"p1"`) {
		t.Errorf("last-wins key not queryable: %d %s", resp.StatusCode, qbody)
	}

	// Empty id -> 400 naming the (blank-line-aware) line number.
	status, body = doNDJSON(t, ts.URL+"/v1/lists/people/entries",
		`{"id":"ok","keys":["a"]}`+"\n"+`{"id":"","keys":["b"]}`+"\n")
	if status != 400 || !strings.Contains(body, "line 2") {
		t.Errorf("empty id: %d %s, want 400 naming line 2", status, body)
	}
}

// A single line over 1 MiB -> 400 naming line 1; unknown list -> 404 (checked
// before the body is read).
func TestNDJSONOversizeLineAndUnknownList(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")

	status, body := doNDJSON(t, ts.URL+"/v1/lists/people/entries",
		`{"id":"big","keys":["`+strings.Repeat("a", 1<<20)+`"]}`+"\n")
	if status != 400 || !strings.Contains(body, "line 1") {
		t.Fatalf("oversize line: %d %.200s, want 400 naming line 1", status, body)
	}
	if n := entryCount(t, ts, "people"); n != 0 {
		t.Errorf("entries = %d, want 0", n)
	}

	status, _ = doNDJSON(t, ts.URL+"/v1/lists/nope/entries", `{"id":"x","keys":["a"]}`+"\n")
	if status != 404 {
		t.Errorf("unknown list: %d, want 404", status)
	}
}

// ?replace=true is honored on the JSON-array path too — it must never be
// silently ignored just because the body wasn't NDJSON.
func TestArrayReplace(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people", `[{"id":"p1","keys":["Ann"]},{"id":"p2","keys":["Bob"]}]`)

	resp, body := do(t, "POST", ts.URL+"/v1/lists/people/entries?replace=true",
		`[{"id":"only","keys":["Solo Entry"]}]`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"replaced":1`) {
		t.Fatalf("array replace: %d %s, want 200 replaced:1", resp.StatusCode, body)
	}
	if n := entryCount(t, ts, "people"); n != 1 {
		t.Errorf("entries = %d, want 1 after replace", n)
	}
}

// The replace parameter is strict: only absent, "true", or "false" are
// accepted. Anything else ("TRUE", "1", a typo) must be 400 naming the value —
// silently appending when the client intended a full swap would double the
// list.
func TestReplaceParamStrict(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people", `[{"id":"p1","keys":["Ann"]}]`)

	// Array path, uppercase value.
	resp, body := do(t, "POST", ts.URL+"/v1/lists/people/entries?replace=TRUE",
		`[{"id":"p2","keys":["Bob"]}]`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), `\"TRUE\"`) {
		t.Fatalf("replace=TRUE: %d %s, want 400 naming the value", resp.StatusCode, body)
	}

	// NDJSON path, numeric value.
	status, nbody := doNDJSON(t, ts.URL+"/v1/lists/people/entries?replace=1",
		`{"id":"p2","keys":["Bob"]}`+"\n")
	if status != 400 || !strings.Contains(nbody, `\"1\"`) {
		t.Fatalf("replace=1: %d %s, want 400 naming the value", status, nbody)
	}

	// Neither rejected request touched the list.
	if n := entryCount(t, ts, "people"); n != 1 {
		t.Errorf("entries = %d after rejected replace values, want 1", n)
	}

	// Param absent: plain append still works on both paths.
	mustUpsert(t, ts, "people", `[{"id":"p2","keys":["Bob"]}]`)
	if status, nbody := doNDJSON(t, ts.URL+"/v1/lists/people/entries",
		`{"id":"p3","keys":["Cid"]}`+"\n"); status != 200 || !strings.Contains(nbody, `"upserted":1`) {
		t.Fatalf("append with param absent: %d %s, want 200 upserted:1", status, nbody)
	}
	if n := entryCount(t, ts, "people"); n != 3 {
		t.Errorf("entries = %d after appends, want 3", n)
	}

	// replace=false is explicitly accepted and means append.
	if status, nbody := doNDJSON(t, ts.URL+"/v1/lists/people/entries?replace=false",
		`{"id":"p4","keys":["Dee"]}`+"\n"); status != 200 || !strings.Contains(nbody, `"upserted":1`) {
		t.Errorf("replace=false: %d %s, want 200 upserted:1 (append)", status, nbody)
	}
}

// Envelope-boundary line: under the 1 MiB scanner bound on the wire, but the
// journal record envelope ({"op":"upsert","entry":...} + newline, +25 bytes)
// pushes it over the engine's line limit. The scanner accepts it, so the
// rejection comes from the store at flush: a typed 400 naming the entry ID
// (no line number — the engine doesn't know lines), the whole pending batch
// discarded (per-batch appendJournal validates before writing anything), and
// an accurate applied count.
func TestNDJSONEnvelopeBoundaryLine(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")

	// Line length = 21 + K + 3 = 1048570 <= 1048576 (scanner passes); journal
	// record = line + 25 = 1048595 > 1048576 (engine rejects).
	boundary := `{"id":"big","keys":["` + strings.Repeat("a", 1048546) + `"]}`
	status, body := doNDJSON(t, ts.URL+"/v1/lists/people/entries",
		`{"id":"ok","keys":["Ann"]}`+"\n"+boundary+"\n")
	if status != 400 {
		t.Fatalf("envelope-boundary line: %d %.200s, want 400", status, body)
	}
	if !strings.Contains(body, `\"big\"`) || !strings.Contains(body, "line limit") {
		t.Errorf("error must be the store's message naming the entry: %.300s", body)
	}
	if !strings.Contains(body, `"applied":0`) {
		t.Errorf("applied count must be 0 (batch never applied): %.300s", body)
	}
	// The pending batch is discarded whole: "ok" from line 1 is gone too.
	if n := entryCount(t, ts, "people"); n != 0 {
		t.Errorf("entries = %d, want 0 (pending batch discarded)", n)
	}
}
