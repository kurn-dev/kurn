# Changelog

## v0.5.1 — 2026-08-08

### Bounded filtered exact-query cancellation

Exact-mode walks with a non-empty compiled payload filter now poll context
cancellation every 512 walked ordinals instead of every 4,096. The tighter
cadence bounds work after cancellation when a hot exact key fans out across
large payloads and several declared paths. Cancellation still returns no
partial candidates or `FilterStats`; callers distinguish cancellation through
`ctx.Err()` as before.

Unfiltered exact queries retain their existing 4,096-ordinal cadence, early
stop, result bytes, and allocation shape. On the locked 100,000-entry,
2,048-byte-payload, eight-path reference case, independently reproduced p95
return time after cancellation is 1.345 ms against the 3 ms release bar;
filtered late/miss throughput remains flat against v0.5.0.

### Drift and parser assurance

The permanent engine suite adds a fixed-seed 4,114-case differential between
the strict one-pass payload evaluator and an independent complete JSON decoder.
It covers nested arrays and paths, escaped names and values, exact typed
scalars and `IN`, duplicate members, malformed tails, U+0000/controls, numeric
boundaries, caller mutation, and repeated execution. No accepted filter
spelling or production parser behavior changes in this patch.

Platform verification also gains a PostgreSQL concurrency sentinel proving
that equivalent canonical filters registered through separate connections
converge on one standing-check identity while unequal filters remain distinct.
This is test-side assurance; no database migration is required.

### Documentation boundary

Public documentation now presents kurn as an in-memory search engine for fast
fuzzy and exact retrieval over application-owned collections. Public-feed
mappings remain ingestion recipes and evaluation evidence rather than the
product category. The workload boundary is explicit: single-node resident
short-string retrieval, not full-text ranking, faceting, distributed sharding,
or entity resolution. The release operations runbook moves to the downstream
platform repository; contributor-facing DCO and test rules remain public.

## v0.5.0 — 2026-08-08

### Typed equality and `IN` payload filters

Query filters now accept exact JSON booleans and numbers as well as strings,
plus one set operator: `{"in":[...]}`. Logical names remain ANDed; values in
an `in` set are ORed; types never coerce (`"1"`, `1`, and `true` are distinct).
The v0.4.0 string APIs and repeatable CLI `-filter name=value` syntax remain
source- and behavior-compatible wrappers. Typed callers gain the immutable
`TypedFilter`, `ParseTypedFilter`, `PrepareTypedFilteredQuery`, and
`QueryTypedFilteredCtx` APIs; the CLI gains mutually exclusive
`-filter-json '<object>'` using that same parser.

Accepted filters have one deterministic identity and echo: names are sorted;
`in` values are sorted and deduplicated; singleton sets collapse to equality;
numbers use exact fixed-decimal canonical form without `float64`; and scalar
order is `false`, `true`, numeric value, then UTF-8 string bytes. Limits are
enforced before admission or execution: 8 names, 64 alternatives per set,
512 decoded runes per string, bounded exact-number syntax, and a 32 KiB
canonical expression. The maximum v0.4.0 string envelope remains valid.
Duplicate or unknown members, nested values, nulls, empty sets, and limit
violations fail closed with field-naming 400s. Valid payload values of another
JSON type are non-matches; malformed stored payloads still abort the query.

Admission remains exactly unchanged when no filter is present. Filtered
queries additionally price compiled alternatives and canonical scalar bytes;
the shared 48-case corpus and ordinary/no-floor benchmark matrix pin parser,
HTTP, and downstream PostgreSQL agreement, including exact U+0000 handling.

### Filter execution telemetry

Successful non-empty-filter responses now include `filter_stats`, keyed by
list name, with the number of live score-qualified predicate evaluations and
complete-filter rejections actually performed. The values are execution
diagnostics for the pinned snapshot and plan—not cardinality, coverage, or
path-safety claims—and are omitted from unfiltered and failed responses. They
can expose bounded information about hidden score-qualified candidates and
remain covered by the query rate limit.

The engine adds immutable per-run `FilterStats` via `ExecuteStats`; existing
prepared-query and query entry points retain their signatures and unfiltered
allocation behavior. `kurn query -stats` writes one JSON stats object to
stderr after a successful filtered query while leaving the stdout candidate
stream unchanged.

## v0.4.0 — 2026-08-07

### Query-time payload filters

