// Multi-tenancy. Tenancy is KEY-SCOPED: the tenant is
// resolved from the presented API key and routes to that tenant's own
// engine.Store — /v1/* paths are unchanged, a tenant cannot even name
// another tenant's namespace, and single-tenant deployments (no registry)
// keep today's exact behavior. The registry carries sha256 key DIGESTS,
// never plaintext: a leaked tenants file exposes no usable credential.
// The registry swaps atomically (SetTenants), which is what makes SIGHUP
// reload safe: in-flight requests finish on the registry they resolved
// against; a rejected reload never half-applies.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"

	"golang.org/x/time/rate"

	"github.com/kurn-dev/kurn/engine"
)

// TenantQuotas are the per-tenant limits (parsed and carried at T1;
// enforcement lands in T2/T3). Zero values mean unlimited.
type TenantQuotas struct {
	MaxLists        int     `json:"max_lists,omitempty"`
	MaxTotalKeys    int64   `json:"max_total_keys,omitempty"`
	ScratchBudgetMB int64   `json:"scratch_budget_mb,omitempty"`
	RatePerSec      float64 `json:"rate_per_sec,omitempty"`
}

// TenantSpec is one tenant's declaration in the tenants file.
type TenantSpec struct {
	KeyDigests []string     `json:"key_digests"`
	Quotas     TenantQuotas `json:"quotas,omitempty"`
	// SharedReads routes this tenant's READS to the node's shared store
	// (the one passed to NewServer) instead of a private one, and refuses
	// every list mutation with 403 — the free-tier shape: per-tenant
	// keys, rate limits, and metering over platform-published public
	// lists. A shared-reads tenant declares no store of its own.
	SharedReads bool `json:"shared_reads,omitempty"`
}

// tenantNameRE mirrors the engine's list-name rule: tenant names become
// directory names (<data>/tenants/{id}).
var tenantNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ParseTenants decodes and validates a tenants file. Validation here is
// per-tenant shape; cross-tenant digest uniqueness is checked at
// SetTenants (where the routing map is built).
func ParseTenants(data []byte) (map[string]TenantSpec, error) {
	var m map[string]TenantSpec
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("tenants: no tenants declared")
	}
	for name, spec := range m {
		if !tenantNameRE.MatchString(name) {
			return nil, fmt.Errorf("tenants: invalid tenant name %q", name)
		}
		if len(spec.KeyDigests) == 0 {
			return nil, fmt.Errorf("tenants: %s: no key_digests (a tenant without keys is unreachable)", name)
		}
		for _, d := range spec.KeyDigests {
			if len(d) != 2*sha256.Size {
				return nil, fmt.Errorf("tenants: %s: key digest %q is not sha256 hex (%d chars, want %d)", name, d, len(d), 2*sha256.Size)
			}
			if _, err := hex.DecodeString(d); err != nil {
				return nil, fmt.Errorf("tenants: %s: key digest %q: %w", name, d, err)
			}
		}
		q := spec.Quotas
		if q.MaxLists < 0 || q.MaxTotalKeys < 0 || q.ScratchBudgetMB < 0 || q.RatePerSec < 0 {
			return nil, fmt.Errorf("tenants: %s: negative quota", name)
		}
	}
	return m, nil
}

// TenantRuntime pairs a tenant's spec with its opened store. The caller
// (kurnd) owns opening stores — reusing existing ones across reloads —
// because only it knows the data directory layout.
type TenantRuntime struct {
	Spec  TenantSpec
	Store *engine.Store
}

// tenant is the resolved runtime object requests carry in their context.
type tenant struct {
	name   string
	st     *engine.Store
	shared bool // reads route to the node's shared store; mutations 403
	spec   TenantSpec

	// admit is the tenant's scratch-budget slice (nil = no tenant bound):
	// acquired BEFORE the global admitter, so one tenant's scan storm
	// queues against its own budget before it can touch anyone else's.
	admit *admitter
	// limiter is the tenant's query token bucket (nil = unlimited).
	limiter *rate.Limiter
}

// tenantRegistry is the immutable resolution state; swapped whole.
type tenantRegistry struct {
	byDigest map[[sha256.Size]byte]*tenant
	ordered  []*tenant // name-sorted, for readiness/metrics iteration
}

