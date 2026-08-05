// Package server exposes an engine.Store over HTTP: list lifecycle, entry
// mutation, and multi-list fuzzy query. Stdlib-only (net/http with Go 1.22
// method+path patterns). All responses are JSON; errors are
// {"error":"message"} with a matching status code; an unknown list is 404
// on every endpoint.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/kurn-dev/kurn/engine"
	"github.com/kurn-dev/kurn/engine/exact"
)

const (
	maxQueryChars = 512      // query q length bound (runes)
	maxBodyBytes  = 32 << 20 // whole-body bound for JSON bodies (NDJSON is per-line bounded instead)
	defaultTopK   = 100      // global post-merge top-K when the request omits it
	maxTopK       = 1000     // largest per-request topk; bounds per-list collection and the merge

	// maxNDJSONLine bounds each NDJSON line; matches the engine's maxLine
	// (journal/base line limit), so a line the server accepts is one the
	// store can persist (modulo the small journal-record envelope).
	maxNDJSONLine = 1 << 20

	// ndjsonBatch is the append-mode NDJSON batch size: entries applied per
	// st.Upsert call, bounding memory on multi-GB streamed loads.
	ndjsonBatch = 10000
)

type srv struct {
	st           *engine.Store // single-tenant store; nil in pure multi-tenant mode
	admit        *admitter     // nil: no query admission bound
	queueTimeout time.Duration // max admission wait beyond the request ctx
	ready        atomic.Pointer[readyCache]
	reg          *metricsRegistry
	audit        *slog.Logger
	auth         *authGate                      // legacy single-tenant keys; nil = open
	tenants      atomic.Pointer[tenantRegistry] // nil = single-tenant mode
}

// stOf routes a request to its store: the resolved tenant's in
// multi-tenant mode, the single store otherwise. The gate middleware
// guarantees a tenant is in ctx for every non-exempt route when a
// registry is installed.
func (s *srv) stOf(ctx context.Context) *engine.Store {
	if t, _ := ctx.Value(tenantCtxKey{}).(*tenant); t != nil {
		return t.st
	}
	return s.st
}

// tenantName returns the resolved tenant's name, "" in single-tenant mode
// (metrics omit the label then).
func tenantName(ctx context.Context) string {
	if t, _ := ctx.Value(tenantCtxKey{}).(*tenant); t != nil {
		return t.name
	}
	return ""
}

// Config tunes the handler. The zero value preserves the unbounded
// behavior (no admission control).
type Config struct {
	// QueryMemBudget bounds the total scratch memory of in-flight queries
	// (see admit.go); <= 0 disables admission control.
	QueryMemBudget int64
	// QueryQueueTimeout bounds how long an over-budget query waits for
	// admission before a 503 (default 2s when a budget is set).
	QueryQueueTimeout time.Duration

	// APIKeys, when non-empty, requires every /v1/* request to present one
	// of these keys (Authorization: Bearer or X-API-Key); probe and metric
	// endpoints stay keyless. Empty keeps the daemon open (the default).
	APIKeys []string

	// AuditHandler receives the mutation audit stream (one slog record per
	// acknowledged mutation). Nil = JSON lines on stdout, keeping the
	// audit stream separable from kurnd's stderr logging.
	AuditHandler slog.Handler
}

// Server is the HTTP front end. Single-tenant: NewServer(st, cfg) and
// serve. Multi-tenant: NewServer(nil, cfg), then SetTenants BEFORE
// serving (and again on each reload).
type Server struct {
	s       *srv
	handler http.Handler
}

