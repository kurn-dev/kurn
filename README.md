# kurn

**Fuzzy lookup, next to your code.**

kurn is an in-memory engine for matching short strings — product names,
customers, domains, denylists, any list you have. Typo-tolerant
(character-ngram, IDF-weighted, scored 0–100) or exact, it runs as a Go
library inside your service or as a sidecar (`kurnd`) beside it, so there is
no search cluster to maintain. Every answer carries the content-addressed
version of the data it was computed against: same query, same version, same
result — byte for byte, on any machine, forever.

A common shape: your database stays the system of record, a query exports the
rows worth matching, `kurn build` turns them into a content-addressed bundle,
and every instance answers lookups from its own copy. The database then sees
one query per refresh instead of one per lookup — independent of how much
traffic you serve or how many instances you run.

kurn is built on measured algorithm choices: the character 2+3-gram
roaring-bitmap inverted index, the IDF-scored pigeonhole candidate scan,
and build/query analyzer symmetry, implemented domain-blind and driven
entirely by per-list config. Measured results and methodology are in
[bench/README.md](bench/README.md).

## Quickstart

Build and run the daemon (Go 1.26+):

```sh
go build -o kurnd ./cmd/kurnd
./kurnd -data ./data     # creates the data dir if missing; listens on :8080
```

Create a fuzzy person-name list, add entries, query with a typo'd,
reordered name:

```sh
curl -s -X PUT localhost:8080/v1/lists/people \
  -d '{"analyzer":{"preset":"person-name"},"match":{"mode":"ngram"}}'
# {"list":"people"}

curl -s -X POST localhost:8080/v1/lists/people/entries \
  -d '[{"id":"p1","keys":["Elena Vasquez","Vasquez, Elena M."],"payload":{"listed":"2024-01-01"}},
       {"id":"p2","keys":["Marcus Chen"]}]'
# {"dropped_keys":0,"keyless_entries":0,"upserted":2}

curl -s -X POST localhost:8080/v1/query -d '{"q":"vasquez elna","lists":["people"]}'
# {"candidates":[{"list":"people","entry_id":"p1","score":100,"key":"Elena Vasquez",
#   "payload":{"listed":"2024-01-01"}}],"versions":{"people":"empty@0+j178"},"took_us":44}
```

Note: the minimal `{"mode":"ngram"}` above uses the defaults only. The
benchmarked configuration additionally set `"strip_spaces":true` — recommended
for fuzzy name lists, since it makes matching robust to token fusion and
splits (see Configuration below).

Bulk-load via NDJSON (one entry per line) with `?replace=true`, which swaps
the whole list content atomically, then compact:

```sh
cat > people.ndjson <<'EOF'
{"id":"p1","keys":["Elena Vasquez","Vasquez, Elena M."],"payload":{"listed":"2024-01-01"}}
{"id":"p2","keys":["Marcus Chen"]}
{"id":"p3","keys":["Ingrid Åström"]}
EOF
curl -s -X POST 'localhost:8080/v1/lists/people/entries?replace=true' \
  -H 'Content-Type: application/x-ndjson' --data-binary @people.ndjson
# {"dropped_keys":0,"keyless_entries":0,"replaced":3}

curl -s -X POST localhost:8080/v1/lists/people/compact
# {"name":"people","entries":3,"overlay":0,"tombstones":0,"version":"ca764d77fea0@3+j0",
#  "mode":"ngram","dropped_keys":0,"keyless_entries":0}

curl -s -X POST localhost:8080/v1/query -d '{"q":"astrom ingrid","lists":["people"]}'
# {"candidates":[{"list":"people","entry_id":"p3","score":100,"key":"Ingrid Åström"}],
#  "versions":{"people":"ca764d77fea0@3+j0"},"took_us":43}
```

Domain blocklist (exact mode, parent-suffix fallback): listing a domain
covers all its subdomains — 100 = exact hit, 90 = parent match:

