# Releasing

This checklist is a decision tree, not a happy path: most changes do not
require every step. Classify first, then execute only the applicable
sections. Every mutation (commit, push, tag, publish) is approved by the
maintainer before it runs — prepare and verify locally, then ask.

## 0. Baseline and identity (always)

- Start from clean, current trees: every repo involved is on `main`, in
  sync with its remote, with no uncommitted changes except the ones this
  release exists to ship.
- Verify the commit identity *before* committing, per repo convention:
  `git config --get user.name` and `git config --get user.email` in each
  repo. NEVER write the flag after the key (`git config user.name
  --global`): in that position Git can treat the invocation as a WRITE
  and store the literal flag as the identity — this exact slip twice
  produced `author --global <--global>` commits, and only object-level
  verification caught them.
  CONTRIBUTING requires DCO sign-off: create commits with
  `git commit -s`.
- After committing, verify identity from the Git OBJECTS, not from a
  formatted log rendering: `git cat-file -p <commit>` and
  `git cat-file -p <tag>` show the raw author/committer/tagger lines and
  the `Signed-off-by` trailer — confirm all of them.
  (Lesson recorded: a formatted `%(taggeremail)` rendering once produced
  a doubled-bracket false alarm, and a broken identity once reached the
  public remote — object-level checks catch both directions.)

## 1. Classify the change (always)

- Which repositories changed? Engine, downstream consumers, site/docs.
- Does the engine change alter module behavior or API? Tests, comments,
  docs, and CI-only changes ship WITHOUT a tag. Behavior or API changes
  get a tag: pre-1.0, a minor bump (`v0.X.0`) signals observable change;
  a patch (`v0.X.Y`) is for invisible fixes. When in doubt, say so in the
  proposal and let the maintainer decide.

## 2. Commits (always)

- Split commits by concern along reviewable file boundaries; write the
  messages before mutating and get them approved. Create each commit with
  `git commit -s` (DCO).
- Every commit must stand alone — verify each builds, not just the final
  tree. Recipe per commit:
  `git worktree add --detach /tmp/<repo>-verify-<sha> <sha>` then
  `(cd /tmp/<repo>-verify-<sha> && go build ./...)`, then
  `git worktree remove /tmp/<repo>-verify-<sha>` when done (the scratch
  checkout never lingers).
- Run the full suite on the final tree: `go vet ./...`,
  `go test -count=1 ./...`, `go test -race -count=1 ./...`.
- Record what is intentionally left uncommitted (review trails,
  scratch work) in the delivery note.

## 3. Downstream testing before an engine tag (when a consumer pins the engine)

- Test the consumer against the UNPUBLISHED engine through a temporary
  Go workspace outside both checkouts. Recipe (paths are placeholders;
  the workspace lives in a scratch dir and is deleted afterwards):

  ```sh
  mkdir -p /tmp/consumer-verify
  printf 'go <go.mod-go-directive>\n\nuse (\n\t/path/to/engine\n\t/path/to/consumer\n)\n\nreplace <engine-module> vX.Y.Z => /path/to/engine\n' \
    > /tmp/consumer-verify/go.work
  cd /path/to/consumer
  GOWORK=/tmp/consumer-verify/go.work go vet ./...
  GOWORK=/tmp/consumer-verify/go.work go test -count=1 ./...   # + race, + DB suite
  rm -rf /tmp/consumer-verify
  ```

  The version-pinned `replace` is required: a bare `use` workspace still
  resolves the required version remotely and fails pre-tag with
  `unknown revision` — that failure is the expected pre-tag behavior,
  not a bug.
- Before committing the consumer, prove no `go.work`, no `replace`, and
  no local module path enters the commit: `git diff --cached go.mod
  go.sum` and a root listing must be clean of workspace artifacts.
- After the engine tag is published, switch the consumer to the
  published version (`go get module@vX.Y.Z && go mod tidy`), confirm the
  checksums entered `go.sum`, and re-run its full suite standalone —
  including its database-backed suite if it has one.

## 4. Tag and push (when a tag is warranted)

- The tag candidate's tree must already contain the release's changelog
  heading: verify with `git show <candidate>:CHANGELOG.md` (a tag message
  pointing at CHANGELOG.md must not reference a section that does not
  exist in the tagged tree — once shipped, the tag is not moved to fix
  it).
- Annotated tag on the exact reviewed commit, message per convention
  (`vX.Y.Z — <themes>`, body pointing at CHANGELOG.md, breaking/additive
  notes named). Verify the tag object with `git cat-file -p` before
  pushing.
- Push with explicit refs: `git push origin main`, then
  `git push origin vX.Y.Z` — never a broad `git push --follow-tags`.
- Verify on the remote afterwards: `git ls-remote origin main
  refs/tags/vX.Y.Z 'refs/tags/vX.Y.Z^{}'` — the branch head and the
  peeled tag target must equal the reviewed commit.
- Do not move or re-create a published tag. Exceptional repairs (e.g.
  identity metadata) require object-level target verification, proxy/sum
  checks, and explicit maintainer approval. Narrow fact on record:
  rewriting identity metadata while keeping the exact commit target does
  not change the module source zip, so existing `go.sum` entries remain
  valid.

## 5. Post-delivery evidence (scoped to delivered surfaces)

- CI: the workflow's first run on the delivered head is green; check the
  head SHA matches, and inspect any must-run sentinel steps' logs rather
  than inferring from the job conclusion.
- Downstream consumers: pin updated, full suites re-run standalone.
- Site/content surfaces: only when served bodies changed — publish, then
  independently fetch the canonical bodies cache-busted and compare
  hashes against the tree (plus any required content-type header). Treat
  the publish script's own success and the external comparison as
  separate evidence.
- Write the delivery record: heads, tags, run IDs, what was verified,
  what was intentionally left uncommitted, and any deviation from this
  checklist (a deviation found while following it is this document's own
  review).