Lists may declare filterable payload paths under logical names
(`filterable: [{"name": "program", "path": "program"}]`, at most 8,
normalized name-sorted, part of resolved-config identity). Queries then
filter per request: `"filter": {"program": "SDN"}` — exact
decoded-JSON-string equality, ANDed names, array auto-descent, evaluated
per score-qualifying candidate BEFORE the top-K cut, so a filtered match
can never be starved by unfiltered higher scorers. Scores are unchanged
by filtering.

Fail-closed throughout: an undeclared name is a 400 naming the field and
list (never a silent clear); duplicate names and a repeated `filter`
member are 400s; a malformed stored payload aborts the query with an
error rather than dropping a candidate. Successful non-empty filtered
responses echo the exact applied filter, including empty results — a
pre-filter node ignores the member and omits the echo, so clients that
require echo equality never mistake an old node for an empty list.

Admission charges filtered queries from segment facts (payload bytes and
paths × ordinals, level-scaled for exact mode) only when a filter is
present; the unfiltered path is unchanged — no filter, no charge, no
new behavior. Engine: additive `PrepareFilteredQuery`/`QueryFilteredCtx`
entry points. CLI: repeatable `-filter name=value` on `kurn query`.

## v0.3.0 — 2026-08-06

### Direct library construction normalizes analyzer presets

`NewList` now resolves a preset-shaped `Analyzer` to its explicit steps
before the list is constructed — the same normalization the Store has
always applied before persisting `config.json`. The documented contract
is now true on every path: `Config()` returns the resolved configuration,
and `ConfigDigest()` identifies what is installed, never the caller's
spelling. A preset and its equivalent explicit steps produce equal
`Config()` and `ConfigDigest()` values.

Caller impact, precisely:

- Direct `NewList` callers passing a preset see `Config()` change from
  preset form to expanded steps form, and their `ConfigDigest()` value
  changes once — caller-owned cache keys or persisted identities derived
  from that digest invalidate on upgrade.
- Ordinary direct-list `Version()` values are process-local generation
  strings and do not carry the config digest; they are unaffected.
- Callers opting into the exported content-addressed base helpers observe
  the changed digest in a `+c` stamp.
- Store-managed lists, persisted `config.json` files, bundles, manifests,
  and platform-served stamps already carry expanded steps and are
  byte-for-byte unchanged (an exact explicit-config digest golden pins
  this).
- Query behavior is unchanged everywhere: preset and explicit forms
  already ran the same resolved steps, so no normalized key, candidate
  set, rank, or score moves. This release changes identity only.

## v0.2.2 — 2026-08-06

### Manifests bind the complete resolved configuration

`kurn build` now emits manifest v2 with a required `config_sha256`: the
digest of the bundle's RESOLVED configuration — the same value the served
version stamp carries as its `+c` half. v0.2.1 bound the manifest's mode
and analyzer declarations to `config.json`, which left an acknowledged
residual: a `config.json` swapped after build in fields those checks
cannot see (threshold, top-K, grams, fallback, golden probes, compaction
policy) still loaded cleanly, the stamp's `+c` half merely self-attesting
the swap. With v2 the manifest — the bundle's trust root — attests the
complete configuration. The field binds the configuration when the
manifest is trusted; it does not authenticate the manifest (anyone able
to rewrite both `config.json` and the unsigned manifest can recompute the
digest — authenticity needs an external trust anchor). v1 manifests
remain loadable: consumers grandfather them under the original
mode+analyzer checks and verify the field only when present.

### Added

- `List.ConfigDigest()` — the resolved-config sha256 the version stamp
  carries as `+c…`. Builders embed it, verifiers compare it against a
  loaded list; nobody re-derives the algorithm or parses stamps.
- Public CI (`.github/workflows/ci.yml`): gofmt gate, `go build`,
  `go vet`, `go test`, `go test -race` on pushes and pull requests to
  main. No secrets, no deploy steps.

### Changed

- Admission's per-gram scratch charge raised from 64 B to 200 B. Measured
  peak allocation of the two per-distinct-gram structures (the dedup-map
  entry and the 24-byte `gramInfo`, map-bucket and slice growth
  transients included) is 165–194 B/gram across query shapes; the model's
  "every term errs upward" contract is now literally true for the gram
  term instead of compensated by unrelated terms. Flood-shaped queries
  are charged ~140 KB more per ngram segment at the default 512-rune
  shape, so scratch budgets admit correspondingly fewer of them at once.
