# Artifact-only example

These disposable fixtures demonstrate that the runtime needs neither the
feed nor its mapping. From the repository root:

```sh
mkdir -p ./out/example
go build -o ./out/example/kurn ./cmd/kurn
go build -o ./out/example/kurnd ./cmd/kurnd

./out/example/kurn build \
  -mapping ./examples/prepared-artifact/ngram-mapping.json \
  -in ./examples/prepared-artifact/feed.ndjson \
  -artifact ./out/example/people.kurn -name people
./out/example/kurn verify -semantic ./out/example/people.kurn
```

Copy only `kurnd` and `people.kurn` to a clean runtime directory. That runtime
package contains no feed or mapping. Start and query from there:

```sh
mkdir -p ./out/runtime
cp ./out/example/kurnd ./out/example/people.kurn ./out/runtime/
(
set -eu
cd ./out/runtime

# Omitting -artifact-verify selects the default full verification.
./kurnd -artifact ./people.kurn \
  -addr 127.0.0.1:8080 >./kurnd.log 2>&1 &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null; wait "$pid" 2>/dev/null' EXIT
wait_ready() {
  attempt=0
  while [ "$attempt" -lt 100 ]; do
    if ! kill -0 "$pid" 2>/dev/null; then cat ./kurnd.log; return 1; fi
    if curl --max-time 1 -fsS 127.0.0.1:8080/readyz >/dev/null; then return 0; fi
    attempt=$((attempt + 1))
    sleep 0.1
  done
  cat ./kurnd.log
  return 1
}
wait_ready

# n-gram query
curl --max-time 2 -s -X POST 127.0.0.1:8080/v1/query \
  -d '{"q":"lovelce ada","lists":["people"]}'

# the same n-gram index with an exact typed payload filter
curl --max-time 2 -s -X POST 127.0.0.1:8080/v1/query \
  -d '{"q":"lovelce ada","lists":["people"],"filter":{"country":"GB"}}'
# Both queries include entry_id "p1" and key "Ada Lovelace".

# stop and restart from the same two-file runtime package
kill -TERM "$pid"
wait "$pid"
./kurnd -artifact ./people.kurn \
  -addr 127.0.0.1:8080 >./kurnd.log 2>&1 &
pid=$!
wait_ready
curl --max-time 1 -fsS 127.0.0.1:8080/readyz
kill -TERM "$pid"
wait "$pid"
trap - EXIT
)
```

The subprocess test copies disposable inputs, deletes them after build, and
runs this query/restart journey with their original paths unavailable.

Trusted startup is optional only after the artifact has been verified and its
delivery and storage have remained trusted. It has weaker content assurance
than the full default:

```sh
./out/runtime/kurnd -artifact ./out/runtime/people.kurn \
  -artifact-verify trusted -addr 127.0.0.1:8080
```

For a new data version, build and verify a new pathname such as
`people-v2.kurn`; build refuses to overwrite an existing artifact. Direct-mode
update or rollback means stopping the daemon and restarting it with the new or
retained prior file, so the switch has an interruption. There is no managed
status/log/update/rollback command or remote router in this example.

To exercise exact matching, build a separate artifact with
`exact-mapping.json`, use `-name people-exact`, and query `Ada Lovelace`
against `people-exact`. One artifact contains one list and one match mode.

The Dockerfile is an unexecuted packaging recipe. Prepare `out/kurnd` as a
static target-Linux binary (`CGO_ENABLED=0 GOOS=linux GOARCH=<target> go
build`) and `out/people.kurn` as a validated artifact inside this
directory's build context. It intentionally has no build toolchain, source
feed, mapping, keys, or network-dependent base image. See
[`../../docs/prepared-artifacts.md`](../../docs/prepared-artifacts.md) for
the trust and deployment boundary.
