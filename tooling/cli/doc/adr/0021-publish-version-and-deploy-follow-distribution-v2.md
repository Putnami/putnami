# ADR 0021 — Publish, version, and deploy follow the distribution v2 model

- **Status**: accepted, superseded in part by [ADR 0061](0061-channel-and-ci-commands-leave-the-core-cli.md)
- **Scope**: `@putnami/cli` (`tooling/cli`), `@putnami/extension-sdk`
  (`tooling/extension-sdk`), and the publishers of `@putnami/typescript`,
  `@putnami/go`, `@putnami/cloud`

## Context

Distribution v2 is the distribution model: a release set for every artifact, several
members per project, several channels per release, metadata-only promotion,
versions derived from git, version lines, environments that follow channels,
and a workload contract for deploy. The
[distribution protocol](../../../../protocols/distribution/README.md) defines the
release set and its channels. This ADR is the CLI's share of it. The plan
and the release call themselves follow
[ADR 0017](0017-one-release-set-plan-against-the-channel-head.md).

## Decision

### Publish and release

1. **Every listed channel head is resolved before the plan.**
   `publish --channel a,b` measures impact against the first head and inherits
   from it; the release advances every listed channel with its own
   expectation. Channel names are portable, deduplicated, and bounded by the
   protocol's per-release maximum.
2. **The selection fingerprint comes from the engine.** It is derived from the
   deterministic execution key of the member's declared package step, with the
   embedded version omitted. The engine computes that key even when the task
   disables cache restoration. A provider may name a distinct installed package
   publisher for that step; omitted, the metadata publisher is the default. The
   metadata provider remains the publication owner, and the planner keeps both
   extensions and follows the package job's real dependency closure.
3. **A project yields several members.** The plan keys members by
   `(ecosystem, coordinate)`. The coordinator admits every ecosystem an
   installed extension declares and names none itself. A candidate version
   follows the owning profile's `Version.Pattern`; the `v`-prefixed spelling is
   used when the bare one fails and the prefixed one passes.
4. **A tagged commit publishes a cohort.** When HEAD carries a tag matching its
   line's pattern, `publish` republishes every member of that line at the tag's
   version, and asks the provider to create an immutable channel named after
   the tag in the portable encoding (`ts/v0.3.0` becomes `ts-v0.3.0`). A
   member of another line inherits its head record when the head carries it.
   A dirty tree is refused. When HEAD carries the tags of several lines,
   `--scope <line>` names the cohort, and without it the publish is refused. A tagged publish widens the project
   selection to every project, like an impacted one, so the selection
   fingerprints do not move between a tagged and an untagged publish.
5. **`putnami channel set <c> --from <channel | rs_id>`** is promotion and
   rollback: one provider call, no publisher, no build, no checkout.
   `putnami channel status <c>` shows desired versus observed per registry. A
   channel that `putnami.ci.json` marks protected moves only through
   `channel set`; `publish --channel` refuses it.
6. **The release carries declared policy; the CLI computes none.** It carries
   the visibility chain read from `putnami.ci.json`, with every member selector
   resolved; `publish --visibility <level>` fills the set level. It carries the
   mirror intent `distribution.registries.<ecosystem>.mirror.to` as
   `ReleaseRequest.mirrors`. The CLI validates the bounded destination syntax,
   copies both at planning time, and sends the copy at finalization, including
   when the set is already current, so a new mirror destination needs no
   repackaging. It never decides which members are public, widens visibility,
   or handles provider credentials. The provider owns mirror acceptance, retry
   and completion
   ([Distribution ADR 0004](../../../../protocols/distribution/doc/adr/0004-mirror-intent-in-the-release-transaction.md)).
   Channel promotion adds no mirror policy.
7. **Without a release-set provider**, a full publish (`--all`, or a tagged
   commit) publishes every member to the declared registries and moves no
   channel. `publish --impacted --channel` is refused, because it needs a head.
   `putnami ci validate` refuses `distribution` and `envs` in
   `putnami.ci.json`.

### Version

8. **The version is derived from git.** A tagged commit carries the tag's
   version. Otherwise the version is `<next>-<suffix>`, with the ordered suffix
   of [ADR 0018](0018-ordered-prerelease-versions.md). `<next>` starts from the
   line's last reachable tag and advances by the conventional commits that
   touch a project of the line, attributed by changed files: `!` or
   `BREAKING CHANGE:` is minor before 1.0 and major after, `feat` is minor,
   `fix` and `perf` are patch, anything else does not advance. A pre-release is
   always at least a patch above the last tag. A line with no reachable tag
   starts at `0.0.0` with no advance. A shallow clone degrades an ordinary build
   to `0.0.0`; a publication refuses it. D-004 in the root `decisions.json`
   supersedes the `feat` rule: before 1.0 a `feat` is a patch. D-005 caps at a
   patch a commit that touches no project the support catalog lists as stable
   and no unlisted project a stable one depends on.
9. **`putnami version tag --scope <line>`** regenerates the line's changelog
   from the same commits, creates the release commit, then the annotated tag on
   it; `--push` pushes both, `--yes` skips the confirmation, and an explicit
   name overrides the computed one. The changelog is rendered once into the tag
   message (written with `--cleanup=verbatim`, so markdown headings survive),
   the GitHub release when the repository is on GitHub, and
   `<line>/CHANGELOG.md`.
10. **`validate` refuses** a pull request or squash title that is not a
    conventional commit, and a project that belongs to no line or to two.

### Upgrade and install

11. **`upgrade --channel` stays native per ecosystem**, as
    [ADR 0020](0020-upgrade-resolves-channels-natively.md) decides.
12. **Native credential stores follow a contract.** Writers touch only entries
    for Putnami hosts, merge non-destructively, preserve every other account,
    write atomically with `0600`, and refuse a credential that is not one line.
    `cloud logout` removes only Putnami entries. No publish path scrubs a
    store.

### Deploy

13. **`deploy --env <name>`** resolves the environment's channel from
    `putnami.ci.json`, or takes `--release <id>`, and synchronizes the selected
    workloads on the named set, skipping members already served at the same
    digest and config. Selection uses `--projects`, `--tag`, `--impacted`, and
    the environment's `workloads` rules. `--env` and `--release` are core job
    flags, not global flags. A deploy in the same session as a publish takes
    the set that session releases: after the release, the barrier completes
    each workload contract with the set ref and the member digests that
    workload owns. Such a session refuses `--release`, because two sources for
    one deploy would silently ignore one of them.
14. **Workload contract.** A workload is a project with an `oci` member plus
    optional `put` config and migration members from the same set. Order:
    migrations forward, config, image. Migrations are forward-only; a
    synchronization to a set behind the applied migrations proceeds only when
    the migration protocol marks the migrations in between reversible or
    compatible, otherwise it is refused for that workload and reported.
    Progressive rollout applies to the image only.

## Consequences

- The Docker publisher pushes by digest and tags the version only; channel
  tags are written by the release projection.
- Every publisher recognizes an existing immutable version and reuses its
  digest, which keeps a repeated tagged publish idempotent.
- `--stable`, `upgrade --branch`, `publish --also-branch-tag`, `version set`,
  `version bump`, and a declared base version do not exist.
