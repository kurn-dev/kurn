package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/prepared"
	"github.com/kurn-dev/kurn/engine/prepared/builder"
	"github.com/kurn-dev/kurn/server"
)

func preparedServer(t *testing.T, keys ...string) (*engine.Store, *server.Server) {
	return preparedServerAt(t, prepared.LevelFull, keys...)
}

func preparedServerAt(t *testing.T, level prepared.Level, keys ...string) (*engine.Store, *server.Server) {
	t.Helper()
	buf, _, err := builder.Encode("people", engine.ListConfig{
		Analyzer:   engine.AnalyzerConfig{Preset: "person-name"},
		Match:      engine.MatchConfig{Mode: "ngram"},
		Filterable: []engine.FilterField{{Name: "country", Path: "country"}},
	}, []engine.Entry{
		{ID: "p1", Keys: []string{"Ada Lovelace"}, Payload: json.RawMessage(`{"country":"GB"}`)},
		{ID: "p2", Keys: []string{"Grace Hopper"}, Payload: json.RawMessage(`{"country":"US"}`)},
	}, builder.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "people.kurn")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := engine.OpenPrepared(path, engine.PreparedOptions{Verify: level})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, server.NewServer(st, server.Config{APIKeys: keys})
}

func TestPreparedReadRoutesKeepResponseShapes(t *testing.T) {
	st, h := preparedServer(t, "secret")
	wantVersion := st.Lists()[0].Version()
	tests := []struct {
		path, body string
		check      func(t *testing.T, got map[string]any)
	}{
		{"/v1/query", `{"q":"Ada Lovelace","lists":["people"]}`, func(t *testing.T, got map[string]any) {
			cs, _ := got["candidates"].([]any)
			if len(cs) == 0 || cs[0].(map[string]any)["entry_id"] != "p1" {
				t.Fatalf("query candidates: %v", got)
			}
			if got["versions"].(map[string]any)["people"] != wantVersion {
				t.Fatalf("query versions: %v", got["versions"])
			}
		}},
		{"/v1/query", `{"q":"Ada Lovelace","lists":["people"],"filter":{"country":"GB"}}`, func(t *testing.T, got map[string]any) {
			cs, _ := got["candidates"].([]any)
			if len(cs) != 1 {
				t.Fatalf("filtered candidates = %v", got["candidates"])
			}
			if cs[0].(map[string]any)["entry_id"] != "p1" {
				t.Fatalf("filter failed to exclude p2: %v", cs)
			}
		}},
		{"/v1/batch-query", `{"checks":[{"q":"Ada Lovelace","lists":["people"]},{"q":"Grace Hopper","lists":["people"]}]}`, func(t *testing.T, got map[string]any) {
			rs, _ := got["results"].([]any)
			if len(rs) != 2 {
				t.Fatalf("batch results = %v", got["results"])
			}
			for i, id := range []string{"p1", "p2"} {
				cs := rs[i].(map[string]any)["candidates"].([]any)
				if len(cs) == 0 || cs[0].(map[string]any)["entry_id"] != id {
					t.Fatalf("batch result %d = %v, want %s", i, rs[i], id)
				}
			}
		}},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.path, rr.Code, rr.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		tc.check(t, got)
		if rr.Header().Get("Kurn-Artifact-Verification") != "full" {
			t.Fatalf("%s verification header=%q", tc.path, rr.Header().Get("Kurn-Artifact-Verification"))
		}
		if tc.path == "/v1/query" {
			bases, ok := got["version_bases"].(map[string]any)
			if !ok || bases["people"] != engine.StampBasisVerified {
				t.Fatalf("%s version_bases=%v", tc.path, got["version_bases"])
			}
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/lists", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var all []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &all); err != nil || len(all) != 1 || all[0]["version_basis"] != engine.StampBasisVerified {
		t.Fatalf("list all basis: err=%v body=%s", err, rr.Body.String())
	}
}

type unreadBody struct{ read bool }

func (b *unreadBody) Read([]byte) (int, error) { b.read = true; return 0, io.EOF }
func (*unreadBody) Close() error               { return nil }

func TestPreparedWriteRoutesRefuseBeforeBodyAfterAuth(t *testing.T) {
	_, h := preparedServer(t, "secret")
	tests := []struct{ method, path string }{
		{http.MethodPut, "/v1/lists/people"},
		{http.MethodPost, "/v1/lists/people/entries"},
		{http.MethodDelete, "/v1/lists/people/entries/p1"},
		{http.MethodPost, "/v1/lists/people/compact"},
		{http.MethodPost, "/v1/lists/people/reload"},
	}
	for _, tc := range tests {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			body := &unreadBody{}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Authorization", "Bearer secret")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusConflict || body.read {
				t.Fatalf("status=%d body_read=%v response=%s", rr.Code, body.read, rr.Body.String())
			}
			if got := rr.Header().Get("Kurn-Artifact-Verification"); got != "full" {
				t.Fatalf("refusal verification header=%q", got)
			}

			body = &unreadBody{}
			req = httptest.NewRequest(tc.method, tc.path, body)
			rr = httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized || body.read {
				t.Fatalf("unauthorized status=%d body_read=%v", rr.Code, body.read)
			}
			if got := rr.Header().Get("Kurn-Artifact-Verification"); got != "full" {
				t.Fatalf("auth verification header=%q", got)
			}
		})
	}
}