func (v *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { v.handler.ServeHTTP(w, r) }

// New returns the HTTP handler for st. It also wires st.OnArtifactError to
// log.Printf: base.idx artifact save failures are non-fatal by design (the
// artifact is a pure cache with a rebuild fallback), so they surface in the
// server log rather than as request errors. Set up before serving; do not
// reassign OnArtifactError afterwards. (Open-time degradations — a
// quarantined journal — arrive on st.Quarantined before New runs; kurnd
// logs those at startup.)
func New(st *engine.Store) http.Handler { return NewWith(st, Config{}) }

// NewWith is New with admission-control configuration.
func NewWith(st *engine.Store, cfg Config) http.Handler { return NewServer(st, cfg) }

// NewServer is NewWith returning the concrete *Server (needed for
// SetTenants). st may be nil ONLY when SetTenants is called before
// serving.
func NewServer(st *engine.Store, cfg Config) *Server {
	if st != nil {
		st.OnArtifactError = func(list string, err error) {
			log.Printf("server: list %s: artifact save failed (non-fatal, will rebuild on next open): %v", list, err)
		}
	}
	qt := cfg.QueryQueueTimeout
	if qt <= 0 {
		qt = 2 * time.Second
	}
	ah := cfg.AuditHandler
	if ah == nil {
		ah = slog.NewJSONHandler(os.Stdout, nil)
	}
	s := &srv{st: st, admit: newAdmitter(cfg.QueryMemBudget), queueTimeout: qt,
		reg: newMetricsRegistry(), audit: slog.New(ah), auth: newAuthGate(cfg.APIKeys)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /livez", s.livez)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /v1/lists", s.listAll)
	mux.HandleFunc("PUT /v1/lists/{list}", s.createList)
	mux.HandleFunc("GET /v1/lists/{list}", s.listStats)
	mux.HandleFunc("POST /v1/lists/{list}/entries", s.upsertEntries)
	mux.HandleFunc("DELETE /v1/lists/{list}/entries/{id}", s.deleteEntry)
	mux.HandleFunc("POST /v1/lists/{list}/compact", s.compact)
	mux.HandleFunc("POST /v1/lists/{list}/reload", s.reload)
	mux.HandleFunc("POST /v1/query", s.query)
	mux.HandleFunc("POST /v1/batch-query", s.batchQuery)
	// The mux's own 404 (unrouted path) and 405 (wrong method) responses are
	// plain text, breaking the all-JSON contract; wrap them into envelopes.
	// The gate wraps OUTSIDE the fallback writer so a 401 is also a JSON
	// envelope, and unrouted paths under auth still answer 401 before 404
	// (no route probing without a key).
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(&jsonFallbackWriter{ResponseWriter: w}, r)
	})
	return &Server{s: s, handler: s.gate(h)}
}

