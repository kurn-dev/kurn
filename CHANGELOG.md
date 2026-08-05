# Changelog

## Unreleased (v0.2.1)

### Version stamps now identify journal content

A store-managed list's version was `<hash>@<entries>+j<journalBytes>` —
and the byte position alone let two journals of equal encoded length but
different mutations share a stamp while answering differently. Living
lists now stamp `<hash>@<entries>+j<bytes>.<journalHash>`, a
domain-separated sha256 prefix of the journal's exact byte content
(omitted for an empty journal, so `+j0` stamps and bundle `version_id`
prefixes are unchanged). Stamps recorded by earlier versions will not
match the new format; that is deliberate — the old suffix was not
evidence of content.

### Added

- `List.QueryVersioned` — candidates and version from one atomic
  snapshot; `QueryCtx`/`Query` delegate to it. The server's query path
  and audit trail use it: a response or audit line can no longer pair
  one snapshot's answer with another snapshot's version.
- `Store.UpsertVersioned`, `UpsertGenVersioned`, `DeleteVersioned`,
  `ReplaceVersioned`, `CompactVersioned`, `CreateListVersioned`,
  `ReloadListVersioned` — each mutation's committed version, captured
  under the mutation lock. The unversioned methods are unchanged.
- `List.ScratchBytesFor(topK)` — the admission charge for a query shape;
  `ScratchBytes()` remains as the unlimited worst case.
- `engine.ErrJournalDamaged` — appends are refused (reads keep serving)
  when a failed append cannot be rolled back; Replace/Compact/ReloadList
  repair.

### Fixed

- **Bounded hit collection** — the ngram lookup no longer materializes
  every qualifying hit merely to sort and discard all but top-K; results
  are byte-identical (the final order is total). Admission control
  charges a conservative ceiling (~8 B/ordinal for the scan accumulators
  plus a bounded per-hit term) instead of the 4 B/ordinal that
  undercharged flood shapes about sixfold.
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
  contract (skippable to 1 MiB, fatal input ceiling at 16 MiB).
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
