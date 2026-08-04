package server_test

// Capacity quota enforcement — 403s naming the quota and
// numbers; exact boundary admitted; replace credits the replaced list;
// quota-less and legacy tenants unlimited.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

// doNDJSONAuth posts an NDJSON body with an API key (the doAuth helper
// declares JSON, which routes to the array parser; ndjson_test's doNDJSON
// is keyless).
func doNDJSONAuth(t *testing.T, ts *httptest.Server, path, body, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// quotaTS builds a one-tenant server ("acme", key "key-acme") with the
// given quotas.
func quotaTS(t *testing.T, q server.TenantQuotas) *httptest.Server {
	t.Helper()
	st, err := engine.Open(filepath.Join(t.TempDir(), "tenants", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	web := server.NewServer(nil, server.Config{})
	if err := web.SetTenants(map[string]server.TenantRuntime{
		"acme": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("key-acme")}, Quotas: q}, Store: st},
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(web)
	t.Cleanup(ts.Close)
	return ts
}

const exactListCfg = `{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`

func TestMaxListsQuota(t *testing.T) {
	ts := quotaTS(t, server.TenantQuotas{MaxLists: 2})
	for _, name := range []string{"one", "two"} {
		if resp, body := doAuth(t, ts, "PUT", "/v1/lists/"+name, exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
			t.Fatalf("create %s: %d %s", name, resp.StatusCode, body)
		}
	}
	// Third list: 403 naming the quota.
	resp, body := doAuth(t, ts, "PUT", "/v1/lists/three", exactListCfg, "X-API-Key", "key-acme")
	if resp.StatusCode != 403 || !strings.Contains(string(body), "max_lists") {
		t.Fatalf("over-quota create: %d %s, want 403 naming max_lists", resp.StatusCode, body)
	}
	// PUT-replace of an EXISTING list at quota: allowed (count unchanged).
	if resp, body := doAuth(t, ts, "PUT", "/v1/lists/two", exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatalf("replace at quota: %d %s, want 200", resp.StatusCode, body)
	}
}

func TestMaxTotalKeysQuota(t *testing.T) {
	ts := quotaTS(t, server.TenantQuotas{MaxTotalKeys: 4})
	if resp, _ := doAuth(t, ts, "PUT", "/v1/lists/codes", exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("create failed")
	}
	// 3 keys in: fine.
	resp, body := doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"a","keys":["k1","k2"]},{"id":"b","keys":["k3"]}]`, "X-API-Key", "key-acme")
	if resp.StatusCode != 200 {
		t.Fatalf("initial load: %d %s", resp.StatusCode, body)
	}
	// +2 would cross 4: 403 with the numbers.
	resp, body = doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"c","keys":["k4","k5"]}]`, "X-API-Key", "key-acme")
	if resp.StatusCode != 403 || !strings.Contains(string(body), "max_total_keys") ||
		!strings.Contains(string(body), "usage 3") {
		t.Fatalf("crossing upsert: %d %s, want 403 with usage", resp.StatusCode, body)
	}
	// +1 lands exactly at the boundary: accepted.
	if resp, body := doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"c","keys":["k4"]}]`, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatalf("boundary upsert: %d %s, want 200", resp.StatusCode, body)
	}
	// At quota now; replace mode credits the replaced list's 4 keys, so a
	// 4-key replacement fits while a 5-key one is refused.
	var four, five strings.Builder
	for i := 0; i < 5; i++ {
		if i < 4 {
			fmt.Fprintf(&four, `{"id":"r%d","keys":["rk%d"]}`+"\n", i, i)
		}
		fmt.Fprintf(&five, `{"id":"r%d","keys":["rk%d"]}`+"\n", i, i)
	}
	req := func(body string) (int, string) {
		return doNDJSONAuth(t, ts, "/v1/lists/codes/entries?replace=true", body, "key-acme")
	}
	if code, b := req(four.String()); code != 200 {
		t.Fatalf("replace within quota: %d %s", code, b)
	}
	if code, b := req(five.String()); code != 403 || !strings.Contains(b, "max_total_keys") {
		t.Fatalf("replace over quota: %d %s, want 403", code, b)
	}
	// Deleting frees quota: -1 key, then +1 fits again.
	if resp, _ := doAuth(t, ts, "DELETE", "/v1/lists/codes/entries/r0", "", "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("delete failed")
	}
	if resp, body := doAuth(t, ts, "POST", "/v1/lists/codes/entries",
		`[{"id":"n","keys":["nk"]}]`, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatalf("post-delete upsert: %d %s, want 200", resp.StatusCode, body)
	}
}

func TestQuotaSpansLists(t *testing.T) {
	// max_total_keys meters the TENANT, not the list.
	ts := quotaTS(t, server.TenantQuotas{MaxTotalKeys: 3})
	for _, name := range []string{"one", "two"} {
		if resp, _ := doAuth(t, ts, "PUT", "/v1/lists/"+name, exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
			t.Fatal("create failed")
		}
	}
	if resp, _ := doAuth(t, ts, "POST", "/v1/lists/one/entries",
		`[{"id":"a","keys":["k1","k2"]}]`, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatal("load one failed")
	}
	resp, body := doAuth(t, ts, "POST", "/v1/lists/two/entries",
		`[{"id":"b","keys":["k3","k4"]}]`, "X-API-Key", "key-acme")
	if resp.StatusCode != 403 {
		t.Fatalf("cross-list quota: %d %s, want 403 (2 in list one + 2 > 3)", resp.StatusCode, body)
	}
}

func TestQuotalessUnlimited(t *testing.T) {
	ts := quotaTS(t, server.TenantQuotas{})
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("l%d", i)
		if resp, _ := doAuth(t, ts, "PUT", "/v1/lists/"+name, exactListCfg, "X-API-Key", "key-acme"); resp.StatusCode != 200 {
			t.Fatalf("quota-less create %s failed", name)
		}
	}
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, `{"id":"e%d","keys":["k%d"]}`+"\n", i, i)
	}
	if code, body := doNDJSONAuth(t, ts, "/v1/lists/l0/entries?replace=true", b.String(), "key-acme"); code != 200 {
		t.Fatalf("quota-less bulk load failed: %d %s", code, body)
	}
}