// gate is the request-time auth/tenancy switch. With a tenant registry
// installed, every non-exempt request must present a key that resolves a
// tenant, which rides the context to route stores and label metrics.
// Without one, the legacy single-tenant key gate (if configured) applies.
// Checked per request so a SIGHUP-driven SetTenants applies immediately.
func (s *srv) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reg := s.tenants.Load(); reg != nil {
			if exemptFromAuth(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			t := reg.resolve(presentedKey(r))
			if t == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="kurn"`)
				jsonError(w, http.StatusUnauthorized, "missing or invalid API key")
				return
			}
			// Shared-reads tenants are read-only by construction: every
			// list mutation lives under /v1/lists/ behind a non-GET
			// method (queries are POST /v1/query|batch-query, list READS
			// are GET), so one rule here covers present and future
			// mutation routes.
			if t.shared && r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/lists/") {
				jsonError(w, http.StatusForbidden, "tenant is read-only (shared_reads): this node's lists are platform-published")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantCtxKey{}, t)))
			return
		}
		if s.auth != nil && !exemptFromAuth(r.URL.Path) && !s.auth.allow(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kurn"`)
			jsonError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// jsonFallbackWriter rewrites the mux's default plain-text 404/405 responses
// into the package's JSON error envelope. Handler-written responses are
// untouched: every handler sets Content-Type: application/json BEFORE
// WriteHeader (writeJSON/jsonError), so a 404/405 arriving WITHOUT that
// content type can only be the mux's own. Headers already set by the mux
// (notably Allow on a 405) are preserved.
type jsonFallbackWriter struct {
	http.ResponseWriter
	wroteHeader bool
	swallow     bool // body replaced with the envelope: drop the mux's text
}

func (w *jsonFallbackWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		w.Header().Get("Content-Type") != "application/json" {
		w.swallow = true
		msg := "not found"
		if code == http.StatusMethodNotAllowed {
			msg = "method not allowed"
		}
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(code)
		json.NewEncoder(w.ResponseWriter).Encode(map[string]string{"error": msg})
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *jsonFallbackWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		return len(b), nil // the envelope already went out; drop the mux's text
	}
	return w.ResponseWriter.Write(b)
}

func (s *srv) healthz(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"status": "ok"}
	if s.admit != nil {
		queued, inflight := s.admit.depth()
		resp["query_queue_depth"] = queued
		resp["query_inflight_bytes"] = inflight
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- lists ----

// createList PUT-replaces a list: the Store wipes any prior data files and
// installs a fresh empty list (see Store.CreateList).
func (s *srv) createList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("list")
	var cfg engine.ListConfig
	if !decodeBody(w, r, &cfg) {
		return
	}
	if msg := checkListQuota(r.Context(), name); msg != "" {
		jsonError(w, http.StatusForbidden, msg)
		return
	}
	_, cv, err := s.stOf(r.Context()).CreateListVersioned(name, cfg)
	if err != nil {
		// Caller input (bad name, bad preset/steps, bad match mode) is a
		// typed engine.ConfigError → 400. Everything else CreateList can
		// return is a store IO fault (mkdir/remove/config write) → 500.
		var cfgErr *engine.ConfigError
		if errors.As(err, &cfgErr) {
			jsonError(w, http.StatusBadRequest, err.Error())
		} else {
			jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.reg.creates.Add(1)
	s.auditMut(r.Context(), "create", name, 0, cv, false)
	writeJSON(w, http.StatusOK, map[string]string{"list": name})
}

type statsResp struct {
	Name       string `json:"name"`
	Entries    int    `json:"entries"`
	Overlay    int    `json:"overlay"`
	Tombstones int    `json:"tombstones"`
	Version    string `json:"version"`
	Mode       string `json:"mode"`
	// Build-time loss counters (engine BuildStats): keys the analyzer
	// collapsed to "" and entries left with NO indexable key — the latter
	// count in Entries while being unfindable.
	DroppedKeys    int `json:"dropped_keys"`
	KeylessEntries int `json:"keyless_entries"`
	// Omitted when zero, which is every healthy list: it reports a stale
	// base.idx, not a routine property, and the normal response stays
	// exactly as it was.
	UnindexedEntries int `json:"unindexed_entries,omitempty"`
}

func statsOf(l *engine.List) statsResp {
	entries, overlay, tombstones := l.Stats()
	dropped, keyless := l.BuildStats()
	return statsResp{
		Name:             l.Name(),
		Entries:          entries,
		Overlay:          overlay,
		Tombstones:       tombstones,
		Version:          l.Version(),
		Mode:             l.Config().Match.Mode,
		DroppedKeys:      dropped,
		KeylessEntries:   keyless,
		UnindexedEntries: l.UnindexedEntries(),
	}
}

// mutationResp is the success envelope for entry loads: the applied count
// under verb ("replaced"/"upserted") plus the list's build-loss counters, so
// a load whose keys silently analyzed away is visible in the acknowledgment
// itself, not only in a later stats call.
func mutationResp(l *engine.List, verb string, n int) map[string]int {
	if l == nil {
		// Unreachable in practice (the mutation just succeeded on this list);
		// degrade to the bare count rather than panic on a wild race.
		return map[string]int{verb: n}
	}
	dropped, keyless := l.BuildStats()
	out := map[string]int{verb: n, "dropped_keys": dropped, "keyless_entries": keyless}
	if u := l.UnindexedEntries(); u > 0 {
		out["unindexed_entries"] = u
	}
	return out
}

// mustList re-loads the list after a successful mutation (the mutation
// installed a fresh snapshot; the handler-entry *List predates it).
func mustList(st *engine.Store, name string) *engine.List {
	l, _ := st.List(name)
	return l
}

func (s *srv) listStats(w http.ResponseWriter, r *http.Request) {
	l, ok := s.stOf(r.Context()).List(r.PathValue("list"))
	if !ok {
		jsonError(w, http.StatusNotFound, fmt.Sprintf("unknown list %q", r.PathValue("list")))
		return
	}
	writeJSON(w, http.StatusOK, statsOf(l))
}

func (s *srv) listAll(w http.ResponseWriter, r *http.Request) {
	lists := s.stOf(r.Context()).Lists()
	out := make([]statsResp, 0, len(lists))
	for _, l := range lists {
		out = append(out, statsOf(l))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- entries ----

// upsertEntries mutates a list's entries. Two body formats, branched on
// Content-Type:
//
//   - application/x-ndjson: one entry per line, streamed (see upsertNDJSON).
//   - anything else: a single JSON array of entries, bounded at maxBodyBytes.
//     Lenient default — clients that never set Content-Type keep working; only
//     an explicit NDJSON declaration switches parsers.
//
// ?replace=true swaps the WHOLE list content atomically (st.Replace: new base
// + empty journal; nothing applied on any failure) instead of upserting;
// honored on both body formats so the parameter is never silently ignored.
// The parameter is STRICT: only absent, "true", or "false" are accepted — any
// other value ("TRUE", "1", a typo) is 400 naming the value, because silently
// falling through to append when the client intended a full swap would double
// the list. The unknown-list check runs before any body read on every path.
func (s *srv) upsertEntries(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("list")
	st := s.stOf(r.Context())
	if _, ok := st.List(name); !ok {
		jsonError(w, http.StatusNotFound, fmt.Sprintf("unknown list %q", name))
		return
	}
	replaceParam := r.URL.Query().Get("replace")
	if replaceParam != "" && replaceParam != "true" && replaceParam != "false" {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("invalid replace value %q: must be \"true\" or \"false\"", replaceParam))
		return
	}
	replace := replaceParam == "true"
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-ndjson" {
		s.upsertNDJSON(w, r, name, replace)
		return
	}
	var entries []engine.Entry
	if !decodeBody(w, r, &entries) {
		return
	}
	for i := range entries {
		if entries[i].ID == "" {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("entry %d: empty id", i))
			return
		}
	}
	if msg := checkKeyQuota(r.Context(), name, entries, replace); msg != "" {
		jsonError(w, http.StatusForbidden, msg)
		return
	}
	if replace {
		v, err := st.ReplaceVersioned(name, entries)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		s.reg.replaces.Add(1)
		s.auditMut(r.Context(), "replace", name, len(entries), v, false)
		writeJSON(w, http.StatusOK, mutationResp(mustList(st, name), "replaced", len(entries)))
		return
	}
	v, err := st.UpsertVersioned(name, entries)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	s.reg.upserts.Add(1)
	s.auditMut(r.Context(), "upsert", name, len(entries), v, false)
	writeJSON(w, http.StatusOK, mutationResp(mustList(st, name), "upserted", len(entries)))
}

// upsertNDJSON streams an NDJSON body — one entry per line — into the list.
// The whole-body maxBodyBytes bound does NOT apply (streaming arbitrarily
// large loads is the point); instead each LINE is bounded at maxNDJSONLine,
// matching the engine's journal/base line limit, and blank lines are skipped
// (they still count for line numbering, which is 1-based).
//
// Append mode: entries are applied in batches of ndjsonBatch to bound memory.
// A failure mid-stream (malformed line, empty id, oversize line/record, IO)
// leaves already-applied batches applied — PARTIAL SUCCESS by design, since
// applied batches are journaled and acknowledged — and the pending batch is
// discarded. The error response reports both the failing line and how many
// entries were applied: {"error": "...", "applied": N}. Success:
// {"upserted": N}.
//
// Replace mode: all entries are accumulated and st.Replace runs once at the
// end, so ANY failure means nothing was applied — the list is unchanged.
// (Memory: a full list must fit in memory for its base segment anyway.) An
// EMPTY body is a legitimate "clear the list": {"replaced": 0}. Duplicate IDs
// last-win in both modes, same as the array path.
//
// BULK LOADS should use ?replace=true, not append: append mode rebuilds the
// whole overlay segment on every 10k batch — O(N²/batch) index work over the
// load — and grows the journal by one line per entry, all replayed on the
// next Open. Replace builds the index once and lands as base + empty journal.
// After a large APPEND session, POST /v1/lists/{list}/compact folds the
// overlay into the base and truncates the journal.
func (s *srv) upsertNDJSON(w http.ResponseWriter, r *http.Request, name string, replace bool) {
	st := s.stOf(r.Context())
	var (
		acc     []engine.Entry // replace: everything; append: current batch
		applied int            // append mode: entries durably applied so far
		lastVer string         // version committed by the most recent batch
		lineNo  int
	)
	// Append mode applies in batches, and each batch would otherwise resolve
	// the list by NAME again — so a PUT or reload arriving mid-stream would
	// silently land the remainder of this upload in the new list while the
	// client is told the whole upload succeeded. Pin the generation here and
	// refuse to cross it (409); already-applied batches stay applied and are
	// reported, as with any other mid-stream failure.
	var gen *engine.List
	if !replace {
		gen, _ = st.List(name)
	}
	// fail writes the single error response: replace mode applied nothing, so
	// it's a plain error envelope; append mode reports the partial success.
	fail := func(status int, msg string) {
		if replace {
			jsonError(w, status, msg)
			return
		}
		if applied > 0 {
			// Partial success is durable (journaled batches were
			// acknowledged) — it must reach the audit stream too, stamped
			// with the last batch's committed version.
			s.auditMut(r.Context(), "upsert", name, applied, lastVer, true)
		}
		writeJSON(w, status, map[string]any{"error": msg, "applied": applied})
	}
	// flush applies the pending append-mode batch; on failure it writes the
	// response and returns false.
	flush := func() bool {
		if len(acc) == 0 {
			return true
		}
		if msg := checkKeyQuota(r.Context(), name, acc, false); msg != "" {
			fail(http.StatusForbidden, msg)
			return false
		}
		v, err := st.UpsertGenVersioned(name, gen, acc)
		if err != nil {
			fail(storeErrStatus(err), err.Error())
			return false
		}
		lastVer = v
		applied += len(acc)
		acc = acc[:0]
		return true
	}
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 0, 64*1024), maxNDJSONLine)
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e engine.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			fail(http.StatusBadRequest, fmt.Sprintf("line %d: malformed JSON: %v", lineNo, err))
			return
		}
		if e.ID == "" {
			fail(http.StatusBadRequest, fmt.Sprintf("line %d: empty id", lineNo))
			return
		}
		acc = append(acc, e)
		if !replace && len(acc) >= ndjsonBatch {
			if !flush() {
				return
			}
		}
	}
	if err := sc.Err(); err != nil {
		// The scanner failed on the line AFTER the last one it returned.
		if errors.Is(err, bufio.ErrTooLong) {
			fail(http.StatusBadRequest, fmt.Sprintf("line %d: exceeds %d bytes", lineNo+1, maxNDJSONLine))
			return
		}
		fail(http.StatusBadRequest, fmt.Sprintf("line %d: reading body: %v", lineNo+1, err))
		return
	}
	if replace {
		if msg := checkKeyQuota(r.Context(), name, acc, true); msg != "" {
			fail(http.StatusForbidden, msg)
			return
		}
		v, err := st.ReplaceVersioned(name, acc)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		s.reg.replaces.Add(1)
		s.auditMut(r.Context(), "replace", name, len(acc), v, false)
		writeJSON(w, http.StatusOK, mutationResp(mustList(st, name), "replaced", len(acc)))
		return
	}
	if !flush() {
		return
	}
	// Deliberate asymmetry (review-noted): the global op counter ticks per
	// REQUEST here while NDJSON flushes applied per batch — tenant metering
	// (inside auditMut) counts acknowledged-mutation EVENTS, which is the
	// number the platform bills on; the global family answers ops-shaped
	// questions and need not match.
	s.reg.upserts.Add(1)
	s.auditMut(r.Context(), "upsert", name, applied, lastVer, false)
	writeJSON(w, http.StatusOK, mutationResp(mustList(st, name), "upserted", applied))
}

