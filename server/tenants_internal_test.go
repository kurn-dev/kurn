package server

// White-box: reload carryover of governor state. A reload
// with unchanged budget/rate keeps the tenant's live admitter and limiter
// (in-flight releases stay consistent, token buckets don't refill);
// changed values build fresh ones.

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/kurn-dev/kurn/engine"
)

func TestReloadCarriesGovernorState(t *testing.T) {
	st, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	web := NewServer(nil, Config{})
	d := sha256.Sum256([]byte("k"))
	spec := TenantSpec{
		KeyDigests: []string{hex.EncodeToString(d[:])},
		Quotas:     TenantQuotas{ScratchBudgetMB: 64, RatePerSec: 10},
	}
	if err := web.SetTenants(map[string]TenantRuntime{"acme": {Spec: spec, Store: st}}); err != nil {
		t.Fatal(err)
	}
	t1 := web.s.tenants.Load().ordered[0]
	if t1.admit == nil || t1.limiter == nil {
		t.Fatal("governor objects not built")
	}

	// Same quotas: same objects.
	if err := web.SetTenants(map[string]TenantRuntime{"acme": {Spec: spec, Store: st}}); err != nil {
		t.Fatal(err)
	}
	t2 := web.s.tenants.Load().ordered[0]
	if t2.admit != t1.admit || t2.limiter != t1.limiter {
		t.Fatal("unchanged quotas rebuilt governor state (in-flight releases would desync; bucket refilled)")
	}

	// Changed budget: fresh admitter, limiter still carried.
	spec2 := spec
	spec2.Quotas.ScratchBudgetMB = 128
	if err := web.SetTenants(map[string]TenantRuntime{"acme": {Spec: spec2, Store: st}}); err != nil {
		t.Fatal(err)
	}
	t3 := web.s.tenants.Load().ordered[0]
	if t3.admit == t2.admit {
		t.Fatal("changed budget kept the old admitter")
	}
	if t3.limiter != t2.limiter {
		t.Fatal("unchanged rate rebuilt the limiter")
	}
}