```sh
curl -s -X PUT localhost:8080/v1/lists/blocked-domains \
  -d '{"analyzer":{"preset":"domain"},"match":{"mode":"exact","fallback":"parent_domain"}}'
printf '{"id":"d1","keys":["tempmail.com"]}\n' | \
  curl -s -X POST 'localhost:8080/v1/lists/blocked-domains/entries?replace=true' \
    -H 'Content-Type: application/x-ndjson' --data-binary @-
curl -s -X POST localhost:8080/v1/query -d '{"q":"smtp.tempmail.com","lists":["blocked-domains"]}'
# {"candidates":[{"list":"blocked-domains","entry_id":"d1","score":90,"key":"tempmail.com"}],...}
# querying "tempmail.com" itself scores 100
```

Everything persists in the data directory (per-list base snapshot +
append-only journal); restart the daemon and the lists come back.

## Configuration

A list is declared once at `PUT /v1/lists/{name}` with an analyzer and a
match config. Index-time fields are frozen at creation; to change them,
recreate the list.

### Analyzer

One normalization pipeline applied identically at index and query time.
Either a `preset` or an explicit ordered `steps` list:

| step | effect |
|---|---|
| `lowercase` | Unicode lowercasing |
| `fold_diacritics` | strip combining marks: `Zürich` → `Zurich` (see Limitations) |
| `strip_punctuation` | drop punctuation, keep word-inner hyphens: `O'Neil, Vega-Ruiz` → `ONeil Vega-Ruiz` |
| `strip_words:<w1,w2,…>` | drop the listed whole tokens (post-lowercase titles etc.) |
| `sort_tokens` | sort tokens, making matching token-order-blind |
| `trim` | collapse runs of whitespace |
| `idna` | domain normalization: strip trailing dots, IDN → punycode (`münchen.example` → `xn--mnchen-3ya.example`) via `x/net/idna`; lenient mapping (blocklists contain technically-invalid real-world domains); unmappable input degrades to the dot-stripped string, never errors |

Presets are exactly these step lists:

| preset | steps |
|---|---|
| `person-name` | `lowercase, fold_diacritics, strip_punctuation, strip_words:mr,mrs,ms,dr,prof, sort_tokens` |
| `free-text` | `lowercase, fold_diacritics, strip_punctuation` |
| `identifier` | `lowercase, trim` |
| `domain` | `lowercase, trim, idna` |

### Match

| field | when | meaning |
|---|---|---|
| `mode` | index-time | `"ngram"` (fuzzy) or `"exact"` (normalized whole-key membership) |
| `fallback` | query-time | `"parent_domain"` (exact mode only): when the full analyzed query misses, probe parent suffixes one label at a time (never bare TLDs), stopping at the first level with hits; exact hits score 100, parent-suffix hits score 90 |
| `grams` | index-time | ngram sizes, default `[2,3]` |
| `strip_spaces` | index-time | remove spaces before gramming (recommended for names; makes matching robust to token fusion/splits) |
| `threshold` | query-time default | minimum score fraction, overridable per query. Defaults to `0.6` in ngram mode; exact-mode lists get NO default (0 = return all match levels), and threshold > 0.9 suppresses parent-suffix matches (score 90) |
| `topk` | query-time default | per-list candidate cap, overridable per query. Defaults to `100` in ngram mode; exact-mode lists get NO default (0 = unlimited). The server's global post-merge top-K of 100 still applies when the query omits `topk` |

### List-level options

| field | when | meaning |
|---|---|---|
| `golden` | readiness | optional probe set: `[{"q","expect_id","min_score"} \| {"q","absent":true}]`. `/readyz` runs each through the normal query path — `expect_id` must appear among the candidates (with score ≥ `min_score` when set), `absent` must return none. A failing golden takes the node out of rotation with the reason in the body |
| `overlay_auto_compact` | mutation-time | when > 0, the daemon folds the overlay into a new base in the background once it reaches this many entries (0 = disabled, the default). Append-mode mutations rebuild the whole overlay each time, so an ungoverned overlay is the first wall a write-heavy list hits. Caveat: compaction recomputes segment-local IDF over the folded corpus, so ngram scores can shift slightly at each trigger (see score semantics) |

**Score semantics** (`score` is 0–100): IDF-weighted coverage of the query's
grams *that are known to the index* — grams absent from the indexed corpus
are excluded from the denominator. This is the reference-faithful behavior:
on large corpora almost every gram is known and the score behaves as
expected, but on tiny corpora it inflates (an unknown query token simply
doesn't count against the match — note the `score:100` on the typo'd
quickstart query over a 2-entry list).

