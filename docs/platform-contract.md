# kurn ⇄ platform interface contract (v1)

The open/closed boundary in writing: everything a management
control plane consumes from or produces for open kurnd nodes. This
document is the CONTRACT and lives in the open repo, versioned with the
code that implements it — any control plane pins a version of this file,
never the reverse. Compatibility rule within v1:
additive only (kurnd's tenants-file parser tolerates unknown fields;
metric families are never renamed, only added).

## 1. Tenants file (registry v1)

One JSON object per node, shipped by the control plane, applied with
`SIGHUP` (systemd `ExecReload=/bin/kill -HUP $MAINPID`); restart is the
fallback. kurnd validates before applying — an invalid file keeps the
previous registry serving and logs the rejection, so shipping a bad file
degrades to "no change" rather than an outage.

```jsonc
{
  "<tenant>": {                       // ^[a-z0-9][a-z0-9_-]{0,63}$ (becomes a dir)
    "key_digests": ["<sha256 hex>"],  // of the FULL key string; >= 1 required
    "quotas": {                       // all optional; 0/absent = unlimited
      "max_lists": 10,
      "max_total_keys": 5000000,      // live raw keys across the tenant's lists
      "scratch_budget_mb": 256,       // tenant slice of query scratch memory
      "rate_per_sec": 200             // query token bucket (burst 2x)
    }
  }
}
```

Semantics the control plane may rely on:

- Digests, never keys: nodes hold no plaintext credentials. `kurn keygen`
  emits key+digest pairs; the platform stores its own hash in Postgres and
  shows plaintext exactly once at issuance.
- Reload is atomic; a tenant absent from the new file stops resolving
  immediately (data stays on disk under `<data>/tenants/<id>`).
- A reload leaving a tenant's `scratch_budget_mb`/`rate_per_sec` unchanged
  carries its live governor state (no token refill, no admission reset).
- Duplicate digests across tenants are rejected (whole file refused).

Key lifecycle procedures built on this: **issue** = add digest + ship;
**rotate** = ship old+new digests, wait for the tenant to migrate, ship
without the old; **revoke** = remove digest + ship. The propagation window
is ship + SIGHUP — seconds, not instant; plan revocation SLAs accordingly.

## 2. Metering scrape (billing contract)

The control plane scrapes `GET /metrics` (keyless) and aggregates into
its usage store; kurnd persists nothing. Per-tenant families:

| family | type | meaning |
|---|---|---|
| `kurn_tenant_queries_total{tenant}` | counter | queries served, all lists |
| `kurn_tenant_mutations_total{tenant}` | counter | acknowledged mutations (ticks exactly when an audit line is emitted) |
| `kurn_tenant_429s_total{tenant}` | counter | rate-limited queries |
| `kurn_tenant_keys{tenant}` | gauge | live raw keys (= `max_total_keys` usage) |
| `kurn_tenant_lists{tenant}` | gauge | lists (= `max_lists` usage) |
| `kurn_tenant_query_queue_depth{tenant}` | gauge | queries waiting on the tenant's scratch slice |
| `kurn_tenant_inflight_bytes{tenant}` | gauge | tenant scratch bytes in flight |

Caveats that are part of the contract: counters are process-local and
reset on restart (use `increase()`-style aggregation); per-list series
(`kurn_queries_total{tenant,list}`, latency histograms, …) exist below
the tenant rollups and may be scraped for diagnostics but are NOT the
billing basis.

## 3. Audit stream

One JSON line per acknowledged mutation on kurnd's **stdout** (stderr
carries operational logging — the streams are separable by fd):

```json
{"time":"...","level":"INFO","msg":"mutation","op":"replace",
 "list":"codes","n":18234,"version":"ca764d77fea0@18234+j0",
 "tenant":"acme","partial":false}
```

- `op` ∈ create | upsert | replace | delete | compact | reload; `n` is
  the entry count the acknowledgment covered (create = 0, compact and
  reload = live entries).
