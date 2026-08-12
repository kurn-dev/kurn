# `kurn-calibration/v1` threshold-calibration report

`kurn calibrate -report-json <path>` writes privacy-safe decision evidence for
a caller-labelled, file-backed n-gram corpus. It is deliberately separate from
`kurn-eval/v1`: a calibration sweep and a single benchmark run have different
identities and consumers. Neither schema silently widens the other.

## Command boundary

```sh
kurn calibrate -config list.json -entries entries.jsonl \
  -corpus corpus.csv -thresholds 0.45,0.60,0.75 \
  -report-json calibration.json
```

- N-gram mode only; exact matching has no fuzzy-threshold decision loop.
- The threshold set contains 2–32 finite, unique values in `[0,1]` and is
  serialized in ascending order. Zero is an explicit no-floor query.
- `entries.jsonl` contains one Kurn entry per line.
- `corpus.csv` has header `query,truth_id,category`. Every non-empty truth ID
  must exist in a unique entry ID; invalid inputs fail before measurement or
  report publication.
- One immutable list snapshot serves the entire sweep. The list's resolved
  default threshold is distinct from the applied sweep thresholds.

## Typed field order

- `schema`: exactly `kurn-calibration/v1`.
- `build`: module version and VCS identity from Go build settings.
- `environment`: Go version, OS/architecture, logical CPU count, and effective
  `GOMAXPROCS`.
- `identity`:
  - `benchmark_mode`: exactly `ngram`.
  - `resolved_config`: installed analyzer steps; installed match mode,
    fallback, grams, strip-space setting, **default** threshold, and top-K;
    counts of filter declarations and golden probes; overlay-auto-compact
    value; and the full resolved-config SHA-256 digest. Probe queries and
    declarations themselves are not copied into the report.
  - `input`: `files` plus SHA-256 of the exact entry and corpus byte streams.
  - canonical `thresholds`, entry count, distinct-key count, and corpus-case
    count.
- `results`: one row per canonical threshold:
  - `categories`: built-in benchmark category order followed by additional
    category names in lexical order.
  - `overall`: the same metrics over every case.
  - each category/overall result records cases, found, misses, recall,
    returned-candidate count p50/p99/max, and truth-rank p50/p99.
- `observations`: build duration, approximate index HeapAlloc delta, bytes per
  distinct key, process HeapAlloc, then per-threshold latency p50/p99 and QPS.

Latency samples wrap the Kurn query call. QPS measures the complete sequential
harness loop, including the returned-candidate scan that finds the labelled
truth rank; neither is an HTTP or concurrent-service throughput claim.

Candidate-count percentiles include zero-result queries. Truth-rank
percentiles include found cases only and use the first truth-ID position in
Kurn's returned deterministic order, numbered from one; ranks are zero when no
case is found. Integer percentiles sort values and select
`min(n-1, floor(n*p/100))`. Misses remain explicit.

Returned-candidate burden is within the resolved top-K. It is not a count of
every score-qualifying entry beyond that collection cut, and the calibration
layer never re-ranks candidates.

## Determinism and observations

For the same exact input bytes, resolved configuration, Kurn build, and
threshold set, identity and result fields are comparable and deterministic.
Build/query duration, QPS, and allocator observations are not: hardware,
scheduling, thermals, garbage collection, and allocator state can move them.

The index-memory fields retain `kurn-eval/v1`'s caveat: they are approximate
HeapAlloc deltas around `Replace`, with the materialized entry slice excluded.
Allocator slack and other GC-window movement land in the estimate; a negative
delta is clamped to zero. Corpus results describe only the measured labels and
are calibration evidence, not an automatic threshold selection. Threshold
selection belongs to the evaluator's loss function.

## Privacy and atomic publication

The report contains no source/config/report path, hostname, username,
environment-variable value, raw entry, payload, raw query, truth ID, or file
content.
Category names remain because they define aggregate result groups. Inputs are
represented by byte SHA-256 identities and counts.

The writer validates the complete typed object before creating a temporary
file beside the target, then writes, fsyncs, closes, and renames it. Creation,
validation, write, sync, close, or rename failure removes the temporary file
and leaves no partial final artifact. A target of `-` is rejected so stdout
remains the human report.
