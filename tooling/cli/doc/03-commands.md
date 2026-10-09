# Commands Reference

> **The command list itself is generated, not written here.**
>
> ```bash
> putnami help                 # categorized command list with global flags
> putnami help <command>       # one command: usage, subcommands, flags, examples
> putnami help --markdown      # the full reference as Markdown
> putnami help --man           # the full reference as a man page
> ```
>
> Every command surface — help, man, Markdown, shell completion, structured
> help — is derived from a single command catalog
> ([ADR 0001](adr/0001-cli-foundation-boundaries.md) §1). A hand-maintained
> table restating it in this file would be a second vocabulary that drifts, and
> the ones that used to live here did. This document covers what the catalog
> cannot carry: the concepts, and the behavior behind each command.

The CLI provides two categories of commands: **job commands** provided dynamically by extensions, and **structured commands** with fixed subcommand trees.

## Job Commands

Job commands are provided by extensions and operate on workspace projects: an
extension's manifest declares which commands it serves, so the set a workspace
has depends on the extensions it installs. `build`, `test`, `lint`, `serve`,
`format`, `package`, `publish`, `deploy` and `generate` are the conventional
ones. `putnami help` lists the commands the *current* workspace's extensions
actually provide — there is no fixed list, which is why one is not printed here.

Job commands support the full set of project selection and execution flags described below.

### Multi-Command Execution

Multiple commands can be run in a single invocation using comma separation:

```bash
putnami lint,test,build --impacted
```

All commands share the same project selection and execution flags. They run sequentially in the order specified. If a command fails and `--continue-on-error` is not set, subsequent commands are skipped.

### Publish and `--dry-run`

`putnami publish` pushes packaged artifacts to registries (`sideEffects: "registry"`); a project's declared `publish` channels auto-activate the matching steps, so `--npm`/`--go`/`--docker` flags are rarely needed.

