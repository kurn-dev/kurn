# Security policy

## Reporting a vulnerability

Mail **ops@kurn.it**. Do not open a public issue for anything you believe
is a vulnerability.

You will get an acknowledgement within 72 hours and a assessment within 14
days. We practice coordinated disclosure: we ask for up to 90 days before
public disclosure, and we will credit you in the fix's release notes unless
you prefer otherwise.

## Supported versions

The latest tagged release. kurn is a single static binary — upgrading is
replacing it and restarting (sub-second cold start by design), so fixes are
not backported.

## Scope notes for reporters

- kurnd binds loopback-style deployments by default and ships with auth
  OFF; an unauthenticated kurnd reachable from an untrusted network is a
  deployment configuration issue, not a vulnerability — but auth-bypass
  when `-api-keys-file`/`-tenants-file` IS configured absolutely is one.
- The engine parses untrusted artifacts (`base.idx`) and feeds
  (`kurn build`): malformed-input crashes, out-of-bounds reads, or
  resource-exhaustion bypasses of the documented admission bounds are all
  in scope and very welcome reports.
- Score/matching behavior is documented as similarity, not identity;
  "name X should (not) have matched" is a matching-quality report, not a
  security one — issues are fine for those.