- `/metrics` per-list gauges (entries, overlay, tombstones, dropped_keys,
  keyless_entries, unindexed_entries) render from one `Status()` snapshot
  per list per scrape; a mutation landing mid-scrape can no longer make
  one list's gauges disagree with each other. Exposition format, ordering
  and labels are unchanged.

## v0.2.1 — 2026-08-06

### Version stamps identify data, journal content, AND configuration

A store-managed list's version was `<hash12>@<entries>+j<journalBytes>`.
Two things made that stamp weaker evidence than it claimed: the journal
byte position alone let equal-length journals with different mutations
share a stamp, and nothing identified the configuration — an ngram list
and an exact list over byte-identical bases shared a version while
answering the same query differently. Living lists now stamp

    <baseHash>@<entries>+j<bytes>[.<journalHash>]+c<configHash>

with COMPLETE sha256 hashes (a 48-bit prefix falls to ~2^24 deliberate-
collision work — incompatible with self-verifying evidence): the base
content, the journal's exact bytes (omitted for an empty journal), and
the resolved configuration — defaults applied, analyzer preset expanded.
Bundle manifests keep their 12-hex `version_id` as a join/display key;
it remains a prefix of the stamp's base half, whose full value equals
the manifest's `sha256`. Stamps recorded by earlier versions will not
match the new format; that is deliberate. Artifacts saved by earlier
versions rebuild once on the next open (the recorded identities
lengthened).

### Added

- `List.QueryVersioned` — candidates and version from one atomic
  snapshot; `QueryCtx`/`Query` delegate to it. The server's query path
  and audit trail use it: a response or audit line can no longer pair
  one snapshot's answer with another snapshot's version.
- `List.PrepareQuery` / `PreparedQuery.Cost` / `PreparedQuery.Execute` —
  pin one snapshot together with its admission cost and run exactly that
  snapshot. The server prepares, admits the summed prepared costs, then
  executes the prepared snapshots: a mutation landing while a request
  queues can no longer grow the executed work past what was charged.
- `Store.UpsertVersioned`, `UpsertGenVersioned`, `DeleteVersioned`,
  `ReplaceVersioned`, `CompactVersioned` (returning a `CompactResult`:
  the list generation operated on, committed version, folded entry
  count), `CreateListVersioned`, `ReloadListVersioned` — each mutation's
  committed identity, captured under the mutation lock. The unversioned
  methods are unchanged.
- `List.ScratchBytesFor(topK)` — the admission charge for a query shape;
  `ScratchBytes()` remains as the unlimited worst case.
- `List.Status` / `ListStatus` — entries, overlay, tombstones, version,
  mode, build-loss counters, and build state from one atomic snapshot.
- `engine.ErrListDamaged` — append-path mutations are refused (reads
  keep serving the acknowledged snapshot) when the process can no longer
  vouch for disk state: a failed append that could not be rolled back,
  or a destructive operation (Replace/Compact/CreateList) that failed
  AFTER publishing disk changes it cannot undo. A successful Replace,
  Compact, or PUT re-create repairs; reload is not a repair (it refuses
  non-empty journals by ship discipline).

### Fixed

- **Bounded hit collection** — the ngram lookup no longer materializes
  every qualifying hit merely to sort and discard all but top-K; results
  are byte-identical (the final order is total). Admission control
  charges a conservative ceiling (~8 B/ordinal for the scan accumulators
  plus a bounded per-hit term) instead of the 4 B/ordinal that
  undercharged flood shapes about sixfold; the touched-ordinal scratch
  is preallocated at exactly its charged size (append growth retained
  ~8-11% beyond the model); and a single query charged more than the
  entire budget is refused with an actionable 503 instead of quietly
  running past the ceiling.
- **Journal truncation is fsynced** before Replace/Compact acknowledge,
  so power loss can no longer resurrect the pre-operation journal over
  an acknowledged new base. The `.creating` marker's removal is fsynced
  before CreateList acknowledges for the same reason.
- **Failed journal appends roll back in every failure mode** (partial
  write, fsync, close, group commit); an append can no longer leave a
  torn fragment that silently swallows the next acknowledged write.
- **`Store.Close` stops the interval-mode group committer** — each
  retired interval-fsync store leaked one goroutine for the process
  lifetime.
- **List configs share no slices with callers** — mutating the config
  passed to `NewList` (or the copy `Config` returns) could panic queries
  through the shared backing arrays.
