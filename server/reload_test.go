package server_test

// The bundle publish path — ship files into the list dir,
// POST /v1/lists/{list}/reload. Atomic swap under concurrent queries, a
// corrupt bundle keeps the old content serving, the response version
// matches the manifest, goldens report in the response, tenant-scoped.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/ingest"
	"github.com/kurn-dev/kurn/server"
)

// buildBundle builds a tiny exact-mode bundle from NDJSON.
func buildBundle(t *testing.T, dir, feed string, golden []engine.GoldenProbe) *ingest.Manifest {
	t.Helper()
	lc := engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Steps: []string{"lowercase", "trim"}},
		Match:    engine.MatchConfig{Mode: "exact"},
		Golden:   golden,
	}
	m := &ingest.Mapping{Format: "ndjson", ID: "id", Keys: []ingest.KeyRule{{Path: "k"}}, List: lc}
	man, err := ingest.Build(m, strings.NewReader(feed), dir, ingest.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return man
}

// shipBundle copies bundle files into the list dir with the contract's
// discipline: temp-name + rename, base.idx removed first, renamed last.
func shipBundle(t *testing.T, bundle, listDir string) {
	t.Helper()
	if err := os.MkdirAll(listDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(listDir, "base.idx"))
	os.Remove(filepath.Join(listDir, "journal.jsonl")) // bundle replaces all content
	ship := func(name string) {
		src, err := os.ReadFile(filepath.Join(bundle, name))
		if err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(listDir, "."+name+".ship")
		if err := os.WriteFile(tmp, src, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(listDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	ship("config.json")
	ship("base.jsonl")
	ship("base.idx")
}

func TestReloadPublish(t *testing.T) {
	dataDir := t.TempDir()
	st, err := engine.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	t.Cleanup(ts.Close)

	// Publish v1 into a list that doesn't exist yet.
	b1 := filepath.Join(t.TempDir(), "v1")
	man1 := buildBundle(t, b1, `{"id":"a","k":"AA-1"}`+"\n"+`{"id":"b","k":"BB-2"}`,
		[]engine.GoldenProbe{{Q: "AA-1", ExpectID: "a"}})
	shipBundle(t, b1, filepath.Join(dataDir, "codes"))
	resp, body := do(t, "POST", ts.URL+"/v1/lists/codes/reload", "")
	if resp.StatusCode != 200 {
		t.Fatalf("reload v1: %d %s", resp.StatusCode, body)
	}
	var rr struct {
		Entries int    `json:"entries"`
		Version string `json:"version"`
		Golden  []struct {
			Q  string `json:"q"`
			OK bool   `json:"ok"`
		} `json:"golden"`
	}
	if err := json.Unmarshal(body, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.Entries != 2 || !strings.HasPrefix(rr.Version, man1.VersionID+"@") {
		t.Fatalf("reload response: %+v, want version prefix %s", rr, man1.VersionID)
	}
	if len(rr.Golden) != 1 || !rr.Golden[0].OK {
		t.Fatalf("golden results: %+v", rr.Golden)
	}
	resp, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"aa-1","lists":["codes"]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"a"`) {
		t.Fatalf("published content not served: %d %s", resp.StatusCode, body)
	}

	// Publish v2 (entry replaced) while queries hammer the list — the swap
	// must never surface an error or a mixed state.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"bb-2","lists":["codes"]}`)
			if resp.StatusCode != 200 {
				t.Errorf("query during publish: %d %s", resp.StatusCode, body)
				return
			}
		}
	}()
	b2 := filepath.Join(t.TempDir(), "v2")
	man2 := buildBundle(t, b2, `{"id":"a","k":"AA-9"}`+"\n"+`{"id":"b","k":"BB-2"}`, nil)
	shipBundle(t, b2, filepath.Join(dataDir, "codes"))
	resp, body = do(t, "POST", ts.URL+"/v1/lists/codes/reload", "")
	close(stop)
	wg.Wait()
	if resp.StatusCode != 200 {
		t.Fatalf("reload v2: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &rr); err != nil || !strings.HasPrefix(rr.Version, man2.VersionID+"@") {
		t.Fatalf("v2 version: %+v want %s", rr, man2.VersionID)
	}
	// New content live, old gone.
	_, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"aa-9","lists":["codes"]}`)
	if !strings.Contains(string(body), `"a"`) {
		t.Fatalf("v2 content missing: %s", body)
	}
	_, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"aa-1","lists":["codes"]}`)
	if !strings.Contains(string(body), `"candidates":[]`) {
		t.Fatalf("v1 content still serving: %s", body)
	}

	// Corrupt publish: broken config ships, reload fails, v2 keeps serving.
	if err := os.WriteFile(filepath.Join(dataDir, "codes", "config.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, body = do(t, "POST", ts.URL+"/v1/lists/codes/reload", "")
	if resp.StatusCode != 409 || !strings.Contains(string(body), "previous content still serving") {
		t.Fatalf("corrupt reload: %d %s, want 409", resp.StatusCode, body)
	}
	_, body = do(t, "POST", ts.URL+"/v1/query", `{"q":"aa-9","lists":["codes"]}`)
	if !strings.Contains(string(body), `"a"`) {
		t.Fatalf("old content lost after failed reload: %s", body)
	}
}

// The ship discipline is enforced, not just documented: a leftover
// non-empty journal makes reload refuse (replaying it would serve content
// the bundle's manifest doesn't describe), and the old content keeps
// serving. An empty journal (a prior Replace's truncation) is fine.
func TestReloadRefusesLeftoverJournal(t *testing.T) {
	dataDir := t.TempDir()
	st, err := engine.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st))
	t.Cleanup(ts.Close)

	b1 := filepath.Join(t.TempDir(), "v1")
	buildBundle(t, b1, `{"id":"a","k":"AA-1"}`, nil)
	shipBundle(t, b1, filepath.Join(dataDir, "codes"))
	if resp, body := do(t, "POST", ts.URL+"/v1/lists/codes/reload", ""); resp.StatusCode != 200 {
		t.Fatalf("clean reload: %d %s", resp.StatusCode, body)
	}
	// An interactive mutation journals; a shipper that forgets to clear it
	// must be refused.
	if resp, _ := do(t, "POST", ts.URL+"/v1/lists/codes/entries", `[{"id":"x","keys":["XX-9"]}]`); resp.StatusCode != 200 {
		t.Fatal("upsert failed")
	}
	b2 := filepath.Join(t.TempDir(), "v2")
	buildBundle(t, b2, `{"id":"a","k":"AA-2"}`, nil)
	shipNoJournalRemoval := func(bundle, listDir string) {
		for _, name := range []string{"config.json", "base.jsonl", "base.idx"} {
			src, _ := os.ReadFile(filepath.Join(bundle, name))
			os.WriteFile(filepath.Join(listDir, name), src, 0o644)
		}
	}
	shipNoJournalRemoval(b2, filepath.Join(dataDir, "codes"))
	resp, body := do(t, "POST", ts.URL+"/v1/lists/codes/reload", "")
	if resp.StatusCode != 409 || !strings.Contains(string(body), "journal") {
		t.Fatalf("leftover journal accepted: %d %s", resp.StatusCode, body)
	}
	// Old content (incl. the journaled upsert) still serves.
	if _, body := do(t, "POST", ts.URL+"/v1/query", `{"q":"xx-9","lists":["codes"]}`); !strings.Contains(string(body), `"x"`) {
		t.Fatalf("journaled content lost: %s", body)
	}
	// Proper ship (journal removed) succeeds.
	shipBundle(t, b2, filepath.Join(dataDir, "codes"))
	if resp, body := do(t, "POST", ts.URL+"/v1/lists/codes/reload", ""); resp.StatusCode != 200 {
		t.Fatalf("post-cleanup reload: %d %s", resp.StatusCode, body)
	}
}