- `version` is the content-addressed stamp — the same value query
  responses echo, so audit lines join against query-time versions.
- `partial: true` marks an NDJSON append that failed mid-stream after
  durably acknowledging `n` entries.
- Failed or unauthorized mutations and ALL queries emit nothing.
- `tenant` is absent in single-tenant deployments.

## 4. Node control surface

- Keyless (LB/scraper-facing): `GET /livez`, `GET /readyz` (503 body
  enumerates failures: tenant, list, probe, reason), `GET /metrics`,
  `GET /healthz`.
- **The platform must keep those four off any network a tenant can reach.**
  Keyless and tenant-labelled together make them a roster: `/metrics`
  names every tenant along with its list count, live key count, query and
  mutation totals, queue depth and 429s — §2's billing basis, readable by
  anyone who can reach the port — and a `/readyz` failure adds the list
  name and the golden probe's query string. kurnd cannot enforce this,
  since the endpoints exist to answer before any key is presented, so the
  boundary is the platform's to draw: a management interface, or a proxy
  that filters these paths out of tenant traffic.
- Keyed: everything under `/v1/`.
- Signals: `SIGHUP` = tenants-file reload. Anything else (fsync mode,
  budgets, list configs) is restart-applied; kill -9 is a legal stop
  (crash-only; RTO = artifact-load time, ~0.9 s/1M keys measured).
- Flags the platform provisions: `-data`, `-addr`, `-tenants-file`,
  `-journal-fsync[-interval]`, `-query-mem-budget-mb`,
  `-query-queue-timeout`.

## 5. Bundles: build, ship, publish, roll back

A **bundle** is one list's complete published state, produced offline by
`kurn build` (open tooling — the platform orchestrates it, feeds come
from the platform's sources):

```
bundle/
  config.json      # resolved ListConfig
  base.jsonl       # canonical entries (sha256 below is over THIS file)
  base.idx         # serialized index (analyzer digest inside)
  manifest.json    # see below
  delta.jsonl      # with a previous bundle: add/update/delete records
```

`manifest.json` v1: `{"v":1, "sha256", "version_id" (12-hex prefix),
"analyzer", "mode", "entries", "keys", "source", "created_at",
"prev_sha256", "delta":{"adds","updates","deletes"}}`. The identity
property the platform may build on: **`version_id` is the PREFIX of the
version stamp the node reports after loading the bundle** (the full
stamp is `<version_id>@<entries>+j<journalBytes>.<journalHash>`, where
the journal suffix is `+j0` right after a clean publish and gains a
content hash of the journal's exact bytes once mutations land on top) —
publish-time and query-time identity share one number,
so "which list version answered this query" joins directly against the
registry of built bundles.

**Ship discipline** (per file: write temp name in the list dir, then
rename): remove `base.idx` first, rename it into place last —
`config.json`, `base.jsonl`, then `base.idx`. Remove any `journal.jsonl`
(a bundle replaces ALL list content) — this is ENFORCED, not advisory:
reload refuses a list dir with a non-empty journal (409) rather than
replay mutations the manifest doesn't describe. This is the same order
the engine's own persistence uses; a crash mid-ship leaves a state the
reload validates rather than serves.

**Publish**: `POST /v1/lists/{list}/reload` (keyed; tenant-scoped like
every /v1 route). Success returns fresh stats — `version` carries the
`version_id` — plus the list's golden-probe results inline: the gate
outcome arrives in the publish response. Failure returns 409 and the
PREVIOUS content keeps serving; fix the files and reload again.
Delta production is build-time; delta CONSUMPTION (re-checking) is
Stage 4.

**Rollback** = re-ship the previous bundle's files + reload. Bundles are
immutable; keep N previous bundles in the registry and rollback is a
copy.

## 6. The other side of the contract

The management plane that consumes this contract (tenant registry, key
issuance, metering, billing) is a separate closed-source service; nothing
in this repo depends on it. Anyone can implement their own consumer of
this contract — that is the point of specifying it.
