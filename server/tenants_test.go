package server_test

// Tenant registry, key-scoped routing, isolation, and atomic
// reload. Legacy single-tenant behavior is covered by the rest of the
// suite (no registry installed there).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/server"
)

func digestOf(key string) string {
	d := sha256.Sum256([]byte(key))
	return hex.EncodeToString(d[:])
}

// multiTS builds a Server with tenants acme (key "key-acme") and beta
// (key "key-beta"), each on its own store under dir/tenants/<id>.
func multiTS(t *testing.T, dir string) (*httptest.Server, *server.Server) {
	t.Helper()
	web := server.NewServer(nil, server.Config{})
	rts := map[string]server.TenantRuntime{}
	for _, name := range []string{"acme", "beta"} {
		st, err := engine.Open(filepath.Join(dir, "tenants", name))
		if err != nil {
			t.Fatal(err)
		}
		rts[name] = server.TenantRuntime{
			Spec:  server.TenantSpec{KeyDigests: []string{digestOf("key-" + name)}},
			Store: st,
		}
	}
	if err := web.SetTenants(rts); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(web)
	t.Cleanup(ts.Close)
	return ts, web
}

func TestParseTenants(t *testing.T) {
	good := fmt.Sprintf(`{"acme":{"key_digests":[%q],"quotas":{"max_lists":5}}}`, digestOf("k"))
	specs, err := server.ParseTenants([]byte(good))
	if err != nil || specs["acme"].Quotas.MaxLists != 5 {
		t.Fatalf("valid file rejected: %v %+v", err, specs)
	}
	for name, bad := range map[string]string{
		"empty":        `{}`,
		"bad-name":     fmt.Sprintf(`{"ACME":{"key_digests":[%q]}}`, digestOf("k")),
		"no-keys":      `{"acme":{"key_digests":[]}}`,
		"short-digest": `{"acme":{"key_digests":["abcd"]}}`,
		"not-hex":      `{"acme":{"key_digests":["` + strings.Repeat("zz", 32) + `"]}}`,
		"neg-quota":    fmt.Sprintf(`{"acme":{"key_digests":[%q],"quotas":{"max_lists":-1}}}`, digestOf("k")),
	} {
		if _, err := server.ParseTenants([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTenantIsolation(t *testing.T) {
	ts, _ := multiTS(t, t.TempDir())

	// Same list name in both tenants, different content.
	for _, tn := range []string{"acme", "beta"} {
		resp, body := doAuth(t, ts, "PUT", "/v1/lists/codes",
			`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`, "X-API-Key", "key-"+tn)
		if resp.StatusCode != 200 {
			t.Fatalf("%s create: %d %s", tn, resp.StatusCode, body)
		}
		resp, _ = doAuth(t, ts, "POST", "/v1/lists/codes/entries",
			fmt.Sprintf(`[{"id":"e-%s","keys":["ID-%s"]}]`, tn, tn), "X-API-Key", "key-"+tn)
		if resp.StatusCode != 200 {
			t.Fatalf("%s upsert: %d", tn, resp.StatusCode)
		}
	}
	// Each key sees only its tenant's entry.
	for _, tn := range []string{"acme", "beta"} {
		other := "beta"
		if tn == "beta" {
			other = "acme"
		}
		resp, body := doAuth(t, ts, "POST", "/v1/query",
			fmt.Sprintf(`{"q":"id-%s","lists":["codes"]}`, tn), "X-API-Key", "key-"+tn)
		if resp.StatusCode != 200 || !strings.Contains(string(body), fmt.Sprintf(`"e-%s"`, tn)) {
			t.Fatalf("%s own query: %d %s", tn, resp.StatusCode, body)
		}
		_, body = doAuth(t, ts, "POST", "/v1/query",
			fmt.Sprintf(`{"q":"id-%s","lists":["codes"]}`, other), "X-API-Key", "key-"+tn)
		if !strings.Contains(string(body), `"candidates":[]`) {
			t.Fatalf("%s sees %s's entry: %s", tn, other, body)
		}
	}
	// Unknown/missing key → 401; probes stay open.
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "X-API-Key", "wrong"); resp.StatusCode != 401 {
		t.Fatalf("unknown key: %d, want 401", resp.StatusCode)
	}
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "", ""); resp.StatusCode != 401 {
		t.Fatalf("missing key: %d, want 401", resp.StatusCode)
	}
	for _, p := range []string{"/livez", "/readyz", "/metrics", "/healthz"} {
		if resp, _ := doAuth(t, ts, "GET", p, "", "", ""); resp.StatusCode == 401 {
			t.Errorf("%s gated in multi-tenant mode", p)
		}
	}

	// Metrics carry the tenant label.
	_, body := doAuth(t, ts, "GET", "/metrics", "", "", "")
	if !strings.Contains(string(body), `kurn_queries_total{tenant="acme",list="codes"}`) {
		t.Fatalf("tenant label missing from metrics:\n%s", body)
	}
}

func TestTenantReloadAddRemove(t *testing.T) {
	dir := t.TempDir()
	ts, web := multiTS(t, dir)
	seed := func(tn string) {
		resp, _ := doAuth(t, ts, "PUT", "/v1/lists/codes",
			`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"}}`, "X-API-Key", "key-"+tn)
		if resp.StatusCode != 200 {
			t.Fatalf("%s create: %d", tn, resp.StatusCode)
		}
	}
	seed("acme")
	seed("beta")

	// Reload v2: acme rotated to a NEW key (old + new both live — overlap
	// rotation), beta dropped, gamma added. Note the caller-owns-stores
	// contract: kurnd's reload loop reuses its opened-store map so a
	// surviving tenant keeps its live *Store across reloads (re-opening
	// the same dir would race the live store); this test supplies a
	// distinct dir for the "reused" store to keep that contract honest
	// without reaching into server internals.
	gammaSt, err := engine.Open(filepath.Join(dir, "tenants", "gamma"))
	if err != nil {
		t.Fatal(err)
	}
	acmeLive, err := engine.Open(filepath.Join(dir, "tenants", "acme-2"))
	if err != nil {
		t.Fatal(err)
	}
	cur := map[string]server.TenantRuntime{
		"acme": {
			Spec:  server.TenantSpec{KeyDigests: []string{digestOf("key-acme"), digestOf("key-acme-new")}},
			Store: acmeLive,
		},
		"gamma": {
			Spec:  server.TenantSpec{KeyDigests: []string{digestOf("key-gamma")}},
			Store: gammaSt,
		},
	}
	if err := web.SetTenants(cur); err != nil {
		t.Fatal(err)
	}

	// New acme key works, old key still works (overlap rotation), beta 401s,
	// gamma works.
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "X-API-Key", "key-acme-new"); resp.StatusCode != 200 {
		t.Fatalf("rotated-in key: %d", resp.StatusCode)
	}
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "X-API-Key", "key-acme"); resp.StatusCode != 200 {
		t.Fatalf("overlap key: %d", resp.StatusCode)
	}
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "X-API-Key", "key-beta"); resp.StatusCode != 401 {
		t.Fatalf("removed tenant's key: %d, want 401", resp.StatusCode)
	}
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "X-API-Key", "key-gamma"); resp.StatusCode != 200 {
		t.Fatalf("added tenant's key: %d", resp.StatusCode)
	}

	// A rejected reload (duplicate digest across tenants) keeps serving on
	// the current registry.
	bad := map[string]server.TenantRuntime{
		"a": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("dup")}}, Store: gammaSt},
		"b": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("dup")}}, Store: gammaSt},
	}
	if err := web.SetTenants(bad); err == nil {
		t.Fatal("duplicate digest across tenants accepted")
	}
	if resp, _ := doAuth(t, ts, "GET", "/v1/lists", "", "X-API-Key", "key-gamma"); resp.StatusCode != 200 {
		t.Fatalf("rejected reload disturbed serving: %d", resp.StatusCode)
	}
}

