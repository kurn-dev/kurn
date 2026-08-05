# Changelog

## Unreleased (v0.2.0)

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