## HTTP API

| method & path | description | notable semantics |
|---|---|---|
| `GET /healthz` | health + admission metrics | `query_queue_depth` / `query_inflight_bytes` when admission control is on |
| `GET /livez` | liveness | 200 once the process serves — never depends on data state |
| `GET /readyz` | readiness | 200 iff every list dir opened cleanly AND every configured golden probe passes; 503 body enumerates each failure (list, probe, reason). Cached ~5s, invalidated instantly by any mutation |
| `GET /metrics` | Prometheus text exposition | per-list query counters, hit rate, latency histogram (100µs–10s log buckets), entry/overlay/tombstone/build-loss gauges, mutation counters, admission queue depth |
| `GET /v1/lists` | stats for all lists | — |
| `PUT /v1/lists/{list}` | create a list with config | replaces any existing list of that name, wiping its data |
| `GET /v1/lists/{list}` | one list's stats | entries/overlay/tombstones/version/mode |
| `POST /v1/lists/{list}/entries` | upsert entries | body is a JSON array, or NDJSON when `Content-Type: application/x-ndjson` (streamed, 1 MiB per-line bound, no whole-body bound). `?replace=true` atomically swaps the whole list content instead of upserting; the parameter is strict — only `true`/`false`, anything else is a 400. **Bulk loads should use replace**: append mode rebuilds the overlay per batch and journals every entry; NDJSON append failures mid-stream are partial (response reports `applied`). An append stream is pinned to the list generation it started on: if a `PUT`/reload replaces the list mid-upload, the stream stops with **409** rather than letting the remainder land in the new list |
| `DELETE /v1/lists/{list}/entries/{id}` | delete one entry | idempotent tombstone |
| `POST /v1/lists/{list}/reload` | publish a shipped bundle: re-open the list from disk | atomic swap; failure keeps previous content serving (409); response = fresh stats + golden-probe results |
| `POST /v1/lists/{list}/compact` | fold journal/overlay into a fresh base | run after large append sessions |
| `POST /v1/query` | query one or more lists | `{"q","lists",["threshold"],["topk"]}`; `threshold` must be in `[0, 1]` and `topk` in `[1, 1000]` when present (400 otherwise) — an ABSENT field means "list default", while an explicit `"threshold": 0` queries with no score floor (the two are distinct); results from all named lists are merged (score desc), each candidate tagged with its `list`; global top-K after merge (100 when `topk` omitted); an empty `candidates` array is an explicit "no match", not an error; unknown list anywhere in the request is a 404 and nothing runs. Library note: `engine.QueryOpts` uses zero = list default and NEGATIVE = explicit zero (no floor / unlimited) |
| `POST /v1/batch-query` | run up to 100 query checks in one request | `{"checks":[{"q","lists",["threshold"],["topk"]},...]}` (1–100 checks); `results[i]` is either a full single-query envelope or `{"error":"..."}` for that check, order preserved |

Batch checks share one round trip; per-check errors are inline, so one bad
check never fails the batch.

All responses are JSON; errors are `{"error":"message"}` with a matching
status code.

Domain blocklists: use analyzer preset `domain` with `"fallback":
"parent_domain"` — score 100 is an exact hit, 90 a parent-suffix match, and
`key` names the listed domain either way. List the bare parent domain, not
wildcard rows: a listed `*.tempmail.com` matches only the literal starred
query, while a listed `tempmail.com` covers every subdomain via fallback.

Scores are only comparable within a list: 90 means "parent-suffix match" on
an exact-mode list but "90% fuzzy similarity" on an ngram list — aggregators
merging candidates across lists must not conflate the two.

## Library usage

The engine is usable directly, without the server:

```go
package main

import (
	"fmt"
	"log"

	"github.com/kurn-dev/kurn/engine"
)

func main() {
	// In-memory list. For a durable on-disk store, use engine.Open(dir)
	// and st.CreateList / st.Replace / st.List instead.
	l, err := engine.NewList("people", engine.ListConfig{
		Analyzer: engine.AnalyzerConfig{Preset: "person-name"},
		Match:    engine.MatchConfig{Mode: "ngram"}, // grams 2+3, threshold 0.6, topk 100
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := l.Replace([]engine.Entry{
		{ID: "p1", Keys: []string{"Elena Vasquez", "Vasquez, Elena M."}},
		{ID: "p2", Keys: []string{"Marcus Chen"}},
	}); err != nil {
		log.Fatal(err)
	}
	for _, c := range l.Query("vasquez elna", engine.QueryOpts{}) {
		fmt.Printf("%s score=%.1f key=%q\n", c.EntryID, c.Score, c.Key)
	}
	// p1 score=100.0 key="Elena Vasquez"
}
```

