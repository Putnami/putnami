# ADR 0001 — The observe action releases by copy

- **Status**: accepted
- **Date**: 2026-11-07
- **Scope**: `@putnami/observe` (`action/`), feature `tooling/observe-action`,
  and `.github/workflows/observe-action-release.yml`

## Context

GitHub users find and trust a CI step through the GitHub Marketplace. The
Marketplace lists an action only from the root `action.yml` of a public
repository that holds one action. The action runs `putnami observe`, whose
flags, docs and tests live in this repository, and the how-to "Observe a
change in your CI" documents both.

## Decision

The action's source is `tooling/observe/action/`: `action.yml`, the
`observe.sh` script it runs and its README. The gate of this repository tests
the script against a fake CLI, so the action and the observe docs change in
one pull request.

The public repository `Putnami/observe-action` holds only releases. The
manually dispatched workflow `observe-action-release.yml` runs `release.sh`,
which replaces the root of `Putnami/observe-action` with the action and the
licence, tags `vX.Y.Z`, and moves `vX` in one atomic push. It refuses a
version that is not `X.Y.Z` or that exists. It runs from `main` only, in the
GitHub environment `observe-action`, whose branch policy allows `main` and
which holds the only token that writes to `Putnami/observe-action`.

A workflow names `@v1`, a moving tag, to get the newest compatible release. A
release that breaks an input or a default takes the next major tag.

## Rejected alternative

The source lives in `Putnami/observe-action` with its own CI. Nothing in this
repository's gate would test it, and a change to `putnami observe` and its
action would take two pull requests in two repositories.

## Consequences

- A contributor never edits `Putnami/observe-action` by hand: the next release
  replaces its root.
- The first Marketplace listing, and acceptance of the Marketplace Developer
  Agreement, are web steps an organization owner takes on a release.