func TestArtifactzVerifiedShapeAndAuthScope(t *testing.T) {
	st, h := preparedServer(t, "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifactz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	info := st.Prepared()
	want := map[string]any{
		"container_sha256": info.ContainerSHA256,
		"format":           fmt.Sprintf("KURNART1 %d.%d", info.Major, info.Minor),
		"name":             info.Name,
		"entries":          float64(info.Entries),
		"base_id":          info.BaseID,
		"config_digest":    info.ConfigDigest,
		"analyzer_digest":  info.AnalyzerDigest,
		"stamp":            st.Lists()[0].Version(),
		"verification":     "full",
		"stamp_basis":      engine.StampBasisVerified,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
	if got["verified"].(map[string]any)["base_id"] != info.BaseID || got["declared"].(map[string]any)["base_id"] != info.DeclaredBaseID {
		t.Fatalf("identity evidence=%v", got)
	}
	for _, forbidden := range []string{"path", "source", "keys", "dropped", "keyless", "payload"} {
		if bytes.Contains(rr.Body.Bytes(), []byte(forbidden)) {
			t.Errorf("response leaks forbidden field class %q: %s", forbidden, rr.Body.String())
		}
	}

	mutable, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mh := server.NewServer(mutable, server.Config{APIKeys: []string{"secret"}})
	rr = httptest.NewRecorder()
	mh.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifactz", strings.NewReader("")))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("mutable /artifactz without auth = %d, want 401", rr.Code)
	}
}

func TestTrustedArtifactEvidenceAndAssuranceHeaders(t *testing.T) {
	st, h := preparedServerAt(t, prepared.LevelTrusted, "secret")
	for _, tc := range []struct {
		method, path, key string
		want              int
	}{
		{http.MethodGet, "/readyz", "", http.StatusOK},
		{http.MethodPost, "/v1/query", "", http.StatusUnauthorized},
		{http.MethodGet, "/v1/not-a-route", "secret", http.StatusNotFound},
		{http.MethodGet, "/v1/query", "secret", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/lists/people/compact", "secret", http.StatusConflict},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"q":"Ada","lists":["people"]}`))
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != tc.want || rr.Header().Get("Kurn-Artifact-Verification") != "trusted" {
			t.Errorf("%s %s: status=%d header=%q body=%s", tc.method, tc.path, rr.Code, rr.Header().Get("Kurn-Artifact-Verification"), rr.Body.String())
		}
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifactz", nil))
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || got["verification"] != "trusted" || got["stamp_basis"] != engine.StampBasisDeclared || got["verified"] != nil {
		t.Fatalf("trusted artifactz: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := got["container_sha256"]; ok {
		t.Fatalf("trusted response exposed top-level verified container: %s", rr.Body.String())
	}
	if _, ok := got["base_id"]; ok {
		t.Fatalf("trusted response exposed top-level verified base: %s", rr.Body.String())
	}
	if got["declared"].(map[string]any)["base_id"] != st.Prepared().DeclaredBaseID {
		t.Fatalf("declared identity=%v", got["declared"])
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/batch-query", strings.NewReader(`{"checks":[{"q":"Ada Lovelace","lists":["people"]}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var batch map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	result := batch["results"].([]any)[0].(map[string]any)
	if result["version_bases"].(map[string]any)["people"] != engine.StampBasisDeclared {
		t.Fatalf("batch basis=%v", result)
	}
}

func TestMutableResponsesOmitPreparedAssurance(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateList("people", engine.ListConfig{Match: engine.MatchConfig{Mode: "exact"}}); err != nil {
		t.Fatal(err)
	}
	h := server.NewServer(st, server.Config{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/lists", nil))
	if rr.Header().Get("Kurn-Artifact-Verification") != "" || bytes.Contains(rr.Body.Bytes(), []byte("version_basis")) {
		t.Fatalf("mutable assurance leaked: header=%q body=%s", rr.Header().Get("Kurn-Artifact-Verification"), rr.Body.String())
	}
}

func TestStructureArtifactzRemainsDiagnostic(t *testing.T) {
	_, h := preparedServerAt(t, prepared.LevelStructure)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/artifactz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("structure artifactz=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTenantAssuranceUsesResolvedStore(t *testing.T) {
	preparedStore, _ := preparedServerAt(t, prepared.LevelTrusted)
	mutableStore, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "tenant-key"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	tenant := func(st *engine.Store) map[string]server.TenantRuntime {
		return map[string]server.TenantRuntime{
			"tenant": {Spec: server.TenantSpec{KeyDigests: []string{digest}}, Store: st},
		}
	}
	request := func(h *server.Server, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/lists", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	preparedRoot := server.NewServer(preparedStore, server.Config{})
	if err := preparedRoot.SetTenants(tenant(mutableStore)); err != nil {
		t.Fatal(err)
	}
	if rr := request(preparedRoot, key); rr.Code != http.StatusOK || rr.Header().Get("Kurn-Artifact-Verification") != "" {
		t.Fatalf("mutable tenant inherited prepared-root assurance: status=%d header=%q", rr.Code, rr.Header().Get("Kurn-Artifact-Verification"))
	}
	if rr := request(preparedRoot, ""); rr.Code != http.StatusUnauthorized || rr.Header().Get("Kurn-Artifact-Verification") != "" {
		t.Fatalf("unresolved tenant leaked assurance: status=%d header=%q", rr.Code, rr.Header().Get("Kurn-Artifact-Verification"))
	}

	mutableRoot := server.NewServer(mutableStore, server.Config{})
	if err := mutableRoot.SetTenants(tenant(preparedStore)); err != nil {
		t.Fatal(err)
	}
	if rr := request(mutableRoot, key); rr.Code != http.StatusOK || rr.Header().Get("Kurn-Artifact-Verification") != "trusted" {
		t.Fatalf("prepared tenant assurance: status=%d header=%q", rr.Code, rr.Header().Get("Kurn-Artifact-Verification"))
	}

	ready := httptest.NewRecorder()
	preparedRoot.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if got := ready.Header().Get("Kurn-Artifact-Verification"); got != "" {
		t.Fatalf("mixed-store readiness claimed one policy %q", got)
	}
}