`List.Query` is lock-free and safe under concurrent mutation; mutations
(`Replace`/`Upsert`/`Delete`/`Compact`) serialize internally.

## Performance

Every figure comes from `cmd/kurn bench`. Latency depends on corpus size, so
each one names its corpus and its machine; full tables, methodology and the
caveats that qualify them are in [bench/README.md](bench/README.md).

**Fuzzy (ngram) lists**, person-name preset, grams 2+3, threshold 0.6:

| corpus | machine | p50 | throughput |
|---|---|---|---|
| 148,837 entries (five public lists) | 4 vCPU server | 0.64 ms | 5,034 q/s, 8 clients |
| 10M synthetic keys | M1 Pro laptop | 13.2 ms | 71 q/s, single-threaded |

The first row is the median of three runs in one session; p50 moves about
±15% between runs on shared vCPUs.

- **Memory**: ~114 B/key end-to-end (≈69 B/key postings, the rest the
  ID→ordinal map; HeapAlloc-delta estimates). Process RSS is larger, since
  entries and payloads are resident too: the five public lists index in about
  22 MB and settle at about 128 MB resident.
- **Recall**: 0.940 overall right-entity recall at threshold 0.6, and 0.975
  at 0.45 for roughly 3× the latency — measured on synthetic perturbations,
  which are an easier corpus than real-world name data, so the *shape*
  (per-class floors, threshold trade-off) is the transferable claim rather
  than the level. On real ground truth (OFAC SDN aliases, indexed the way
  screening lists actually ship): 1.000.

**Exact lists**, identifier preset, 2M keys: 133 B/key, p99 4 µs, 925k
queries/s single-threaded.

**Cold open**: both modes reopen from the serialized index artifact
(`base.idx`) instead of rebuilding — 0.91 s at 1M keys, against 5.5 s for a
full rebuild. The artifact is a pure cache: any load failure, including an
analyzer change detected through its recorded digest, falls back to a
rebuild. Recovery time is open time.

## Ingestion: mapping, dry-run, bundles

Any feed — a database export, a CSV, an XML publication, an NDJSON dump —
loads through a declarative **mapping** instead of custom code: dot-paths,
per-instance equals-only filters, multi-path joins. The examples in
[docs/examples/](docs/examples/) map the major public screening lists;
the same file shape maps a `SELECT` from your own tables:

```sh
# 1. Gate: what would this feed become? (CI-able: exits 1 over the rate)
kurn ingest -dry-run -mapping ofac.mapping.json -in sdn.xml -max-empty-rate 0.05

# 2. Build the publishable bundle offline (never in the serving process);
#    -prev emits delta.jsonl for downstream re-checking
kurn build -mapping ofac.mapping.json -in sdn.xml -out bundle/ -prev old-bundle/ -source "OFAC SDN 2026-07-29"

# 3. Publish: ship bundle files into the list dir, then
curl -s -X POST localhost:8080/v1/lists/sanctions/reload
# -> fresh stats + golden-probe results; on failure the old content keeps serving
```

The bundle manifest's `version_id` equals the version stamp the node
serves — publish identity and query identity are one number. Ship order,
manifest schema, and rollback are specified in
[docs/platform-contract.md](docs/platform-contract.md).

## Multi-tenancy

`-tenants-file tenants.json` turns one kurnd into a shared multi-tenant
node. Tenancy is **key-scoped**: the tenant is resolved from the presented
API key and routed to its own store under `<data>/tenants/{id}` — `/v1/*`
paths are unchanged, and a tenant cannot even name another tenant's
namespace. Without the flag, nothing changes (single-tenant, exactly as
above).

