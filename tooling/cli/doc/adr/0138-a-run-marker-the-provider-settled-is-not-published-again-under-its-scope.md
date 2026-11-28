# ADR 0138 — A run marker the provider settled is not published again under its scope

- **Status**: accepted
- **Date**: 2026-11-28
- **Scope**: `protocols/cache` (the `marker-scope` capability and
  `InitializeResult.markerScope`), `@putnami/cli` (`internal/cacheprovider`:
  the session keeps the scope; `internal/jobs/remotecache`: a publish returns
  it; `internal/workspace_state`: the settled marker and its witness;
  `internal/engine`: the publish is skipped while the witness holds),
  `@putnami/cloud-extension` (the scope its cache provider names)

## Context

A successful run publishes its run marker (10-caching.md, "Successful run
markers"). Publishing starts the cache provider, which for `@putnami/cloud`
starts `putnami cloud token --for cache`, a full CLI child, and then answers
`initialize` over the network. On this repository, an unchanged signed-in
run spent about 1 s of its 3.5 s there, at a commit whose marker the
provider had already published, kept as not advanced, or refused as
read-only.

Core cannot tell that answer apart without starting the provider. The
provider owns its configuration and credentials, and core reads neither
(10-caching.md, "Enabling and configuration"). A record keyed by the
branch, the commands and the commit alone would skip a publish that a
different account, endpoint or workspace still needs.

## Decision

1. **The provider names the scope of its answer.** A core that lists
   `marker-scope` at `initialize` receives, from a provider that echoes it,
   `markerScope`. It holds an opaque key of the endpoint, the account and the
   workspace the provider resolved. It also holds the absolute files and the
   variables, by name or by prefix, that these were read from. A provider
   names a scope only when every input of its answer is in those files and
   variables. Otherwise it names none.
2. **Core keeps a settled marker under that scope.** Core witnesses the
   scope when the session initializes, as the provider reads it: the stat of
   each scope file (present or absent, link and target) and a salted digest of
   the scope variables. A file written within 2 s, or in a directory whose
   stats the file index does not trust, is not witnessed, as in ADR 0113. When
   the provider publishes the marker of a commit, answers that the marker in
   effect is that commit's, or refuses it as read-only, and the witness still
   holds, core records it with the last-build entry of the branch, commands
   and parameters. The record holds the commit, the run-marker workspace
   identity, the provider's extension, version and runtime, the scope key and
   the witness.
3. **A later successful run at the same commit publishes nothing** while
   every part of the record holds. It starts no provider for the marker.
   Any other answer records nothing, and the next run publishes again: not
   ready, a write that published nothing and names another marker or none, a
   failed write, a failed initialize, or a provider that names no scope.
4. **The worst case is an older baseline.** A record that holds while the
   provider's answer would have changed can only leave the remote marker at an
   older commit. A later `--impacted` run elsewhere then starts from an older
   baseline, never a wrong one.

## Rejected alternatives

- **Core reads the provider's configuration to key the record.** This breaks
  the boundary of 10-caching.md: core would name files and variables the
  provider owns, and any new source in the provider would silently keep a
  stale record.
- **A record keyed by the commit alone.** A sign-in with a writing
  credential, another account or another endpoint would never publish until
  the next commit.

## Consequences

- An unchanged signed-in run on this repository starts no cache provider and
  no `cloud token` child for its marker. Its wall time goes from about 3.4 s
  to about 2.3 s.
- A run publishes again after any change of a scope file. A token refresh
  rewrites `auth.json`, so the next unchanged run publishes once, then skips
  again.
- `@putnami/cloud` names a scope for the signed-in user's recipe and for a
  token in a variable. A configured token command, a token URL, a metadata
  identity or a hosted run, whose credential arrives in `authenticate`, names
  none, and those runs publish every marker. Other cache providers keep
  publishing every marker until they declare the capability.