func TestReadyzNamesTenant(t *testing.T) {
	ts, _ := multiTS(t, t.TempDir())
	// A failing golden in acme: /readyz names the tenant.
	resp, _ := doAuth(t, ts, "PUT", "/v1/lists/codes",
		`{"analyzer":{"steps":["lowercase","trim"]},"match":{"mode":"exact"},
		  "golden":[{"q":"AA-1","expect_id":"c1"}]}`, "X-API-Key", "key-acme")
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	resp, body := doAuth(t, ts, "GET", "/readyz", "", "", "")
	if resp.StatusCode != 503 {
		t.Fatalf("readyz: %d %s, want 503", resp.StatusCode, body)
	}
	var r struct {
		Failures []struct {
			Tenant string `json:"tenant"`
			List   string `json:"list"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Failures) != 1 ||
		r.Failures[0].Tenant != "acme" || r.Failures[0].List != "codes" {
		t.Fatalf("failure does not name the tenant: %s", body)
	}
}

// Two private tenants pointing at one store share a namespace silently:
// each sees the other's lists under its own name, reads them, and can
// overwrite them, while every isolation report still looks intact.
func TestSetTenantsRefusesASharedStore(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	web := server.NewServer(nil, server.Config{})
	err = web.SetTenants(map[string]server.TenantRuntime{
		"acme": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("key-acme")}}, Store: st},
		"beta": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("key-beta")}}, Store: st},
	})
	if err == nil {
		t.Fatal("two tenants were allowed to share one store")
	}
	if !strings.Contains(err.Error(), "same store") {
		t.Fatalf("error does not explain the aliasing: %v", err)
	}
}

// The aliasing that mattered most slipped past a check on the DECLARED
// pointers: only one tenant named a store, so nothing looked duplicated,
// and shared_reads resolution then handed the same store to both — with
// the private tenant holding write access to the namespace every
// shared-read tenant serves from.
func TestSetTenantsRefusesPrivateAliasOfTheSharedStore(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	web := server.NewServer(st, server.Config{})
	err = web.SetTenants(map[string]server.TenantRuntime{
		"private": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("k1")}}, Store: st},
		"free":    {Spec: server.TenantSpec{KeyDigests: []string{digestOf("k2")}, SharedReads: true}},
	})
	if err == nil {
		t.Fatal("a private tenant was allowed to alias the shared-read store")
	}
	if !strings.Contains(err.Error(), "shared_reads") {
		t.Fatalf("error does not explain the mismatch: %v", err)
	}

	// Several shared_reads tenants on one store stay legal: they are refused
	// every mutation, so many readers of one published namespace is the
	// intended shape, not the bug.
	if err := web.SetTenants(map[string]server.TenantRuntime{
		"free1": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("k3")}, SharedReads: true}},
		"free2": {Spec: server.TenantSpec{KeyDigests: []string{digestOf("k4")}, SharedReads: true}},
	}); err != nil {
		t.Fatalf("two shared_reads tenants were refused: %v", err)
	}
}
