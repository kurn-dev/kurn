// Capacity quotas: max_lists at list creation,
// max_total_keys at entry admission. Checks are read-then-act against
// live stats and therefore SOFT under concurrent mutation races — a
// tenant can overshoot by at most one in-flight batch per concurrent
// writer; the control plane reconciles from metering. Violations are 403
// envelopes naming the quota and the numbers, so the caller can see
// exactly how far over they are. Zero-valued quotas and non-tenant
// (legacy) requests are unlimited.
package server

import (
	"context"
	"fmt"

	"github.com/kurn-dev/kurn/engine"
)

// tenantFrom returns the request's resolved tenant, nil in legacy mode.
func tenantFrom(ctx context.Context) *tenant {
	t, _ := ctx.Value(tenantCtxKey{}).(*tenant)
	return t
}

// tenantKeyUsage sums live raw keys across every list of the tenant's store.
func tenantKeyUsage(st *engine.Store) int64 {
	var n int64
	for _, l := range st.Lists() {
		n += l.KeyCount()
	}
	return n
}

// checkListQuota gates CreateList: creating a NEW list (PUT-replace of an
// existing one holds the count steady) must not exceed max_lists.
// Returns "" when admitted.
func checkListQuota(ctx context.Context, name string) string {
	t := tenantFrom(ctx)
	if t == nil || t.spec.Quotas.MaxLists <= 0 {
		return ""
	}
	if _, exists := t.st.List(name); exists {
		return "" // replace: no new list
	}
	if have := len(t.st.Lists()); have >= t.spec.Quotas.MaxLists {
		return fmt.Sprintf("quota max_lists reached: %d of %d lists in use", have, t.spec.Quotas.MaxLists)
	}
	return ""
}

// checkKeyQuota gates an entry batch: the projected live key total after
// applying it must not exceed max_total_keys. For replace the current
// list's keys are credited (the batch supersedes them wholesale); for
// append/upsert the projection is conservative — an upsert superseding an
// existing entry is charged its new keys without crediting the old ones
// until it lands (the credit appears immediately after, so only the
// in-flight projection is pessimistic). Returns "" when admitted.
func checkKeyQuota(ctx context.Context, name string, entries []engine.Entry, replace bool) string {
	t := tenantFrom(ctx)
	if t == nil || t.spec.Quotas.MaxTotalKeys <= 0 {
		return ""
	}
	var batch int64
	for i := range entries {
		batch += int64(len(entries[i].Keys))
	}
	// Everything reads t.st: quotas are tenant-scoped by definition, and a
	// store parameter here would invite usage/credit drifting onto
	// different stores (review finding).
	usage := tenantKeyUsage(t.st)
	projected := usage + batch
	if replace {
		if l, ok := t.st.List(name); ok {
			projected -= l.KeyCount()
		}
	}
	if projected > t.spec.Quotas.MaxTotalKeys {
		return fmt.Sprintf("quota max_total_keys exceeded: usage %d + batch %d would reach %d of %d",
			usage, batch, projected, t.spec.Quotas.MaxTotalKeys)
	}
	return ""
}
