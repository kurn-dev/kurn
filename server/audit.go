// Mutation audit trail: one structured JSON line per
// acknowledged mutation — tenant, op, list, entry count, the new
// content-addressed version stamp — via stdlib log/slog. The stream is
// append-only through stdout/journald; shipping and retention are the
// deployment's business, and kurnd stores nothing. The query path emits
// nothing (hot-path discipline): queries are metered by counters, not
// logged per request.
package server

import (
	"context"
	"log/slog"
)

// auditMut records one acknowledged mutation. n is the entry count the
// acknowledgment covered; version is the stamp the mutation COMMITTED,
// returned by the store's Versioned mutation variant under the mutation
// lock — never looked up afterwards, when a later concurrent mutation could
// already have installed ITS version and this line would pair operation A
// with version B ("" is the degenerate no-commit case: a zero-entry
// stream). partial marks an NDJSON append that failed mid-stream AFTER
// acknowledging n entries (partial success is still durable, so it must
// still be auditable). Also bumps the tenant's metering counter — the audit
// line and the billing tick cover exactly the same event.
func (s *srv) auditMut(ctx context.Context, op, list string, n int, version string, partial bool) {
	attrs := []slog.Attr{
		slog.String("op", op),
		slog.String("list", list),
		slog.Int("n", n),
		slog.String("version", version),
	}
	if tn := tenantName(ctx); tn != "" {
		attrs = append(attrs, slog.String("tenant", tn))
		s.reg.tenantMutations(tn).Add(1)
	}
	if partial {
		attrs = append(attrs, slog.Bool("partial", true))
	}
	s.audit.LogAttrs(ctx, slog.LevelInfo, "mutation", attrs...)
}
