package server_test

// Regression test: CreateList errors must be classified —
// caller input (bad name/config) is 400, store IO faults are 500. Pre-fix
// every error was 400, presenting e.g. a permission failure as the caller's
// fault.

import (
	"net/http/httptest"
	"os"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func TestCreateListErrorClassification(t *testing.T) {
	dir := t.TempDir()
	st, err := engine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	t.Cleanup(ts.Close)

	// Caller input: unknown match mode → 400.
	resp, _ := do(t, "PUT", ts.URL+"/v1/lists/bad",
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"nope"}}`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad config: status %d, want 400", resp.StatusCode)
	}
	// Caller input: invalid list name → 400.
	resp, _ = do(t, "PUT", ts.URL+"/v1/lists/UPPER",
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"exact"}}`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad name: status %d, want 400", resp.StatusCode)
	}

	// Store IO fault: data dir unwritable → MkdirAll fails → 500, not 400.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	resp, body := do(t, "PUT", ts.URL+"/v1/lists/newlist",
		`{"analyzer":{"steps":["lowercase"]},"match":{"mode":"exact"}}`)
	if resp.StatusCode != 500 {
		t.Fatalf("IO fault: status %d (%s), want 500", resp.StatusCode, body)
	}
}