func TestReloadTenantScoped(t *testing.T) {
	dir := t.TempDir()
	ts, _ := multiTS(t, dir)
	bundle := filepath.Join(t.TempDir(), "b")
	man := buildBundle(t, bundle, `{"id":"x","k":"KEY-1"}`, nil)
	shipBundle(t, bundle, filepath.Join(dir, "tenants", "acme", "codes"))

	resp, body := doAuth(t, ts, "POST", "/v1/lists/codes/reload", "", "X-API-Key", "key-acme")
	if resp.StatusCode != 200 || !strings.Contains(string(body), man.VersionID) {
		t.Fatalf("tenant reload: %d %s", resp.StatusCode, body)
	}
	// acme sees it; beta doesn't.
	_, body = doAuth(t, ts, "POST", "/v1/query", `{"q":"key-1","lists":["codes"]}`, "X-API-Key", "key-acme")
	if !strings.Contains(string(body), `"x"`) {
		t.Fatalf("acme content missing: %s", body)
	}
	if resp, _ := doAuth(t, ts, "POST", "/v1/query", `{"q":"key-1","lists":["codes"]}`, "X-API-Key", "key-beta"); resp.StatusCode != 404 {
		t.Fatalf("beta sees acme's reloaded list: %d", resp.StatusCode)
	}
	if resp, _ := doAuth(t, ts, "POST", "/v1/lists/codes/reload", "", "", ""); resp.StatusCode != 401 {
		t.Fatalf("keyless reload: %d, want 401", resp.StatusCode)
	}
}