func (s *srv) deleteEntry(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("list"), r.PathValue("id")
	st := s.stOf(r.Context())
	if _, ok := st.List(name); !ok {
		jsonError(w, http.StatusNotFound, fmt.Sprintf("unknown list %q", name))
		return
	}
	// Deleting an ID that isn't live is a no-op in the engine; the delete is
	// still journaled and acknowledged (idempotent tombstone semantics).
	v, err := st.DeleteVersioned(name, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	s.reg.deletes.Add(1)
	s.auditMut(r.Context(), "delete", name, 1, v, false)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *srv) compact(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("list")
	st := s.stOf(r.Context())
	l, ok := st.List(name)
	if !ok {
		jsonError(w, http.StatusNotFound, fmt.Sprintf("unknown list %q", name))
		return
	}
	v, err := st.CompactVersioned(name)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	s.reg.compacts.Add(1)
	e, _, _ := l.Stats()
	s.auditMut(r.Context(), "compact", name, e, v, false)
	// l's snapshot was swapped in place by Compact: statsOf reads fresh state.
	writeJSON(w, http.StatusOK, statsOf(l))
}

// reload re-opens a list from its on-disk dir — the bundle publish path
// (see docs/platform-contract.md §6: ship files, then call
// this). A failed reload keeps the old list serving and returns the
// reason; success returns fresh stats PLUS the list's golden-probe
// results, so the publisher sees the gate outcome synchronously instead
// of polling /readyz. Reload is deliberately outside capacity quotas:
// bundles arrive via the platform's build pipeline, which owns sizing.
func (s *srv) reload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("list")
	st := s.stOf(r.Context())
	l, rv, err := st.ReloadListVersioned(name)
	if err != nil {
		var cfgErr *engine.ConfigError
		if errors.As(err, &cfgErr) {
			jsonError(w, http.StatusBadRequest, err.Error())
		} else {
			jsonError(w, http.StatusConflict, fmt.Sprintf("reload failed, previous content still serving: %v", err))
		}
		return
	}
	s.ready.Store(nil) // readiness must re-evaluate the swapped list
	s.reg.replaces.Add(1)
	s.auditMut(r.Context(), "reload", name, func() int { e, _, _ := l.Stats(); return e }(), rv, false)

	type goldenResult struct {
		Q      string `json:"q"`
		OK     bool   `json:"ok"`
		Reason string `json:"reason,omitempty"`
	}
	var goldens []goldenResult
	for _, p := range l.Config().Golden {
		reason := runGolden(l, p)
		goldens = append(goldens, goldenResult{Q: p.Q, OK: reason == "", Reason: reason})
	}
	writeJSON(w, http.StatusOK, struct {
		statsResp
		Golden []goldenResult `json:"golden,omitempty"`
	}{statsOf(l), goldens})
}

