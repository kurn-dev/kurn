# Prepared artifacts

A prepared artifact is a reusable, read-only version of one Kurn list. Build
it from an application-owned short-string feed, carry it with a
target-compatible `kurnd`, and serve it without the feed, mapping, database
contact, or an index rebuild. The database or source remains authoritative;
the artifact is a deployable search version, not a backup or snapshot policy.

## Build and verify

Build the two executables for the machine or container that will run them:

```sh
mkdir -p ./out
go build -o ./out/kurn ./cmd/kurn
go build -o ./out/kurnd ./cmd/kurnd
./out/kurn build -mapping ./examples/prepared-artifact/ngram-mapping.json \
  -in ./examples/prepared-artifact/feed.ndjson \
  -artifact ./out/people.kurn -name people
./out/kurn verify -semantic ./out/people.kurn
```

`kurn build -artifact` self-verifies its output. `kurn verify` independently
performs full structural, content-digest, canonical-record and base-identity
verification. `-semantic` additionally rebuilds the index and recomputes the
engine configuration/analyzer identities. Neither command authenticates who
produced the file; preserve provenance and container/image digests through a
trusted build and delivery system.

An ngram mapping whose `match` carries `"exact_companion": true` produces an
artifact with one more section, the exact companion index over the same keys
(format `KURNART1 1.1`, section kind 7); `kurn verify` prints an
`exact companion` line for it and `-semantic` rebuilds and compares that
section too. Such a list answers `exact_any_of` checks (see the engine
README) beside ordinary fuzzy queries. A daemon built before the companion
existed refuses these artifacts as incompatible rather than serving them
without the section.

Build is absent-only: it refuses to replace an existing artifact. Give every
new version a new pathname (for example, `people-v2.kurn`) rather than
overwriting or modifying served bytes.

After verification, move or copy only the target-compatible daemon and the
artifact into the runtime package. The original feed and mapping are not
needed there. Start the daemon in the foreground from the artifact alone:

```sh
./out/kurnd -artifact ./out/people.kurn -addr 127.0.0.1:8080
```

With the daemon running, query from another shell:

```sh
curl -s 127.0.0.1:8080/artifactz
curl -s 127.0.0.1:8080/readyz
curl -s -X POST 127.0.0.1:8080/v1/query \
  -d '{"q":"lovelce ada","lists":["people"],"filter":{"country":"GB"}}'
# The response includes entry_id "p1", key "Ada Lovelace".
```

`/readyz` reports when the chosen verification policy completed and queries
can be served; `/artifactz` reports the installed artifact and identity basis.
The foreground process writes startup and refusal details to its normal logs.
Stop it with your process supervisor or a termination signal, then run the
same command to restart—there is no ingestion step.

For a direct update, build and verify a separate immutable file such as
`people-v2.kurn`, stop the foreground daemon, and restart it with that file.
To return to the prior version, repeat the stop/restart using the retained
`people.kurn`. Direct mode does not provide hot reload, an atomic deployment,
or an availability guarantee; each switch has a serving interruption. Use
independent replicas and an external rollout strategy where availability is
required.

The example directory contains exact and n-gram mappings. Its README gives
commands for exact, fuzzy, filtered, restart, and cleanup checks in a bounded
[single-shell lifecycle](../examples/prepared-artifact/README.md).

## Troubleshooting

- **Address already in use:** choose another explicit `-addr`, or stop the
  process occupying that listener before restarting.
- **Invalid or incompatible artifact:** the daemon refuses startup and logs
  the error; rebuild with a compatible `kurn`/`kurnd` pair and do not fall
  back from full to trusted verification.
- **Query names the wrong list:** `lists` must contain the logical `-name`
  stored at build time, not the artifact filename. Inspect `/artifactz`.
- **Build path already exists:** choose a new pathname. Build never overwrites
  an existing artifact.
- **Memory pressure:** the CLI uses the heap loader and has no configured hard
  cap. Measure and provision from your own artifact and query workload.
- **Not ready or requests refused:** check `/readyz` and process logs. During
  open there is no installed prepared store; during drain requests are
  refused while the established verification header remains available.
- **Write endpoint refused:** prepared stores are read-only. Rebuild a new
  artifact from the authoritative source instead of issuing list or entry
  mutations to the artifact-serving daemon.

## Full and trusted startup

