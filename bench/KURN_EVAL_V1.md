# `kurn-eval/v1` evaluation report

`kurn bench -report-json <path>` writes a machine-readable companion to the
existing human report. The flag is opt-in; without it, stdout and exit behavior
are unchanged. A target of `-` is rejected because stdout belongs to the human
report.

Example:

```sh
go run ./cmd/kurn bench \
  -n 1000000 -sample 2000 -seed 42 -threshold 0.6 \
  -report-json ./kurn-eval.json
```

The schema is a typed, map-free JSON object. Fields are emitted in the order
below, category rows have a stable order, and units are part of observation
field names. Writers reject NaN, infinity, invalid counts, inconsistent
recall, and unsupported schema versions.

## Fields

- `schema`: exactly `kurn-eval/v1`.
- `build`: Go build identity for the engine binary.
  - `module_path`
  - `version`: may honestly be `(devel)` or `unknown`.
  - `vcs_revision`: the revision from Go build settings, or `unknown`.
  - `vcs_modified`: `true`, `false`, or `unknown`.
- `environment`: facts needed to interpret a run.
  - `go_version`, `goos`, `goarch`
  - `logical_cpu`
  - `gomaxprocs`: the effective value during the run.
- `identity`: inputs that define the evaluation claim.
  - `benchmark_mode`
  - `resolved_config.analyzer_steps`: installed analyzer steps, never the
    caller's preset spelling.
  - `resolved_config.match`: installed `mode`, `fallback`, `grams`,
    `strip_spaces`, `threshold`, and `topk`.
  - `resolved_config.digest_sha256`: the engine's resolved-configuration
    digest. A preset and the equivalent explicit steps therefore record the
    same configuration identity.
  - `input.mode`: `synthetic` or `files`.
  - `input.synthetic`, present only in synthetic mode: generator identifier,
    `seed`, `entries`, and `sample`.
  - `input.files`, present only in file mode: SHA-256 of the exact entry-file
    bytes and exact corpus-file bytes. Hash identity depends on content, not
    file location.
  - `entry_count`, `distinct_key_count`
- `results`:
  - `categories`: canonical built-in category order followed by any additional
    category names in lexical order. Each row has `category`, `cases`, `found`,
    and `recall`.
  - `overall`: the same fields with category `OVERALL`; its counts equal the
    category totals.
- `observations`: run-dependent measurements, deliberately outside
  `identity`.
  - `build_duration_ns`
  - `index_heap_delta_bytes`
  - `index_bytes_per_distinct_key`
  - `process_heap_alloc_bytes`
  - `latency_p50_us`, `latency_p99_us`
  - `qps`

Identity and result fields may be compared for otherwise identical runs.
Observation fields must not be included in such equality claims: scheduling,
thermals, garbage collection, and allocator state can change them.

## Memory caveat

`index_heap_delta_bytes` and `index_bytes_per_distinct_key` are approximate
HeapAlloc deltas around `Replace`, with the materialized entry slice excluded.
Allocator slack and anything else the garbage collector moves in that window
land in the number, so it is an estimate rather than an allocation-accounting
guarantee. A negative measured delta is allocator/GC noise and is recorded as
zero in the portable report. `process_heap_alloc_bytes` is the post-build
process HeapAlloc observation and is not an index-only measurement.

## Privacy and write behavior

The report contains no source path, report target path, hostname, username,
environment variable, raw entry, raw query, truth ID, or file content. File
inputs are represented only by their byte SHA-256 values and aggregate counts.
Category names remain because they define the result grouping.

The writer creates a temporary file beside the target, writes and fsyncs it,
closes it, then renames it over the target. Validation, creation, write, sync,
close, or rename failure makes the command fail and removes the temporary
file; it never publishes a partial final report.

This report is an evaluation artifact. It is not a compatibility promise for
the query API and does not endorse any particular corpus.
