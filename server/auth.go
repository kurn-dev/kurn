// Optional API-key auth. Off unless keys are configured —
// self-hosters keep today's open daemon. When on, every /v1/* route
// requires a key; the probe/metric endpoints stay keyless (the LB and
// Prometheus must reach them, and they expose no list data). TLS is the
// edge's job; the keys here are bearer credentials, so run kurnd behind
// TLS termination when auth matters. Rotation = restart (documented).
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// authGate holds the configured key digests. Presented keys are compared
// as sha256 digests: constant-time over fixed-length arrays, no key-length
// leak, and the plaintext keys don't linger in memory.
type authGate struct {
	digests [][sha256.Size]byte
}

func newAuthGate(keys []string) *authGate {
	if len(keys) == 0 {
		return nil
	}
	g := &authGate{}
	for _, k := range keys {
		g.digests = append(g.digests, sha256.Sum256([]byte(k)))
	}
	return g
}

// exemptFromAuth lists the endpoints that must stay reachable without a
// key: liveness/readiness for the load balancer, metrics for the scraper,
// healthz for compatibility. None expose list contents.
func exemptFromAuth(path string) bool {
	switch path {
	case "/healthz", "/livez", "/readyz", "/metrics":
		return true
	}
	return false
}

// presentedKey extracts the API key from either accepted header form
// (Authorization: Bearer, or X-API-Key), "" when absent.
func presentedKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// allow reports whether r carries a configured key. A nil gate allows
// everything.
func (g *authGate) allow(r *http.Request) bool {
	if g == nil {
		return true
	}
	presented := presentedKey(r)
	if presented == "" {
		return false
	}
	d := sha256.Sum256([]byte(presented))
	ok := 0
	for i := range g.digests {
		// No early exit: every configured key is compared so timing does
		// not reveal which (if any) digest matched.
		ok |= subtle.ConstantTimeCompare(d[:], g.digests[i][:])
	}
	return ok == 1
}