`putnami publish --all --dry-run` is special-cased: instead of the plan-only preview `--dry-run` means everywhere else, the publish jobs **execute in their own declared dry-run mode** — each publisher prints the artifact set it would push (registry, name, version, tags) and the run ends with the aggregated `Artifacts: … (dry run)` summary. A dry run writes nothing to a registry. It reads: each publisher asks its registry whether it already holds the member at the planned version (see [Registry checks](#registry-checks)). Two guarantees make that safe:

- A task whose traits declare side effects but whose manifest does **not** declare a `dry-run` flag is excluded from the plan (loudly, with its dependents) — an extension that cannot interpret the param can never publish for real under `--dry-run`.
- The synthesized `dry-run` param is part of cache and run-marker keys, so a dry-run publish never satisfies a lookup a real publish would have written.

Side-effect-free dependencies (`package~*`) still run — dry-run output is computed from real local artifacts, and the cache makes them cheap. `--plan` remains the pure DAG preview. Only the exact single-command form opts in: a mixed list such as `putnami build,publish --dry-run` keeps preview semantics.

#### Registry checks

A registry refuses to overwrite a published version. A dry-run publish
therefore asks each target registry, with GET or HEAD requests only, whether it
already holds each member at the version the publish would write. Each request
carries the credential the real publish asks for: the host-only credential
seam carries the registry host and nothing else, so the cloud decides what the
credential grants. A public member needs no credential: when none resolves,
the request is anonymous.

When every job has ended, the CLI prints one report on standard error:

```text
putnami: publish --dry-run registry checks
  conflict    npm @acme/widget@2.0.0 at https://registry.npmjs.org: the registry already holds this version with another digest; the real publish cannot overwrite it
  unverified  go example.com/mod@v2.0.0 at https://go.example.com: the registry could not be reached: connection refused
  reused      oci acme/api@2.0.0 at registry.example.com: the registry holds the same digest sha256:…, so the publish reuses it (compared with the image the last `package` staged; re-run `package` if the source changed since)
  absent      3 member(s) are not in their registry: the publish uploads them
  warning: publish will move tag 2.0.0 of acme/worker on oci.putnami.dev from sha256:… to sha256:…
```

| State | Meaning | Dry run |
|-------|---------|---------|
| `absent` | The registry does not hold the version. | Passes. |
| `identical` (printed `reused`) | The registry holds the version with the same content digest. The publish reuses it. When the compared artifact is one the last `package` staged, the line names it. | Passes. |
| `tag-move` (printed as a warning) | The managed registry `oci.putnami.dev` holds an image's version tag at another digest. The publish moves the tag to the image it builds. On any other registry the same answer is a `conflict`, because that registry may refuse the move. | Passes. |
| `conflict` | The registry holds the version and the real publish cannot reuse it: the digest differs, the dry run has no artifact to compare, or the registry may have released the version, which the publisher refuses. The reason says which. | Fails. |
| `unverified` | The registry gave no usable answer: no network, a timeout, a 401 or 403, a server error. | Fails. |

A dry-run `package` builds no artifact for some publishers. Under a release-set
plan such a publisher compares with the artifact the last real `package` left;
with none, the conflict names the remedy: run `putnami package` for the project
without `--dry-run`, then the dry run again.

The run exits non-zero when any check is `conflict` or `unverified`, and when
a probe under a release-set plan speaks about a member the plan does not select
or at another version than the planned one. The failure names every such
member, its registry and its version, so one dry run shows every conflict of
the release. With `--output=json`, the failed row of the run also carries one
error diagnostic per such member. A dry run without network access fails as
`unverified`: it never passes without an answer.

The report also prints warnings, which do not fail the run:

- `publish will move tag …`: a `tag-move` on `oci.putnami.dev`, with the
  registry digest and the local one, or `the image this publish builds` when
  the dry run has no image.
- `not probed`: under a release-set plan, a selected member whose publisher
  reported no check, with the publisher and the publish step, or with the
  statement that the publish route is unknown.
- An `absent` answer obtained without a credential. A registry answers such a
  request for a private member as it does for a missing one. Sign in and run
  the dry run again to confirm.
- No publish step reported a check, for a publish without a plan.

A dry run without a credential reads anonymously. A private registry that
refuses the read fails the dry run as `unverified`.

`--quiet` keeps the failures and the warnings. The design is recorded in
[ADR 0056](adr/0056-a-publish-dry-run-asks-each-registry.md).

### Publish to a release-set channel

The release-set namespace comes from `distribution.namespace` in `putnami.ci.json`.
It can differ from the workspace name: resolve, publication and channel expectations
all use that declared provider identity. Without a declared namespace, the workspace
name remains the default. Native package coordinates keep their own declared names;
the provider still checks publication authority.

`putnami publish --channel <channel>` names the channels the release-set coordinator advances. Several are allowed: `--channel canary,staging` measures impact against the **first** head and advances every listed channel to the same snapshot in one transaction. Every name must be portable (`^[a-z0-9][a-z0-9._-]{0,63}$`), and a channel the repository declared `protected` in `putnami.ci.json` is refused before any provider call — a protected channel moves only through `putnami channel set`.

`putnami publish --baseline-channel <channel>` names one channel the publication **reads and never advances**. It is the head impact is measured against when the first channel of `--channel` has no head yet: the first push of a pull request advances an empty `pr-7` and measures against `canary`, so it republishes what the branch changed instead of the whole workspace, and `canary` — which belongs to `main` — is not moved. As soon as the advanced channel has a head of its own, that head is the baseline again and the flag changes nothing. The name is portable like any channel, must not be one of the channels `--channel` advances, and requires both a channel to publish and `--impacted`: a publication that advances nothing falls to the legacy per-package path where no head is read, `--all` republishes everything by definition, and a tagged publish releases its whole version line. Each of those is refused rather than ignored, the way the document half refuses a `baseline` on a rule that publishes nothing. A protected channel is a valid baseline, because protection forbids advancing a channel, not reading it. The one resolve of the publication names the advanced channels plus this one, so 16 advanced channels leave no room for a baseline and the flag is refused at parse time. The rule that renders it in CI is `rules[].baseline` in `putnami.ci.json`; `--baseline` remains the global git ref `--impacted` resolves against, which a release-set publish never uses (D16).

Every selection is accepted. The coordinator, not the flag, decides which members are republished:

| Selection | What the coordinator does |
|---|---|
| `--impacted --channel <c>` | Resolves every listed head once — plus the `--baseline-channel` head, when one is named — republishes every member whose **selection fingerprint** differs from the baseline head's record plus every member that depends on one, inherits every unchanged member from that head, and advances each **listed** channel by compare-and-swap from its own resolved ref. A member the head does not know is new and is published. |
| `--all --channel <c>` | Resolves every listed head once, republishes **every** member, and advances each channel from its own resolved ref. This is the manual full-republication lever: it never depends on what a head recorded. |
| a tagged commit | HEAD carrying its line's tag publishes that line as one cohort: every member of the line, at the tag's version, plus an immutable channel named after the tag in the portable encoding (`ts/v0.3.0` becomes `ts-v0.3.0`). No selection flag and no `--channel` is needed, and a dirty tree is refused. With several tags on HEAD, `--scope <line>` names the one being released. |

An empty channel resolves to a null head: every member is selected and that channel's release expects no head, unless `--baseline-channel` names a channel that does have one, in which case the unchanged members are inherited from it and only the changed ones are republished. There is no bootstrap mode.

Impact is measured against the channel head, never against a git baseline. A member's **selection fingerprint** is derived from the deterministic execution key of its declared package step with the embedded version omitted, whether or not that task permits cache restoration: a change to the sources, to the packager (its implementation, not a version-only rebuild; see [Extension Implementation](10-caching.md#extension-implementation)), to the toolchain, to a task contract, or to any upstream task moves it, and nothing else does. The run's selection reports `baseline: <head id>` with source `release-set-head`, or `release-set-baseline-channel` when that head came from `--baseline-channel` rather than from the channel being advanced, so a run that follows several skipped CI builds publishes exactly the members whose recipe changed since the head it names.

Members are keyed by `(ecosystem, coordinate)`, not by project: one project yields one member per publication it actually configures, so a project can publish an npm package and an image and appear twice in the same snapshot, while a Docker-only service contributes no accidental Go or npm member. Which ecosystems exist is not a CLI constant — each installed extension declares its own profile in `putnami.extension.json`, and `oci` is declared by the extension SDK. The metadata envelope retains which extension declared each member plus its exact package and publish steps; that extension supplies the jobs even when it only `uses` a shared profile owned elsewhere. Before execution, the coordinator requires both declared steps for every selected member and removes the publish step of every sibling member the plan did not select, so reconciliation never discovers an accidental registry write after it happened. An unselected sibling's package step is local preparation: it is removed only when nothing the plan keeps still depends on it, so a build step that shares that node with another member of the same project keeps its input.

`putnami publish --visibility <internal|private|public>` fills the per-publication level of the inheritance chain. Every other level — the repository level, the level of each registry, the level a stable or pre-release version takes, and the level a member selector confers — is read from `distribution` in `putnami.ci.json` and carried to the provider as declared. A project may also state its member level in its own `putnami.json`, or inherit it from its scope, with `"distribution": {"visibility": "public"}`; that declaration wins over the `members[]` rules, and `publish` refuses a rule that disagrees with it (see [Distribution visibility](05-configuration.md#distribution-visibility)). The CLI computes no visibility; the provider resolves the chain per member and never narrows a level already resolved.

`distribution.registries.<ecosystem>.mirror.to` requests a copy to an external registry, for example `https://registry.npmjs.org` or `docker.io/putnami`. `publish` captures these destinations with the visibility policy and carries them in its single release request, even when the set is already current. The provider authorizes the destinations and durably records copies of members whose stored visibility is public. Credentials belong to the provider's registry configuration; destinations cannot include credentials, a query or a fragment. Release success confirms acceptance, while external copy completion is asynchronous. Removing a destination from the policy creates no further copy intent and does not cancel work already accepted.

The channels are resolved exactly once and never again. The run's `data.releaseSet` outcome carries the exact `{id, digest}` and the head each channel now points at, with its generation. A compare-and-swap conflict on **any** channel writes nothing on any of them: the publication fails naming every head whose observed ref differs from the expectation, and re-running the publish plans against the current heads. A plan that selects nothing still releases: the provider answers `already-current` and the outcome names the unchanged heads.

A credential provider that negotiates `publication-v1` takes the release-set provider's place: it resolves and releases over its session, and the engine uploads every npm, Go module, OCI and Put registry member; see [`--providers`](#credential-provider---providers).

Without a release-set provider, `putnami publish --all` still publishes every member to the registries the workspace declares, with git-derived versions. `--channel` is refused, and `distribution` and `envs` in `putnami.ci.json` fail `putnami ci validate`.

### Promote and roll back: `channel set`, `channel status`

Promotion and rollback are the same gesture and neither runs a publisher, a build, or a checkout:

```bash
putnami channel set latest --from canary          # promote the head canary points at
putnami channel set latest --from rs_<64 hex>     # roll back to an exact snapshot
putnami channel set latest --from canary --expected rs_<64 hex>
putnami channel status canary                     # desired versus observed, per registry
putnami channel status canary --wait 2m           # poll until every projection converges
```

`channel set` moves a channel to a set that already exists, through one provider call, compare-and-swapped against the channel's current head; `--expected` states that head explicitly instead of resolving it. `channel status` prints the desired head (id and generation) and what each registry projection has actually applied, and exits `1` while any observed generation is behind.

Both address the namespace the publisher writes to — `distribution.namespace` from `putnami.ci.json`, falling back to the workspace name — so a repository whose declared namespace differs from its workspace name promotes the sets it actually published. A provider failure names that namespace and the channel asked for; provider stderr stays out of the message because it is human-owned and may carry credentials.

### Deploy an environment

`putnami deploy --env <name>` synchronizes a declared environment. The environment is declared once, under `envs` in `putnami.ci.json`, and names the channel it follows:

```bash
putnami deploy --env prod                    # synchronize on the head of the channel prod follows
putnami deploy --env prod --tag api          # only the workloads carrying the api tag
putnami deploy --env prod --release rs_<64 hex>   # synchronize on one exact snapshot
```

The channel is resolved once for the whole environment, so two workloads can never converge on two different reads of it. `--release <rs_id>` replaces that read with one exact immutable set.

Which workloads belong to the environment is the intersection of the run's own selection (`--projects`, `--tag`, `--impacted`) with the environment's `workloads` rules. Rules apply in declaration order, **first match wins**, and each may override the environment's channel, rollout, or constraints; a workload no rule selects is not part of the environment at all. An environment that declares no rule takes every selected project at its own values.

Each selected deploy task receives the set ref and the members of that set its own project published: its `oci` image, and the `put` configuration and migration members published beside it. The CLI carries that contract and executes none of it — the backend applies migrations forward, then the configuration, then the image switch, refuses a rollback behind migrations that are neither reversible nor compatible, and performs any progressive rollout with its hosting provider's own means.

A deploy that shares a session with a publish keeps the release barrier: every publication finishes, the set is released, and only then does the deploy run, synchronizing on the snapshot that publish just released. `--release` is therefore refused in that combined session instead of being silently ignored; use it with a deploy-only command to select an older immutable snapshot.

An environment is a state a channel is followed into, so `deploy --env` needs a release-set provider. Without one it is refused rather than deploying whatever the last build left behind.

## Structured Commands

Usage lines, subcommands, flags and examples for each command below are
generated: run `putnami help <command>`. The sections here describe the
behavior that a usage line cannot state.

### `change-plan`

Emit a revision-pinned CI admission plan for the affected projects and their
transitive dependents:

```bash
putnami change-plan --base origin/main --output=json
```

It rejects dirty trees, a non-checked-out `--head`, and a base that is not an
ancestor of HEAD. The structured result contains the full immutable document in
`data`; see [CI Change Plans](17-ci-change-plans.md) for the v1 schema and
digest rules.

### `projects`

Inspect and manage workspace projects.

```bash
putnami projects list                       # List all projects (name, path, type, tags)
putnami projects describe <name>            # Full project details
putnami projects create                     # Scaffold a new project from a template
putnami projects sync                       # Sync project references with workspace
putnami projects tag <name> <tag>           # Manage project tags
```

`projects list` and `projects describe` support `--output=jsonl` for structured output.

`projects create <name> --template <template>` renders the template and lists
the project in the workspace config. On a host without Go, a Go project first
installs the Go the workspace lock pins through the workspace installers. When
those installers fail but still leave a Go, the project stays and the command
exits non-zero with the command to run next: `putnami deps install`.

When the template renders `<%= goFrameworkVersion %>`, `projects create` asks
for the newest version of `go.putnami.dev/app` the way the `go` command
reaches a module, before it writes anything:

1. It reads `GOPROXY`, `GONOPROXY` and `GOPRIVATE` from the environment, then
   from the file `go env -w` writes. Without either, Go's default
   `https://proxy.golang.org,direct` applies.
2. It asks each proxy for the module's `@latest` version, in order. A 404 or a
   410 passes to the next proxy; any other failure does only after a `|`.
3. `direct`, and a module that `GONOPROXY` or `GOPRIVATE` matches, read the
   module's `go-import` meta tag and ask the module proxy its `mod` tag names.
4. A request carries the credentials the `go` command would send: the user
   information of a `GOPROXY` URL, and over HTTPS your netrc entry for the
   host (`NETRC`, else `~/.netrc`, or `%USERPROFILE%\_netrc` on Windows).
   `GOAUTH=off` sends none. No credential is printed.

When no source answers with a valid version, `projects create` writes nothing
and fails. The error names each source it asked and its answer, the setting
to fix, and the command to run again. With `--verbose`, a successful create
names the source that answered.

`projects describe` shows:
- Project name, path, type, tags
- Dependencies and dependents in the workspace graph
- Extensions and their provided jobs
- Configuration (options, build settings, publish channels)
- Exports and bin entries

### `scopes`

List workspace scopes — the path-keyed groupings declared by `putnami.json`
`scopes` and `includes` fields — with their projects, groups, and tags.

```bash
putnami scopes list                          # Tabular view (scope, projects, groups, tags)
putnami scopes list --output=jsonl           # Structured output (one ScopeInfo per line)
```

### `infra`

Show the aggregated infrastructure plan inferred from project descriptors
(databases, queues, services, etc.). Supports `--output=jsonl` for
structured consumers.

It READS `<workload>/.gen/requirements.json`; it never produces one. That
artifact is emitted by the `infra` step of each language's `build` pipeline
(aggregation moved into the extensions), so a workload with
no plan has not been built yet.

```bash
putnami infra plan
putnami infra plan --output=jsonl
```

### Specification-driven development — `features`, `specs`, `architecture`, `contracts`

These four command groups are **not part of the CLI**. They are provided by the
first-party `@putnami/sdd` extension, which also contributes the `validate` and
`validate-workspace` jobs and the five `sdd.*` MCP tools. A workspace that does
not declare the extension has none of them, and a run that plans zero jobs
prints a courtesy hint naming it.

```json
{
  "extensions": ["@putnami/sdd"]
}
```

| Group | Subcommands | Reference |
|---|---|---|
| `features` | `list` `validate` `snapshot` `inspect` `diff` | [@putnami/sdd commands](../../sdd-extension/doc/03-commands.md#features) |
| `specs` | `list` `validate` `inspect` `init` | [@putnami/sdd commands](../../sdd-extension/doc/03-commands.md#specs) |
| `architecture` | `validate` `snapshot` `inspect` | [@putnami/sdd commands](../../sdd-extension/doc/03-commands.md#architecture) |
| `contracts` | `generate` `check` | [@putnami/sdd commands](../../sdd-extension/doc/03-commands.md#contracts) |

The two validation jobs are in the canonical gate:

```bash
putnami lint,test,build,validate --impacted --enforce-coverage
```

See [ADR 0013](adr/0013-sdd-as-a-standalone-extension.md) for why this vertical
is an extension, and
[@putnami/sdd getting started](../../sdd-extension/doc/01-getting-started.md)
for how to declare it.

### `workspace`

Initialize and inspect workspaces.

```bash
putnami init                    # Create a new workspace (putnami.workspace.json)
putnami workspace describe      # Show workspace details
```

`init` writes a `README.md`, a `.gitignore` and a `.gitattributes` with `* text=auto eol=lf` when they do not exist. The README names the workspace and the commands that check a change. The `.gitattributes` gives every checkout, Windows included, LF line endings. An existing file is left as it is; `putnami doctor` reports a checkout that converts line endings.

`init` can also scaffold a starter project with `--project <name>`. Use `--project-path <path>` to place that project somewhere other than the default `<project-name>` (at workspace root).

`init` resolves the extensions, the template and the starter's dependencies on one release channel. It takes the first of:

1. `--channel <name>`.
2. `PUTNAMI_CHANNEL`, when it is not empty.
3. The channel the running CLI was installed from: the `<tag>` of the installed file `putnami-<variant>-<tag>`, when that tag is a channel. A tag that is a version, a `source-…` build, `dev` or a name outside the channel alphabet is no channel. On Windows the active `putnami.exe` is a copy with no tag, so this step never applies there.
4. `latest`.

```bash
putnami init --project my-app --channel canary
PUTNAMI_CHANNEL=tooling-v0.4.0 putnami init --project my-app
```

`stable` reads `latest`. A channel name starts with a lowercase letter or a digit and holds only lowercase letters, digits, `.`, `_` and `-`, 64 characters at most: the alphabet every registry accepts. `init` refuses any other name, an empty one and an exact version before it writes anything: the release lines do not share one version, so a version cannot name a release set.

On a channel other than `latest`, `init` prints the channel and what chose it, and every step reads that channel:

| Step | What `init` asks |
|---|---|
| Extension, agent-content extension, template | The put registry's `download?channel=<channel>` |
| TypeScript starter | The version the npm dist-tag `<channel>` names for each `@putnami/*` package it seeds in the workspace catalog |
| Go starter | The version the Go version query `@v/<channel>.info` names for `go.putnami.dev/app` |

A channel that names no release for one of them fails the run; `init` does not fall back to `latest`. The Go starter renders one version for its four framework modules, so a channel must publish all four at the version it names for `go.putnami.dev/app`. A tagged publish does.

The channel is a target of that run only. `putnami.workspace.json` keeps bare extension and template names, `putnami.lock.json` records the exact versions, and the TypeScript workspace catalog gets the exact versions the channel names, never the channel name. `putnami install` reads the lock afterwards, and `putnami upgrade` still follows `latest` by default. See [ADR 0056](adr/0056-init-resolves-on-one-channel.md).

`init` declares the selected starter's agent-content extension (`@putnami/contributor` for every built-in starter) in `extensions`, opts into its content with `extension:<name>` in `agentArtifacts`, installs that extension, and materializes its content. When the extension cannot be installed, both declarations stay and `init` names `putnami install` as the next step. A starter that opts into nothing adds nothing — there is no flag that turns agent workflows on, because the declaration in `putnami.workspace.json` is the only opt-in. `init --force` on a workspace that already opted in keeps those declarations rather than rewriting them away. See [Agent Workflows](18-agent-workflows.md#lifecycle-init-install-upgrade).

`workspace describe` shows the workspace name, version, project count, extension count, and configuration summary. Supports `--output=jsonl`.

### `version`

Read the version each line is at (`get`), release a line (`tag`), and manage the installed CLI binaries (`list`/`use`).

```bash
putnami version get                   # one line per version line: "<line> <version>"
putnami version tag --scope typescript --dry-run
putnami version tag --scope typescript --yes --push

putnami version list                  # installed CLI binaries (* = active)
putnami version list --global         # ...in ~/.putnami/bin
putnami version use go-dev            # switch the active CLI binary
```

There is no `version set` and no `version bump`: a version is **derived from git**, never declared. A commit carrying its line's tag has the tag's version; any other commit takes the line's last tag advanced by the conventional commits that touch the line, plus an ordered pre-release suffix. `version tag` regenerates the line's changelog from the same commits, creates the release commit, then the annotated tag on it; `--push` pushes both. See [Version Management](14-version-management.md).

`list`/`use` manage the binaries in `.putnami/bin` (or `~/.putnami/bin` with `--global`).

### `pin`

Pin the CLI version this workspace uses.

```bash
putnami pin 1.2.3                   # Pin an exact version
putnami pin                         # Show the current pin
putnami pin --remove                # Drop the pin
```

The pin records the CLI's per-platform digest in `putnami.lock.json`, so every later `putnami` invocation in the workspace resolves *and verifies* that exact binary rather than whatever is on `PATH`. Pins are exact — there is no range — which is what makes them the supported way to stay on an older wire contract when a current build has removed one (see [Machine result contract v2](../../../protocols/cli/doc/02-result-v2.md#rolling-back)).

One `putnami pin <version>` call records the digest for every platform Putnami publishes by default (`darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64`), plus any platform an earlier pin already carried — so switching versions from a single machine does not strand a hosted CI runner on another platform. See [Per-platform digests](14-version-management.md#per-platform-digests).

`pin` is one of the few commands exempt from the relaunch it configures: it must run as the *invoked* binary, because the pinned one may be exactly what is being replaced. See [Version Management](14-version-management.md).

### `install`

Run workspace-level installation: extensions, extension install hooks, templates (if configured), agent workflows (if declared), and dependencies.

```bash
putnami install
```

#### Agent workflows

When `putnami.workspace.json` opts into an extension's agent content (`extension:<name>` in `agentArtifacts`), `install` materializes it **from the extension release the committed lock pins**. It resolves nothing for the content and never rewrites the lock for it, so a clone and a CI runner reproduce exactly the files the lock describes. For an npm extension the root `package.json` declares in `devDependencies`, the content comes from the package the package manager installs into `node_modules`, so `install` materializes it after the workspace installers ran.

Content whose bytes do not match the extension's pin, or a file you own that the content would overwrite, fails the command with a non-zero exit and leaves your files unchanged. A separately declared artifact (`name`, `name:channel` or `/path`) is refused with nothing written and names `putnami migrate agent-content <extension>`. A workspace that declares no `agentArtifacts` skips the phase entirely. See [Agent Workflows](18-agent-workflows.md#lifecycle-init-install-upgrade).

#### Private registries: credentials at install time

`@putnami/*` npm packages and `go.putnami.dev/*` Go modules are private. Both
`install` and `upgrade --deps` refresh your per-user credential for them before
the package manager runs: the extension takes the registry host from the
workspace's own configuration and runs
`putnami cloud registry-token --host <host> --materialize`, and `@putnami/cloud`
writes the native credential itself — the `//<host>/:_authToken=` line in
`~/.npmrc` for npm, the `machine <host>` entry in `~/.netrc` for Go.

No token passes through the framework, and the host is the whole request. The
refresh never blocks an install: with no cloud extension, no session
(`run putnami cloud login`), or a registry the cloud does not manage, the command
logs one line and continues with whatever credential the machine already has.

`putnami cloud registry-token` never runs the first-use install, even when the
lock changed since the last install (the state mid-`upgrade`, once the extension
phase rewrote the lock and the artifact phase downloads a private archive): its
stdout is the bearer and nothing else. The CLI skips the install for that
command, and the framework's credential children also set
`PUTNAMI_NO_AUTO_INSTALL=1`. The invoking command owns workspace state.

The CLI binary download (`putnami upgrade --cli`) sends the host credential of
the put registry; see
[Version Management](14-version-management.md#binary-upgrading).

With `--providers install`, the CLI's own archive downloads (extensions,
templates, agent workflows, the CLI binary) ask the workspace's credential
provider for its `read` credential first; see
[`--providers`](#credential-provider---providers). The npm and Go credential
refresh above stays on `putnami cloud registry-token` in this release.

#### First-use auto-install

A fresh checkout, a new worktree, or a CI runner has no installed workspace
state yet. The first time you run an ordinary command there — `putnami build`,
`putnami test`, an extension command like `putnami cloud …` — the CLI notices
that the recorded install state is missing or stale and runs `putnami install`
once before the command, so dependencies and extension-local state (for example
Cloud's `.putnami/cloud-link.json`, `.putnami/cache.json`, and registry
token-source recipes) are already in place. You no longer have to remember to run
`putnami install` first.

The check is marker-gated: `putnami install` records a content hash of the
workspace install inputs (`putnami.lock.json`, `bun.lock`, `uv.lock`, `go.work`,
…) in `.putnami/install-state.json` (gitignored). A later command stat-compares
those files (~0.015 ms warm) and re-installs only when their **content** actually
changes — a `git pull` that merely touches mtimes does not trigger a reinstall.
Go checksum outputs (`go.sum`, `go.work.sum`) are ignored because ordinary
build/deploy commands can update them and `putnami install` does not materialize
Go module dependencies. It is best-effort: if the auto-install fails (offline,
missing token), the command still runs and surfaces its own error.

With [`--credential-fd`](#run-credential---credential-fd), the check ignores the
marker and the install always runs. A repository can commit a marker that
matches its locks, and the install it would skip is the one that fetches the
dependencies before any repository code runs.

The input set is an **intersection**, not a source: a workspace-root file counts
only when the extension that installs from it declared it in its adapter's
`workspace.inputs` *and* it is one of the lock-shaped names above. Probe inputs
and install inputs are different questions — a root `biome.json` or
`tsconfig.base.json` decides what a project resolves to, and installs nothing —
so declaring one does not enlist it here.

It is inert under `--plan` and `--dry-run` (which only print a note when the
workspace is out of date), and for the `install`/`deps`/`upgrade` commands
themselves. Set `PUTNAMI_NO_AUTO_INSTALL=1` to disable it — for CI pipelines that
prefer to run `putnami install` as an explicit, isolated step.

### `upgrade`

Upgrade the CLI, extensions, templates, agent workflows, and framework dependencies in one command.

```bash
putnami upgrade                       # Upgrade everything from the stable channel
putnami upgrade --cli                 # Only upgrade the CLI binary
putnami upgrade --extensions          # Only upgrade extensions and templates
putnami upgrade --deps                # Only upgrade framework dependencies
putnami upgrade --channel canary      # Follow the canary channel on every registry
putnami upgrade --release rs_<64-hex> --namespace putnami
                                      # Use one exact immutable release set
putnami upgrade --version 1.2.3       # Use an exact release version
putnami upgrade --from-source         # Build the CLI from this workspace's source
putnami upgrade --dry-run             # Resolve and print the plan without writing files
```

When the extensions phase moves an extension or template that
`putnami.workspace.json` pins to one exact release (`"@putnami/go": "1.2.3"`,
or the template entry `"@scope/tpl:1.2.3"`), it writes the new release to that
pin as well as to `putnami.lock.json`, so the two files agree. Only the pin's
value changes; the rest of the file keeps its formatting. Ranges (`^1.0.0`),
channels, `latest`, unpinned entries and local paths are left as they are. A
pin in the global putnami config is not edited: the command prints a warning
that names it. When a template is declared more than once across
`putnami.workspace.json` and the global putnami config, only the entry the config reader keeps is rewritten; if that rewrite
would leave another entry in effect, the command stops before writing the lock
and names the template. `--dry-run` lists each pin it would move. `extensions update`
and `templates update` follow the same rule; with `--output=jsonl`, their
events carry the replaced value as `replacesPin` and a stale global pin as
`globalPin`.

`--from-source` is the self-host escape hatch: it builds and installs the CLI from `tooling/cli/cmd/putnami` (add `--global` to replace the global binary), bypassing the download channel. See [Version Management](14-version-management.md#self-hosting-a-local-cli-build).

#### How a channel resolves

`--channel <c>` asks each ecosystem for the channel it already publishes. There
is no release-set call and no namespace to configure, so any workspace can
follow a channel — not only the publisher's own.

| Ecosystem | Where the channel lives | Source name |
|---|---|---|
| npm | `dist-tags[<c>]` in the packument, read per package from the registry `registries.npm.scopes` names for that package's scope | `dist-tag` |
| Go | `@v/<c>.info` on the module origin `registries.go.origin` names, resolved **per module** because one channel carries different versions per module | `go-proxy` |
| CLI, extensions, templates, agent workflows | the channel projection of the put registry `registries.put.registry` names, with the user's credential on the archive download | — |

Every endpoint comes from the workspace's `registries` entry for that ecosystem
(see [Configuration](05-configuration.md#registries)); no registry URL is
hard-coded and none is hand-written into a dotfile. npm metadata is read with
the `_authToken` your workspace or user `.npmrc` carries for that registry: an
anonymous read hides versions a private registry only shows to an authenticated
client, which would report a published channel as missing. The archive
downloads for the CLI and the extensions send the same user credential, and a
`401`/`403` names `putnami cloud login`.

Each ecosystem resolves every coordinate and reports what it found before it
writes anything, with the **source revision** of every resolved version — the
trailing `-<sha>` segment of an ordered pre-release, `-` for a stable one — so
the table says which commit a version came from:

```
  Target @putnami/* version: 0.1.0-20260902173000-8d5edb751 (dist-tag)   revision 8d5edb751
  Target go.putnami.dev/* version: v0.1.0-20260902173000-8d5edb751 (canary via go-proxy)   revision 8d5edb751
```

A failure names the package or module, the channel, the registry host and the
HTTP status, and leaves that ecosystem's metadata untouched. The dependency run
stops at the first failing ecosystem, so a channel npm serves and the Go origin
does not moves neither. Add `--continue-on-error` to let each ecosystem report
its own outcome instead.

A channel is mutable, so a channel that moves mid-run can mix two publications
across ecosystems. Use `--release <id> --namespace <ns>` when a build must pin
one exact publication.

#### `--release` needs `--namespace`

`--release <id>` is the only selector that still uses a release set: an
immutable id cannot be expressed as a native tag. A release-set id is scoped by
the namespace that **published** it, which the consuming workspace does not
know, so you name it:

```bash
putnami upgrade --release rs_<64-hex> --namespace putnami
```

`--namespace` is rejected with any other selector. `--release` needs the
`cloud-release-set` provider installed and fails closed without it; a channel
needs no provider at all. See
[ADR 0020](adr/0020-upgrade-resolves-channels-natively.md).

The dependency phase pins root workspace selectors before installing: Go
modules are written as `go.work` `replace` directives, and TypeScript packages
are written as exact `@putnami/*` versions in the root `package.json`
`dependencies` and `overrides` sections.
The `--extensions` phase also upgrades agent content, but **only** for extensions the workspace already opts into in `agentArtifacts`: upgrade never enrols a workspace that did not opt in. Each opted-in extension's content follows the release its extensions phase moved to, and is verified and planned before anything is written, so a collision or a bad digest leaves the previous files and the previous ownership record. Content the workspace no longer opts into is retired. When the workspace opts into an npm extension and the command runs the dependency phase (`upgrade`, `upgrade --deps`), the agent phase runs after that phase instead, so the content follows the package the package manager installed. `--dry-run` reports the opt-ins and what would be retired without resolving or downloading anything. See [Agent Workflows](18-agent-workflows.md#lifecycle-init-install-upgrade).

### `extensions`

Manage workspace extensions.

```bash
putnami extensions install          # Install/update all extensions and run install hooks
putnami extensions list             # List discovered extensions with their jobs
putnami extensions update           # Update extension versions (lock and exact pins)
putnami extensions remove <name>    # Remove an extension
```

`extensions list` shows each extension's name, root path, provided commands, activation files, and priority. Use `--output=jsonl` for machine-readable output.

#### The user scope: `--user`

`--user` manages the extensions you pin for yourself in `~/.putnami/user`,
outside any workspace. It needs no workspace and never reads or writes one,
even when you run it inside a workspace.

```bash
putnami extensions install --user @acme/audit          # Pin the latest version
putnami extensions install --user @acme/audit@1.4.0    # Pin one version
putnami extensions list --user                         # List the user-scope pins
putnami extensions remove --user @acme/audit           # Drop a pin
```

- `install --user` takes exactly one registry extension,
  `@scope/name[@version]`. It downloads from `PUTNAMI_REGISTRY_URL` or the
  default registry, verifies the SHA-256 digest like a workspace install
  (including the `PUTNAMI_UNSAFE_INSTALL` rule), then writes the entry to
  `~/.putnami/user/putnami.lock.json` and links the extension. It runs no
  install hook and touches nothing in the current directory. A re-run keeps
  the recorded pin; `--latest` moves it to the newest release.
- A local path, `--platform` and `--dest` are usage errors with `--user`.
- `install` and `list` accept `--output=json|jsonl`.
- `update` does not take `--user`: run `install --user --latest`, or install
  an explicit version, to move a pin.

Once pinned, an extension's subcommands declared `interactive: true` and
`workspace: "optional"` run in any directory that is not inside a workspace;
every other command still prints `putnami: no workspace found (looking for
putnami.workspace.json)`. Inside a workspace only the workspace's own pins
count. See [The User Scope](07-extensions.md#the-user-scope) and
[ADR 0051](adr/0051-user-scope-extensions-run-without-a-workspace.md).

#### Materializing artifacts for another platform

`extensions install` normally materializes artifacts for the machine it runs on. `--platform` and `--dest` turn it into a packaging step that produces artifacts for a *different* target — for example warming an image layer for `linux/amd64` from a `darwin/arm64` laptop:

```bash
putnami extensions install --platform linux/amd64 --dest ./.gen/warm-artifacts
```

- `--platform <os>/<arch>` resolves each extension at its **lock-pinned** version, but downloads the archive for the requested platform and verifies it against that platform's entry in `putnami.lock.json` → `integrities`.
- `--dest <dir>` writes into that directory as a drop-in artifact-store root (`<dir>/sha256/<xx>/<digest>/`), so a packaging step can copy it to `~/.putnami/artifacts` without reaching into `$HOME`. It must not point at the machine-global store.

Either flag makes the run a **materialization**, which is deliberately side-effect free on the invoking workspace:

- install hooks do not run (they belong to *this* machine's extensions),
- the stable `.putnami/bin/extensions/<name>` links are not repointed,
- `putnami.lock.json` is not rewritten — the digests were read from it,
- the shared AI context is not regenerated.

A materialization also **fails closed**: if the lock records no integrity for the requested platform and the registry advertises none, the command errors instead of falling back to an unverified install. `PUTNAMI_UNSAFE_INSTALL=1` does not apply here.

The produced tree is reproducible: file modes are canonicalized and every timestamp is stamped to a fixed value, so the same lock and platform yield byte-identical output across machines and runs — which is what lets the result be hashed into an image content key.

### `templates`

Manage workspace templates.

```bash
putnami templates install              # Install all configured templates (default)
putnami templates install <name>       # Install a specific template
putnami templates update               # Update to latest compatible versions (lock and exact pins)
putnami templates list                 # List configured and discovered templates
putnami templates remove <name>        # Remove an installed template
```

`templates list` shows each template's name, constraint, installed version, and source (config or workspace). Use `--output=jsonl` for machine-readable output.

### `deps`

Manage workspace dependencies. `install` delegates to the appropriate package
manager via extensions; `add`/`remove` edit a Go module's `go.mod` for you (so
you don't hand-edit it) and reconcile the closure with `go mod tidy`.

```bash
putnami deps install                       # Install all dependencies (default)
putnami deps add golang.org/x/text@latest  # Add a Go module (sole Go module, or --projects <name>)
putnami deps add rsc.io/quote@v1.5.2 --projects api
putnami deps remove golang.org/x/text      # Drop a Go module and tidy
putnami deps prune                         # Remove declared edges no import backs
putnami deps prune --dry-run               # List them and change nothing
putnami deps prune --projects api          # Narrow to one project
```

`add`/`remove` currently support Go modules. The target module is the workspace's
sole Go module, or the one named by `--projects <name>`; `go get` runs with
`GOWORK` pointed at the workspace file so `replace` directives resolve. Both run
the go command the Go extension's tasks run: the release the lock pins, or else
a `go` on PATH. On a host with neither, they first pin and install the pinned Go,
as `projects create` does. When the workspace installers fail but still leave a
Go, `add`, `remove` and `prune` edit the modules with it and exit non-zero with
the command to run next: `putnami deps install`. For
TypeScript, edit `package.json` and run `putnami deps install`.

`prune` reads the provider view that the last install or build recorded. When a
file that view was recorded from changed since, such as a hand-edited `go.mod`,
`prune` refuses, names the changed files, and asks you to run `putnami install`
first. It drops a Go requirement with `go mod edit -droprequire`, not `go get`,
so a requirement that only `go.work` satisfies, one that no module proxy serves,
is removed too.

### `cache`

Manage the local cache store.

```bash
putnami cache clean                 # Delete this repo's cached data (all its worktrees)
putnami cache clean --all           # Delete every repo's cached data on the machine
putnami cache gc                    # GC global build, Go, Bun, and binary stores to their budgets
putnami cache verify --impacted     # Audit deterministic keys, artifacts, writes, and restores
```

Build-cache data lives in the machine-global per-repo store at `~/.putnami/store/<repo-id>/`, shared across all worktrees of the repo (override with `PUTNAMI_STORE_DIR`). Go compiler/modules at `~/.putnami/cache/go` and Bun downloads at `~/.bun/install/cache` (for a Bun that Putnami installed, `install/cache` under `~/.putnami/toolchains/bun/bun-<version>`) are shared across **all repositories and worktrees**. `cache clean` therefore clears the build cache for every worktree of the current repo; `cache gc` enforces the separate build, Go, Bun, and artifact-store budgets. Running with `--no-cache` skips Putnami action-cache lookups but still uses the native Go and Bun caches; `--no-cache-projects <selector>` skips them for the named projects and their planned dependents only, so the rest of the selection’s dependency closure keeps its cache. A task that failed is cached too: a re-run with unchanged inputs replays the same failure, with its original output, instead of executing it again — `--retry-failed` forces that one task to run while every other cache hit is kept, and `--no-cache` suppresses the replay without removing the record: only a run in which the task passes at the same key does that, whatever its cache policy or selection. See [Caching](10-caching.md) for the full model.

`cache verify` runs the selected projects' `lint`, `test`, and `build` tasks in
detached temporary worktrees and isolated stores. It requires a clean worktree
and supports the ordinary project-selection flags plus structured output. See
[Cache Determinism Verification](10-caching.md#cache-determinism-verification)
for its checks and `.gen` classification rules.

Cache subcommands do not support `--dry-run`. Passing it returns a usage error
before any store is mutated, extension cache hook is invoked, or verification
worktree is created.

To share the cache across machines and CI, see the [Remote Build Cache](10-caching.md#remote-build-cache) (opt-in, configured via `.putnami/cache.json` or `PUTNAMI_CACHE_*`).

### `config`

Inspect and modify workspace configuration.

```bash
putnami config show                 # Display merged configuration
putnami config set <key> <value>    # Set a configuration value
```

`config show` displays the fully merged config (global + workspace + local scopes). Supports `--output=jsonl`.

### `context`

Manage assistant guidance.

```bash
putnami context generate            # Regenerate the Putnami guidance block in CLAUDE.md and AGENTS.md
putnami context map                 # Refresh .putnami/context-map/repo-map.{json,md}
putnami context map --print         # Render the map to stdout (markdown); write nothing
putnami context map --print=json    # Same, as JSON
```

`context generate` writes the guidance block and the agent content of the
extensions the workspace declares by path and opts into, and nothing else. It
never writes `.mcp.json`: `init`, `install` and `upgrade` register the MCP
server, and `putnami mcp install` repairs the registration. See [`mcp`](#mcp).

#### `context map`

`context map` renders the workspace orientation map — every project with its
path, tags, dependency edges, endpoint table, config keys, and README summary,
plus the workspace `docs/` index — into
`.putnami/context-map/repo-map.json` and `.putnami/context-map/repo-map.md`.

The map is **ephemeral, gitignored, CLI-owned state**, never committed.
It is a pure projection of committed inputs, so committing the projection bought
nothing and cost a permanent merge hotspot (one flat sorted list, so parallel
branches collide), a drift class, and a CI gate. Every build refreshes it and the
MCP `workspace_map` tool serves it fresh, so there is nothing to regenerate by
ritual and nothing that can go stale in a review.

It is byte-deterministic on an unchanged tree: entries are sorted by project
path, and the documents carry no timestamp, no absolute path, and no CLI version.
A repeat run over an unchanged tree therefore costs one digest sweep and writes
nothing.

Generation is a map-reduce:

- **map** — one fragment per project, computed from that project's own
  `putnami.json`, `schema/openapi.json`, `schema/config.jsonschema.json`, and
  `README.md`, cached under `.putnami/context-map/<project>/map-fragment.json`
  with a digest of exactly those inputs.
- **reduce** — the *live* project set from the workspace manifest plus the
  fragments. A cached fragment is reused only when its recorded inputs digest
  still matches the files on disk, so a selection-scoped run stays cheap while
  the rendered map is always complete. A project the workspace no longer
  contains simply contributes nothing — deletions and renames need no diffing —
  and the previous map is never an input to the new one.

`--projects <list>` and `--impacted` scope which fragments are *refreshed*, never
which projects the map renders. `--print[=json|md]` runs the same reduce entirely
in memory, writes nothing at all (no fragments, no documents), and emits the
document on stdout — `--output` is already the run report's shape, which is why
the flag is spelled `--print`. `--output=jsonl` emits the machine-readable run
report.

A project's dependency edges and language identity come from the merged provider
view (`.putnami/workspace-index.json`, which is not committed). On a clone that
has none, every project would resolve from authored config alone and the map
would render without provider-derived edges — a confident wrong answer — so in a
workspace that declares extensions, `context map` refuses and points at
`putnami projects sync`. The build attachment is never affected: the engine
probes before the finalizer runs.

##### Build attachment

`putnami build` refreshes the map after a successful session, in every
workspace — writing into the CLI's own gitignored state directory is harmless
anywhere, so there is no adoption ceremony:

| Environment | Behavior |
| --- | --- |
| `PUTNAMI_CONTEXT_MAP=off` | Never runs |
| `PUTNAMI_CONTEXT_MAP=write` | Always writes, CI included |
| `CI` set (non-empty) | **off** — nothing is committed and a runner has no map consumer |
| otherwise | **write** — the map is left correct as a side effect |

The attachment never runs for `--plan`/`--dry-run` (they never execute), and it
does nothing when the session failed or was aborted. A map it cannot write is a
stderr warning, not a failed build: the artifact is a convenience, and the MCP
tool rebuilds it in memory regardless.

##### Serving it to agents

The MCP `workspace_map` tool (see [`mcp`](#mcp)) renders the same document on
every call from the working tree, so an agent never depends on whether a build
ran. It takes `section` (`projects`, `dependencies`, `intercalls`, `apis`,
`config-keys`, `schemas`, `docs`, `all`) and an optional `project`, and reports
whether the on-disk artifact matches the fresh render.

### `doctor`

Run a read-only production-readiness preflight over the selected projects.

```bash
putnami doctor
putnami doctor --profile production
putnami doctor --profile production --project /tooling/cli
putnami doctor --profile production --output=jsonl
```

Findings are derived from **committed** artifacts — manifests, declared capabilities, required config keys, committed schemas, config shadowing — and graded by deployment profile: under `--profile production` a high or critical finding exits `2`, while `dev`/`test` stay advisory. Config *values* are never read or emitted, so the report is safe to attach to a build log.

Three codes are repository hygiene rather than production readiness and never block a profile: `doctor.missing_readme` reports a project with no `README.md` at its root (a template project, which ships `README.md.template` for the project it renders, and a generated client project are exempt), `doctor.committed_manifest_stability` reports a committed generated artifact whose content is derived from workspace state (a workspace version string, a dependency-closure enumeration, a source binding) instead of the project's own declared inputs, and `doctor.undeclared_schema_commit` reports a project that tracks generated schemas without declaring `options.generate.schema`. The last two are explained in [configuration](05-configuration.md#generated-schema-commit-regime). All three are waivable per project and per field.

`putnami doctor` also checks the workstation the workspace is checked out on. These findings are scoped to the workspace root (`/`), never block a profile, and are waivable. `doctor.crlf_checkout` reports tracked text files that the working tree holds with CRLF endings while the commit has LF, or `core.autocrlf=true` with no `.gitattributes` rule that sets `eol=lf`. On Windows only, `doctor.long_paths_disabled` reports that the `LongPathsEnabled` registry value (`HKLM\SYSTEM\CurrentControlSet\Control\FileSystem`) is not 1, `doctor.git_long_paths_disabled` reports that Git does not set `core.longpaths`, and `doctor.vc_runtime_missing` reports that `vcruntime140.dll` is not in the Windows system directory, so Biome, which `@putnami/typescript` runs to lint and format TypeScript, cannot start. The human report prints each finding's fix. The production gate inside job commands does not run these checks.

The same gate runs inside a job command: `putnami <jobs> --profile production` aborts between plan and execution on an unwaived high/critical finding, so nothing runs against a workspace that would fail the preflight.

### `sessions`

Inspect recorded execution sessions.

```bash
putnami sessions list               # Show recent sessions
putnami sessions list --revision <sha>   # Sessions whose recorded tree sat on that commit
putnami sessions inspect <id>       # Show session details and events
putnami sessions inspect --run <ref>     # A remote attempt by reference; resumes it when not imported
putnami sessions export > sessions.jsonl   # Every retained record, one per line
putnami sessions export --since 2026-09-01T00:00:00Z
putnami sessions summary            # One ledger row per recorded run
putnami sessions summary --command lint,test,build,validate
putnami sessions summary --by-digest    # Task records grouped by input digest
```

Sessions record every job execution with metadata, git state, timing, and JSONL event streams. The gate hard-fails cache-hit-rate and job-count regressions; timing findings warn unless `timing.severity` is promoted to `error` in the baseline. `--scenario <name>` records the session as a named performance reference point instead of refreshing the budget; scenarios are never evaluated by the gate.

`sessions export` streams the recorded documents themselves, oldest first and deduplicated by session id, so per-run CPU, task counts and cache reuse can leave a worktree before it is deleted. The store keeps 20 records by default; `sessions.keep` in the workspace config changes that (see [05-configuration.md](05-configuration.md#session-retention)).

`sessions summary` reduces the same records to one row per run — commands, selection, task total/executed/reused, `run.cpu.actualMs`, wall and outcome. A run a task spawned is marked nested and never counted as a gate, and `--command <list>` matches the recorded command set exactly, so `--command lint,test,build,validate` lists the gates and none of the builds they spawned. `--by-digest` groups the selected sessions' task records by `inputDigest`, the cache key each task was keyed on, and counts how many executed, reused or failed. See [11-session-recording.md](11-session-recording.md) for details.

### `tree`

Identify the worktree the CLI runs in, by content.

```bash
putnami tree fingerprint                 # One digest, nothing else
putnami tree fingerprint --output=json   # Adds dirty and headSHA
```

The digest covers HEAD, the bytes at every tracked path that differs from it —
recursing through submodules — and the content of every untracked non-ignored
file. It exists because "the same files are dirty" is not
"the same bytes are on disk": `git status` reports paths and status codes, so an
agent that gates a tree and then edits a file it had already dirtied leaves that
output identical, and a path list would accept the gate.

It reads the repository and writes nothing — no session record, no workspace
state — and needs no workspace, so it answers inside any git repository. Every
recorded session carries the same digest for the tree it opened on, under
`tree.fingerprint`; this command is the one implementation of the calculation.
See [11-session-recording.md](11-session-recording.md#the-gated-tree).

`tree verify` checks the local evidence of an execute run against that same
digest and prints one JSON verdict. It takes exactly one mode:

```bash
putnami tree verify --snapshot --base origin/main       # Dossier skeleton for the current tree
putnami tree verify --ref .context/review.md            # The file's path as given and its SHA-256
putnami tree verify --record .context/dossier.json      # Verify a dossier
putnami tree verify --gate .putnami/sessions/<id>/session.json --report .putnami/reports/<id>.json
```

`--record` checks the dossier's tree binding, changed files, policy files,
consulted scopes, independent review, gate, qualification verdicts and
acceptance evidence, and fails if the dossier changes while it reads it.
`--gate` checks that a recorded gate session and its report can replace the
gate the PR finalizer would run. A gate must cover the impacted plan, which the
workspace CLI of the checked repository answers as a dry run (`--plan`).
`--base` names the base revision. `--snapshot` and `--gate` default it to
`origin/main`. With `--record` it has no default; when given, the dossier's
base SHA must equal `git merge-base <rev> HEAD`.

With `--record` and `--gate`, the gate's report must record
`enforceCoverage: true`, and the gate must satisfy the CI policy,
`putnami.ci.json` version 3: it runs every blocking command, and every flag the
policy appends has evidence. `--fix=false` passes only when the gate's report
records `fix: false`, which a run given `--fix=false` or `--no-fix` writes.
`--enforce-coverage` and `--continue-on-error` need nothing more. Any other
flag, `--fix=true` included, fails the check. The impacted plan carries the
policy's `--enforce-coverage` and `--fix=false` flags, never
`--continue-on-error`, so a `--fix=false` gate is checked against the lint
tasks `--fix=false` plans.

Paths resolve against the Git top level, not the working directory. Evidence is
read strictly: invalid UTF-8, duplicate JSON members, or a file that is not
JSON fail the check. A failure prints
`{"verdict":"not-verified","reason":"..."}` on one line, exits 1 and writes
nothing to stderr, and `--output` does not change the document. The verifier
writes no file and needs no workspace. It is a consistency check, not a signed
review or an access boundary.

### `compose`

Serve a workload together with the workloads it runs with.

```bash
putnami compose @example/go-items-consumer                        # Target watches; its runsWith members run production-mode
putnami compose /typescript/samples/06-database --port 0          # Ephemeral proxy port for the target
putnami compose /go/samples/migrations-feature --no-watch         # Target production-mode too
putnami compose @example/go-items-consumer --output=json          # Start document, then exit document
```

The composition is the transitive closure of `runsWith`, resolved by project
name then by id; the dependency graph is never consulted. Members start in
topological order, dependencies first, each only after the one before it emitted
its typed `ready` event (`--ready-timeout`, default `60s`).

- **Stable URLs.** Every member binds port `0` and sits behind a reverse proxy
  on `127.0.0.1`. The proxy URL is known before any process starts and survives
  every restart of the member; while the member has no port, the proxy answers
  `503` with `{"status":"unavailable","checks":{"compose":"backend not ready"}}`.
  The target's proxy port is `--port`, else `options.serve.port`, else `3000`.
- **Injected configuration.** A member receives `CONFIG_DATA` on its serve step:
  `clients.services.<id>.url` for each member it runs with, under the provider's
  client-contract service id and its project name, and a `database` binding for
  each datasource of its `infra/requirements.json`. Nothing is written into the
  project tree, and the value is never printed or recorded. A member whose
  environment already carries `CONFIG_DATA` is refused.
- **Databases.** With Docker, each (member, datasource) gets its own database,
  created before the member starts and dropped when the composition stops;
  with `PUTNAMI_TEST_PG_URL` the members share that server's database and the
  composition reports `isolation: none`. Workloads apply their own migrations.
- **Preparation.** The serve pipelines' finite steps (config merge, generate,
  describe) run through the ordinary engine first, cached, with a session record;
  only the long-running serve step is started by `compose`.
- **Teardown and recovery.** On `SIGINT` or `SIGTERM` every member, proxy and
  database is released. A lease under `.putnami/compose/<id>/` records what a
  composition owns, so the next invocation reaps what an ungraceful death left
  behind and lists it under `reaped`.

With `--output=json|jsonl`, stdout carries exactly two result documents — one when
every member is ready (`id`, `target`, `members[]` with `project`, `proxyUrl`,
`backendPort`, `databases`, `configSections`, `readyMs`, `isolation`, `reaped`)
and one at exit adding `cleanup` — and serve logs go to stderr. A failure is one
failure document whose `data` names `code`, `member` and `phase`. See
[24-workload-qualification.md](24-workload-qualification.md).

### `qualify`

Run a workload's derived smoke contract against a target and print one verdict.

```bash
putnami qualify /go/samples/service-to-service --print-contract              # The derived contract, nothing run
putnami qualify /go/samples/service-to-service --print-contract --output=json
putnami qualify @example/06-database --target https://pr-123.preview.example --expect-sha 3cc91b658
putnami qualify /go/samples/task-api --target http://127.0.0.1:3801 --expect-sha "$(git rev-parse HEAD)" --output=json
```

The contract is **derived, never authored**: every exact, public `GET` or `HEAD`
route in the workload's route inventory (`schema/http-routes.json`, then
`.gen/schema/http-routes.json`), except the platform endpoints, sorted by path
and capped at 25. Each request passes when it answers below `500`. The command
then checks readiness (`<prefix>/readyz` answers `200` with `status: ok`), checks
that `<prefix>/version` reports a sha starting with `--expect-sha`, runs the
requests, and reduces everything to one verdict.

The verdict **fails closed**. Only `passed` exits `0`; `failed`, `unsupported`,
`not_run`, `timed_out`, `canceled`, `target_unreachable`, `digest_mismatch` and
`composition_failed` exit `1` and still print the verdict — under `--output=json`
it is the failure envelope's `data`. A URL target without `--expect-sha` is a
usage error (`2`): a green verdict on a stale deployment proves nothing. A
workload with no inventory, or with no safe route, is `unsupported`, never
`passed`.

`--target local` composes the workload on this machine the way `compose
--no-watch --port 0` does, runs the contract against its proxy, and tears the
composition down on every path. Its verdict binds to the worktree fingerprint:
read before the serve pipelines are prepared, and read again once every member is
ready. A tree that changed in between is `digest_mismatch`, a composition that
cannot start is `composition_failed` naming the member and phase, and `cleanup`
records what teardown released. `--expect-sha` with a local target exits `2`;
serve logs appear only with `--verbose`, on stderr.

```bash
putnami qualify /go/samples/migrations-feature --target local                 # Compose, smoke, tear down
putnami qualify @example/go-items-consumer --target local --output=json       # The verdict as data
```

See [24-workload-qualification.md](24-workload-qualification.md) for the
targets, the phases and states, the binding rules and the verdict document.

### `report`

Read back the bounded synthesis a finished run recorded under `.putnami/reports/<session-id>.json`.

```bash
putnami report                                    # The newest CLI-driven run's report
putnami report --session 20260723-002049-9108d9   # One report, by session ID
putnami report --output=json                      # The recorded document itself
```

Where a session is the run's full debug payload, the report is the small closed document a longitudinal consumer files: the run's verdict and counts, one row per root command (tests, coverage, fresh wall, error and warning totals), and a bounded, prioritized job list with failures first.

Three behaviors are contract, not presentation:

- **`--output=json` prints the recorded document RAW** — no result envelope, and the bytes on disk rather than a re-serialization. The report *is* the contract a consumer binds to (`reportFile`, [protocols/cli doc/03-report.md](../../../protocols/cli/doc/03-report.md)), so a CI runner posts this output verbatim and a member added by a newer producer survives an older reader. Failures still use the standard envelope.
- **No report is exit `1`**, not a usage error: the document is written at finalize, so an interrupted run records none, and "the last run was killed" is an answer about the workspace.
- **The default lookup withholds MCP-origin reports.** An agent's background run must not answer "what did my last run do"; `--session <id>` reads any report by name, agent runs included.

Because a killed run writes nothing, the newest report can belong to an *earlier* run. A consumer that files a report against a commit verifies `git.sha` instead of trusting recency.

### `migrate`

Migrate Putnami's own workspace files forward.

```bash
putnami migrate vnext --check       # Report the pending putnami.lock.json migration (read-only)
putnami migrate vnext --apply       # Perform it
```

`vnext` migrates `putnami.lock.json` to the current format (currently v4). For a v1 input it records the task-contract version each installed extension manifest already declares; for v2 and v3 it performs the explicit projection that initializes current-format dimensions such as `agentArtifacts`. It never changes a pin, resolves a version, or contacts a registry.

Since 0.3.0 every other command **refuses** a v1 lock, so this is the command that unblocks such a workspace. Two consequences follow, both deliberate: `migrate vnext` is the only reader allowed below the format floor, and no lifecycle command migrates a committed lock as a side effect.

- `--check` (the default) writes nothing. It exits `0` when the lock is already migrated and `2` when a migration is pending, so CI can gate on it. `--output=json` emits the plan in the standard result envelope.
- `--apply` performs the conversion atomically (temp file + rename) and is idempotent — running it twice leaves identical bytes.
- A pinned extension that is not installed cannot have its contract read, so it is reported under "Not recorded" and left alone rather than guessed at. Install it and re-run to record it.

```bash
putnami migrate agent-content @acme/contributor              # Plan the move (read-only)
putnami migrate agent-content @acme/contributor --apply      # Perform or finish it
putnami migrate agent-content @acme/contributor --rollback   # Restore the previous state
```

`agent-content` moves the separately declared agent artifacts that an installed extension's content supersedes (`agentContent.supersedes`) to that content. No other command installs or reads those artifacts, and every agent-content command refuses them until they move:

- their `agentArtifacts` entries become one `extension:<name>` entry;
- their pins leave the lock;
- their ownership records merge into the extension's record.

It never resolves or downloads anything: the extension must already be installed at a pinned release, or be declared by path. A clone without ownership records hands over no ownership, so the content adopts only files already identical to what it ships.

- `--check` (the default) writes nothing. It lists what moves and every file the content adds, changes, removes or releases. It exits `2` while a migration is pending. `--output=json` emits the report in the standard result envelope.
- `--apply` plans everything first and writes only when the plan has no blocking file. It keeps a journal under `.putnami/agent-content-migrations/`, so a rerun finishes a migration that stopped.
- `--rollback` restores the previous declarations, pins, ownership records and files from that journal. It refuses rather than overwrite a change made since. A completed journal stays as the rollback point, so a second migration to the same extension is refused until you roll the first back or remove its journal directory.

See [agent workflows](18-agent-workflows.md#migrating-separate-artifacts-to-an-extensions-content).

### `dev`

Develop extensions and templates. These commands are for **extension/template authors**, not end users.

```bash
putnami dev extension validate [path]   # Validate extension manifest

putnami dev template validate [path]    # Validate template manifest
putnami dev template test [path]        # Test by rendering and validating output
putnami dev template package [path]     # Package for distribution
```

Template archives contain `putnami.template.json` plus the renderable/static
template content. Source-workspace declarations at the template root
(`putnami.json`, `putnami.features.json`, and `specs/`) and root-local state such
as `node_modules/` are excluded so a generated project does not inherit the
template project's root SDD identity. Nested directories are ordinary template
content and are copied as authored. `README.md` and `LICENSE.md` are ordinary
optional template content:
they are included when present and are not required to package a template.

### `telemetry`

Manage anonymous usage telemetry.

```bash
putnami telemetry status            # Show effective state and precedence rule
putnami telemetry on                # Explicitly enable telemetry
putnami telemetry off               # Disable telemetry
putnami telemetry show              # View buffered events pending flush
```

Telemetry is eligible by default for interactive CLI use. The first eligible
interactive run prints a one-time notice; automation stays silent until that
notice has been shown on the same machine. `telemetry off` is an explicit
opt-out and deletes the local buffer and device ID. See
[12-profiling-and-telemetry.md](12-profiling-and-telemetry.md).

### `mcp`

Run the Model Context Protocol server over stdio so an AI agent harness can
drive the workspace through typed tools instead of shelling out to the CLI.

```bash
putnami mcp                         # Spawned by an MCP client; speaks JSON-RPC 2.0 on stdin/stdout
putnami mcp install                 # Write the putnami entry into .mcp.json at the workspace root
```

The core server exposes eleven tools: ten read-heavy workspace tools
(`list_projects`, `describe_project`, `agent_context`, `workspace_map`, `deps`,
`find_owner`, `why_impacted`, `topo_sort`, `impacted`, `get_diagnostics`) plus
`run_jobs`, whose `dryRun` mode returns the planned DAG without executing
subprocesses. `workspace_map` renders the whole
workspace orientation map in memory on every call — independent of whether a
build ever wrote the ephemeral `.putnami/context-map/repo-map.json` — and takes
`section` (`projects`, `dependencies`, `intercalls`, `apis`, `config-keys`,
`schemas`, `docs`, `all`) plus an optional `project`, so a scoped question costs
a few KB instead of the full map. It also exposes
`workspace://context` as a
read-only resource for live workspace graph/layout/channel/version context plus
dependency-first topological project order. The server exits with the session.

Before it answers the first request, the server brings the workspace's agent
workflows to the version `putnami.lock.json` pins. A host starts the server when
an agent session starts, so a worktree that moved to another lock serves the
matching skills from that session on. The pass only compares the local
ownership record with the pin when they match, copies from the local artifact
store when they do not, and never downloads. A pin missing from the store, or a
managed file you edited, is a warning on stderr; the server starts either way.
See [ADR 0040](adr/0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md).

Installed extensions add their own dot-namespaced tools after the core ones.
`@putnami/sdd` contributes the four SDD tools — `sdd.list_features`,
`sdd.feature_context`, `sdd.list_specs`, `sdd.spec_context` — which were core
tools under undotted names before moving into the extension. **The undotted names are gone with no
alias and are refused with `-32602`**; see
[@putnami/sdd MCP tools](../../sdd-extension/doc/04-mcp-tools.md).

`run_jobs` refuses to EXECUTE a command whose resolved traits declare a mutation
outside the workspace — `publish` (`sideEffects: "registry"`), `deploy`
(`sideEffects: "cloud"`), and any extension verb declaring `sideEffects` in its
manifest traits. Spending CPU on the workspace is reversible and local; pushing
a release or converging a cloud environment is neither, and there is no
confirmation step between an agent's tool call and the effect, so those stay a
terminal decision. Planning is unaffected: `dryRun: true` still returns the DAG
for them, exactly as it does for `serve` (which `run_jobs` also refuses to
execute, because the engine promotes it to watch mode and one blocking tool call
wedges the whole stdio session).

Agent IDEs (Claude Code, Cursor, VS Code) auto-discover the server through a
`.mcp.json` file at the workspace root:

```json
{
  "mcpServers": {
    "putnami": { "command": "putnami", "args": ["mcp"] }
  }
}
```

`putnami init`, `putnami install` and `putnami upgrade` add the `putnami` entry
to this file when it is missing, so a new agent session finds the server with no
manual step. The first-use install that runs before another command does not.
The write is merge-aware and non-destructive: other servers and unknown keys in
an existing file are preserved, a `putnami` entry that differs from the one
above is kept as written, and a `.mcp.json` that fails to parse is left
untouched with a warning.

`putnami mcp install` is the explicit form of the same write. It is the one
command that rewrites a diverged `putnami` entry, which is what repairs a
hand-edited one, and it fails on a file it cannot parse. See
[ADR 0040](adr/0040-register-the-mcp-server-and-reconcile-agent-workflows-at-session-start.md).

Agent request provenance is disabled by default. A harness can opt in and
explicitly name its active model in the MCP server entry (the model is never
inferred):

```json
{
  "mcpServers": {
    "putnami": {
      "command": "putnami",
      "args": ["mcp"],
      "env": {
        "PUTNAMI_AGENT_IDENTITY": "1",
        "PUTNAMI_AGENT_MODEL": "claude-sonnet-4-5"
      }
    }
  }
}
```

The server captures the MCP `initialize.clientInfo` name/version for that
session. Opted-in API requests use `putnami-mcp/<version>` provenance plus
`Putnami-Agent-Harness` and `Putnami-Agent-Model` headers. Values containing
control characters are rejected and every client-supplied value is limited to
128 ASCII characters. These headers may be retained by API or proxy logs, so
enable them only when that provenance is appropriate. Prompts, credentials,
tokens, user identity, and session secrets are never included.

See [15-mcp-server-spike.md](15-mcp-server-spike.md) for the design, the
hand-rolled-vs-SDK decision, contract stability, and the evidence.

### `completion`

Generate shell completion scripts.

```bash
putnami completion bash
putnami completion zsh
putnami completion fish
```

Pipe the output to the appropriate completion file for your shell.

### `help`

Renders the generated reference (see the note at the top of this file). It is
the only command with no behavior of its own to document here.

## Global Flags

The global flag vocabulary — project selection, execution, output, and advanced
flags, each with its accepted values and its default — is catalog data, so it is
generated rather than restated here:

```bash
putnami help                 # categorized global flags
putnami help --markdown      # the same, plus per-command flags
```

`putnami help` also documents the smart default selection: with no target, a
feature branch runs `--impacted` against the trunk, while `main`/`master` uses a
local same-command/same-params run marker first, then an active remote-cache run
marker, and falls back to every project.

### Credential provider: `--providers`

`--providers <list>` lets this process ask the workspace's credential provider
for registry credentials. The list is a comma list of `install` and `publish`;
the flag can repeat, and `PUTNAMI_PROVIDERS` supplies the list when the flag is
absent. The flag wins over the variable. Any other value is a usage error that
names its source. The feature is off by default.

```bash
putnami install --providers install
PUTNAMI_PROVIDERS=install putnami build --projects my-app
```

| Value | Credential | Used by |
|---|---|---|
| `install` | `read` | the CLI's archive downloads: extensions, templates, agent workflows, the CLI binary |
| `publish` | `publish` | the engine's uploads in a release-set publish, when the provider negotiates `publication-v1` |

The choice holds for one process. The CLI removes `PUTNAMI_PROVIDERS` from the
environment of the tasks, hooks and extension commands it starts, so a nested
`putnami` starts with the feature off unless its own command line turns it on.
The one exception continues the same invocation: when `upgrade` restarts into
the CLI it just installed, it sets `PUTNAMI_PROVIDERS` to the list this process
enabled, from the flag or the variable.

The credential provider is the one installed extension that declares the
`credential-provider` command
([`protocols/registry`](../../../protocols/registry/README.md),
[ADR 0002](../../../protocols/registry/doc/adr/0002-one-credential-call-per-purpose.md)).
The CLI starts it on the first download that needs a credential and asks it
once per credential for the whole process, again only when the credential
nears its expiry. When `publish` is on, the CLI starts it before the first
hook instead, locally as on a hosted run, so the publish credential comes from
a provider that started before any repository code.

When the provider also echoes the `publication-v1` capability, a release-set
publish runs through that one session
([ADR 0057](adr/0057-publication-authority-stays-in-the-engine.md)):

1. The provider resolves the channels. No release-set provider process starts.
2. The engine opens the plan once, after every task that neither publishes nor
   depends on a task that publishes has succeeded. A bound request opens after
   its barrier commands.
3. Each publication job packs its members into a private outbox that
   `PUTNAMI_PUBLICATION_OUTBOX` names, and receives no registry or cloud
   credential. A publication job whose result is reused from a cache, or
   shared with another run, packed nothing in this run, so the release refuses
   the run and names the job.
4. The engine hashes every packed file again, refuses a member the plan does
   not assign to that job, and uploads each npm, Go module, OCI and Put
   registry member itself with the `publish` credential. Each upload node
   reports one `published-member` event in the session.
5. The engine releases the set over the same session. A refusal names its code
   and moves no channel.

A Put registry member is a release archive (ecosystem `archive`) or a config,
migration or site-content member (ecosystem `put`). The engine uploads its
blobs and its manifest to the project's `registries.put.registry`, else the
default Put registry, with [`put-write/v1`](../../../protocols/put/README.md),
which moves no channel. Its digest is the SHA-256 of the manifest payload the
registry stores. The engine refuses the release when a selected Put registry
member has no engine upload, or when a job reports one itself.

The plan names the commit, its members and the channel heads, and nothing of
the run that opens it, so a local run and a hosted run of one commit open the
same plan and release the same set.

Without the echo, the release set publishes through its release-set provider,
unchanged.

- **No provider.** When no installed extension declares the command, the flag
  changes nothing: every download uses the host-keyed credential
  (`putnami cloud registry-token --host <host>`), as without the flag.
- **Two providers.** When two extensions declare the command, the first
  download that needs a credential fails and names both. A command that
  downloads nothing, such as `putnami extensions remove`, still runs.
- **Hosts.** The provider's credential names the hosts it is valid for. A
  download from any other host uses the host-keyed credential.
- **No credential.** A provider that holds no credential answers that it has
  none, and downloads use the host-keyed credential.
- **Refusal.** A provider refuses only when policy forbids the purpose, for
  example a suspended account. A refusal fails every download of that purpose,
  whatever its host, with the refusal code. The CLI never retries it with
  another credential.
- **Provider failure.** When the provider crashes, times out or answers a
  malformed line, the download fails. The CLI tries the provider again for a
  download that starts 30 s later or after, and restarts it first when it
  exited.

Downloads that run before the command line is parsed, such as a cold
workspace-pin launch, keep the host-keyed credential. Extension processes keep
it too in this release. With
[`--credential-fd`](#run-credential---credential-fd), no download of the CLI
uses the host-keyed credential, and an extension process that asks for one
through the SDK's `registrycred` helpers gets none.

A `--where remote` run carries the list in the execution request only when the
runner provider echoes the `invocation-providers-v1` capability at initialize;
the executing engine then enables exactly those purposes and ignores its own
`PUTNAMI_PROVIDERS`. The one exception is `publish`: a request without
`invocation.publication` plans no publication, so the executing engine leaves
`publish` off and says so on stderr. When the runner provider does not echo the capability,
the CLI refuses the run with a usage error that names it, before anything is
submitted.

### Run credential: `--credential-fd`

`--credential-fd <n>` reads the run credential from the open descriptor `<n>`.
A hosted runner uses it to give the engine a credential that no process the
repository controls can read: `./putnamiw`, a hook, a task, or an extension
command. The runner writes the credential into a pipe, closes the write end,
and starts the CLI binary with the read end open as descriptor `<n>`. Start the
binary itself, not `./putnamiw`: the wrapper is a repository file, and any
process that holds the descriptor can read it.

```bash
putnami build --impacted --credential-fd 3 3< <(printf '%s' "$RUN_CREDENTIAL")
```

- **What it reads.** The CLI reads the descriptor to its end, at most 16384
  bytes, before it starts any process or reads the workspace. One trailing
  newline is removed. An empty credential, a credential that holds whitespace
  or one over the bound is a usage error that names the flag and the
  descriptor, never the bytes. So are a descriptor that is not open, a
  descriptor below 3 (the standard streams), a value that is not a number, and
  the flag given twice.
- **Nothing is kept.** The CLI closes the descriptor once it has read it, so no
  process it starts inherits it. The credential goes in no environment variable
  and no file, and the CLI never prints it. With the flag, the CLI also removes
  `PUTNAMI_CACHE_TOKEN`, `PUTNAMI_CLOUD_TOKEN`,
  `PUTNAMI_SESSION_REPORTER_TOKEN` and `PUTNAMI_LOG_REPORTER_TOKEN` from its
  environment, so no process it starts receives any of them. For each
  variable that held a value, one line on stderr says so. A job that publishes
  with `PUTNAMI_CLOUD_TOKEN` therefore fails on a run with the flag. A
  session reporter or log reporter receives the run credential over its
  protocol instead ([session reporting](16-session-reporting.md#hosted-runs)).
- **The same invocation keeps it.** When the CLI replaces itself with the
  pinned CLI of the workspace, or restarts into the CLI `upgrade` just
  installed, the next image receives the credential on a new descriptor, and
  its command line names that descriptor with `--credential-fd`.
- **A hosted run accepts only a pinned CLI at or above the custody
  level.** The custody level numbers what a CLI guarantees for the
  credential. This CLI is at level 2, and its `--help` line for
  `--credential-fd` ends with `custody level 2`. Before the CLI replaces
  itself with the pinned CLI, it runs `<pinned CLI> --help` in an empty
  temporary directory, with only `PATH`, `HOME`, `PUTNAMI_NO_RELAUNCH=1` and
  `PUTNAMI_NO_AUTO_INSTALL=1` in its environment. When that help lists no
  `--credential-fd`, advertises no custody level, or advertises a lower one,
  the run stops with exit 2 and a message that names the pinned version:
  move the pin with `putnami pin <version>`. When the help cannot run, the
  run stops with exit 1. Without the flag, the CLI runs no such check.
- **A hosted run needs an artifact store outside the workspace.** With the
  flag and neither `HOME` nor `PUTNAMI_ARTIFACT_DIR` set, the store would be
  `<workspace>/.putnami/artifacts`, where the repository could commit the
  pinned CLI or an extension runtime. The run stops with exit 2 before it
  reads the workspace.
- **No Putnami command runs inside a credentialed fetch.** The engine sets
  `PUTNAMI_HOSTED_FETCH=1` in the dependency fetch that receives the job
  credential, and in no other job. A CLI started with that variable set
  stops with exit 2 before it reads the workspace or loads an extension.
  Extensions advertise no custody level, and the engine starts every fetch.
  A fetch built on an older extension SDK can
  start `putnami cloud registry-token`; that command fails instead of
  running the workspace's extensions beside the credential. Move the
  extension's pin with `putnami upgrade`, run without the flag, and commit
  the lock.
- **Store extensions and the workspace's path extensions run.** With the
  flag, the CLI runs the extensions it installed from the artifact store and
  the workspace's own path extensions: a workspace project with a
  `putnami.extension.json`, or an `extensions` entry whose key is a path
  inside the workspace that starts with `/` or `./`, such as
  `/tools/my-extension`. Any other key names an extension, and the CLI loads
  the build the lock pins from the artifact store, even when a workspace
  directory or project has that name. When that build is not installed, the
  CLI skips a project of that name with a reason that names the pin, and the
  extension stays absent. The CLI skips any other extension with a reason:
  one from an absolute path, or from a path that resolves outside the
  workspace. It also skips, with a reason of its own, an extension whose path
  lies inside a `node_modules` directory at any depth. `putnami extensions
  install <path>` accepts only a path extension of the workspace. The CLI reads no registry that the workspace
  declares: `PUTNAMI_REGISTRY_PUT_URL`, `PUTNAMI_REGISTRY_URL` or the default
  registry decides.
- **A path extension serves no provider.** With the flag, the CLI removes the
  `credential-provider`, `cache-provider`, `runner-provider`,
  `session-reporter`, `log-reporter` and `cloud-release-set` commands of a
  path extension. The extension still loads. When a run needs a provider and
  no store extension serves it, the error names each path extension and
  command that the CLI removed. Every provider comes from a store extension. The path extension's
  other commands, jobs and hooks run after the credential's last handoff, as
  the next items describe.
- **The committed files stay as they are.** With the flag, `putnami install`
  writes no `putnami.lock.json`, `bun.lock`, `.npmrc`, `go.work`, `go.mod`,
  manifest or assistant content (`AGENTS.md`, `.mcp.json`, agent workflows).
  A configured extension or template that the committed lock does not pin
  fails the install: run `putnami install` without the flag and commit the
  lock.
- **Toolchains come from outside the workspace.** With the flag, the CLI starts
  no toolchain candidate that resolves inside the workspace, such as a Go
  release the workspace holds under `.putnami/extensions`, and no step
  installs one. The runner provides the pinned Go and bun on its `PATH`, in
  `GOROOT`, or in the Putnami home.
- **No process starts with the credential after repository code.** Once a
  hook, a task, a path extension's runtime, its workspace probe or the probe
  of a runtime toolchain that its manifest declares has started, the CLI
  gives the credential to no new process: a fetch job, a cache provider or a
  credential provider that would receive it later fails the run with an
  error that names both, and a reporter that would receive it later fails
  its delivery. With the flag, the store extensions' dependency fetch runs
  first, then the remote cache provider starts, then the selected reporters
  start, then the path extensions'
  `workspace-fetch`, then the install hooks, the installers, the `before`
  hooks and the tasks. Only the store extensions' dependency fetch receives
  the job credential. It starts no path extension's runtime. A path
  extension's `workspace-fetch`, and a `workspace-fetch` that a plan or an
  alias selects, run offline, like any other task. In the store extensions'
  dependency fetch, a `workspace-fetch` task that is not its extension's own
  runtime (`{extensionRuntime}`) fails a hosted install before it starts, and
  no job runs an extension's `preBuild` hook.
- **A credential holder is a native executable.** With the flag, a cache
  provider or a credential-provider starts only as its extension's native
  runtime: its task command is `{extensionRuntime}`, and the runtime
  executable is a regular file in the format of the host: ELF on Linux,
  Mach-O on macOS, PE on Windows. A `#!` script, a link or a file of another
  format is refused. Any other holder, such as a shell launcher or a bun or
  node entry, fails the run before its first hook, with an error that names
  the holder and `{extensionRuntime}`: a launcher or an interpreted entry
  reads more store files after it starts, when repository code may have
  rewritten them. The CLI cannot check what a native runtime reads after it
  starts, so a provider that serves a hosted run loads nothing from the store
  once it runs. A hosted run with a remote cache therefore needs an
  `@putnami/cloud` release whose `cache-provider` task runs
  `{extensionRuntime}`: move its pin with `putnami upgrade`, run without the
  flag, and commit the lock. A session reporter or a log reporter follows
  the same rule but does not fail the run: one that is not its native
  runtime, or that does not speak session reporting v2, starts without a
  credential and without its token, and the CLI prints why. Without the
  flag, a provider starts as its task declares.
- **`putnami upgrade` refuses the flag.** An upgrade rewrites the lock that a
  hosted run executes as committed. Upgrade without the flag and commit the
  lock.
- **Watch refuses the flag.** `--watch`, and `serve`, which always watches,
  stop with exit 2 before anything starts. Each watch iteration starts after
  repository code ran, when the CLI hands the credential to no new process,
  and a hosted runner runs one finite invocation per fresh sandbox. Run a
  finite command, such as `putnami build`, without `--watch`.
- **Extension command groups refuse the flag.** A command group that an
  extension adds, such as `putnami cloud …`, resolves its remote cache only
  after the install ran repository code, too late for the cache provider of
  a hosted run. It stops with exit 2 before anything starts. Run a job
  command, such as `putnami build`, or a built-in command, such as
  `putnami install`.
- **One credentialed invocation per fresh sandbox.** The runner passes
  `--credential-fd` to one invocation per fresh sandbox: a fresh checkout,
  `HOME`, artifact store and `TMPDIR`, in a container or VM that no earlier
  process outlives. An earlier invocation's repository processes can leave
  `.git/config` entries that git runs, such as `core.fsmonitor` or
  `core.hooksPath`, untracked state under `.putnami`, a rewritten store or
  home, and detached daemons. `putnami build --credential-fd <n>` installs
  the workspace itself; run no separate `putnami install` with the flag
  first.
- **The first downloads use the user scope's provider.** The pinned CLI and
  the lock-pinned extensions download before the workspace's credential
  provider starts. A credential-provider declared by an extension of the user
  scope (`~/.putnami/user`) serves the `read` purpose for those downloads, and
  for no other. It receives the run credential in `initialize` only, and
  stops before the pinned CLI starts and before the workspace's provider
  starts. `--providers install` or `PUTNAMI_PROVIDERS=install` enables it
  without `--credential-fd`. A workspace extension never serves these
  downloads. Without the flag and without a user-scope declarer, they keep
  the host-keyed credential.
- **No host-keyed credential.** With the flag, no download of the CLI uses
  the host-keyed credential, and the CLI never starts
  `putnami cloud registry-token`, the child that answers it: that child is a
  CLI without the run credential that loads the workspace's extensions.
  `putnami projects create` skips the module-origin credential it asks for
  without the flag. An extension process that asks through the SDK's
  `registrycred` helpers starts no such child either: the helpers start no
  process when `PUTNAMI_OFFLINE_DEPENDENCIES=1` is set or the engine handed
  the job a credential descriptor, even one that holds no credential. A
  download that no credential-provider serves goes out without a credential.
  When the registry refuses it (HTTP 401, 403 or 404), the run fails with an
  error that says to install a user-scope credential-provider with
  `putnami extensions install --user <extension>`. A 404 also says that the
  version may not exist.
- **Linux.** The CLI marks itself non-dumpable (`prctl(PR_SET_DUMPABLE, 0)`)
  before it reads the descriptor. It does the same when `--providers` enables
  a credential provider, with or without `--credential-fd`. A process of the
  same user can then no longer read its `/proc/<pid>/environ` or
  `/proc/<pid>/mem`, list its descriptors, or attach to it with `ptrace`. A
  process with `CAP_SYS_PTRACE`, such as root, still can. The mark covers the
  CLI only: the processes it starts begin dumpable.
- **macOS.** The descriptor handling is the same. macOS has no equivalent of
  the non-dumpable mark that a process can set without side effects, so the
  CLI sets none.
- **Windows.** The flag is a usage error: "--credential-fd is not supported
  on Windows".

The flag changes no cache key and no run marker: every task receives the
arguments of the same invocation without it. Without the flag and without
`--providers install`, the CLI behaves exactly as before.

### Structured output

`--output=jsonl` and `--output=json` are accepted only by the commands that
declare structured output in the catalog; `putnami help <command>` says whether
one does. A structured command that does **not** support it rejects the flag
with an explicit error rather than silently ignoring it — mutating and
diagnostic paths (`projects create`, `deps install`, `cache clean`, `install`,
`upgrade`, …) emit free-form text only.

## Job-Specific Flags

Extensions can declare additional flags in their manifest. These are passed through to job subprocesses. For example:

```bash
# The --target flag is declared by @putnami/go for the build command
putnami build --all --target linux/amd64
```

Unknown flags are passed through to job processes. The CLI provides typo suggestions using Levenshtein distance when a flag doesn't match any known flag.

## Command Aliases

### Built-in Aliases

Single-letter shortcuts for the conventional job commands. They are catalog
data, so the current set is printed by `putnami help --markdown` (the "Aliases"
block) rather than listed here.

### User-Defined Aliases

Define custom aliases in `putnami.workspace.json`:

```json
{
  "aliases": {
    "dev": "serve",
    "ci": "lint,test,build",
    "check": "lint,test"
  }
}
```

Alias resolution chains are supported (up to 10 levels to prevent infinite loops). Built-in aliases take precedence, then user aliases.
