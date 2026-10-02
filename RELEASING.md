# Releasing Putnami

This document states when Putnami releases, what a release may contain, the
checklist a release must pass, and who can roll one back.

Who approves each step is in [GOVERNANCE.md](GOVERNANCE.md#how-decisions-are-made).
What the release promises to users is in the
[First Public-Release Contract](README.md#first-public-release-contract). The
release *plan* it executes — candidate platforms, the read-only rehearsal, and
the intended license transition — is [RELEASE.md](RELEASE.md).

## Cadence

Before v1.0.0, releases are **event-driven, not calendar-driven**. A release is
cut when the [release checklist](#release-checklist) passes and the release owner
approves it. The project does not promise a release every week or every month,
and it does not hold a finished fix back to fill a calendar slot.

Three bounds are commitments rather than intentions:

- A **security fix** for a confirmed vulnerability in the supported core ships as
  a patch release, or as a documented mitigation, on the clock in
  [SECURITY.md](SECURITY.md#patch-policy).
- A **breaking change** to the stable core ships with the migration the user has
  to run, named in the release notes. Pre-1.0 breaking changes are allowed; a
  breaking change a user has to discover is not.
- A **version window does not move in a patch.** Which artifact versions a build
  reads is stated in
  [Compatibility and Migration](tooling/cli/doc/21-compatibility-and-migration.md),
  and every number there is pinned by a test against prior-release bytes.

The post-1.0 cadence is decided before v1.0.0 ships and recorded in this section.
Until then, treating the absence of a calendar as an oversight would be reading a
promise the project has not made.

## What a release may contain

| Release | May contain | Must not contain |
|---|---|---|
| Patch (`0.y.Z`, before 1.0) | Fixes and features that break nothing, with their tests | A breaking change, or any change to a version window |
| Minor (`0.Y.0`, before 1.0) | Breaking changes to the stable core, with their migrations | A silent breaking change — one without release notes naming the user action |
| Patch (`x.y.Z`, from 1.0) | The fix and its test, cut from the released tag | A new feature, an unrelated dependency bump, or any change to a version window |
| Minor (`x.Y.0`, from 1.0) | Features that break nothing | A breaking change |
| Major (`X.0.0`, from 1.0) | Whatever the version contract of the day allows | — |

Pre-1.0, minor releases do not promise backward compatibility with earlier
minors. That is the published contract, not an accident, and it is the reason
only a minor may break: a pre-1.0 patch is the one release users can take
without reading anything. npm and Cargo read `^0.3.0` as `>=0.3.0 <0.4.0`, so a
feature shipped in a minor would stop every automatic upgrade for nothing to
migrate; `putnami version` proposes a patch for it.

The table holds for the projects `putnami.support.json` lists as `stable`. A
`preview` or `experimental` project, or one the catalog does not list, promises
no compatibility, so its changes, breaking ones included, ship in a patch; a
breaking one still names its migration in the release notes.

## Release checklist

Every item names the evidence that satisfies it. An item that cannot be
satisfied is either fixed or waived through the
[exception path](GOVERNANCE.md#the-exception-path) — never skipped.

1. **The gate is green on the whole workspace.**
   `putnami lint,test,build,validate --all --enforce-coverage`, the same tasks
   Putnami Cloud's native CI runner executes (see
   [Maintainer CI](CONTRIBUTING.md#maintainer-ci)).
2. **The compatibility budget still matches behavior.** The prior-release gates
   in `protocols/cli`, `protocols/extension` and `tooling/cli/internal/lockfile`
   pass, the version-budget gate in `protocols/support` passes, and
   [Compatibility and Migration](tooling/cli/doc/21-compatibility-and-migration.md)
   states every window this release ships.
3. **The support catalog matches the reviewed decisions.**
   [`putnami.support.json`](putnami.support.json) is strict, canonical, and
   current, and any status change in this release was approved as a
   [promotion or demotion](GOVERNANCE.md#support-status-promotion-and-demotion).
4. **The contributor path works with no Putnami Cloud credentials.** The neutral
   path in [CONTRIBUTING.md](CONTRIBUTING.md#contributor-ci-without-putnami-cloud-credentials)
   is executed by the repository's own tests with every credential removed and
   every remote endpoint refusing requests.
5. **The public-cut gate is green,** and neither of its registers grew.
6. **Release notes exist,** naming every breaking change, its migration, the
   security fixes that are now public, and any exception this release carries.
   [CHANGELOG.md](CHANGELOG.md) is updated in the same change.
7. **A rollback plan is named before the release,** not after: the version users
   pin to if this one is bad, and who is on the hook to say so.
8. **The CLI provenance record is complete.** It follows the
   [release provenance policy](tooling/cli/doc/release-provenance.md): full
   source commit and clean-tree state, exact Putnami version, builder actor or
   execution-run identity, every target's authoritative SHA-256, all five smoke
   results, and the release owner's approval. Current archives are unsigned and
   are not claimed to be byte-for-byte reproducible; the record must say so
   instead of upgrading the checksum into a signature or reproducibility claim.
9. **The public golden path works against the candidate's own channel.** Publish
   the candidate to a pre-release channel. When an external release execution
   plane is configured, it must run
   `tooling/cli/scripts/smoke-check-release.sh <channel>` on actual
   `darwin/amd64`, `darwin/arm64`, `linux/amd64`, and `linux/arm64` runners, and
   `tooling/cli/scripts/smoke-check-release.ps1 <channel>` on a `windows/amd64`
   host created for the run and deleted after it, before promotion, retaining
   `SMOKE_DIAGNOSTICS_DIR` on failure. Each fetches its public installer
   (`install.sh`, or `install.ps1` on Windows), executes the exact TypeScript
   init/serve path, verifies generated locks/config/MCP state, and requires
   typed readiness, a real HTTP response, and graceful shutdown. Each hands
   `<channel>` to `init` as `PUTNAMI_CHANNEL`, so the extensions, the template
   and the starter's dependencies come from the candidate's release set, not
   from `latest`; `<channel>` is a channel name, never an exact version. The
   Windows host has no Docker, so `putnami compose` is not smoked there. The
   plane also runs `tooling/cli/scripts/smoke-first-use.sh <channel>` on
   `linux/amd64` and `linux/arm64` runners with Docker: it pastes the
   documented block into an image that holds Bash, `curl`, `tar` and a SHA-256
   tool and nothing else, then runs `putnami lint,test,build` and the Go path.
   A failed or missing runner blocks promotion and is escalated to the release
   owner. If
   the execution plane is not configured or is unavailable, the release owner
   runs the same five candidate jobs manually: four Unix runners and one
   Windows host; one host is not a substitute. Any configured plane also runs
   `latest` nightly through the default public installer URL on the four Unix
   runners, and escalates failure before the next release. The Windows host
   exists only for a run, so `latest` has no nightly Windows job. Until that
   automation is configured or restored, the release owner runs the same
   four-target Unix `latest` smoke nightly. This post-publication schedule
   remains outside maintainer CI by the accepted neutral-CI decision — see
   [Installing the CLI](tooling/cli/doc/22-installing-the-cli.md#the-release-smoke).
10. **The release owner approves,** and the approval is recorded with the
   completed checklist.

The public-root switch is not on this checklist. It is a separate, explicitly
approved human operation, and no automated step performs it. The release owner
renames the private repository to the archive name, pushes one signed root
commit of the released tree to a new private repository, publishes from that
root, and makes the new repository public last. The archive keeps the full
private history and is never published.

## Version lines and their tags

A version is derived from git, per **version line** — a scope whose
`putnami.json` declares a `line` block with the pattern of its tags
(`protocols/workspace` ADR 0002). This repository has six:
`typescript` (`ts/v{version}`), `go` (`go/v{version}`), `tooling`,
`protocols`, `python`, and `sites` (each `<scope>/v{version}`).

```sh
putnami version get                        # what every line is at, right now
putnami version tag --scope tooling        # print the proposal, write nothing
putnami version tag --scope tooling --yes --push
```

A commit carrying its line's tag has the tag's version, with no suffix. Any
other commit takes the line's last tag advanced by the conventional commits
that touch the line — breaking is a minor before 1.0, `feat`, `fix` and `perf`
a patch, anything else nothing, and a commit that touches no `stable` project a
patch at most — floored at a patch, plus the ordered
suffix `<yyyymmddHHMMSS>-<sha>`. `version tag` regenerates
`<line>/CHANGELOG.md` from those same commits, creates the release commit that
carries it, then the annotated tag on that commit.

A shallow clone is refused: its history and tags are a fraction of the
repository, so every version computed from it would be a plausible number
derived from an arbitrary cut of the past. Run
`git fetch --unshallow --tags` first.

### Seeding the lines (once, on the public root commit)

The public root has no tags, so every line computes `0.0.0-<suffix>` until it
carries its first tag. `putnami version tag` would propose `<line>/v0.0.1` and
add a release commit, so the release owner seeds the six lines by hand, with
signed tags on the root commit:

```sh
ROOT=$(git rev-parse main)
for tag in ts/v0.2.0 go/v0.2.0 tooling/v0.2.0 protocols/v0.2.0 python/v0.2.0 sites/v0.2.0; do
  git tag -s "$tag" -m 'Putnami 0.2.0' "$ROOT"
done
putnami version get                        # every line reads 0.2.0
```

After the tagged publish succeeds, push the six tags by name. Never run
`git push --tags`: it also pushes every other tag of the local clone.

```sh
git push origin ts/v0.2.0 go/v0.2.0 tooling/v0.2.0 protocols/v0.2.0 python/v0.2.0 sites/v0.2.0
```

## Publication

`canary` is a release-set channel served by `@putnami/cloud`. Every push to `main`
publishes it, and the rule that says so lives in `putnami.ci.json`:

```sh
putnami publish --impacted --channel canary   # the default on main
putnami publish --all --channel canary        # manual full-republication lever
```

`--impacted` resolves the channel head once and republishes exactly the members
whose **selection fingerprint** — the execution key of the member's package task
minus the embedded version — differs from the head's record, plus their
dependents; every unchanged member inherits its head record. `--all` republishes
every member. Both release the new snapshot with one compare-and-swap from the
resolved head; an empty channel resolves to a null head and needs no special
mode (see
[ADR 0021](tooling/cli/doc/adr/0021-publish-version-and-deploy-follow-distribution-v2.md)).

`--channel a,b` advances several channels to the same snapshot in one
transaction: impact is measured against the first, and a conflict on any one of
them writes nothing on any of them.

### A tag publishes its line as a cohort

Pushing a line's tag is the release act. On that commit `putnami publish` needs
no selection flag and no channel: it republishes every member of the line at the
tag's version and asks the provider for an **immutable** channel named after the
tag in the portable encoding — `ts/v0.3.0` becomes `ts-v0.3.0`. That channel
accepts one head and refuses every later move, so the tag names one snapshot
forever. A dirty tree is refused.

### Promotion is a channel move

`latest` is declared `protected` in `putnami.ci.json`: no publish advances it.
Promotion is one provider call by a person, with no publisher, no build and no
checkout:

```sh
putnami channel set latest --from ts-v0.3.0   # promote the tag's immutable channel
putnami channel status latest --wait 5m       # desired versus observed per registry
```

Rolling back is the same command pointed at the previous immutable channel or at
an exact `rs_<64 hex>` id. It never rewrites an artifact or an existing release
set.

The successful publish result is the deployment authority: automation forwards
`data.releaseSet.ref.id` and `.digest`, never the channel name as a request to
resolve again. An environment follows a channel and synchronizes with
`putnami deploy --env <name>`.

## Rollback

**Releases are immutable.** A published version is never rewritten in place: a
bad release is superseded by a new patch release, and the bad version is called
out in the superseding release's notes.

| Question | Answer |
|---|---|
| Who decides to roll back | The release owner |
| Who can stop a release in progress | Any maintainer, without approval — stopping restores the state the project was already in |
| What "rollback" means | Ship a superseding patch, and tell users the version to pin until they take it |
| How a user rolls back | `putnami pin <previous-version>` in the workspace, or `putnami upgrade --version <previous-version>` for the machine-wide CLI |
| What is never a rollback lever | An environment variable that re-enables an old wire format. The rollback is a version pin ([ADR 0002](tooling/cli/doc/adr/0002-cli-vnext-contracts.md) §4) |

A pin is fail-closed: it records a SHA-256 per platform and refuses to run a
binary that does not match, so rolling back is an explicit, verifiable act
rather than a hope about which build is installed. See
[Version Management](tooling/cli/doc/14-version-management.md#pinning-the-cli-per-workspace).

## Security releases

A security release follows this checklist with two differences: the fix may be
developed privately until it ships, and its release notes are written to be
readable by someone deciding whether they are affected. The triage clock, the
severity bands, and the patch policy live in
[SECURITY.md](SECURITY.md#patch-policy).
