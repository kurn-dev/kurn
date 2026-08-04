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

	"github.com/kurn-dev/kurn/engine"
)

// auditMut records one acknowledged mutation. n is the entry count the
// acknowledgment covered; partial marks an NDJSON append that failed
// mid-stream AFTER acknowledging n entries (partial success is still
// durable, so it must still be auditable). Also bumps the tenant's
// metering counter — the audit line and the billing tick cover exactly
// the same event.
func (s *srv) auditMut(ctx context.Context, st *engine.Store, op, list string, n int, partial bool) {
	version := ""
	if l, ok := st.List(list); ok {
		version = l.Version()
	}
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
