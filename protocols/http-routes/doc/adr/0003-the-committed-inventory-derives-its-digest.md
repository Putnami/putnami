# ADR 0003 — The committed inventory derives its digest

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/http-routes` (`protocols/http-routes`),
  its producers (`@putnami/go` `build-describe`, `@putnami/typescript`
  `build-generate`) and its readers

## Context

A project commits its route inventory to `schema/http-routes.json`, and the
build fails when the committed bytes differ from what it regenerates
(protocols/extension ADR 0004). Until now the committed file was a verbatim
copy of the artifact under `.gen/`, `digest` line included. The digest is the
SHA-256 of every route: two pull requests that each change a different route
both rewrite that one line, so the second conflicts with the first even when
their routes merge cleanly. A rebase that regenerates the file resolves the
conflict, but it costs a rebase, a build and a hosted run per pull request.

The repository owner decided to remove the conflict at its
source: fewer conflicts are worth more than a committed digest, even with the
rebase that regenerates the file.

## Decision

1. **An inventory has two forms of one version.** The artifact form,
   `.gen/schema/http-routes.json`, carries `digest`; it is the file a build
   leaves for package and deploy, and nothing changes in it. The committed
   form, `schema/http-routes.json`, is the canonical artifact bytes without
   the `digest` member: the same `$schema`, `protocol` and `routes`, in the
   same order, indentation and trailing newline.
2. **`digest` becomes optional in `putnami.http-routes.v1`, not a new
   major.** The digest rule does not change: it is still the SHA-256 of the
   canonical `protocol`/`routes` projection, so an inventory keeps its digest
   whichever form carries it. A present digest must match, as before.
3. **A reader derives a missing digest.** The strict parsers of the Go module
   and of `@putnami/application` accept a manifest without `digest`, validate
   it as before, and return the manifest with the digest they computed, so a
   reader that consumes the digest gets the same value from either form.
4. **The producers write the committed form.** `build-describe` and
   `build-generate` write the artifact form under `.gen/` and promote its
   committed form, derived from the artifact by the module's
   `CommittedJSON`. The committed bytes stay a function of the routes alone,
   so a stale committed inventory still fails the build with
   `generated-output-drift`.
5. **Readers upgrade first, as ADR 0002 orders.** A reader built before this
   change refuses a committed inventory without a digest, which fails closed:
   it applies nothing. A reader that pins this module upgrades before it
   reads a committed inventory a new producer wrote. A reader of the artifact
   form needs no change.

## Rejected alternatives

- **`putnami.http-routes.v2` without `digest`.** Every reader would carry two
  decoders for the same routes, and an old reader fails closed on either
  change.
- **Dropping `digest` from the artifact form too.** Deploy and its records
  carry the artifact; they would each recompute a value the build already
  states.
- **Keeping the committed digest and relying on the regenerating rebase.**
  It resolves each conflict after the fact, at the cost of a rebase and a run
  per pull request.

## Consequences

- Two pull requests that change different routes no longer conflict on the
  committed inventory; they conflict only where their routes do.
- The committed and the artifact bytes differ by one member. A tool that
  compares them strips `digest` from the artifact, or compares the routes.
- Hand-written committed inventories, such as the samples of the stacks
  without a route producer, carry no digest either; a present one is still
  checked.
- An out-of-repository reader that parses committed inventories with a
  pinned module, such as a source declarations reader, needs the upgrade
  before the producers it reads emit the committed form.
- This repository cannot keep the readers-first order for its own
  inventories: its producers build from this tree, and drift fails every
  build whose committed inventory keeps a digest, so they are regenerated in
  the change that introduces the committed form. Deploy reads the artifact
  form and is unaffected. A pinned reader of committed inventories refuses
  this repository's inventories, as a whole and without applying any, until
  it upgrades the module.
