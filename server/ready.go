// Liveness/readiness. /livez answers "is the process serving"
// — nothing else, so a wedged data state never makes the orchestrator
// restart-loop a node that is otherwise draining traffic correctly. /readyz
// answers "should this node receive traffic": every list dir opened cleanly
// AND every configured golden probe passes through the NORMAL query path.
// The 503 body enumerates each failure — the readiness page is the
// diagnosis, not just a bit.
package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/kurn-dev/kurn/engine"
)

// readyCacheTTL bounds how often a probe storm can re-run golden queries.
// Correctness does not depend on it: the cache is also keyed by every
// list's version, so any mutation invalidates it immediately — the TTL
// only coalesces identical checks between mutations.
const readyCacheTTL = 5 * time.Second

type readyFailure struct {
	Tenant string `json:"tenant,omitempty"` // multi-tenant mode only
	List   string `json:"list"`
	Q      string `json:"q,omitempty"` // set for golden failures; empty for load-state failures
	Reason string `json:"reason"`
}

type readyResult struct {
	Ready    bool           `json:"ready"`
	Failures []readyFailure `json:"failures,omitempty"`
}

// readyKey identifies one list's state at evaluation time. The version
// covers DATA identity (content-addressed), but not config: a PUT-replace
// with different golden probes can reproduce an identical version, so the
// *List pointer (fresh per CreateList) covers config identity.
type readyKey struct {
	l       *engine.List
	version string
}

type readyCache struct {
	keys    map[string]readyKey // list name -> identity at evaluation time
	expires time.Time
	result  readyResult
}

func (s *srv) livez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *srv) readyz(w http.ResponseWriter, r *http.Request) {
	res := s.readyResult()
	status := http.StatusOK
	if !res.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, res)
}

// tenantStores enumerates the units readiness covers: every tenant's
// store in multi-tenant mode, the single store otherwise.
func (s *srv) tenantStores() []struct {
	name string
	st   *engine.Store
} {
	var out []struct {
		name string
		st   *engine.Store
	}
	if reg := s.tenants.Load(); reg != nil {
		for _, t := range reg.ordered {
			out = append(out, struct {
				name string
				st   *engine.Store
			}{t.name, t.st})
		}
		return out
	}
	out = append(out, struct {
		name string
		st   *engine.Store
	}{"", s.st})
	return out
}

func (s *srv) readyResult() readyResult {
	units := s.tenantStores()
	keys := make(map[string]readyKey)
	for _, u := range units {
		for _, l := range u.st.Lists() {
			keys[u.name+"/"+l.Name()] = readyKey{l: l, version: l.Version()}
		}
	}
	if c := s.ready.Load(); c != nil && time.Now().Before(c.expires) && keysMatch(c.keys, keys) {
		return c.result
	}
	var failures []readyFailure
	for _, u := range units {
		res := evaluateReadiness(u.st, u.st.Lists())
		for _, f := range res.Failures {
			f.Tenant = u.name
			failures = append(failures, f)
		}
	}
	res := readyResult{Ready: len(failures) == 0, Failures: failures}
	s.ready.Store(&readyCache{keys: keys, expires: time.Now().Add(readyCacheTTL), result: res})
	return res
}

func keysMatch(cached, current map[string]readyKey) bool {
	if len(cached) != len(current) {
		return false
	}
	for k, v := range current {
		if c, ok := cached[k]; !ok || c != v {
			return false
		}
	}
	return true
}

// evaluateReadiness runs the full check: load state first (a skipped dir or
// quarantined journal means data this node SHOULD serve is not live), then
// every golden probe through the normal query path with list-default
// options — the deployment-shaped proof that data and matching work.
func evaluateReadiness(st *engine.Store, lists []*engine.List) readyResult {
	var failures []readyFailure
	for _, sk := range st.Skipped {
		failures = append(failures, readyFailure{
			List:   sk.List,
			Reason: fmt.Sprintf("list dir skipped at open (%v) — repair with a fresh PUT /v1/lists/%s or remove the directory", sk.Err, sk.List),
		})
	}
	for _, q := range st.Quarantined {
		failures = append(failures, readyFailure{
			List:   q.List,
			Reason: fmt.Sprintf("journal quarantined at open (%v) — journaled operations not live until %s is repaired", q.Err, q.Path),
		})
	}
	for _, l := range lists {
		for _, p := range l.Config().Golden {
			if reason := runGolden(l, p); reason != "" {
				failures = append(failures, readyFailure{List: l.Name(), Q: p.Q, Reason: reason})
			}
		}
	}
	return readyResult{Ready: len(failures) == 0, Failures: failures}
}

// runGolden evaluates one probe; "" means pass.
func runGolden(l *engine.List, p engine.GoldenProbe) string {
	cands := l.Query(p.Q, engine.QueryOpts{})
	if p.Absent {
		if len(cands) > 0 {
			return fmt.Sprintf("expected no candidates, got %d (first: %s)", len(cands), cands[0].EntryID)
		}
		return ""
	}
	for _, c := range cands {
		if c.EntryID != p.ExpectID {
			continue
		}
		if p.MinScore > 0 && c.Score < p.MinScore {
			return fmt.Sprintf("entry %s found but score %v is below min_score %v", p.ExpectID, c.Score, p.MinScore)
		}
		return ""
	}
	return fmt.Sprintf("expected entry %s not among %d candidates", p.ExpectID, len(cands))
}
