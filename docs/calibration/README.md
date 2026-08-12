# Calibrate Kurn on application-owned examples

`kurn calibrate` answers a bounded evaluator question: over one fixed set of
application entries and human-labelled queries, how does a caller-chosen fuzzy
threshold change right-entity recall and the number/rank of candidates a
reviewer receives?

It does not choose a threshold. Only the application owner knows the relative
cost of a miss, an extra candidate, and downstream review. A Kurn score is
string similarity, not a probability that two records identify the same
entity.

## 1. Export entries

Write one NDJSON object per application row. Keep IDs stable across exports.
Put every caller-owned spelling or alias that should find the same row in
`keys`; an alias is evidence supplied by the caller, not something fuzzy
matching inferred:

```json
{"id":"item-1","keys":["Harbor Desk Lamp","Desk Lamp Harbor"],"payload":{"available":true}}
```

The complete invented fixture is [`entries.jsonl`](entries.jsonl). It is
deliberately small enough to read; a real evaluation should use a
representative sample.

## 2. Label held-out queries

Create a CSV with the exact header `query,truth_id,category`. Each query needs
a human-labelled truth ID that exists in the entry export. Categories are
caller vocabulary for useful slices such as exact strings, one-character
errors, aliases, or token changes:

```csv
query,truth_id,category
Harbor Desk Lamp,item-1,EXACT
Harbor Desk Lmap,item-1,TYPO_1
```

Do not generate the labelled queries from the same transformation you are
trying to evaluate and call that independent truth. The complete neutral
fixture is [`corpus.csv`](corpus.csv).

## 3. Pin the real list configuration

[`list.json`](list.json) is a complete list configuration. The command resolves
the preset and defaults through the same engine path as a real list and records
the resolved-configuration digest.

Decide likely filter vocabulary while designing the list. Unknown filter names
fail closed, and adding a declaration later changes the configuration half of
the version stamp. A declaration does **not** re-index data or rewrite
`base.idx`; publishing the changed configuration is still a controlled
deployment/version event. This calibration corpus is unfiltered—the declared
`available` path demonstrates the design decision but does not change these
measurements.

## 4. Sweep thresholds over one immutable snapshot

From the repository root:

```sh
go run ./cmd/kurn calibrate \
  -config docs/calibration/list.json \
  -entries docs/calibration/entries.jsonl \
  -corpus docs/calibration/corpus.csv \
  -thresholds 0.45,0.60,0.75 \
  -report-json ./kurn-calibration.json
```

The command hashes the exact entry and corpus bytes it decodes, rejects
duplicate/absent truth IDs, builds one list snapshot, and runs every canonical
threshold against that unchanged snapshot. Threshold `0` means an explicit
no-floor query, not the list default. The list's default threshold and the
applied sweep remain separate in the artifact identity.

## 5. Read the decision evidence

For every threshold and category, the report contains:

- `found / cases` right-entity recall and an explicit miss count;
- returned-candidate count p50, p99, and maximum; and
- truth-rank p50 and p99 over found cases only.

Candidate count includes zero-result queries. Percentiles sort the integer
observations and select index `min(n-1, floor(n*p/100))`, so p50 is the upper
median for an even number of cases. A category with no found truths has rank
percentiles of zero; misses remain explicit rather than disappearing from the
rank distribution.

Review burden is bounded by the configured top-K. It describes candidates
actually returned for review, never all score-qualifying entities beyond that
cut. Compare at least recall, misses, candidate count, and rank together; a
lower threshold is not a free recall gain.

Timing, QPS, and HeapAlloc deltas are observations of one process run. They
move with hardware, scheduling, thermals, garbage collection, and allocator
state and are not part of deterministic result equality. Memory remains an
approximate delta with the materialized entries excluded. Corpus results are
evidence for the measured sample, not a universal product claim.

Latency samples wrap the in-process Kurn query. QPS covers the complete
single-threaded harness loop, including the candidate scan used to locate the
truth rank; neither number includes HTTP transport or models concurrent
serving throughput.

The optional JSON artifact follows
[`kurn-calibration/v1`](../../bench/KURN_CALIBRATION_V1.md). It contains input
hashes and aggregate counts, not source/report paths, entries, payloads,
queries, truth IDs, usernames, hostnames, or environment values. Report writes
fail closed and cannot publish a partial final file.