Full verification is the default:

```sh
./out/kurnd -artifact ./out/people.kurn -artifact-verify full \
  -addr 127.0.0.1:8080
```

Full recomputes the artifact's content identities at every start. Trusted is
an explicit operator assertion that the artifact was fully or semantically
verified before packaging and that its build, delivery, and storage remained
trusted:

```sh
./out/kurnd -artifact ./out/people.kurn -artifact-verify trusted \
  -addr 127.0.0.1:8080
```

Trusted still validates the format, bounds and relationships used by the
parser and index, configuration/analyzer identities, metadata digests, and
JSON syntax for non-empty payloads. It deliberately skips content-section
digests, canonical record/base-identity recomputation, and the whole-file
digest. Structurally valid content corruption or semantic misrepresentation
can therefore escape trusted startup. There is no fallback: a failed full
open never retries as trusted, and a failed trusted open never serves.

Neither policy is an authenticity mechanism. Embedded unsigned hashes,
read-only permissions, and a successful open do not prove who built an
artifact. Neither policy universally validates UTF-8 within arbitrary JSON
payload strings; IDs and keys have their own format checks. Do not describe
either mode as payload cleansing.

The daemon log names the effective policy. Prepared responses also expose it:

- `Kurn-Artifact-Verification: full|trusted` appears on every `/v1/*`
  response and `/readyz`, including authentication errors and refusals after
  a prepared store is installed.
- List stats include `version_basis`; query results include `version_bases`.
  `verified-base-id` means full recomputed the base identity.
  `declared-base-id` means the stamp uses the artifact's declaration.
- `/artifactz` separates `verified` identities from `declared`. Trusted has
  `"verified": null`; it never presents declarations as verified values.

Readiness means the requested startup policy completed, the engine recomputed
and matched configuration identities, and queries can be served. While the
daemon is still opening, the outer lifecycle gate emits no verification
header because no prepared store has been established. During drain after a
successful start, `/v1/*` and `/readyz` refusals retain the established header.
Clients that ignore the header and version-basis fields cannot distinguish a
full-verified response from an operator-trusted one.

No startup-speed claim is made for trusted mode. It still walks records and
indexes and checks payload syntax, and optional metadata can be large.

## Loader and access boundaries

The `kurnd` CLI currently exposes the heap loader only. The Go library has a
separately reviewed mapped loader, but that is not exposed by
`-artifact-loader`; `mapped` is refused. Heap loading means the artifact file
is not needed after a successful open.

Prepared stores are read-only, but read-only is not access control. Existing
`-api-keys-file` authentication is unchanged and should use operator-provided
secrets mounted at runtime. `/artifactz`, `/readyz`, and other management
probes remain keyless on the same serving listener; restrict access to that
listener at the surrounding network or proxy. Do not bake keys into an image.
kurn prepared serving is one-node search and does not provide tenant isolation
or a key management service. Managed local lifecycle and
remote-router operation remain separate, unimplemented product boundaries.

## Container packaging

[`examples/prepared-artifact/Dockerfile`](../examples/prepared-artifact/Dockerfile)
is a packaging recipe, not a build pipeline. It copies only a prebuilt
target-Linux `kurnd` and an already validated artifact into a `scratch`
image. It contains no Go source, source feed, mapping, package download, or
credential. The recipe's container commands are documentation-only and were
not executed as part of this change.

Build and verify in a trusted build stage outside that Dockerfile. A scratch
image needs a static target-platform executable; for example, replace the
target architecture as needed:

```sh
mkdir -p ./examples/prepared-artifact/out
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -o ./examples/prepared-artifact/out/kurnd ./cmd/kurnd
cp ./out/people.kurn ./examples/prepared-artifact/out/people.kurn

# UNEXECUTED container recipe:
docker build -t kurn-prepared:local ./examples/prepared-artifact
docker run --read-only --rm -p 127.0.0.1:8080:8080 \
  kurn-prepared:local
```

The `docker` commands above were not run or validated in this work. Place the
Linux binary and artifact in the recipe's context, then build under normal supply
chain controls. Run with a read-only root filesystem and operator-provided
authentication material. Pin deployments to the resulting image digest.
Publish an update as a new immutable image and roll processes; never modify a
served artifact in place. A read-only image reduces accidental mutation but
does not authenticate its contents.
