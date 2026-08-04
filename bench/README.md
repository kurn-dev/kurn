# kurn bench — measured results

Measured 2026-08-04 at the v0.1.0 release, this machine:

- Machine: Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.5.1
- Go: go1.26 darwin/arm64
- Config: analyzer preset `person-name`, grams 2+3, StripSpaces, topK 100,
  single-threaded query loop (bench.Run), right-entity recall (the truth ID
  must be among the returned candidates; any-hit numbers would be higher).
- Corpus: synthetic (`bench.Generate`, seed 42). Synthetic perturbations are
  an EASIER corpus than real-world name data, so the *shape* (per-category
  floors, threshold trade-off) is the transferable claim, not the level —
  see the real-data section at the bottom for ground-truth measurements.
- Laptop latency numbers move ±15% run to run (thermals, GC); the recall
  tables are deterministic (seeded), latency is not.

## 10M keys, threshold 0.6

```
go run ./cmd/kurn bench -n 10000000 -sample 2000 -seed 42 -threshold 0.6
```

```
entries            10000000
distinct keys      9998797
build time         1m3.5s
index B/key        113.7  (approx: HeapAlloc delta around Replace, entries excluded)
process HeapAlloc  2196.1 MiB

category          cases  found  recall
EXACT             2000   2000   1.000
TYPO_1            2000   1984   0.992
TYPO_2            2000   1579   0.789
TRANSPOSE_NONADJ  2000   1525   0.762
SPACE_INSERT      2000   1964   0.982
FUSED             2000   1997   0.999
DOUBLE_TOKEN      2000   2000   1.000
REMOVE_TOKEN      2000   1998   0.999
OVERALL           16000  15047  0.940

p50  13215 µs
p99  30511 µs
QPS  71
```

## 10M keys, threshold 0.45

```
go run ./cmd/kurn bench -n 10000000 -sample 2000 -seed 42 -threshold 0.45
```

```
entries            10000000
distinct keys      9998797
build time         57.3s
index B/key        113.7
process HeapAlloc  2196.1 MiB

category          cases  found  recall
EXACT             2000   2000   1.000
TYPO_1            2000   1988   0.994
TYPO_2            2000   1847   0.923
TRANSPOSE_NONADJ  2000   1798   0.899
SPACE_INSERT      2000   1977   0.989
FUSED             2000   1997   0.999
DOUBLE_TOKEN      2000   2000   1.000
REMOVE_TOKEN      2000   1998   0.999
OVERALL           16000  15605  0.975

p50  40615 µs
p99  80953 µs
QPS  24
```

The threshold trade in one line: 0.45 buys +3.5 recall points (mostly
TYPO_2/TRANSPOSE) at ~3× the latency and a higher false-positive rate —
which is why 0.6 is the default.

## Corpus-size scaling

Same engine/config at 3.2M keys
(`go run ./cmd/kurn bench -n 3200000 -sample 200 -seed 42 -threshold 0.6`):
p50 3.1 ms / p99 6.6 ms, QPS 310 single-threaded, OVERALL 0.935, index
103.4 B/key. Latency grows with corpus size as postings lengthen
(3.1 ms → 13.2 ms p50 from 3.2M to 10M). At sample 200 per category the
recall table is noisier than the 10M runs (±1–2 pts). QPS here is
single-threaded; Lookup is concurrency-safe and scales with cores — at
real-list scale (148,837 entries, the five public sanctions/exclusion
lists) the same engine measures p50 0.91 ms and 6,022 q/s with 8
concurrent clients on a 4 vCPU server.

## Memory decomposition

113.7 B/key end-to-end is the whole-list HeapAlloc delta around `Replace`:
roaring postings PLUS the `byID` ID→ordinal map (needed for tombstoning and
upserts). The map's share, measured standalone (10M pre-existing 9-char IDs
into a pre-sized `map[string]uint32` — the exact analog of the bench
delta): 44.7 B/entry, so the postings side is ≈ 113.7 − 44.7 ≈ 69 B/key.
Trap worth recording: Go's map table sizing is quantized in n, so the
per-entry cost swings 34–45 B across nearby sizes (34.3 at 7M, 44.7 at
10M, 34.4 at 14M) — the decomposition is only valid at the delta's own n.
HeapAlloc deltas are estimates: allocator slack lands in the number.

## Exact mode

```
go run ./cmd/kurn bench -n 2000000 -sample 500 -seed 42 -mode exact
```

Identifier preset (lowercase+trim), 2M keys:

- **Memory**: 133.0 B/key end-to-end (packed postings + key arena).
- **Latency**: p50 0 µs / p99 4 µs per query, single-threaded QPS 924,784.
- Recall categories other than EXACT score 0 by construction (exact mode
  returns analyzed-key membership only) — the EXACT row is the meaningful
  one (1.000).

## Cold open

Both list modes reopen via a serialized index artifact (`base.idx`),
skipping the index rebuild; the remainder is dominated by `base.jsonl`
JSON decode. The artifact is saved on Replace/Compact and is a pure cache:
any load failure (including an analyzer-config mismatch, per the recorded
analyzer digest) falls back to a full rebuild.

Measured (BenchmarkStoreOpen in the engine package; 1M ngram keys,
person-name preset):

| path | Open time |
|---|---|
| artifact fast path (normal restart) | 0.91 s |
| full rebuild (artifact missing/rejected) | 5.5 s |

This is the crash-only RTO number: recovery time IS Open time.
Reproduce: `go test ./engine/ -run xxx -bench BenchmarkStoreOpen -benchtime 3x`.

## Real-data measurement: OFAC aliases

Ground-truth run of the harness's file-input mode (`bench
-entries/-corpus`). Source: OFAC SDN list, publish date 2026-07-29
(public; 19,175 records). Extract: 7,472 Individuals; 8,668 strong
Latin-script aliases (a.k.a. + f.k.a., primary-equal excluded). The
extractor is committed at `bench/ofac` (the SDN data itself stays out of
the repo — public, but daily-changing snapshots belong outside git); its
filters reproduce this section's counts and recall exactly against the
same publication:

1. `go run ./bench/ofac -in sdn.xml -entries ofac-entries.jsonl -corpus ofac-corpus.csv`
   (add `-index-aliases` for the deployment-shaped control);
2. `go run ./cmd/kurn bench -entries ofac-entries.jsonl -corpus ofac-corpus.csv -threshold <t>`.

Results:

- **Aliases held out of the index** (the matcher must bridge primary↔alias
  itself): recall **0.541 @ 0.6**, **0.713 @ 0.45**.
- **Aliases indexed as keys** (how screening lists actually ship):
  **1.000 @ 0.6** — alias data does the bridging, not fuzzy cleverness.
- Misses @ 0.6 (3,981): 41.2% are identity changes (zero shared tokens —
  unbridgeable by any string matcher); a strict phonetic (Metaphone)
  second channel would recover only 6.9% of misses — measured evidence
  that it is not worth building.

## Reproducing

```sh
go test ./... -race
go run ./cmd/kurn bench -n 10000000 -sample 2000 -seed 42 -threshold 0.6
go run ./cmd/kurn bench -n 10000000 -sample 2000 -seed 42 -threshold 0.45
go run ./cmd/kurn bench -n 3200000 -sample 200 -seed 42 -threshold 0.6
go run ./cmd/kurn bench -n 2000000 -sample 500 -seed 42 -mode exact
# file-input mode: kurn gen -o entries.jsonl -corpus corpus.csv, then
go run ./cmd/kurn bench -entries entries.jsonl -corpus corpus.csv -threshold 0.6
```
