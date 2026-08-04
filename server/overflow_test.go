package server_test

// Regression test: an exact-mode hot key (>2^20-1 entries
// sharing one analyzed key) must be a 400 with a JSON error envelope — before
// the fix it was a panic mid-Replace (net/http recovered per connection, so
// the client saw an EOF with no response at all). The list must be left
// exactly as it was, and the server keeps serving.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestHotKeyReplaceRejected(t *testing.T) {
	ts := newTS(t)

	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"g1","keys":["good"]}]`)
	if resp.StatusCode != 200 {
		t.Fatalf("seed upsert: %d", resp.StatusCode)
	}

	// 1<<20 entries, one analyzed key — one over the packed run cap. NDJSON
	// replace: any failure applies nothing.
	var buf bytes.Buffer
	for i := 0; i < 1<<20; i++ {
		fmt.Fprintf(&buf, `{"id":"e%07d","keys":["hot"]}`+"\n", i)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/v1/lists/codes/entries?replace=true", &buf)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("hot-key replace: connection error (panic?): %v", err)
	}
	var body bytes.Buffer
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("hot-key replace: status %d, want 400 (%s…)", resp.StatusCode, body.String()[:min(200, body.Len())])
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body.Bytes(), &env); err != nil || env.Error == "" {
		t.Fatalf("error envelope missing/invalid: %v %s", err, body.String()[:min(200, body.Len())])
	}
	if !strings.Contains(env.Error, `"hot"`) {
		t.Fatalf("error should name the offending key: %s", env.Error)
	}

	// The list is untouched and the server keeps serving: the seeded entry
	// still hits, and a following good mutation succeeds.
	resp, body2 := do(t, "POST", ts.URL+"/v1/query", `{"q":"good","lists":["codes"]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body2), `"entry_id":"g1"`) {
		t.Fatalf("prior content lost after rejected replace: %d %s", resp.StatusCode, body2)
	}
	resp, _ = do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"g2","keys":["also good"]}]`)
	if resp.StatusCode != 200 {
		t.Fatalf("list wedged after rejected replace: %d", resp.StatusCode)
	}
}
