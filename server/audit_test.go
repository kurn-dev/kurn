package server_test

// Every mutation path emits exactly one audit line (with
// tenant/op/list/n/version), the query path emits none, and the metering
// rollups move under traffic.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

// syncBuffer is a slog sink safe for the handler's concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestAuditTrail(t *testing.T) {
	buf := &syncBuffer{}
	st, err := engine.Open(filepath.Join(t.TempDir(), "tenants", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	web := server.NewServer(nil, server.Config{AuditHandler: slog.NewJSONHandler(buf, nil)})
	if err := web.SetTenants(map[string]server.TenantRuntime{
		"acme": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("key-acme")}}, Store: st},
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(web)
	t.Cleanup(ts.Close)

	// One line per mutation path: create, upsert(2), NDJSON replace(3),
	// delete, compact.
	if resp, _ := doAuth(t, ts, "PUT", "/v1/lists/codes", exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("create failed")
	}
	if resp, _ := doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"a","keys":["k1"]},{"id":"b","keys":["k2"]}]`, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("upsert failed")
	}
	if code, _ := doNDJSONAuth(t, ts, "/v1/lists/codes/entries?replace=true",
		"{\"id\":\"r1\",\"keys\":[\"x\"]}\n{\"id\":\"r2\",\"keys\":[\"y\"]}\n{\"id\":\"r3\",\"keys\":[\"z\"]}\n", "key-acme"); code != 200 {
		t.Fatal("replace failed")
	}
	if resp, _ := doAuth(t, ts, "DELETE", "/v1/lists/codes/entries/r1", "", "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("delete failed")
	}
	if resp, _ := doAuth(t, ts, "POST", "/v1/lists/codes/compact", "", "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("compact failed")
	}

	lines := buf.lines()
	if len(lines) != 5 {
		t.Fatalf("audit lines = %d, want exactly 5:\n%+v", len(lines), lines)
	}
	wantOps := []string{"create", "upsert", "replace", "delete", "compact"}
	wantN := []float64{0, 2, 3, 1, 2}
	for i, ln := range lines {
		if ln["op"] != wantOps[i] {
			t.Errorf("line %d op = %v, want %s", i, ln["op"], wantOps[i])
		}
		if ln["n"] != wantN[i] {
			t.Errorf("line %d n = %v, want %v", i, ln["n"], wantN[i])
		}
		if ln["tenant"] != "acme" || ln["list"] != "codes" || ln["msg"] != "mutation" {
			t.Errorf("line %d envelope wrong: %+v", i, ln)
		}
		if v, _ := ln["version"].(string); v == "" {
			t.Errorf("line %d missing version stamp", i)
		}
	}
	// The compact line carries the fresh content-addressed stamp.
	if v := lines[4]["version"].(string); !strings.Contains(v, "@2+j0") {
		t.Errorf("compact version = %q, want content stamp @2+j0", v)
	}

	// Queries emit nothing (hot-path discipline). Also failed mutations:
	// a quota-less 404 list and a 401 must not audit.
	doAuth(t, ts, "POST", "/v1/query", `{"q":"x","lists":["codes"]}`, "X-API-Key", "key-acme")
	doAuth(t, ts, "POST", "/v1/lists/ghost/entries", `[{"id":"a","keys":["k"]}]`, "X-API-Key", "key-acme")
	doAuth(t, ts, "POST", "/v1/lists/codes/entries", `[{"id":"a","keys":["k"]}]`, "X-API-Key", "wrong")
	if n := len(buf.lines()); n != 5 {
		t.Fatalf("queries/failures audited: %d lines, want still 5", n)
	}
}

func TestMeteringRollups(t *testing.T) {
	ts, _ := multiTS(t, t.TempDir())
	if resp, _ := doAuth(t, ts, "PUT", "/v1/lists/codes", exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("create failed")
	}
	if resp, _ := doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"a","keys":["k1","k2"]},{"id":"b","keys":["k3"]}]`, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("upsert failed")
	}
	for i := 0; i < 3; i++ {
		doAuth(t, ts, "POST", "/v1/query", `{"q":"k1","lists":["codes"]}`, "X-API-Key", "key-acme")
	}

	_, body := doAuth(t, ts, "GET", "/metrics", "", "", "")
	text := string(body)
	for metric, want := range map[string]string{
		`kurn_tenant_queries_total{tenant="acme"}`:   "3",
		`kurn_tenant_mutations_total{tenant="acme"}`: "2", // create + upsert
		`kurn_tenant_keys{tenant="acme"}`:            "3",
		`kurn_tenant_lists{tenant="acme"}`:           "1",
		`kurn_tenant_keys{tenant="beta"}`:            "0", // present with zero usage
	} {
		found := false
		for _, ln := range strings.Split(text, "\n") {
			if strings.HasPrefix(ln, metric+" ") {
				found = true
				if got := strings.Fields(ln)[1]; got != want {
					t.Errorf("%s = %s, want %s", metric, got, want)
				}
			}
		}
		if !found {
			t.Errorf("metric %s missing", metric)
		}
	}
}