```jsonc
{
  "acme": {
    "key_digests": ["<sha256 hex of the key>"],   // kurn keygen prints pairs
    "quotas": {"max_lists": 10, "max_total_keys": 5000000,
               "scratch_budget_mb": 256, "rate_per_sec": 200}
  }
}
```

- The file carries **digests, not keys** — a leaked tenants file exposes no
  usable credential. `kurn keygen` mints a key + digest pair.
- **SIGHUP re-reads the file** (systemd `ExecReload` shape): tenants and
  keys are added/removed atomically, surviving tenants keep their live
  stores, and an invalid file keeps the previous registry serving (logged).
  Rotation is overlap-based: ship old+new digests, migrate, remove the old.
- A removed tenant stops resolving immediately; its data stays on disk
  (deletion is a deliberate manual act).
- `/readyz` failures and `/metrics` series carry a `tenant` label;
  `/livez`, `/readyz`, `/metrics`, `/healthz` remain keyless.
- **Those four endpoints must not be reachable by tenants or from an
  untrusted network.** Keyless plus tenant-labelled means they enumerate
  the roster: `/metrics` publishes every tenant's name, list count, live
  key count, query and mutation totals, queue depth and 429s — the billing
  contract, so one tenant could read another's volumes — and a `/readyz`
  failure names the tenant, the list, and the golden probe's query string.
  Bind them to a management interface or filter them at the proxy. This is
  a deployment requirement rather than a default because the endpoints
  exist to be scraped by a control plane.
- Capacity quotas are enforced: `max_lists` at list creation (PUT-replace
  of an existing list is not a new list) and `max_total_keys` — live raw
  keys summed across the tenant's lists — at entry admission, with 403s
  naming the quota and the numbers. Checks are read-then-act: a tenant can
  overshoot by at most one in-flight batch per concurrent writer (the
  control plane reconciles from metering). Zero/absent quotas are
  unlimited.
- The memory governor is two-level: a query acquires its tenant's
  `scratch_budget_mb` slice BEFORE the global `-query-mem-budget-mb`, so
  one tenant's scan storm (worst case: explicit `threshold: 0`) queues
  against its own budget before it can contend with anyone else's; tenant
  and global exhaustion both 503 (the body names which level).
- `rate_per_sec` is a per-tenant token bucket (burst 2×rate) on the query
  path only: over-rate queries get 429 + `Retry-After` immediately —
  throttling is a fast no, never a queue. In a batch, each check spends
  one token and over-rate checks fail inline. Per-tenant queue depth,
  in-flight bytes, and 429 counts are on `/metrics`.
- Rate limiting covers QUERIES only — mutation throughput is bounded by
  capacity quotas and journal I/O, not a token bucket; a platform should
  alert on `kurn_tenant_mutations_total` spikes rather than expect 429s
  on writes.
- **Metering is the metrics surface**: `kurn_tenant_queries_total`,
  `kurn_tenant_mutations_total`, `kurn_tenant_keys`, `kurn_tenant_lists`
  (plus the governor/429 series) are the per-tenant billing contract — a
  control plane scrapes and aggregates them; kurnd persists nothing.
