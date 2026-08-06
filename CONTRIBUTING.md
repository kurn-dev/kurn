# Contributing to kurn

Thank you for considering it. kurn is small on purpose; the bar for adding
things is deliberately high, and the bar for *fixing* things is deliberately
low.

## The ground rules

1. **Dependencies are debt.** The engine is the Go standard library, the
   Go team's `golang.org/x` packages, and exactly one chosen third-party
   library — Roaring bitmaps (which links its own small `bitset`
   dependency; `go version -m` shows the complete honest list, and
   everything else in `go.sum` is test-only checksums of the module
   graph). PRs adding dependencies will be asked to redesign the feature
   instead — that is not hazing, it is the product.
2. **Measured, not asserted.** Performance and matching-quality claims come
   with the benchmark or eval that produced them (`bench/`, and see
   `bench/README.md` for the record-run discipline). "Should be faster"
   doesn't merge; "is 1.4× faster on the record run, here's the output" does.
3. **Determinism is the product.** Anything that could make the same query
   against the same list version produce different bytes — iteration-order
   leaks, time dependence, randomness — is a bug even when scores look
   right. The differential suite (`engine/ngram/differential_test.go`)
   guards this; extend it when you touch scoring.
4. **No real persons in fixtures. Ever.** Test data, examples, and docs use
   fictional names only (the repo's existing fixture names are the pattern;
   note that negative-assertion ngram fixtures need pairwise gram-disjoint
   names — see existing fixtures for why). Public list data is never
   committed, only mappings that parse it.
5. **Tests first, `-race` always.** `go test ./... -race` green, `go vet`
   clean, `gofmt` clean. New behavior needs a test that fails without it.
   CI runs exactly this sequence on every push and pull request, so a CI
   failure always reproduces offline:

       gofmt gate (fails on any gofmt -l output)
       go build ./...
       go vet ./...
       go test -count=1 ./...
       go test -race -count=1 ./...

## Practicalities

- Sign off your commits (DCO, `git commit -s`).
- Small PRs review fast; large ones start as an issue describing the
  problem (not the solution) first.
- Security reports go to ops@kurn.it, never to public issues — see
  [SECURITY.md](SECURITY.md).