// SetTenants installs (or replaces) the tenant registry atomically.
// Requests already in flight finish against the registry they resolved
// on; new requests see the new one. Each store's OnArtifactError is wired
// with the tenant name. An error leaves the previous registry serving.
func (v *Server) SetTenants(ts map[string]TenantRuntime) error {
	if len(ts) == 0 {
		return fmt.Errorf("tenants: refusing to install an empty registry (single-tenant mode is New/NewWith without SetTenants)")
	}
	reg := &tenantRegistry{byDigest: map[[sha256.Size]byte]*tenant{}}
	// Previous registry, for governor-state carryover: a reload that leaves
	// a tenant's budget/rate unchanged keeps its live admitter and limiter —
	// otherwise queries in flight on the old admitter would release into an
	// object nothing consults, transiently over-admitting the tenant, and
	// every reload would refill the token bucket.
	prevByName := map[string]*tenant{}
	if prev := v.s.tenants.Load(); prev != nil {
		for _, t := range prev.ordered {
			prevByName[t.name] = t
		}
	}
	names := make([]string, 0, len(ts))
	for name := range ts {
		names = append(names, name)
	}
	sort.Strings(names)
	// Two PRIVATE tenants pointing at one store share a namespace silently:
	// each would see the other's lists under its own name, read them, and
	// overwrite them, with isolation reported as intact everywhere. kurnd
	// cannot produce it (one Open per tenant directory), but the library API
	// takes the stores from its caller and had no opinion about it.
	// shared_reads is the deliberate version of the same aliasing and is
	// assigned below, so only stores passed IN are checked here.
	byStore := make(map[*engine.Store]string, len(ts))
	for _, name := range names {
		rt := ts[name]
		if rt.Store == nil {
			continue
		}
		if other, dup := byStore[rt.Store]; dup {
			return fmt.Errorf("tenants: %s and %s declare the same store — their lists would silently share one namespace", other, name)
		}
		byStore[rt.Store] = name
	}
	for _, name := range names {
		rt := ts[name]
		if rt.Spec.SharedReads {
			// Shared-reads tenants use the node's own store; a private
			// store alongside would be unreachable dead weight, so it is
			// refused rather than ignored.
			if rt.Store != nil {
				return fmt.Errorf("tenants: %s: shared_reads tenants must not declare a store", name)
			}
			if v.s.st == nil {
				return fmt.Errorf("tenants: %s: shared_reads requires the node's shared store (NewServer with a non-nil store)", name)
			}
			rt.Store = v.s.st
		}
		if rt.Store == nil {
			return fmt.Errorf("tenants: %s: nil store", name)
		}
		if !tenantNameRE.MatchString(name) {
			return fmt.Errorf("tenants: invalid tenant name %q", name)
		}
		if len(rt.Spec.KeyDigests) == 0 {
			return fmt.Errorf("tenants: %s: no key_digests", name)
		}
		t := &tenant{name: name, st: rt.Store, shared: rt.Spec.SharedReads, spec: rt.Spec}
		q := rt.Spec.Quotas
		prev := prevByName[name]
		if prev != nil && prev.spec.Quotas.ScratchBudgetMB == q.ScratchBudgetMB {
			t.admit = prev.admit
		} else if q.ScratchBudgetMB > 0 {
			t.admit = newAdmitter(q.ScratchBudgetMB << 20)
		}
		if prev != nil && prev.spec.Quotas.RatePerSec == q.RatePerSec {
			t.limiter = prev.limiter
		} else if q.RatePerSec > 0 {
			burst := int(2 * q.RatePerSec)
			if burst < 1 {
				burst = 1
			}
			t.limiter = rate.NewLimiter(rate.Limit(q.RatePerSec), burst)
		}
		for _, dh := range rt.Spec.KeyDigests {
			raw, err := hex.DecodeString(dh)
			if err != nil || len(raw) != sha256.Size {
				return fmt.Errorf("tenants: %s: key digest %q is not sha256 hex", name, dh)
			}
			var d [sha256.Size]byte
			copy(d[:], raw)
			if prev, dup := reg.byDigest[d]; dup {
				return fmt.Errorf("tenants: key digest shared by %s and %s — a key must resolve exactly one tenant", prev.name, name)
			}
			reg.byDigest[d] = t
		}
		reg.ordered = append(reg.ordered, t)
		// Wire the artifact hook only on stores that lack one: a NEW store
		// is unpublished until the registry swap below (write happens-before
		// every read), and a carried store already has its hook — assigning
		// per reload would race saveArtifact on the serving path.
		if rt.Store.OnArtifactError == nil {
			tn := name
			rt.Store.OnArtifactError = func(list string, err error) {
				log.Printf("server: tenant %s: list %s: artifact save failed (non-fatal, will rebuild on next open): %v", tn, list, err)
			}
		}
	}
	v.s.tenants.Store(reg)
	v.s.ready.Store(nil) // readiness must re-evaluate against the new tenant set
	return nil
}

// resolve maps a presented key to its tenant, nil when unknown. Lookup is
// by sha256 digest: the map key is the full 32-byte digest of an
// attacker-chosen input, so equality short-circuits leak nothing usable
// (steering the digest requires a preimage).
func (reg *tenantRegistry) resolve(presented string) *tenant {
	if presented == "" {
		return nil
	}
	return reg.byDigest[sha256.Sum256([]byte(presented))]
}

// tenantCtxKey carries the resolved *tenant in the request context.
type tenantCtxKey struct{}