// ---- query ----

// queryReq's Threshold/TopK are pointers so "absent" (use list defaults) and
// "explicitly zero" are distinct: threshold 0 means no score floor, while an
// absent threshold means the list default. topk must be 1..maxTopK when
// present — "unlimited" is not offered over HTTP (the global merge is always
// cut, so an uncapped per-list collection would only buy allocation).
type queryReq struct {
	Q         string   `json:"q"`
	Lists     []string `json:"lists"`
	Threshold *float64 `json:"threshold,omitempty"` // absent = per-list default; else 0..1
	TopK      *int     `json:"topk,omitempty"`      // absent = per-list default + global 100; else 1..maxTopK
}

type respCand struct {
	List    string          `json:"list"`
	EntryID string          `json:"entry_id"`
	Score   float64         `json:"score"`
	Key     string          `json:"key"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type queryResp struct {
	Candidates []respCand        `json:"candidates"` // always present; [] when empty
	Versions   map[string]string `json:"versions"`
	TookUs     int64             `json:"took_us"`
}

// query runs q against every named list and merges the results (see
// runQuery for the semantics).
func (s *srv) query(w http.ResponseWriter, r *http.Request) {
	var req queryReq
	if !decodeBody(w, r, &req) {
		return
	}
	if ra, msg := s.rateCheck(r.Context()); msg != "" {
		w.Header().Set("Retry-After", strconv.Itoa(ra))
		jsonError(w, http.StatusTooManyRequests, msg)
		return
	}
	resp, errStatus, errMsg := s.runQuery(r.Context(), req)
	if errStatus != 0 {
		jsonError(w, errStatus, errMsg)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// runQuery validates req, runs q against every named list, and merges the
// results. ctx (the request context) propagates into the engine scan loops:
// a client disconnect stops a long ngram scan mid-flight (the canceled
// query yields empty candidates — irrelevant, nobody is listening).
// It never touches a ResponseWriter so both the single-query and
// batch-query handlers can share it: on failure errStatus is a non-zero HTTP
// status with errMsg the error text (the single handler turns that into an
// error envelope with that status; the batch handler inlines it per check).
// On success errStatus is 0 and resp is the full envelope.
//
// SCORE SEMANTICS: Score is IDF-weighted coverage of the query's grams that
// are KNOWN to the index — grams absent from the indexed corpus are excluded
// from the denominator. On tiny corpora this inflates scores for queries
// containing unknown tokens (the unknown part simply doesn't count against
// the match). This is the documented, reference-faithful behavior, not a bug.
//
// Deliberate deferral: there is no query-concurrency semaphore yet — each
// concurrent ngram Lookup on a large list allocates ~4B×numOrds of scratch,
// so N simultaneous queries cost N such buffers. Revisit (bounded worker pool
// or scratch reuse) before sustained high-TPS production use.
func (s *srv) runQuery(ctx context.Context, req queryReq) (resp queryResp, errStatus int, errMsg string) {
	if req.Q == "" {
		return queryResp{}, http.StatusBadRequest, "q must be non-empty"
	}
	if utf8.RuneCountInString(req.Q) > maxQueryChars {
		return queryResp{}, http.StatusBadRequest, fmt.Sprintf("q exceeds %d chars", maxQueryChars)
	}
	if len(req.Lists) == 0 {
		return queryResp{}, http.StatusBadRequest, "lists must be non-empty"
	}
	if req.Threshold != nil && (*req.Threshold < 0 || *req.Threshold > 1) {
		return queryResp{}, http.StatusBadRequest, fmt.Sprintf("threshold %v out of range [0, 1]", *req.Threshold)
	}
	if req.TopK != nil && (*req.TopK < 1 || *req.TopK > maxTopK) {
		return queryResp{}, http.StatusBadRequest, fmt.Sprintf("topk %d out of range [1, %d]", *req.TopK, maxTopK)
	}
	// Dedupe list names (order-preserving): a duplicated name would run the
	// same query twice and duplicate every candidate in the merged output.
	names := make([]string, 0, len(req.Lists))
	seen := make(map[string]struct{}, len(req.Lists))
	for _, name := range req.Lists {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	// Resolve ALL lists before running any query: a partially-run query
	// against a request naming an unknown list would waste work and blur the
	// error contract (404 names the missing list).
	lists := make([]*engine.List, len(names))
	for i, name := range names {
		l, ok := s.stOf(ctx).List(name)
		if !ok {
			return queryResp{}, http.StatusNotFound, fmt.Sprintf("unknown list %q", name)
		}
		lists[i] = l
	}

	// effK is the global merge cut — it also bounds each list's collection.
	effK := defaultTopK
	if req.TopK != nil {
		effK = *req.TopK
	}
	// Per-list effective engine top-K, resolved BEFORE admission so the
	// charged cost covers the collection bound each list actually runs
	// with. A list default of zero (exact-mode unlimited) or LARGER than
	// the global cut is clamped to effK: the merge cut makes anything
	// beyond effK per list unreachable (a list contributes at most K to
	// the merged top-K), so collecting more — a list config may declare a
	// default up to 2^20 — would build and copy candidates only to throw
	// them away, far past the response the client can receive. Smaller
	// list defaults are preserved.
	listK := make([]int, len(lists))
	for i, l := range lists {
		k := l.Config().Match.TopK
		if req.TopK != nil {
			k = *req.TopK
		} else if k == 0 || k > effK {
			k = effK
		}
		listK[i] = k
	}

	// Admission: charge the query its scratch cost across every ngram list
	// it touches (exact lists cost 0 and skip the gate entirely), at the
	// top-K bound each list will run with. The wait is bounded by the
	// request context AND the queue timeout; exhaustion is a 503 —
	// backpressure, not OOM.
	var cost int64
	for i, l := range lists {
		cost += l.ScratchBytesFor(listK[i])
	}
	// Two-level admission: the TENANT's slice first, then the global
	// budget — a tenant's scan storm queues against its own budget before
	// it can contend for anyone else's. One queueTimeout bounds the
	// combined wait; deferred releases unwind global-then-tenant.
	actx, acancel := context.WithTimeout(ctx, s.queueTimeout)
	defer acancel()
	if t := tenantFrom(ctx); t != nil && t.admit != nil {
		trelease, terr := t.admit.acquire(actx, cost)
		if terr != nil {
			queued, inflight := t.admit.depth()
			return queryResp{}, http.StatusServiceUnavailable,
				fmt.Sprintf("query admission timed out: tenant scratch budget exhausted (%d queued, %d bytes in flight)", queued, inflight)
		}
		defer trelease()
	}
	release, aerr := s.admit.acquire(actx, cost)
	if aerr != nil {
		queued, inflight := s.admit.depth()
		return queryResp{}, http.StatusServiceUnavailable,
			fmt.Sprintf("query admission timed out: scratch budget exhausted (%d queued, %d bytes in flight)", queued, inflight)
	}
	defer release()

	start := time.Now()
	out := make([]respCand, 0)
	versions := make(map[string]string, len(lists))
	// Translate the HTTP absent/explicit-zero distinction into the engine's
	// zero-default/negative-sentinel convention (see engine.QueryOpts).
	var opts engine.QueryOpts
	if req.Threshold != nil {
		if *req.Threshold == 0 {
			opts.Threshold = -1 // explicit no-floor
		} else {
			opts.Threshold = *req.Threshold
		}
	}
	for i, l := range lists {
		name := names[i]
		lopts := opts
		lopts.TopK = listK[i] // the bound admission charged for (always > 0)
		lstart := time.Now()
		// Candidates and version from ONE snapshot: reading l.Version()
		// separately here let a mutation that landed mid-query stamp this
		// answer with data it never saw.
		cands, ver := l.QueryVersioned(ctx, req.Q, lopts)
		s.reg.observeQuery(tenantName(ctx), name, time.Since(lstart), len(cands) > 0)
		for _, c := range cands {
			out = append(out, respCand{
				List:    name,
				EntryID: c.EntryID,
				Score:   c.Score,
				Key:     c.Key,
				Payload: c.Payload,
			})
		}
		versions[name] = ver
	}
	// Merge: score desc, entry_id asc; stable so full ties keep request
	// list order.
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].EntryID < out[b].EntryID
	})
	// GLOBAL top-K across all lists (each list already cut to its own K).
	if len(out) > effK {
		out = out[:effK]
	}
	return queryResp{
		Candidates: out,
		Versions:   versions,
		TookUs:     time.Since(start).Microseconds(),
	}, 0, ""
}

// maxBatchChecks bounds a batch-query request: enough for the multi-value
// check pattern (email + IP + device + ... per event), small enough that
// one request can't monopolize the server.
const maxBatchChecks = 100

type batchReq struct {
	Checks []queryReq `json:"checks"`
}

type batchResp struct {
	// Results[i] answers Checks[i]: either a full single-query envelope or
	// {"error": "..."} — inline per-check errors, so one bad check never
	// fails the batch. len(Results) always equals len(Checks).
	Results []any `json:"results"`
	TookUs  int64 `json:"took_us"`
}

// batchQuery runs 1..maxBatchChecks independent query checks in one round
// trip. Each check gets the exact validation and semantics of /v1/query (via
// runQuery); only batch-level problems — malformed JSON, an oversize body,
// an out-of-bounds check count — fail the whole request.
func (s *srv) batchQuery(w http.ResponseWriter, r *http.Request) {
	var req batchReq
	if !decodeBody(w, r, &req) {
		return
	}
	if n := len(req.Checks); n < 1 || n > maxBatchChecks {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("checks must contain 1..%d items, got %d", maxBatchChecks, n))
		return
	}
	start := time.Now()
	results := make([]any, len(req.Checks))
	for i, check := range req.Checks {
		if r.Context().Err() != nil {
			return // client gone: stop burning scan time on unread answers
		}
		// Each check spends one rate token (a 100-check batch is 100
		// queries); over-rate checks fail inline, the batch continues.
		if _, msg := s.rateCheck(r.Context()); msg != "" {
			results[i] = map[string]string{"error": msg}
			continue
		}
		resp, errStatus, errMsg := s.runQuery(r.Context(), check)
		if errStatus != 0 {
			results[i] = map[string]string{"error": errMsg}
			continue
		}
		results[i] = resp
	}
	writeJSON(w, http.StatusOK, batchResp{
		Results: results,
		TookUs:  time.Since(start).Microseconds(),
	})
}

// ---- helpers ----

// storeErrStatus classifies a Store mutation error: caller data problems
// rejected by the Store before anything touches disk are 400 — an oversize
// entry (engine.EntryTooLargeError) or an unbuildable index
// (exact.KeyOverflowError, e.g. a million entries sharing one analyzed key).
// Anything else is an IO failure, so 500.
func storeErrStatus(err error) int {
	var tooLarge *engine.EntryTooLargeError
	if errors.As(err, &tooLarge) {
		return http.StatusBadRequest
	}
	var replaced *engine.ListReplacedError
	if errors.As(err, &replaced) {
		// Not the client's fault and not retryable against this generation:
		// the list they addressed no longer exists in the form they addressed.
		return http.StatusConflict
	}
	var overflow *exact.KeyOverflowError
	if errors.As(err, &overflow) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// mapStoreError writes the error envelope for a failed Store mutation with
// the status from storeErrStatus.
func mapStoreError(w http.ResponseWriter, err error) {
	jsonError(w, storeErrStatus(err), err.Error())
}

// decodeBody decodes the request body as JSON into v, requiring exactly one
// value. Every JSON body is bounded at maxBodyBytes (NDJSON bodies bypass
// this — they are per-line bounded in upsertNDJSON instead). On failure it
// writes a 400 (or 413 when the size bound tripped) and returns false.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", mbe.Limit))
			return false
		}
		jsonError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		jsonError(w, http.StatusBadRequest, "malformed JSON body: trailing data")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("server: writing response: %v", err)
	}
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