- **Server clamps oversized per-list top-K defaults** to the global
  merge cut when the request omits `topk`; small list defaults are
  preserved.
- **Ingest bounds the CSV header and XML opening tag** — both were
  materialized outside the record bound; XML gains CSV's two-tier
  contract (skippable to 1 MiB, fatal input ceiling at 16 MiB). The
  header bound counts consumed INPUT (delimiters and quoting included —
  2 MiB of bare commas decoded to zero field bytes) and caps the column
  count at 1024.
- **NaN is rejected** in list threshold and golden min_score; a JSON
  body with a trailing token (a lone `}`/`]`) is rejected; the
  `kurn_list_entries` help text matches the exported value.

## v0.2.0 — 2026-08-05

### Breaking: `engine/artifact` signatures

All four functions changed relative to v0.1.0 — a deliberate pre-1.0
break:

- `Save(path, idx, analyzerDigest)` → `Save(path, idx, analyzerDigest, BuildInfo)`
- `SaveExact(path, idx, analyzerDigest)` → `SaveExact(path, idx, analyzerDigest, BuildInfo)`
- `Load(path) (*ngram.Index, string, error)` → `Load(path) (*ngram.Index, string, *BuildInfo, error)`
- `LoadExact(path) (*exact.Index, string, error)` → `LoadExact(path) (*exact.Index, string, *BuildInfo, error)`

`BuildInfo` binds an index to the base content whose ordinal assignments
it encodes (`BaseID`, the version stamp's hash half) and carries the
build-loss counters (`Entries`, `DroppedKeys`, `KeylessEntries`) that a
reload cannot recompute. Source callers of the artifact package must
update their calls. Compatibility wrappers were considered and rejected:
an old-signature `Save` would write a record-less artifact that the
loader correctly refuses, keeping callers compiling while silently
forcing a rebuild on every open.

**One-time on-disk migration:** artifacts written by v0.1.0 lack the
build record. They are treated as unknown and rebuilt from `base.jsonl`
once on the next open (~5.5 s per 1M ngram keys instead of the ~0.9 s
artifact fast path); the rewritten artifact then carries the record and
fast-paths again. No data is lost and no action is needed.

### Fixed

- **Artifact/base identity** — an artifact is now installed only for the
  exact base content it was built from. Previously a `base.jsonl`
  replaced under a same-length artifact passed every check while the
  postings pointed at the old ordinal positions, so a query could return
  the wrong entity carrying another entry's score and key.
- **`Store.Close`** (new API) — releases a store's claim on its data
  directory: refuses further mutations, drains in-flight mutations and
  background compactions before returning, and gives concurrent closers
  one completion signal. kurnd closes a dropped tenant's store before
  releasing it, so a remove→re-add cycle can no longer run two stores
  against one directory (the abandoned auto-compactor could truncate the
  journal under the new store).
- **NDJSON append streams** are pinned to the list generation they
  started on; a `PUT`/reload mid-upload now stops the stream with 409
  instead of silently landing the remainder in the new list.
- **Ingest record bounds** are uniform across NDJSON/CSV/XML: oversize
  records are skippable bad records with record numbers; CSV input per
  record is capped at 16 MiB (fatal past the cap — the parser cannot
  resynchronize); serialized entry size is checked at the record; a
  UTF-8 BOM is stripped; a second JSON object on an NDJSON line is
  refused however far from the first; XML character data accumulates
  linearly.
- **Bundle builds** stage every output (delta and manifest included) and
  publish `base.jsonl` last, so a failed build leaves a retryable
  directory; a no-delta publish removes a stale `delta.jsonl`.
- **Loss counters** (`dropped_keys`, `keyless_entries`, and the new
  `unindexed_entries`) survive artifact reloads and are validated at
  every boundary; `/metrics` gains `kurn_list_unindexed_entries`.
- **Multi-tenant guards** — two private tenants can no longer share one
  store, and a private tenant cannot alias the shared-read node store;
  the keyless endpoints' tenant-enumeration exposure is documented as a
  deployment requirement.
- **Startup robustness** — `.idx-*` crash temps are swept; an oversize
  journal record costs its tail, not the whole list; journal reads are
  bounded; `threshold`/`topk` list defaults are range-checked; the
  `.creating` marker is fsynced before the wipe it brackets.

## v0.1.0 — 2026-08-04

Initial public release.
