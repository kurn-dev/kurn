package server_test

// Server surface for the build-loss counters: they appear in both the
// mutation acknowledgment and the stats endpoint.

import (
	"encoding/json"
	"testing"
)

func TestDroppedKeyCountersSurfaced(t *testing.T) {
	ts := newTS(t)
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase","strip_punctuation","trim"]},"match":{"mode":"exact"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}

	resp, body := do(t, "POST", ts.URL+"/v1/lists/codes/entries?replace=true",
		`[{"id":"e1","keys":["...","ok"]},{"id":"e2","keys":["!!!"]}]`)
	if resp.StatusCode != 200 {
		t.Fatalf("replace: %d (%s)", resp.StatusCode, body)
	}
	var ack map[string]int
	if err := json.Unmarshal(body, &ack); err != nil {
		t.Fatal(err)
	}
	if ack["replaced"] != 2 || ack["dropped_keys"] != 2 || ack["keyless_entries"] != 1 {
		t.Fatalf("replace ack = %v, want replaced 2, dropped_keys 2, keyless_entries 1", ack)
	}

	_, body = do(t, "GET", ts.URL+"/v1/lists/codes", "")
	var st struct {
		Entries        int `json:"entries"`
		DroppedKeys    int `json:"dropped_keys"`
		KeylessEntries int `json:"keyless_entries"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Entries != 2 || st.DroppedKeys != 2 || st.KeylessEntries != 1 {
		t.Fatalf("stats = %+v, want entries 2, dropped 2, keyless 1", st)
	}
}
