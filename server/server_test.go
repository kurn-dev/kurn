package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func newTS(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, method, url, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	resp.Body.Close()
	return resp, buf.Bytes()
}

func TestListLifecycleAndQuery(t *testing.T) {
	ts := newTS(t)

	// create list
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/people",
		`{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram","grams":[2,3],"strip_spaces":true,"threshold":0.6,"topk":100}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}

	// upsert entries (JSON array)
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/people/entries",
		`[{"id":"p1","keys":["Elena Vasquez"],"payload":{"tier":1}},{"id":"p2","keys":["Marcus Chen"]}]`)
	if resp.StatusCode != 200 {
		t.Fatalf("upsert: %d", resp.StatusCode)
	}

	// query with a typo
	resp, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"Elena Wasquez","lists":["people"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("query: %d %s", resp.StatusCode, body)
	}
	var qr struct {
		Candidates []struct {
			List    string          `json:"list"`
			EntryID string          `json:"entry_id"`
			Score   float64         `json:"score"`
			Key     string          `json:"key"`
			Payload json.RawMessage `json:"payload"`
		} `json:"candidates"`
		Versions map[string]string `json:"versions"`
		TookUs   int64             `json:"took_us"`
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) == 0 || qr.Candidates[0].EntryID != "p1" || qr.Candidates[0].List != "people" {
		t.Fatalf("candidates: %s", body)
	}
	if qr.Versions["people"] == "" {
		t.Error("missing version stamp")
	}

	// no-hit is explicit 200 + empty array
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"zzzz qqqq","lists":["people"]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"candidates":[]`) {
		t.Fatalf("no-hit: %d %s", resp.StatusCode, body)
	}

	// delete + stats
	resp, _ = do(t, "DELETE", ts.URL+"/v1/lists/people/entries/p1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, body = do(t, "GET", ts.URL+"/v1/lists/people", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"entries":1`) {
		t.Fatalf("stats: %d %s", resp.StatusCode, body)
	}
}