- **Every acknowledged mutation emits one JSON audit line** (stdlib slog,
  stdout by default — separable from kurnd's stderr logging): tenant, op,
  list, entry count, and the new content-addressed version stamp; NDJSON
  partial successes are marked `partial`. Queries are metered by counters,
  never logged per request. Shipping/retention is the deployment's job.

## Operations

kurnd is **crash-only**: there is no graceful shutdown and none is needed —
`kill -9` is a legal stop. Recovery is the startup path itself: the base
loads via the `base.idx` artifact fast path and the journal replays on top.
**RTO = Open time**: measured at 1M ngram keys (BenchmarkStoreOpen, this
machine), **0.91 s** with the artifact vs **5.2 s** rebuilding from
`base.jsonl` (the fallback after e.g. an analyzer change). The
`-journal-fsync` knob picks the power-loss window (see Limitations).

Degraded starts never block the daemon; they surface three ways — startup
log lines, `/readyz` failures, and this table:

| state | meaning | repair |
|---|---|---|
| list dir **skipped** | interrupted create/replace, missing/corrupt config, or a stray dir | fresh `PUT /v1/lists/{name}` (re-declare + reload), or remove the directory |
| journal **quarantined** | journal could not be applied (moved to `journal.jsonl.quarantined`) | list serves at base state; inspect/repair the quarantined file, or accept the loss and delete it |
| golden probe failing | data or matching regressed on a known query | `/readyz` body names list, probe, and reason |

Deployment shape (Stage 1): two nodes behind a TCP/HTTP load balancer,
each with its own data dir; publish lists to both (same content ⇒ same
content-addressed versions ⇒ identical answers — compare `versions` in
responses to verify). Terminate TLS at the LB or the nodes; when
`-api-keys-file` is set the keys are bearer credentials, so never run them
over plaintext. A `Dockerfile` ships in the repo root. Readiness-gate the
LB on `/readyz`, alert on `/metrics`.

## Limitations

- **Auth is deliberately minimal**: optional API keys (`-api-keys-file`,
  off by default; gates `/v1/*`, probes and metrics stay open) or the
  key-scoped tenants file — TLS is the deployment's job; terminate it in
  front of kurnd whenever keys are in use (they are bearer credentials).
- **Journal fsync is off by default**: with the default
  `-journal-fsync=none`, appends are atomic and replay on open (process-
  crash-safe), but an acknowledged write can be lost on power loss /
  kernel crash. `-journal-fsync=every` closes that window at ~3.9 ms per
  mutation on laptop hardware (vs ~0.1 ms); `interval` group-commits
  concurrent mutations through one fsync per window (default 2 ms).
  Base/config writes are always fsynced, directory entry included.
- **Query admission is memory-based**: in-flight queries are bounded by a
  scratch-memory budget (`-query-mem-budget-mb`, default 1024; ~4 bytes ×
  list ordinals per query), with FIFO queuing and a 503 after
  `-query-queue-timeout`. The budget counts memory, not CPU — an explicit
  no-floor query (`threshold: 0`) is the most CPU-expensive shape, and
  concurrency capping plus client-disconnect cancellation are what bound
  it.
- **Scores are segment-local**: base and overlay carry separate IDF stats,
  so a candidate's score can shift slightly when compaction folds the
  overlay into the base. Ordering by score within one segment is stable.
- **Version stamps are content-addressed for daemon/store-managed lists**:
  `<hash>@<baseEntries>+j<journalBytes>` where `<hash>` is a sha256 prefix
  of `base.jsonl` (or `empty`) — the same disk state always yields the same
  version across restarts, and equal versions imply equal answers. Only
  lists managed DIRECTLY through the library (`engine.NewList`, no Store)
  still stamp process-local `gen…` counters, which do not survive restarts.
- **`fold_diacritics` only strips combining marks**: foldables that don't
  decompose to base + mark — `ø`, `Ł`, `ß`, `æ`, `Đ` — do not fold.

## Development

```sh
go test ./... -race          # full test suite
go run ./cmd/kurn bench -n 100000 -sample 200 -seed 42 -threshold 0.6   # synthetic bench
```

`kurn` also has `gen` (corpus export: entries NDJSON + query corpus CSV,
readable back via `bench -entries`/`-corpus`) and `query` (one-off queries
against a data dir).

| path | contents |
|---|---|
| `engine/` | store, lists, snapshots, journal, compaction |
| `engine/analyzer/` | normalization pipeline + presets |
| `engine/ngram/` | char-ngram roaring index, IDF pigeonhole lookup |
| `engine/exact/` | exact-match index |
| `engine/artifact/` | serialized index cache (fast reopen) |
| `server/` | HTTP handlers (stdlib only) |
| `cmd/kurnd/` | the daemon |
| `cmd/kurn/` | dev/bench CLI |
| `bench/` | synthetic corpus generator + bench harness, measured results |
| `docs/` | the node ⇄ control-plane contract, list-mapping examples |

## License

kurn is licensed under the [GNU AGPL-3.0](LICENSE). Using kurnd as a
service — including inside your company, modified or not — is free; the
AGPL asks only that modifications to kurn itself be shared if you serve
users over a network with them. Embedding the engine library in a
closed-source product needs a commercial license instead — mail
[ops@kurn.it](mailto:ops@kurn.it).

## Security

See [SECURITY.md](SECURITY.md). Report vulnerabilities to
[ops@kurn.it](mailto:ops@kurn.it) — not in public issues.
