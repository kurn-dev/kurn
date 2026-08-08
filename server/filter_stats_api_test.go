package server_test

// The filter_stats HTTP surface: default-on for successful filtered
// responses (including empty results), keyed by list name so request
// order cannot change the evidence, byte-absent on unfiltered responses
// (old shape preserved) and on every failed response, and inherited
// per-check by the batch endpoint.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

type statsEnvelope struct {
	Candidates []json.RawMessage `json:"candidates"`
	FilterStats map[string]struct {
		Evaluated int64 `json:"evaluated"`
		Rejected  int64 `json:"rejected"`
	} `json:"filter_stats"`
}

// Both fixture entries share the analyzed key, so a filtered query
// evaluates both: evaluated=2 with exactly one survivor per program.
func TestFilterStatsPresentAndExact(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("filtered query: %d %s", resp.StatusCode, body)
	}
	var env statsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	st, ok := env.FilterStats["sanctions"]
	if !ok {
		t.Fatalf("filter_stats missing or not keyed by list: %s", body)
	}
	if st.Evaluated != 2 || st.Rejected != 1 {
		t.Fatalf("stats = %+v, want evaluated=2 rejected=1: %s", st, body)
	}
	if len(env.Candidates) != 1 {
		t.Fatalf("want 1 candidate, body: %s", body)
	}
}

// Unfiltered responses keep the old shape byte-for-byte: no member.
func TestFilterStatsAbsentUnfiltered(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("unfiltered query: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "filter_stats") {
		t.Fatalf("unfiltered response leaked filter_stats: %s", body)
	}
	// The {} == omission identity extends to stats: an empty filter IS
	// the unfiltered path.
	resp, body = do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{}}`)
	if resp.StatusCode != 200 || strings.Contains(string(body), "filter_stats") {
		t.Fatalf("empty-filter response must match unfiltered shape: %d %s", resp.StatusCode, body)
	}
}

// Filtered empty success still reports the work performed.
func TestFilterStatsEmptySuccess(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	resp, body := do(t, "POST", ts.URL+"/v1/query",
		`{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"NONE"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("empty-success query: %d %s", resp.StatusCode, body)
	}
	var env statsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Candidates) != 0 {
		t.Fatalf("want empty candidates: %s", body)
	}
	st, ok := env.FilterStats["sanctions"]
	if !ok || st.Evaluated != 2 || st.Rejected != 2 {
		t.Fatalf("empty success stats = %+v ok=%v, want evaluated=2 rejected=2: %s", st, ok, body)
	}
}

// Two lists, keyed evidence: reordering the request's lists changes
// neither the per-list values nor the keys.
func TestFilterStatsMultiListKeyedEvidence(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "alpha")
	putFilterableList(t, ts.URL, "beta")
	// beta gets one extra rejected entry so the two lists' stats differ —
	// a swap or collapse cannot go unnoticed.
	do(t, "POST", ts.URL+"/v1/lists/beta/entries",
		`[{"id":"e3","keys":["dana kovak"],"payload":{"program":"EU"}}]`)

	read := func(order string) map[string][2]int64 {
		resp, body := do(t, "POST", ts.URL+"/v1/query",
			`{"q":"dana kovak","lists":[`+order+`],"filter":{"program":"SDN"}}`)
		if resp.StatusCode != 200 {
			t.Fatalf("multi-list query [%s]: %d %s", order, resp.StatusCode, body)
		}
		var env statsEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatal(err)
		}
		out := map[string][2]int64{}
		for k, v := range env.FilterStats {
			out[k] = [2]int64{v.Evaluated, v.Rejected}
		}
		return out
	}
	fwd := read(`"alpha","beta"`)
	rev := read(`"beta","alpha"`)
	if len(fwd) != 2 || fwd["alpha"] != [2]int64{2, 1} || fwd["beta"] != [2]int64{3, 2} {
		t.Fatalf("per-list stats wrong: %v", fwd)
	}
	if fwd["alpha"] != rev["alpha"] || fwd["beta"] != rev["beta"] {
		t.Fatalf("list order changed keyed evidence: fwd %v rev %v", fwd, rev)
	}
}

// A failed filtered execution (malformed stored payload → 500) carries
// no partial stats: the error envelope is the entire body.
func TestFilterStatsAbsentOnFailure(t *testing.T) {
	ts, st := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")
	// Neither HTTP nor the Store admits a malformed payload (both
	// marshal); install it via the in-memory library path on the served
	// list, as the malformed-payload test does.
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
		t.Fatalf("malformed stored payload must 500: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "filter_stats") {
		t.Fatalf("failed response leaked stats: %s", body)
	}
}

// Batch: stats appear per successful filtered check only — never on the
// unfiltered check, never on the inline-errored check.
func TestFilterStatsBatchPerCheck(t *testing.T) {
	ts, _ := newFilterTS(t)
	putFilterableList(t, ts.URL, "sanctions")

	resp, body := do(t, "POST", ts.URL+"/v1/batch-query", `{"checks":[
		{"q":"dana kovak","lists":["sanctions"],"filter":{"program":"SDN"}},
		{"q":"dana kovak","lists":["sanctions"]},
		{"q":"dana kovak","lists":["sanctions"],"filter":{"undeclared":"x"}}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("batch: %d %s", resp.StatusCode, body)
	}
	var batch struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Results) != 3 {
		t.Fatalf("want 3 results: %s", body)
	}
	var first statsEnvelope
	if err := json.Unmarshal(batch.Results[0], &first); err != nil {
		t.Fatal(err)
	}
	if st, ok := first.FilterStats["sanctions"]; !ok || st.Evaluated != 2 || st.Rejected != 1 {
		t.Fatalf("filtered check stats wrong: %s", batch.Results[0])
	}
	if strings.Contains(string(batch.Results[1]), "filter_stats") {
		t.Fatalf("unfiltered check leaked stats: %s", batch.Results[1])
	}
	if !strings.Contains(string(batch.Results[2]), "error") ||
		strings.Contains(string(batch.Results[2]), "filter_stats") {
		t.Fatalf("errored check must be error-only: %s", batch.Results[2])
	}
}