func TestQueryValidation(t *testing.T) {
	ts := newTS(t)
	// unknown list
	resp, _ := do(t, "POST", ts.URL+"/v1/query", `{"q":"x","lists":["nope"]}`)
	if resp.StatusCode != 404 {
		t.Errorf("unknown list: %d, want 404", resp.StatusCode)
	}
	// oversized q
	resp, _ = do(t, "POST", ts.URL+"/v1/query", `{"q":"`+strings.Repeat("a", 600)+`","lists":["x"]}`)
	if resp.StatusCode != 400 {
		t.Errorf("oversized q: %d, want 400", resp.StatusCode)
	}
	// empty q
	resp, _ = do(t, "POST", ts.URL+"/v1/query", `{"q":"","lists":["x"]}`)
	if resp.StatusCode != 400 {
		t.Errorf("empty q: %d, want 400", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	ts := newTS(t)
	resp, _ := do(t, "GET", ts.URL+"/healthz", "")
	if resp.StatusCode != 200 {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
}

// mustCreate creates a person-name ngram list or fails the test.
func mustCreate(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()
	resp, body := do(t, "PUT", ts.URL+"/v1/lists/"+name,
		`{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram","grams":[2,3],"strip_spaces":true,"threshold":0.6,"topk":100}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create %s: %d %s", name, resp.StatusCode, body)
	}
}

// mustUpsert posts a JSON array of entries or fails the test.
func mustUpsert(t *testing.T, ts *httptest.Server, list, entries string) {
	t.Helper()
	resp, body := do(t, "POST", ts.URL+"/v1/lists/"+list+"/entries", entries)
	if resp.StatusCode != 200 {
		t.Fatalf("upsert %s: %d %s", list, resp.StatusCode, body)
	}
}

func TestCreateListBadConfig(t *testing.T) {
	ts := newTS(t)
	cases := []struct{ name, cfg string }{
		{"bad preset", `{"analyzer":{"preset":"nope"},"match":{"mode":"ngram"}}`},
		{"bad step", `{"analyzer":{"steps":["bogus"]},"match":{"mode":"ngram"}}`},
		{"bad step arg", `{"analyzer":{"steps":["lowercase:junk"]},"match":{"mode":"ngram"}}`},
		{"bad mode", `{"analyzer":{"preset":"identifier"},"match":{"mode":"levenshtein"}}`},
		{"malformed json", `{"analyzer":`},
	}
	for _, c := range cases {
		resp, body := do(t, "PUT", ts.URL+"/v1/lists/bad", c.cfg)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d %s, want 400", c.name, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), `"error"`) {
			t.Errorf("%s: missing error envelope: %s", c.name, body)
		}
	}
	// Invalid list name (uppercase) is also caller input -> 400.
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/BAD",
		`{"analyzer":{"preset":"identifier"},"match":{"mode":"exact"}}`)
	if resp.StatusCode != 400 {
		t.Errorf("invalid name: %d, want 400", resp.StatusCode)
	}
}

func TestUpsertValidation(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")

	// empty id -> 400 naming the index
	resp, body := do(t, "POST", ts.URL+"/v1/lists/people/entries",
		`[{"id":"ok","keys":["a"]},{"id":"","keys":["b"]}]`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "entry 1") {
		t.Errorf("empty id: %d %s, want 400 naming entry 1", resp.StatusCode, body)
	}

	// malformed JSON -> 400
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/people/entries", `[{"id":`)
	if resp.StatusCode != 400 {
		t.Errorf("malformed JSON: %d, want 400", resp.StatusCode)
	}

	// unknown list -> 404
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/nope/entries", `[{"id":"x","keys":["a"]}]`)
	if resp.StatusCode != 404 {
		t.Errorf("unknown list: %d, want 404", resp.StatusCode)
	}

	// a failed batch must not have been partially applied
	resp, body = do(t, "GET", ts.URL+"/v1/lists/people", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"entries":0`) {
		t.Errorf("stats after rejected upserts: %d %s, want entries:0", resp.StatusCode, body)
	}
}

// An entry whose journal record would exceed the engine's 1 MiB line bound is
// rejected as caller input: 400 naming the entry ID (typed
// engine.EntryTooLargeError mapped by mapStoreError, not string matching),
// nothing applied, and a restart on the same data dir comes up clean.
func TestUpsertOversizeEntry(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	defer ts.Close()
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people", `[{"id":"ok","keys":["Marcus Chen"]}]`)

	// ~2 MiB key: well under the 32 MiB body bound, over the 1 MiB line bound.
	body := `[{"id":"big","keys":["` + strings.Repeat("a", 2<<20) + `"]}]`
	resp, rbody := do(t, "POST", ts.URL+"/v1/lists/people/entries", body)
	if resp.StatusCode != 400 || !strings.Contains(string(rbody), `\"big\"`) {
		t.Fatalf("oversize upsert: %d %.200s, want 400 naming entry \"big\"", resp.StatusCode, rbody)
	}

	// Store unchanged.
	resp, rbody = do(t, "GET", ts.URL+"/v1/lists/people", "")
	if resp.StatusCode != 200 || !strings.Contains(string(rbody), `"entries":1`) {
		t.Fatalf("stats after rejected oversize: %d %s, want entries:1", resp.StatusCode, rbody)
	}

	// Restart clean: the rejection persisted nothing unreadable.
	st2, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("restart after oversize rejection: %v", err)
	}
	l, ok := st2.List("people")
	if !ok {
		t.Fatal("restart lost list")
	}
	if e, _, _ := l.Stats(); e != 1 {
		t.Fatalf("restart entries = %d, want 1", e)
	}
}

// Global topk applies across lists: each list would return one candidate, the
// merged result must be cut to exactly one.
func TestGlobalTopKAcrossLists(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "a")
	mustCreate(t, ts, "b")
	mustUpsert(t, ts, "a", `[{"id":"a1","keys":["Elena Vasquez"]}]`)
	mustUpsert(t, ts, "b", `[{"id":"b1","keys":["Elena Vasquez"]}]`)

	resp, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"Elena Vasquez","lists":["a","b"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("query: %d %s", resp.StatusCode, body)
	}
	var qr struct {
		Candidates []struct {
			List    string `json:"list"`
			EntryID string `json:"entry_id"`
		} `json:"candidates"`
		Versions map[string]string `json:"versions"`
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) != 2 {
		t.Fatalf("without topk: %d candidates, want 2: %s", len(qr.Candidates), body)
	}
	if qr.Versions["a"] == "" || qr.Versions["b"] == "" {
		t.Errorf("missing version stamps: %s", body)
	}

	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"Elena Vasquez","lists":["a","b"],"topk":1}`)
	if resp.StatusCode != 200 {
		t.Fatalf("query topk=1: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) != 1 {
		t.Fatalf("topk=1: %d candidates, want exactly 1: %s", len(qr.Candidates), body)
	}
	// Equal scores tiebreak entry_id asc: "a1" < "b1".
	if qr.Candidates[0].EntryID != "a1" {
		t.Errorf("topk=1 survivor: %+v, want a1 (entry_id asc tiebreak)", qr.Candidates[0])
	}
}

// Threshold override is plumbed through: "elena vasquez" fully covers p1 and
// only partially covers p2 (shared "elena" grams), so a loose threshold
// out-hits a strict one.
func TestThresholdOverride(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people",
		`[{"id":"p1","keys":["Elena Vasquez"]},{"id":"p2","keys":["Elena Sandoval"]}]`)

	count := func(threshold float64) int {
		t.Helper()
		req := fmt.Sprintf(`{"q":"elena vasquez","lists":["people"],"threshold":%g}`, threshold)
		resp, body := do(t, "POST", ts.URL+"/v1/query", req)
		if resp.StatusCode != 200 {
			t.Fatalf("query threshold=%g: %d %s", threshold, resp.StatusCode, body)
		}
		var qr struct {
			Candidates []json.RawMessage `json:"candidates"`
		}
		if err := json.Unmarshal(body, &qr); err != nil {
			t.Fatal(err)
		}
		return len(qr.Candidates)
	}
	loose, strict := count(0.3), count(0.95)
	if loose <= strict {
		t.Errorf("loose threshold (%d hits) should out-hit strict (%d hits)", loose, strict)
	}
}

func TestCompactEndpoint(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people",
		`[{"id":"p1","keys":["Elena Vasquez"]},{"id":"p2","keys":["Marcus Chen"]}]`)
	resp, _ := do(t, "DELETE", ts.URL+"/v1/lists/people/entries/p2", "")
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	// Compact returns fresh stats: overlay and tombstones folded away.
	resp, body := do(t, "POST", ts.URL+"/v1/lists/people/compact", "")
	if resp.StatusCode != 200 {
		t.Fatalf("compact: %d %s", resp.StatusCode, body)
	}
	var st struct {
		Name       string `json:"name"`
		Entries    int    `json:"entries"`
		Overlay    int    `json:"overlay"`
		Tombstones int    `json:"tombstones"`
		Version    string `json:"version"`
		Mode       string `json:"mode"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Name != "people" || st.Entries != 1 || st.Overlay != 0 || st.Tombstones != 0 || st.Mode != "ngram" || st.Version == "" {
		t.Errorf("compact stats: %s", body)
	}

	// Queries still work post-compact.
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"Elena Vasquez","lists":["people"]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"entry_id":"p1"`) {
		t.Errorf("query after compact: %d %s", resp.StatusCode, body)
	}

	// Compacting an unknown list is 404.
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/nope/compact", "")
	if resp.StatusCode != 404 {
		t.Errorf("compact unknown list: %d, want 404", resp.StatusCode)
	}
}

// Deleting an ID the engine doesn't know is a no-op tombstone: acknowledged
// with 200, stats unchanged.
func TestDeleteUnknownID(t *testing.T) {
	ts := newTS(t)
	mustCreate(t, ts, "people")
	mustUpsert(t, ts, "people", `[{"id":"p1","keys":["Elena Vasquez"]}]`)

	_, before := do(t, "GET", ts.URL+"/v1/lists/people", "")
	resp, body := do(t, "DELETE", ts.URL+"/v1/lists/people/entries/ghost", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"deleted":"ghost"`) {
		t.Fatalf("delete unknown id: %d %s, want 200", resp.StatusCode, body)
	}
	var b, a struct {
		Entries    int `json:"entries"`
		Overlay    int `json:"overlay"`
		Tombstones int `json:"tombstones"`
	}
	_, after := do(t, "GET", ts.URL+"/v1/lists/people", "")
	if err := json.Unmarshal(before, &b); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &a); err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("stats changed by no-op delete: before %+v after %+v", b, a)
	}

	// Unknown list stays 404.
	resp, _ = do(t, "DELETE", ts.URL+"/v1/lists/nope/entries/x", "")
	if resp.StatusCode != 404 {
		t.Errorf("delete on unknown list: %d, want 404", resp.StatusCode)
	}
}

