# ADR 0020 — `upgrade` resolves a channel on each ecosystem's own registry

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli/internal/commands/lifecycle/upgrade.go`),
  `@putnami/ts` (`typescript/extension/cmd/putnami-ts/deps_upgrade.go`),
  `@putnami/go` (`putnami-go deps-upgrade`, `go/extension/internal/jobs/depsupgrade`)

## Context

A release-set namespace names the **publisher**, not the consumer. A consuming
workspace does not know that namespace, so a channel resolved through the
release-set provider works only in the workspace whose name happens to equal
the publisher's.

The release set exists for atomicity across ecosystems at publish time and for
a replayable id. Neither requires a consumer to use it to follow a channel,
because every ecosystem publishes the channel itself, and the release-set
`release` operation writes those native selectors in the same transaction that
stores the set:

| Ecosystem | Native channel |
|---|---|
| npm | `dist-tags[<channel>]` in the packument |
| Go | `@v/<channel>.info` on the module origin (`@latest` for `latest`) |
| put (CLI, extensions, templates, agent workflows) | `download?channel=<channel>` |

## Decision

**`upgrade --channel <c>` resolves each ecosystem through its native channel.
No release-set call, and no namespace derived from anything.** `stable`, and no
selector at all, read `latest`.

1. The CLI passes the channel name down and does not resolve it. Each language
   extension owns its own registry protocol. Core adds no npm or Go module
   proxy reader: its only npm reader stays the pinned package-membership
   adapter in `internal/extension/discovery.go`.
2. `@putnami/ts` reads `dist-tags[<c>]` **per package**, from the registry the
   workspace `.npmrc` `@putnami:registry=` line names, presenting the
   `_authToken` npm itself would use. A dist-tag is per package: a package
   left out of a publication keeps pointing at its previous release, and that
   is the answer the upgrade uses. Anonymous metadata hides versions a private
   registry only shows to an authenticated client.
3. `@putnami/go` reads `@v/<c>.info` from the module origin **per module**,
   the exact query the `go` command sends for a non-semver selector, instead
   of `go list -m`, whose answer depends on `GOPROXY`/`GONOPROXY` ordering. An
   impacted publication leaves each module at the version of the publication
   that last changed it, so two modules on one channel differ. `latest` uses
   the origin's `@latest` endpoint, which orders by publication time
   ([ADR 0018](0018-ordered-prerelease-versions.md)).
4. `upgrade --cli` and `upgrade --extensions` resolve archive members from the
   put projection of the channel (`channel=` on the put registry) and send the
   user's credential. The workspace `registries.put.registry` entry wins over
   `PUTNAMI_REGISTRY_URL`.
5. Every failure names the coordinate, the channel, the registry host and the
   HTTP status, because those four facts pick the fix: publish the channel, fix
   the credential, or fix the registry.
6. Each ecosystem resolves **every** coordinate and reports the table
   (package, version, the source revision the version carries, and the source:
   dist-tag, go-proxy or release-set) before it writes anything, and rolls its
   metadata back if a later step fails. The dependency run stops at the first
   failing ecosystem, so a channel that npm serves and the Go origin does not
   moves neither. `--continue-on-error` lets each ecosystem report its own
   outcome instead.
7. **`--release <id>` keeps the release set** and requires
   `--namespace <namespace>`. A release-set id is scoped by the namespace that
   published it; the consuming workspace does not know that namespace, so it
   is asked for rather than guessed. `--namespace` is rejected with any other
   selector. `--release`, `--channel` and `--version` are mutually exclusive.
8. `publish` is unchanged: the release set remains the atomic publish cohort.

## Consequences

- A workspace with any name can follow a channel, with no release-set
  configuration and no Cloud provider installed.
- Only `--release` needs the `cloud-release-set` provider, and it fails closed
  when the provider is absent.
- **Cross-ecosystem atomicity is given up on the channel path.** A channel that
  moves between the npm read and the Go read can mix two publications. That is
  inherent to reading mutable tags. `--release <id> --namespace <ns>` remains
  the atomic selector for a build that must pin one exact publication.
- `@putnami/ts` never compares a release-set namespace to the workspace name.
  The orchestrator validates the response against the exact request it made.

## Rejected alternatives

- **Keep the release set and add a workspace-level distribution namespace.**
  It keeps every consumer dependent on a Cloud provider to read a name each
  registry already publishes, and adds a configuration key whose only correct
  value is the publisher's.
- **Resolve both ecosystems in the CLI before dispatching the job.** It buys
  one cross-ecosystem gate at the price of a second npm-manifest reader inside
  core and a second resolution of every coordinate. The per-ecosystem
  guarantee (resolve everything, write, roll back on failure) plus a run that
  stops at the first failing ecosystem covers the same ground.
