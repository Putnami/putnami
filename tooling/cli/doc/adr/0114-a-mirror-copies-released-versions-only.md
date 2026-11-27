# ADR 0114 — A mirror copies released versions only

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: `@putnami/cli` (`internal/jobs/publication`: the mirror intent of
  the release request)

## Context

`distribution.registries.<ecosystem>.mirror.to` in `putnami.ci.json` asks the
release-set provider to copy members to an external registry such as
npmjs.org. `publish` captured the destination on every publication and sent it
with every release request (protocols/distribution ADR 0004, rule 3). The
provider copies every member of the set whose stored visibility is `public`.

Visibility resolves along the chain repo, registry, channel, version, set,
member, and the finest level that states a value wins. A repository that makes
its framework packages public with a `members[]` rule therefore makes them
public on every channel, `canary` and `pr-<number>` included, whatever those
channels declare. With a mirror declared, each push to `main` and each
pull-request publication would have copied its prerelease versions to the
external registry, where a published version cannot be freely withdrawn.

The mirror intent is keyed by ecosystem, and the provider, not the CLI, decides
which members are eligible. Omitting an entry requests no copy and never
revokes accepted work (ADR 0004, rule 6).

## Decision

1. **`publish` carries an ecosystem's mirror destination only when every
   member of that ecosystem in the released set carries a release version**:
   a version with no prerelease part, the shape the visibility chain calls
   `stable`: `MAJOR.MINOR.PATCH`, with an optional `v` and build metadata
   (`0.5.0`, `v0.5.0`). Any other shape, the `0.0.0` placeholder included,
   is not a release. A
   member at `0.5.1-20261126120000-abcdef1`, as every untagged publication
   produces, withholds its ecosystem's destination from the request.
2. The check reads the final set, inherited members included: the provider
   copies the whole set, so a member another line left at a canary version
   withholds the copy as surely as a republished one.
3. A withheld destination never fails the publication. `publish` releases its
   channels and prints one line per withheld ecosystem, naming the first
   prerelease member in coordinate order.
4. There is no option to copy prerelease versions. A repository that wants them
   on an external registry publishes there directly.

## Consequences

- A tagged release copies only once every member of the ecosystem is at a
  release version, whatever line it belongs to. This repository tags every
  line on the release commit, so a release copies. A release of one line
  alone, while another line of the same ecosystem sits at an untagged version,
  copies nothing for that ecosystem and prints the withheld line: the safe
  side of the rule. Copying such a release needs the provider to filter
  members itself, a field the mirror intent does not have.
- An untagged publication that only inherits released members, such as a
  docs-only push after a release, requests the copy again. The provider
  identifies a copy by member, digest and destination, so the request is
  idempotent.
- The CLI still selects no member and computes no visibility: it decides only
  whether to carry a destination at all.