func TestListAll(t *testing.T) {
	ts := newTS(t)
	// Empty store -> 200 with an empty JSON array, not null.
	resp, body := do(t, "GET", ts.URL+"/v1/lists", "")
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("empty listAll: %d %q, want []", resp.StatusCode, body)
	}
	mustCreate(t, ts, "b")
	mustCreate(t, ts, "a")
	resp, body = do(t, "GET", ts.URL+"/v1/lists", "")
	if resp.StatusCode != 200 {
		t.Fatalf("listAll: %d", resp.StatusCode)
	}
	var lists []struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(body, &lists); err != nil {
		t.Fatal(err)
	}
	if len(lists) != 2 || lists[0].Name != "a" || lists[1].Name != "b" {
		t.Errorf("listAll: %s, want name-sorted [a b]", body)
	}
}

// Duplicate list names in a query request must not run the list twice or
// duplicate candidates: the server dedupes names (order-preserving) before
// resolution.
func TestQueryDuplicateListNames(t *testing.T) {
	ts := newTS(t)
	mustDo(t, "PUT", ts.URL+"/v1/lists/people",
		`{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram"}}`)
	mustDo(t, "POST", ts.URL+"/v1/lists/people/entries",
		`[{"id":"p1","keys":["Elena Vasquez"]}]`)

	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"elena vasquez","lists":["people","people"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("query: %d %s", resp.StatusCode, body)
	}
	var qr struct {
		Candidates []struct {
			EntryID string `json:"entry_id"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) != 1 || qr.Candidates[0].EntryID != "p1" {
		t.Errorf("duplicated list name: got %d candidates (%s), want exactly 1", len(qr.Candidates), body)
	}
}

// Domain blocklist pattern end-to-end: domain preset + parent_domain fallback.
// A subdomain query falls back to the listed parent (score 90, key names the
// listed domain); fallback with ngram mode is rejected as a 400 at creation.
func TestDomainFallbackViaAPI(t *testing.T) {
	ts := newTS(t)
	mustDo(t, "PUT", ts.URL+"/v1/lists/domains",
		`{"analyzer":{"preset":"domain"},"match":{"mode":"exact","fallback":"parent_domain"}}`)
	mustDo(t, "POST", ts.URL+"/v1/lists/domains/entries",
		`[{"id":"d1","keys":["tempmail.com"]}]`)

	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"smtp.tempmail.com","lists":["domains"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("query: %d %s", resp.StatusCode, body)
	}
	var qr struct {
		Candidates []struct {
			EntryID string  `json:"entry_id"`
			Score   float64 `json:"score"`
			Key     string  `json:"key"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) != 1 || qr.Candidates[0].EntryID != "d1" ||
		qr.Candidates[0].Score != 90 || qr.Candidates[0].Key != "tempmail.com" {
		t.Errorf("parent fallback via API: %s", body)
	}

	// fallback is exact-only: creating an ngram list with it is a 400.
	resp, body = do(t, "PUT", ts.URL+"/v1/lists/bad",
		`{"analyzer":{"preset":"identifier"},"match":{"mode":"ngram","fallback":"parent_domain"}}`)
	if resp.StatusCode != 400 {
		t.Errorf("ngram+fallback: %d %s, want 400", resp.StatusCode, body)
	}
}

// A request that omits topk runs each list at its own default only when that
// default is a deliberate SMALL bound; a zero default (exact-mode unlimited)
// or one beyond the global merge cut is clamped to the cut, because the
// merge makes anything past it unreachable — a 2^20 list default must not
// make the engine collect a million candidates to return 100. The small-
// default half is the observable contract pinned here; the clamp half is
// what the admission charge (ScratchBytesFor at the resolved bound) relies
// on.
func TestListTopKDefaultRespectedAndClamped(t *testing.T) {
	ts := newTS(t)
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact","topk":3}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var entries []string
	for i := 0; i < 10; i++ {
		entries = append(entries, fmt.Sprintf(`{"id":"c%d","keys":["hot"]}`, i))
	}
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/codes/entries", "["+strings.Join(entries, ",")+"]")
	if resp.StatusCode != 200 {
		t.Fatalf("upsert: %d", resp.StatusCode)
	}

	var qr struct {
		Candidates []struct {
			EntryID string `json:"entry_id"`
		} `json:"candidates"`
	}
	// topk absent: the list's own default (3) caps the answer.
	resp, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"hot","lists":["codes"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("query: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) != 3 {
		t.Fatalf("list default topk=3 ignored: %d candidates", len(qr.Candidates))
	}
	// Explicit topk overrides the list default upward.
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"hot","lists":["codes"],"topk":7}`)
	if resp.StatusCode != 200 {
		t.Fatalf("query: %d %s", resp.StatusCode, body)
	}
	qr.Candidates = nil
	if err := json.Unmarshal(body, &qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Candidates) != 7 {
		t.Fatalf("explicit topk=7 got %d candidates", len(qr.Candidates))
	}
}

// A JSON body is exactly one value. Decoder.More only peeks for another
// VALUE, so a trailing lone "}" or "]" — the classic doubled/truncated
// paste — was accepted silently; the check must reject ANY trailing token.
func TestBodyTrailingTokenRejected(t *testing.T) {
	ts := newTS(t)
	for _, body := range []string{
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"exact"}}}`,
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"exact"}}]`,
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"exact"}}{}`,
	} {
		resp, rbody := do(t, "PUT", ts.URL+"/v1/lists/codes", body)
		if resp.StatusCode != 400 {
			t.Errorf("trailing token accepted (%d): body %q -> %s", resp.StatusCode, body, rbody)
		}
	}
	// The clean body still works, whitespace included.
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"exact"}}`+"\n  ")
	if resp.StatusCode != 200 {
		t.Fatalf("clean body refused: %d", resp.StatusCode)
	}
}

// The scratch budget is a ceiling: a single query charged more than the
// WHOLE budget used to be clamped and run alone, quietly exceeding the
// bound the flag promises. It must be refused with a 503 that names the
// remedy instead.
func TestOverBudgetQueryRefused(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "ngram", Grams: []int{2, 3}, StripSpaces: true},
	}); err != nil {
		t.Fatal(err)
	}
	entries := make([]engine.Entry, 2000)
	for i := range entries {
		entries[i] = engine.Entry{ID: fmt.Sprintf("p%05d", i), Keys: []string{fmt.Sprintf("veko rima %04d", i)}}
	}
	if err := st.Replace("people", entries); err != nil {
		t.Fatal(err)
	}
	// A 1-byte budget no query on this list can fit under.
	ts := httptest.NewServer(server.NewWith(st, server.Config{QueryMemBudget: 1}))
	t.Cleanup(ts.Close)

	resp, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"veko rima 0001","lists":["people"]}`)
	if resp.StatusCode != 503 {
		t.Fatalf("over-budget query: %d %s, want 503", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "needs") || !strings.Contains(string(body), "query-mem-budget") {
		t.Fatalf("503 does not name the remedy: %s", body)
	}
}
