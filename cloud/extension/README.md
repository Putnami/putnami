# @putnami/cloud

Putnami Cloud CLI extension. The workspace project is
`@putnami/cloud-extension`, the Go module is `go.putnami.dev/cloud/extension`,
and the binary is built from `cmd/putnami-cloud`.

Cloud declares the `archive` and `put` ecosystem profiles consumed by the
framework's Publish v2 planner. Archives, configuration and migration members
use native `namespace/package` coordinates and channel projections; their
registry entry accepts `registry`. The release-set command and these profiles
must ship together, otherwise the framework refuses publication before calling
the provider.

A workload can publish both Config and migration members in one release set.
Migration selection binds its exact Put coordinate as well as its project,
so the workload's Config member cannot be mistaken for its migration bundle.
Declare `@putnami/cloud:publish-migration.namespace` explicitly in the workload's
project options. Its migration member uses the Go `package` command's `describe`
step or the TypeScript `generate` step, which emit `.gen/migration-bundle`.
Acceptance compares the bundle's bare hexadecimal semantic digest separately
from the `sha256:` blob and manifest digests returned by Data.

A continuously deployed workload can also publish its deployment declaration
as a `put` member of kind `deployment`, at
`<config namespace>/<project name with / replaced by ->-deployment`. One switch
turns it on: the framework's `deployment` package step, which is off by
default. Set `"deployment": true` under `options["@putnami/go"]` or
`options["@putnami/typescript"]` in the workload's own `putnami.json`, or add
`deployment` to its `publish` channels. Cloud has no option of its own for it.
The workspace probe declares the member only when the project also declares a
Config member and sets `options["@putnami/cloud"].deploy.enabled` to `true`; a
library, a project with the `image` channel, and a project with both language
extensions get none. Workspace-level defaults do not count, because the probe
reads only the project's own options. The language step writes the canonical
declaration to `.gen/deployment.json`, and the
`publish~cloud-publish-deployment` step uploads those bytes unchanged under
`application/vnd.putnami.infra.deployment.v2+json` at the version the
release-set plan chose. When the plan selects the member, a missing,
non-canonical or invalid declaration fails the step. The declaration's
`workload` must equal the project ID without its leading slash: the
workspace-relative path without grouping folders such as `(web)`, or the
project name for a project at the workspace root. An older framework writes
the project name, so the step fails for a project whose name differs from its
path. Pass `--skip-publish-deployment` to skip that step alone; without a
selected member it publishes nothing.

Workspace machine-token creation supports `--allowed-clients <name,...>` (public
client ids such as `review-worker`) and `--allowed-client-ids <uuid,...>` (OAuth
client row UUIDs) to restrict the key's clients; the server binds the union of
both. Review workers require the `review-worker` client alone
(`--allowed-clients review-worker`), the six review scopes, and an explicit
expiry.

The contract-v3 runtime is the archive-relative compiled binary
`compiled/putnami-cloud`. Its version linker symbol is
`go.putnami.dev/cloud/extension/internal/cloudcli.cloudRuntimeVersion`.
Release archives ship that binary so end users do not need Go installed.
Source checkouts prepare the same path with
[`bin/prepare-runtime`](bin/prepare-runtime), which builds with the module's
`GOWORK=off` replace closure into the core-assigned runtime output directory.

Every manifest task and MCP tool runs `{extensionRuntime}`, including the
remote build-cache provider. The Putnami CLI resolves `{extensionRuntime}` to the
archive's `compiled/putnami-cloud`, or to `compiled/putnami-cloud.exe` on
Windows, and checks it with the `__putnami runtime-info` handshake (identity,
version, platform, CLI contract and runtime-protocol version) before it runs a
task. These commands start no shell script, so the Windows archive runs them
like the macOS and Linux ones. In a source checkout, the CLI prepares the
runtime once per source digest with `bin/prepare-runtime`, which needs Go and a
POSIX shell. Contributors work on macOS and Linux.

The `cloud-cache-provider` task runs `{extensionRuntime} cache-provider`. On a
hosted run the cache provider holds the run credential, so it must be one
native executable that the CLI starts directly. A shell launcher would read
more store files after it starts, and a repository process may already have
rewritten them. The runtime is never the workspace build output, which every
`putnami build` rewrites in place. It is the installed archive's binary or, in
a source checkout, a binary the CLI builds in a private staging directory and
publishes by atomic rename, so a concurrent build cannot hand the provider a
half-written file. The task needs no POSIX shell, so Windows starts it like the
other commands. The CLI prepares `{extensionRuntime}` for a cache provider in
every build since 2026-09-28; an older CLI cannot start this provider.

The `cloud-credential-provider` task runs `{extensionRuntime} credential-provider`
on the same terms: it receives the hosted run's credential, so it is the native
runtime and never a cached task. See [Credential provider](#credential-provider).

Cloud declares a marker-only `workspace` adapter for its publish profile. Cloud
setup and install configure an existing Putnami workspace but do not discover
projects or synchronize language-owned workspace metadata; `workspace-install`
is an explicit lifecycle command, not a discovery or synchronization claim.

The extension owns the Cloud login surface directly and does not call the
built-in `putnami auth` commands. Login is global to the user; workspace
targeting is configured separately by `putnami cloud setup`. OAuth
tokens are stored in the standard Putnami CLI credential file:

```text
~/.putnami/auth.json
```

The cloud workspace config is persisted in the root Putnami manifest under
`options["@putnami/cloud"].workspace` (`putnami.workspace.json` for monorepos,
or root `putnami.json` for single-project workspaces). The CLI also writes the
legacy local cache for compatibility:

```text
.putnami/cloud-link.json
```

The root manifest is the source-controlled workspace binding. The local cache
exists only for older tooling and for faster workspace-root discovery. Setup
updates are surgical: the CLI inserts or replaces only
`options["@putnami/cloud"].workspace` and preserves the existing manifest key
order and formatting outside that object.

## Auth and workspace model

`putnami cloud login` creates one global CLI session for the user. It does not
select a workspace, and it ignores a legacy `--workspace` flag.
Repository targeting is explicit:

```bash
putnami cloud login
putnami cloud setup --workspace <workspace-id>
```

### Intelligence

The Intelligence commands and the six `putnami.*` MCP tools ship with the
`@putnami/intelligence` extension, not with this one: `putnami intelligence
setup`, `status`, `review`, `audit-attest` and `provenance`. They use the
workspace link and the session this extension writes.

The setup command verifies the existing repository workspace link when one is
present. Pass `--workspace <workspace-id>` to select or join a different
existing workspace, or omit both an existing link and `--workspace` to create a
workspace from the root manifest name. The selected workspace is committed with
the repository through the root manifest, so every contributor and CI job
resolves the same cloud workspace.

Workspace-scoped API calls (`deploy`, `publish-config`, `secrets`, and
`token`) mint a short-lived access token from the global refresh
token. The scoped access token is cached in `~/.putnami/auth.json` until it is
near expiry, so repeated cloud commands do not rotate the refresh token on
every invocation. If another process rotates the refresh token first, the CLI
reloads the credential file and retries once.

The auth server stamps the selected workspace into the JWT as
`scope_ref.workspace_id`. That claim is a target selector, not a membership
grant. Control-plane and config APIs still authorize the request with the
caller's roles and scopes; missing permissions return `403`.

## Live workspace CI settings

`putnami cloud ci enable` creates or enables the current workspace's operational
CI subscription. `putnami cloud ci disable` revokes admission without a service
deployment; `putnami cloud ci subscription --output=json` reads its current
revision and settings. New subscriptions default to the `canary` runner;
`enable --runner-channel stable` explicitly holds the workspace on `stable`.

```sh
putnami cloud channels follow putnami stable --runner-channel stable
putnami cloud channels follow putnami canary
putnami cloud channels follow putnami rs_exact
```

`channels follow` reads the current revision and makes one authorized
compare-and-swap (CAS) update, preserving the other tracks, required checks and
drift settings. The optional runner change commits in that same update. A
conflict is reported rather than silently overwriting a concurrent workspace
edit. Delivery resolves producer ownership and the current Distribution read
grant; subscription enablement does not grant arbitrary registry access. The
CLI needs an existing subscription before setting a track.

## Commands

Validate a project's authored production Config inputs before starting the full
package and release-set pipeline:

```bash
putnami cloud config validate [project] --env prod
```

The command reads the existing `schema/config.json` (or
`.gen/config-schema.json`), optional `schema/config-authored-fields.json`,
production values, the workspace Cloud link, `.gen/version.json`, and the
canonical GitHub origin and revision. It applies the exact authored-member
normalization and validation used by packaging, including rejection of
undeclared descendants under opaque generated objects. It writes no package
artifact, creates no release-set plan, and makes no network request. Generate
the schema and version artifacts first; this command does not run a build, so
a stale generated schema validates as stale input.

Missing schema and namespace declarations are errors for this explicit command.
The ordinary `putnami validate --projects <project>` command also runs this
check as `validate~cloud-config-member`. It checks the production Config member
when the project declares a native Config namespace; other projects skip the
check without requiring a Cloud link. A declared member with a missing schema
fails. Like the explicit command, this step reads existing generated artifacts
and does not build or package anything.

This step is cached, and a skip is cached too. The step runs again when any
input it reads changes:

- the project's `putnami.json`, including its `options.publish` block
- `schema/config.json`, `schema/config-authored-fields.json` and
  `.gen/config-schema.json`
- the production values: `conf/env.yaml`, `conf/.env.yaml`,
  `conf/env.prod.yaml`, `conf/.env.prod.yaml`, and the same two production
  files under `.gen/conf/`
- the workspace Cloud link: `putnami.workspace.json`, the root `putnami.json`
  and `.putnami/cloud-link.json`
- the `--app` and `--schema-from` parameters
- the base version of the workspace version line: a line tag, or the first
  `feat:` or breaking commit since the last tag, runs every project again
- the extension's own code. A workspace that installs the published extension
  keys it on the extension version. A workspace that runs the extension from a
  local source keys it on every module its `go.mod` replaces with a local
  directory, and on the operating system and architecture, so a macOS laptop
  and Linux CI do not share a result.

The step checks only the project in its working directory, the directory the
key hashes. It takes the project name from that directory's `putnami.json`,
and the CLI resolves that name to the working directory before it searches
the workspace, so a same-name copy elsewhere in the tree does not change the
result. The step refuses `--schema-from` and `--app` naming another project,
because each one would read files the key does not cover. Run
`putnami cloud config validate` for those checks; it is not cached.

The manifest selects `putnami.json` through the glob `putnami.jso[n]`. The
cache hasher keeps every byte of a file that a glob selects. A file selected
by its plain name is hashed as a view that drops any option block another
extension claims, and `@putnami/sdd` claims `options.publish`, one of the
blocks that declares the Config namespace.

With a remote cache, the CLI computes every cache key before the session
runs a task; otherwise it computes each key when the task starts. The step
declares that it reads the project's `gen` resource, so in both cases it runs
before `build-generate` rewrites `.gen` in the same session and reads the
same `.gen` files its key hashed. Two framework tasks write files in the key
without declaring what they write, so a session that runs them with this step
can store a result for files the key did not hash:

- `config-merge` writes `.gen/conf/.env.<APP_ENV>.yaml`; this matters with
  `APP_ENV=prod`
- `config-extract` writes `schema/config.json`, and the TypeScript task also
  `.gen/config-schema.json`

The check never reads `conf/.env.local.yaml`, so a local override does not
start a new run. The key leaves out `.gen/version.json`: the CLI writes that
stamp for every planned project before it computes any key. The stamp is the
keyed base version, or that version plus a commit suffix, so the check on its
form depends only on keyed data. Any other commit therefore reuses the result
when none of the inputs above changed. The Git origin URL is not part of the key:
the check only requires a GitHub origin. After you change the origin of a
checkout, run `putnami validate --no-cache` to check it again.

For projects declaring `publish: ["site-content"]`, ordinary validation also
runs `validate~cloud-site-content`. It checks section mount slugs, nonempty
sections, regular files (no symlinks or special files), and payload-relative
paths with the same source predicates used by packaging. It traverses only
the selected site's section trees, inspecting directory metadata without
reading or hashing file contents. This step is uncached because the cache key
does not cover directory metadata, such as an empty section directory or a
symlink that replaces a regular file. Additions, removals, renames and
file-type corrections are checked on the next run even with warm caches. Its
task identity reads only `putnami.json`; unrelated projects do not launch the
site checker. The cached Config step still launches for every project with a
`putnami.json`, then skips those without a Config namespace.

Every other task of this extension is uncached. Tasks that call Putnami Cloud,
such as login, token, publish, deploy and install, have effects that a cache
hit cannot reproduce. The remaining pipeline tasks state their reason in the
task description of [`putnami.extension.json`](putnami.extension.json).

These source checks do not assemble even an in-memory bundle. No build,
package, publish, copy, generation, archive or compression, install or network
operation is required. Packaging retains payload reads, digests, provenance,
manifest and archive verification. Generated migration bundles, produced image
layers, npm loadability and publication or runtime checks remain at their
existing stages; ordinary validation does not prove those artifacts or SDD
contract drift. The root CLI still refreshes its existing per-project
`.gen/version.json` stamp, and a configured remote cache may communicate with
its provider. Those framework behaviors are separate from this source checker;
a whole ordinary validation invocation is therefore not yet free of all writes
or network I/O.

`putnami cloud config publish --dry-run` is a legacy control-plane publication
preview and is not equivalent: it follows the older schema and value write path
instead of validating the native authored Config member.

The canonical project flow uses Putnami root commands and lets extensions
contribute project tasks to the same command:

```bash
putnami publish <app> --docker
```

There is no deploy verb: a workload deploys through an environment that follows
a channel, once a publish moves that channel.

For app projects that publish a Docker artifact and expose a config schema,
`@putnami/cloud` contributes a `publish~cloud-publish-config` task to
`putnami publish --docker`; pass `--skip-publish-config` to publish the other
channels without registering the config schema and values. Projects whose build
emits a migration bundle (`.gen/migration-bundle`) likewise get a
`publish~cloud-publish-migration` task that uploads the bundle to the Put
registry (put-server). It does nothing when no bundle was built; pass
`--skip-publish-migration` to skip it. Migrations run only through the
continuous deploy, where Data's migration job applies them.

A docs project (a `putnami.json` declaring `publish: ["site-content"]`) gets
one `put` release-set member per section, `cloud/doc-contents-<section>`, in
the Cloud workspace's own Put namespace. Its `package~cloud-site-content` step
assembles and verifies the bundles with no network and keys on the project
tree, so a docs edit republishes the sections. Its
`publish~cloud-publish-site-content` step publishes exactly the planned
members at the planned version and moves the plan's first channel (`canary` on
main) in the same atomic Put publish. Pass `--skip-publish-doc` to skip that
step. Without a release-set plan the step publishes nothing;
`putnami operator publish-doc`, from the `@putnami/operator` extension, stays
the direct operator path. Both resolve the Put credential the way archive
publication does: a stored machine credential for the Put host, else a minted
user token.

A project that declares `publish: ["archives"]` and owns a scoped extension
package, or declares `options.publish.binary-name`, gets an `archive`
release-set member. Archive intent in command options is also recognized;
source extension manifests may inherit the authored project name. Templates
declare their archive through `template-archives` and bind packaging to the
scaffold extension's `template` step. The native coordinate resolver maps the
CLI binary name `putnami` to `putnami/cli`. The Cloud workspace probe keeps
publication ownership under `@putnami/cloud`, while its
`packagePublisher: "@putnami/go"` route selects the real `package~archives`
producer and that job's dependency closure. The resulting
`publish~cloud-publish-archives` step uploads the immutable Put manifest and
emits a `published-member` fact only after Put accepts it or confirms an exact
idempotent readback. Before credentials or uploads, the archive publisher
checks its project, coordinate, version and every requested channel against the
strict release-set plan. The migration compatibility step skips projects with
no bundle; it refuses to upload a real bundle outside the plan's declared
members. Migration membership belongs to the Data publisher.

When the engine runs a publish step under its `publication-v1` capability, it
sets `PUTNAMI_PUBLICATION_OUTBOX` and gives the step no registry credential.
The config, migration, archive, site-content and deployment steps then pack
their one planned member into that outbox: the exact Put manifest and blobs
they would publish, so the artifact digest is the same. They upload nothing,
move no channel, and emit no `published-member` fact; the engine uploads the
packed member and reports it. Before it commits a Put or archive member, a step
runs the same put-write/v1 check as the engine's upload, so an invalid member,
such as an archive with an invalid platform key, fails in the step. The config,
migration and deployment steps, and an archive step with archives to publish,
fail when they hold an outbox but no release-set plan. The site-content step
skips without a plan, and an archive step skips when it has nothing to publish:
no archives channel, or no metadata or archive files under `--if-present`. The
site-content step fails when the plan selects more than one section of the
project. In every case the engine itself refuses a release whose selected Put
or archive member it did not upload.

For generated opaque Config objects (such as external Go structs), declare their
exact public descendant paths and types in `schema/config-authored-fields.json`
using the native `{ "fields": [{ "path": "app.provider.Issuer", "type": "string" }] }`
shape. These declarations only extend opaque, nonsensitive generated objects;
they cannot replace generated fields or open a closed object. Packaging never
infers field permissions from values, and undeclared descendants still fail.
Keep this file alongside the owning workload schema when production keys change.

For extension archive publishes, `putnami.extension.json` owns the registry
package identity, so this project publishes as `@putnami/cloud` even though the
workspace project is named `@putnami/cloud-extension`.

`@putnami/cloud` also contributes file-activated project verbs. `package`
activates across the workspace, including Go projects without a Cloud
dependency. It verifies image layers for `image-layers.json` projects and
builds a canonical Config member for projects with `schema/config.json` or
`.gen/config-schema.json`. `test` is activated by `tests/run.sh`: the
`cloud-shell-test` task runs that harness and takes its exit status as the
verdict. Both are narrow by activation rather than by name: a project without
the activation file schedules neither. An image project such as the CI runner
image is a `type: "image"` project that the framework assembles without a
Docker daemon, from the layers `putnami cloud image-layers` produces. The
`test` verb exists because such a project's subject under test can be a bash
entrypoint, and no language extension provides a `test` job for it. Its task
is uncached: a shell harness that drives real git and process fixtures has
inputs wider than any declarable file set, and a stale hit would report a
green gate for an unexercised entrypoint.

Pass the Docker or config artifact flag explicitly when you invoke `publish`
directly. Cloud config publication consumes the optional typed task input named
`schema`: it matches the `schema` output port from language
`config-extract-exec` tasks, whose durable artifact is `schema/config.json`.
This exact port identity is the planner contract; it does not depend on a
command name, extension version, or whether Cloud happened to load.
`--schema-from` remains an explicit user override, rather than an implicit
discovery fallback. In an ordinary Publish v2 plan, a schema-bearing project
must author `namespace` in its existing Cloud publish options. The package step
then creates the secret-free member at
`<namespace>/<project-path-with-dashes>-config`; the publish step accepts only
the exact selected project, version and source revision and emits membership
after Put accepts the immutable bytes. This first native path supports `prod`
only and rejects other environments. A direct `putnami cloud config publish`
outside a release-set plan retains the compatibility API flow described below.

The compatibility publisher writes non-secret values for the target env from
committed `conf/env.yaml` and `conf/env.<env>.yaml` files, legacy hidden
`conf/.env*.yaml` files, and generated `.gen/conf/env.<env>.yaml` artifacts
when present. If publish cannot find non-secret values required by the schema
shape, it creates `conf/env.<env>.yaml` with `__PUTNAMI_CONFIG_REQUIRED__`
placeholders and fails until the file is edited. Schema-declared sensitive
fields are never written as config values; publish those through
`putnami cloud secrets`. The generic `--config` publisher is separate from
Putnami Cloud config publication and is only needed for legacy config-server
flows. Because the scaffold writes project sources, both config-publication
tasks declare `mutatesSources` with the project-scoped `sources` write resource.

The CLI has 20 commands. Each one answers `help` with its own verbs, for
example `putnami cloud packages help`. The older names, such as `cloud track`
or `cloud tokens`, still run as hidden aliases;
[`internal/cloudcli/surface.go`](internal/cloudcli/surface.go) maps each one to
its new form. The commands the engine or a provider calls, such as
`release-set` and `registry-token`, keep their names. The framework builds
`putnami cloud --help` from `putnami.extension.json`, which has no hidden field
yet, so that list also shows the aliases and three machine commands.

Platform maintenance commands (`sql-proxy`, `publish-doc`,
`source bind|unbind` and `token --global`) are not part of this extension. They
belong to the `@putnami/operator` extension, and this CLI answers each one with
a usage error that names the `putnami operator` command.

```bash
putnami cloud login
putnami cloud logout
putnami cloud whoami [--strict] [--output=json]      # the session and the linked workspace as a status node; exits 1 when either is missing
putnami cloud setup --workspace <workspace-id>
putnami cloud setup --slug <slug>                  # create the workspace with this slug
putnami cloud setup --auto                         # ensure mode: configure only if already linked + signed in
putnami cloud status [--strict] [--output=json]    # one line per entry: ok, degraded, failing or unknown, and the next command
putnami cloud <entry> status [<name>] [--strict] [--output=json]  # one entry in full: header, metrics, checks with their fixes
putnami cloud token --json                         # script-only scoped bearer
putnami cloud token cache                          # bare bearer for the build cache token source (same as --for cache)
putnami cloud token npm                            # bare registry bearer; IAM determines namespace access
putnami cloud token go                             # bare Go registry bearer
putnami cloud token oci                            # bare OCI registry bearer
putnami cloud token put                            # bare Put registry bearer
putnami cloud token npm --materialize              # one-shot native credential
putnami cloud token create --name <n> --scopes <s,...> [--expires <dur>]  # workspace-owned pkt_* machine token (shown once)
putnami cloud token list                           # workspace machine tokens (name, scopes, created, expires, last-used, id), never the secret
putnami cloud token revoke <id|name>               # revoke a workspace machine token
putnami cloud token status                         # machine tokens that are not revoked, and those expiring within 7 days
putnami cloud registries [setup|status [<registry>]]  # registry endpoints and stored key state
putnami cloud packages status [<protocol>]         # the namespace bindings this checkout needs
putnami cloud packages namespaces activate|list    # namespace bindings of the linked workspace
putnami cloud packages grants create|list|revoke   # read grants on the release history
putnami cloud packages mirrors add|list|remove|rotate-credential  # public mirror targets
putnami cloud packages copy <src-ref> <dst-ref>    # server-side image copy on oci.putnami.dev (metadata only)
putnami cloud packages retag <ref> <tag...>        # add tags to an existing digest in one call
putnami cloud packages revert <repo>:<tag>         # restore a tag to the target recorded before its last move
putnami cloud packages publish [<app>]             # publish pre-built archives as one immutable version
putnami cloud channels set <channel> --from <channel|rs_id>  # promotion and rollback
putnami cloud channels status [<channel>] [--wait <duration>]  # channel heads and how far each registry applied them; behind is degraded, still behind at the --wait deadline is failing
putnami cloud channels follow <namespace> <stable|canary|rs_id>  # what the workspace's CI follows
putnami cloud ci init|validate|fmt|explain         # author putnami.ci.json
putnami cloud ci report --conclusion success       # report a delivery-run outcome
putnami cloud ci status                            # CI readiness checks, queue, last run reuse, runs and spend over 7 days
putnami cloud cache [status]                       # the build cache config, its identity, and the reuse of the last CI run
putnami cloud cache disable [--remove]             # disable (enabled:false) or delete the cache config
putnami cloud source connect [--repo owner/name] [--replace]  # browser-assisted GitHub authorization; structured mode requires --repo
putnami cloud source status                        # connected repository, installation, and provider health
putnami cloud source disconnect [--yes]            # idempotently disconnect after confirmation
putnami cloud config status [<app>]                # declared keys against what each environment resolves
putnami cloud config show [<app>] [--env <env>] [--with-secrets]
putnami cloud config show <app> --schema [--format table|json|yaml]
putnami cloud config show <app> --key <key>
putnami cloud config show <app> --secret-keys
putnami cloud config show <app> --reveal-secrets [--yes]
putnami cloud config show [<app>] --keys           # the resolved key names
putnami cloud config show [<app>] --metadata       # the merge metadata
putnami cloud config validate [<app>] --env prod
putnami cloud config drift <app>
putnami cloud config put <app> --config-from <yaml>
putnami cloud config publish [<app>] [--env <env>]
putnami cloud secrets status [<app>]               # declared secrets, set or not; never reads a value
putnami cloud secrets list                         # workspace inventory, metadata only
putnami cloud secrets list <app> [--env <env>]
putnami cloud secrets set <app> <key> --from-stdin  # or --from-file <path> / --value <v>
putnami cloud secrets reveal <app> [<key>] [--yes]
putnami cloud secrets delete <app> <key>
putnami cloud db status [<db>]                     # databases against the workloads putnami.ci.json selects
putnami cloud db list|info|grant|connect
putnami cloud db migrations publish [<app>]
putnami cloud env status [<env>] [--strict] [--output=json]  # each declared environment against what it runs: its channel and each workload
putnami cloud env status [<env>] --health|--provenance  # the deployment table: CD header, then per-workload revision/health; --health adds errors (10m), top error, and on-main; --provenance shows commit → release → revision → config version
putnami cloud env doctor [<env>] [--strict]       # CD prerequisites as a status node; exits 1 when one is missing
putnami cloud env enable [<env>]
putnami cloud deploy publish-v2 --request-file ./publish-v2-deploy.json [--env prod] [--wait] [--timeout <dur>]  # operator: submit an exact release set
putnami cloud deploy status <release-id>           # one release as a status node; exits 1 when it failed, or ended partial or skipped
putnami cloud logs [<app>] [--env <env>] [--since <t>] [--level <sev>] [--follow] [--output=jsonl]
putnami cloud logs [<app>] --cursor <c> | --all     # cursor pagination / follow all pages
putnami cloud traces [<app>] [--env <env>] [--slow <dur>] [--from <t>] [--to <t>] [--output=jsonl]
putnami cloud metrics [<app>] [--env <env>] [--window <dur>] [--output=jsonl]
putnami cloud registry-token --host <host>         # machine command: host-based compatibility alias
```

`putnami cloud status` reads 12 entries at once and gives each a state: `ok`,
`degraded`, `failing` or `unknown`. The entries are `whoami`, `token`,
`registries`, `packages`, `channels`, `ci`, `cache`, `source`, `config`,
`secrets`, `db` and `env`. An entry whose read fails, or that does not answer
within 15 seconds, shows as `unknown`. Each line names the next command: the
entry's fix when it has one, else its own status command. The deployment table
lives under `putnami cloud env status --health` or `--provenance`.

Every status command builds one status node from
[`internal/clicore/statusnode.go`](internal/clicore/statusnode.go) and exits by
one rule: 1 when the node is `failing`, and also when it is `degraded` or
`unknown` under `--strict`; 0 otherwise.

### Machine output (`--output`)

Every command speaks one canonical machine-output contract so an AI agent (or any
script) has a single stable flag to reach for:

- `--output=jsonl`: the canonical structured selector. For a **streaming**
  command (`logs`, `traces`, `metrics`) it emits exactly one JSON object per item
  (newline-delimited JSON). For a **one-shot** command (everything else) it emits
  the shared CLI result envelope as one compact JSON line.
- `--output=json`: equivalent to `--output=jsonl` for a one-shot command (one
  indented object). On a streaming command it also selects structured output;
  the stream is still one object per line.
- `--json`: the compatible boolean **alias**, equivalent to `--output=json`. It
  stays, and keeps working identically for existing scripts.
- default (no selector, or `--output=text`): the unchanged human surface.
- `--output=cloud-logging`: reserved by the shared Putnami CLI contract for
  structured Cloud Logging renderers.

**Resolution.** `--json` is exactly `--output=json`. Combining `--json` with any
different explicit output mode, including `jsonl` or `text`, is a usage error.
An unrecognized `--output` value is also a usage error. This contract has one
source, [`go.putnami.dev/protocol/cli`](../../protocols/cli), consumed through
[`internal/clicore`](internal/clicore), so the meaning cannot fork per command.

**Per-command JSON schema.** One-shot structured output is the shared
`putnami-cli-result-v2` envelope. `command` names the root entry. For a status
command, `data` is the status node, on success and on failure alike. For
example, `putnami cloud whoami --output=json` prints, without its `metrics`:

```json
{
  "protocolVersion": 2,
  "command": "cloud whoami",
  "status": "success",
  "exitCode": 0,
  "data": {
    "id": "whoami",
    "title": "whoami",
    "state": "ok",
    "detail": "you@example.com, workspace ws-acme",
    "children": [
      {"id": "whoami.session", "title": "session", "state": "ok", "detail": "signed in as you@example.com"},
      {"id": "whoami.workspace", "title": "workspace", "state": "ok", "detail": "linked to workspace ws-acme"}
    ]
  }
}
```

`putnami cloud status --output=json` prints `{state, entries}` as `data`: the
worst state, and the node of each entry. `putnami cloud env status --health
--output=json` and `--provenance` keep the deployment JSON:
`workspace_id`, `deployments`, `channel_follow` and, with `--health`, `health`.

`protocolVersion` is how a consumer tells the contracts apart: it is present on
every version-2 document and absent from every version-1 one, so branching on
`typeof doc.protocolVersion === "number"` is the whole compatibility story: no
CLI version, no flag. Version 1 was emitted by builds published before
`protocol/cli` deleted its v1 emitters.

Failures use the same envelope with `status:"failure"` and an `error` object; an
interrupted command reports `status:"aborted"` rather than folding into failure.
Both carry the shared exit-code taxonomy (`0` success, `1` failure, `2` usage,
`3` auth, `4` api, `130` signal).

Streaming commands emit one object per item: `logs` one log entry per line,
`traces` one trace summary per line, `metrics` one series per line (fields as
returned by the workspace `/logs`, `/traces`, `/metrics` endpoints). One-shot
commands place their command-specific payload under `data`. For example,
`cloud ci report` sets `data` to `{run_id, subject_id, status, conclusion,
provenance, verified, idempotent, workspace_id, repo, sha, submission_id,
reports}`, and `cloud token --output=jsonl` sets `data` to the scoped bearer
envelope. `cloud ci report -- <cmd…>` consumes the wrapped command's
`--output=jsonl` stream unchanged (its tee'd test, coverage and build report
batch is independent of this flag on `report` itself).

**App resolution**:

1. First positional after the verb (for example `putnami cloud config apps/auth-server`). This matches the native `putnami build <app>` convention. For `secrets`, project-wide commands use the positional after the subverb (`putnami cloud secrets list apps/auth-server`), and key commands use `<app> <key>` (`putnami cloud secrets set apps/auth-server database.password --from-stdin`).
2. Walked up from the current directory until the nearest workload `putnami.json` (the same resolution the native CLI uses). `cd apps/auth-server && putnami cloud config` resolves `apps/auth-server` automatically.
3. Legacy fallback: a top-level `putnami.json` at the workspace root (for single-project workspaces).
4. Error: `could not determine app; pass it as the first positional or cd into a workload directory`.

Name-based lookup excludes assistant worktree copies under `.context` and
`.claude`; those copies cannot shadow the selected workspace's real project.

`cloud config publish` still accepts its existing compatibility flags. New
config and secrets workflows target apps positionally.

`putnami cloud login` opens the verification link automatically and prints the
device URL, code, and prefilled direct link. Use `--no-open` when you want those
instructions printed without launching a browser. After authorization, the
command prints the active identity when `/userinfo` or token claims provide one
and suggests `putnami cloud setup` when the current repository is not configured
yet. `--workspace` on login is ignored; use
`putnami cloud setup --workspace <id>` to select the repository's cloud
workspace.

`putnami cloud setup` verifies the workspace already recorded in the root
manifest when present. `putnami cloud setup --workspace <id>` selects or joins
an existing workspace by id, and setup without an existing link creates one from
the root manifest name. Only the data needed to connect this repository to its
workspace is committed to the root manifest: the workspace id, plus the
control-plane URL when it differs from the public default
(`https://api.putnami.cloud`). The full resolved set (workspace id, control-plane
URL, default environment, display name, and repository) is mirrored to
`.putnami/cloud-link.json` for the legacy read path and faster workspace-root
discovery. Setup is the one-stop "configure this repository for Cloud"
entrypoint: it also provisions every workspace-local feature, the remote build
cache included (see [Build cache](#build-cache)). Pass `--no-cache` to skip
cache provisioning and leave any existing `.putnami/cache.json` untouched.

`putnami cloud setup --slug <slug>` chooses the slug of the workspace that
setup creates. A slug is the workspace's short name: at most 12 characters,
lowercase letters, digits and single dashes, starting with a letter. The
workspace's services start with `<slug>-`, and the workspace holds the event
topic names that start with `<slug>.`. Setup refuses a malformed slug, the
platform's own names (`reservedWorkspaceSlugs` in
[`internal/cloudcli/setup_slug.go`](internal/cloudcli/setup_slug.go)), and a
slug that ends with a dash and one of them, such as `my-data`, before any
request. Runtime refuses every push receive of a topic named under such a slug:
the subscription ID of a receive of `my-data.orders` reads `data.orders` after
a dash, a platform name. A workspace keeps its slug: when setup links an
existing workspace, it accepts `--slug` only when it is that workspace's slug.
Without the flag, identity-api derives a slug from the workspace id.

`putnami cloud setup --auto` is the non-interactive "ensure" mode. It configures
Cloud only when the repository already has a linked workspace (from the root
manifest or `--workspace`) and the user is signed in; it never creates a
workspace and never fails. When neither condition holds it skips with a short
note and exits `0`.

`putnami install` also runs the Cloud extension's workspace installer. When the
root manifest already records `options["@putnami/cloud"].workspace`, the
installer restores workspace-local Cloud state without a control-plane call:
`.putnami/cloud-link.json`, `.putnami/cache.json` when missing, and the
non-secret registry endpoint index in `~/.putnami/registries.json`. It cannot
write a registry token source until an exact owner and package target is known.
If the root manifest is not linked, installation returns a successful skip even
when a session exists or `--workspace` is supplied. It never creates or selects
a workspace, subscribes or activates a product. A linked checkout needs no
session and no network for this local preparation; hosted reads use the
existing session when invoked. Existing `.putnami/cache.json` settings are
preserved unless the install is forced. The Cloud installer never writes MCP or
Codex wiring: the `@putnami/intelligence` extension's own installer does, when
the repository enables Intelligence.

`putnami cloud token --json` prints a workspace-scoped bearer for
scripts that need to call the control-plane or config APIs directly. The
command reads the repository config from the root manifest, mints or reuses the
cached scoped access token, and returns the token, workspace id, control-plane
URL, token type, and expiry.

The global bearer for platform-operator routes is
`putnami operator token --global`, from the `@putnami/operator` extension.
`putnami cloud token --global` fails with a usage error that names that
command.

`putnami cloud token create|list|revoke` manages **workspace-owned** `pkt_*`
machine tokens for CI and agents, backed by the auth server's `/apikeys`
surface. All three subcommands resolve the linked workspace and a
workspace-scoped session token (carrying `apikeys:write` from login), so the
server gates on workspace membership; a non-member or missing-scope caller
surfaces the server's 403 cleanly.

- `putnami cloud token create --name <n> --scopes <s,...> [--expires <dur>]`
  creates a token owned by the linked workspace (`owner_kind=workspace`,
  `owner_principal_id` = the workspace id, which the server enforces again).
  Scopes are accepted comma- or space-separated and sent as the canonical
  space-joined `allowed_scopes` string; `--expires` accepts a duration (`30d`,
  `12h`, `2w`) or an explicit RFC 3339 timestamp. The raw `pkt_*` is printed
  **exactly once** with a "store it now, it won't be shown again" warning and
  paste-ready hints (a GitHub Actions `gh secret set PUTNAMI_CLOUD_TOKEN` line,
  plus `PUTNAMI_CACHE_TOKEN` and `PUTNAMI_E2E_TOKEN` exports). The CLI never
  persists the secret. Use resource-owned machine scopes such as `cache.read`,
  `cache.write`, `deploy.read`, `deploy.write`, and `delivery.ingest`; human
  management scopes like `apikeys:write` belong to the signed-in session that
  calls `/apikeys`, not to the workspace machine token.
- `putnami cloud token list` prints the workspace's active tokens (name, scopes,
  created, expires, last-used, id). It never prints the secret and hides
  already-revoked tokens.
- `putnami cloud token revoke <id|name>` revokes a token; a name is resolved to
  its id through the list (an ambiguous name is a usage error: revoke by id).

Give each token the fewest scopes it needs. To rotate a token, create the new
one, switch its consumers to it, verify them, then revoke the old one.

`putnami cloud packages` administers the linked workspace's namespace
bindings and release-history grants. It never accepts an owner override:
`.putnami/cloud-link.json` selects the owner, and the serving domain verifies
the signed-in user's authority.

- `packages status [<protocol>]` checks each npm, go, oci and put namespace
  binding the checkout needs. A missing one reads `degraded`, with the
  `namespaces activate` command that records it as its fix. Without an oci
  binding, oci reads `ok` when the workspace holds at least one active OCI
  publisher share from the workspace that owns the namespace.
- `packages namespaces activate --protocol <gomod|npm|put|oci> --namespace
  <exact> --idempotency-key <key>` records a namespace binding through
  Distribution's control-plane API; `namespaces list` reads active bindings.
- `packages grants create --grantee-workspace-id <workspace UUID>
  [--channel <channel>] --idempotency-key <key>` grants read access to the
  owner's release history, optionally limited to one channel. The native Put
  endpoint validates the owner's current grant-admin role; the credential
  carries the `put` protocol marker, which grants no namespace authority itself.
- `packages grants list [--include-revoked]` and `packages grants
  revoke <grant-id>` use the same owner-scoped native Put endpoint. Package,
  protocol, action, principal, scope, audience, TTL and owner overrides are
  refused before credentials or requests.

`putnami cloud login`, `putnami cloud setup`, and `putnami cloud registries
setup` record registry endpoints and host-only token recipes such as
`putnami cloud token --for npm`. Automatic install and setup preserve existing
native credential files. Explicit login or registries setup removes legacy
static npm, Go and OCI entries for the configured hosts; unrelated entries and
existing Put credentials remain intact.

Language dependency installation reads registry hosts from
`putnami.workspace.json` (`registries.npm.scopes` and `registries.go.origin`).
An existing `.npmrc` is not a replacement for that declaration: the language
extension requests fresh native credentials only for explicitly declared hosts.

`putnami cloud token --for put` returns the native Distribution bearer for the
linked human workspace. Archive publication uses that same IAM-backed Put
authority. An explicitly configured machine broker credential for the selected
Put host is exclusive: its exchange failure is terminal and never falls through
to a human session. The CLI derives `putnami/cloud` from the scoped extension
identity `@putnami/cloud`, or the authored binary-name override; it does not
accept owner or direct channel authority. A generic `putnami publish --all
--channel stable,canary` passes its channels through the strict coordinator
plan, and the archive step verifies that plan before writing immutable bytes.

The CLI entry point forwards structured artifact events from publication commands
to the host, so successful uploads provide the verified member records required
for release-set finalization.

Archive publication writes a version without a channel. The release-set v2
coordinator consumes the verified member fact, stores the snapshot under the
separate release namespace (declared in `putnami.ci.json`), and performs the
channel compare-and-swap. The native package coordinate and release namespace
are different identities. Publishing from a CI principal therefore still
requires that principal to have real IAM authority over the existing Put
package; the project name, release namespace, and source workspace grant none.

In a hosted CI run (`CI_RUN_ID` set), a successful `release-set release` then
hands Control the Docker images the run published, so Control can deploy them.
The handoff fails the release step when that image evidence is missing,
ambiguous, or rejected. It is skipped only when the workspace-root
`putnami.ci.json` parses and declares no deploy workloads: no environment lists
a `workloads` selection rule. For example, an environment with only a
`channel` deploys nothing. A missing or invalid `putnami.ci.json` does not skip
the handoff, so the evidence checks still apply.

A workspace that declares `registries.oci.publish` as `oci.putnami.dev/putnami`
keeps the `putnami/…` native coordinates in both the image probe and the
publisher, while Cloud owns its separate release-set heads.

`putnami cloud packages copy <src-ref> <dst-ref>`, `putnami cloud packages retag
<ref> <tag...>`, and `putnami cloud packages revert <repo>:<tag>` copy, retag,
and roll back images on `oci.putnami.dev` **server-side**. Refs are
registry-relative (`ns/name`, `ns/name:tag`, or `ns/name@sha256:...`); the copy
destination is `<repo>[:tag]` (the manifest keeps its digest). Both verbs probe
`GET /v2/_putnami/capabilities` once and use the server-side fast path
(`/v2/_putnami/copy` and `/v2/_putnami/tag-digest`) when advertised. That is a
pure metadata operation within the shared content-addressed store, so no blob
bytes move and a multi-arch image index is copied whole (every child platform
manifest comes along). Against a registry that does not advertise the
capability they fall back to the standard distribution API (cross-repository
blob mount and manifest PUT; a multi-arch index is refused there). Credentials
flow through the same registry token seam as `cloud token --for oci`, never
provider credentials.

`putnami cloud deploy` has two subcommands. `deploy status <release-id>`
reads one release. `deploy publish-v2 --request-file <path>` is the operator
continuous-deploy request: it reads a closed, bounded document containing one
immutable Source revision, the current accepted-definition revision for
Control's compare-and-swap, one immutable release-set id and digest pair, and
exact OCI image plus Put config selectors (and ordered Put migration selectors)
for every workload it names. The CLI first asks Control to accept
`putnami.ci.json` at that Source revision, then submits only the returned
definition revision and the supplied immutable Distribution selectors under
`publish_v2`. It never resolves a channel head or rebuilds selectors from local
manifests. A request that names an older release set and one workload rolls
that workload back. For example:

```json
{
  "protocolVersion": 1,
  "sourceRevision": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "expectedEnvironmentDefinitionRevision": 0,
  "releaseSetRef": {
    "id": "rs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  },
  "projects": [{
    "name": "apps/service",
    "image": {
      "ecosystem": "oci",
      "coordinate": "acme/apps-service",
      "version": "1.2.3",
      "artifact_digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    },
    "config": {
      "ecosystem": "put",
      "coordinate": "config/apps/service",
      "version": "1.2.3",
      "artifact_digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    }
  }]
}
```

The owner APIs revalidate the accepted definition, release set, and member
membership before deployment. Variants and managed-data deployment remain
unsupported and fail closed.

`--wait` polls `GET /deploy/<release_id>` until the release settles or
`--timeout` fires (default 5 m), and reports the durable service-worker
backend, the deploy worker's service and revision, and the attempt number as
they change. The terminal human and JSONL result names per-workload candidate
and serving traffic outcomes and includes separately named control-plane
timing observations.

The CLI sends no classic release body: there is no `--preview`, `--force`,
`--image-version` or `--rebind-datasource`, and no `env sync`.

`putnami cloud config <app>` fetches the published schema and resolved
non-secret config, then prints every expected non-secret key with `set`,
`default`, or `missing` status. This works immediately after
`putnami publish <app>` even when no config values or secrets have been
written yet. `--schema` prints the published schema as a table by default;
`--format json` returns the raw schema JSON and `--format yaml` returns YAML.
`--key <key>` prints one resolved non-secret value, and `--secret-keys` lists
schema-declared secret keys with `set` or `missing` status plus setup commands
for missing secrets. Secret-key status is served by `/api/secrets/status` so
the CLI does not need to infer field status from path-only metadata.
`--reveal-secrets` asks `/api/configs/resolve` to merge plaintext secrets and
requires an interactive confirmation or `--yes`. The legacy `list`, `show`, and
`resolve` subcommands remain for compatibility; `--with-secrets` still uses the
server's redacted merge mode. `putnami cloud config put <app> --config-from
<yaml>` is the explicit override path for forcing non-secret values from a YAML
file; normal `publish` discovers committed env config automatically.

`putnami cloud secrets` calls config-api's `/api/secrets` routes through the
control-plane host. The CLI reads the configured workspace from the root
manifest and mints a short-lived workspace-scoped access token from the global
login session before calling the API. `list` without a project returns a
workspace-wide metadata inventory (project, environment, path, UpdatedAt) and
never decrypts values. Project commands take `<app> <key>`; keys are dotted
(`database.password`) and the CLI splits on the last dot, with
`--block`/`--field` available as the explicit escape hatch. Writes are
schema-gated server-side: if the app's schema has not been published yet, the
CLI surfaces a clear "run `putnami publish` first" hint. `reveal` is the only
new plaintext verb; it requires an interactive confirmation or `--yes` in
non-interactive scripts. `get <key> --reveal` remains as a compatibility path.
`delete` is idempotent (404 returns successfully). Discovery still supports the
current-directory project shortcut for compatibility; pass `<app>` positionally
and `--env` to target an environment other than the configured default
(`prod`).

## Build cache

`putnami cloud setup` provisions the remote build cache by writing
`.putnami/cache.json` at the workspace root. A stock CLI picks this file up on
the next build: it negotiates against the cache, restores hits, and uploads
misses using a per-user bearer. The file is safe to commit. It never contains a
token, only a token *source*:

```json
{
  "enabled": true,
  "url": "https://cache.putnami.cloud",
  "mode": "full",
  "token": { "command": ["putnami", "cloud", "token", "--for", "cache"] }
}
```

- `enabled` (default `true`) plus a non-empty `url` makes the cache active.
- `mode` is one of `minimal`, `toplevel`, or `full` (default `full`). Override at
  setup time with `--cache-mode`; override the endpoint with `--cache-url`.
- `token` is a non-secret *source*, never a bearer:
  - `{ "command": [argv...] }`: the CLI runs the argv and uses trimmed stdout as
    the bearer.
  - `{ "url": "https://..." }`: the CLI fetches the trimmed response body.

**No shared secrets.** Setup records `putnami cloud token --for cache` as the
source. That command mints an ephemeral key restricted to `cache.read` and
`cache.write`, exchanges it once for a short-lived `cache` audience JWT,
and immediately revokes the backing key. It prints only the bare bearer on
stdout. The key also has a bounded expiry if best-effort cleanup fails, and no
long-lived secret is written to disk or committed. The managed cache server
isolates entries by the JWT's `scope_ref.workspace_id` and enforces both cache
scopes on this API-key-derived machine identity.

**CI.** Set `PUTNAMI_CACHE_TOKEN` to a pre-minted bearer to bypass the token
source entirely (useful where interactive login isn't available).
`PUTNAMI_CACHE_URL` and `PUTNAMI_CACHE_MODE` override the persisted `url` and
`mode` for a single run.

**Successful run markers.** The managed cache also stores a separate CI baseline
marker for successful full-target runs. Fresh `main` or `master` CI checkouts
can ask the cache for the last successful SHA for the authenticated cache
namespace, opaque workspace identity, branch, normalized command list, optional
params hash, and `selection:"all"`. This marker is separate from Action Cache
entries: per-job hits never prove that the whole requested target passed at one
commit.

- `POST /v1/cache/run-marker/lookup` accepts
  `{ protocolVersion, workspace, branch, commands, paramsHash?, selection }` and
  returns `{ protocolVersion, marker? }`, where `marker.sha` is the successful
  HEAD SHA and `marker.updatedAt` is the server timestamp.
- `POST /v1/cache/run-marker/publish` accepts the same key fields plus `sha` and
  optional `observedSha`. The server publishes only when the marker is missing or
  the current marker still matches `observedSha`, so late parallel CI runs cannot
  overwrite a newer success. A benign race returns `published:false`; lookup or
  publish failures are best-effort and clients fall back to all projects.

**Inspect and disable.** `putnami cloud cache` (alias `cloud cache status`)
checks the persisted config and the active identity, and shows how many tasks
the last CI run took from the cache, read from that run's builds report.
`putnami cloud cache disable` sets `enabled:false` (keeping the file so it can
be re-enabled by re-running `putnami cloud setup`); add `--remove` to delete
`.putnami/cache.json` outright.

### Cache provider

`@putnami/cloud` ships the remote cache as an out-of-process **cache provider**:
the build scheduler (core) launches the extension binary's `cache-provider` command once per build
run and drives it over the provider RPC defined in
[`go.putnami.dev/protocol/cache`](../../protocols/cache/provider.go)
(`provider.go`). The RPC is newline-delimited JSON request and response over
the subprocess's stdin and stdout, with large blobs moving through a
content-addressed **blob-exchange directory**
(`InitializeParams.BlobExchangeDir`) rather than the pipe. The v3 manifest
contributes the provider through its explicit `cloud-cache-provider` task
identity; command-name and extension-version probes do not select it. The
`initialize` handshake's protocol version remains runtime wire validation. The
cache server and this provider import the same `protocol/cache` types, so the
wire contract cannot drift.

When the provider-RPC v2 handshake is selected, the cache server derives entry
provenance from the verified bearer at commit time: admitted CI workload OIDC
identities produce `trusted` entries, while developer and workspace tokens
produce `hint` entries. The provider never trusts producer or channel values
asserted by core. Old provider sessions use the v1 response shape unchanged;
channel-less legacy entries remain available to the framework's local fallback
policy.

The provider is the client half of the cache; the cache **server** is
cache-server. They are complements, not copies. Token resolution, gzip
compression, and presence each have one source: resolution in
`remotecache.TokenSource`, compression in
[`internal/deliverycli/internal/remotecache/compress.go`](internal/deliverycli/internal/remotecache/compress.go),
and presence in core. All of them use the shared `protocol/cache` vocabulary.

**Responsibility split**: what core owns and what the provider owns:

| Responsibility | Owner | Notes |
| --- | --- | --- |
| **Presence** (`.putnami/cache-present.json`) | **Core** | Core owns its CAS, so it passes the digests it already has in `InitializeParams.KnownDigests`. The provider only *consumes* that set: it skips re-staging those blobs on restore, and keeps no presence file. The provider-owned-presence path via `SummaryResult.KnownDigests` (for very large warm sets) is a deferred optimization; the seam exists, unused. |
| **Eligibility and break-even** (`EligibleForRemote`) | **Core** | Core decides which keys are worth caching; it holds the per-job duration and size metadata. The provider's restore, prefetch and upload params carry only keys, so it serves exactly what core asks and never re-filters. |
| **Capabilities discovery** | **Provider**, per run | The provider's client probes the cache server's capabilities (find-missing, commit-batch, direct-CAS-PUT, and others) on the batch write path and memoizes per session: one client per run, not per job. |
| **Materialization mode** (`minimal`/`toplevel`/`full`) | **Core** drives, provider applies | Core sends the build's intent in `InitializeParams.Mode`; the provider passes it to the server on every negotiate and materializes exactly the blobs the server returns for that mode (minus `KnownDigests`). |
| **gzip by uncompressed digest** | **Provider**, internal | The client transparently gzips on upload and decompresses on download; the CAS digest always addresses the *uncompressed* content, so core only ever sees uncompressed, content-addressed blobs in the exchange directory. |

CAS here means content-addressed storage.

The provider reads the same `.putnami/cache.json` (written by `cloud setup`) and
the same `PUTNAMI_CACHE_URL`, `PUTNAMI_CACHE_MODE` and `PUTNAMI_CACHE_TOKEN`
environment as a stock CLI's in-core cache, so CI configures it identically.

**Run credential.** On a hosted run core lists the `run-credential`
capability in `initialize`. The provider echoes it only when all three hold:

1. A remote cache is configured.
2. `PUTNAMI_CLOUD_INGEST_URL` is an https Delivery ingest base, or http to a
   loopback host, with no user information, query or fragment.
3. The process denied inspection of itself (`procguard.DenyInspection`).

After the echo, core sends the run credential in one `authenticate` op. The
provider posts `{}` to `${PUTNAMI_CLOUD_INGEST_URL}/capabilities/cache` with the
credential as its bearer, and drops the credential:

| Delivery answers | The provider |
| --- | --- |
| 201 `{protocolVersion, token, expiresAt}` | Uses `token` as the session's only cache bearer. It is classed `run` and never re-minted. |
| 204 | Keeps the configured token source (`PUTNAMI_CACHE_TOKEN`, the token command or metadata). |
| 404 (a Delivery without the route) | Keeps the configured token source and logs one stderr line. |
| 5xx, 408, 429, network error | Retries once after 0.5 s. If Delivery still does not answer, keeps the configured token source and logs one stderr line. |
| 201 with a token that does not follow the contract | Keeps the configured token source and logs one stderr line. |
| Other 4xx, such as `run_terminal` or `run_credential_invalid` | Refuses `authenticate` with Delivery's `code`, else `cache_refused`. Core then builds without the remote cache. |

Without the echo, core sends nothing and the configured source serves the
session as before. The object-cache socket is negotiated in `initialize`,
before `authenticate`, so its capability probe uses the configured source.

### Credential provider

The engine asks a **credential provider** for registry credentials when an
invocation enables it with `--providers install` (or `PUTNAMI_PROVIDERS`).
`@putnami/cloud` declares `commands["credential-provider"]` and its
`cloud-credential-provider` task, which runs `{extensionRuntime}
credential-provider`. The engine starts it once per process and drives it over
the JSONL RPC of
[`go.putnami.dev/protocol/registry`](../../protocols/registry/credential.go)
(`credential.go`): one request per stdin line, one answer per stdout line,
diagnostics on stderr.

| Op | Answer |
| --- | --- |
| `initialize` | `credential-v1`, with or without `runCredential`. An invalid run credential is refused with `invalid_run_credential`, and a run credential the process cannot protect with `inspection_guard_unavailable`. |
| `credential`, purpose `read`, hosted run | `POST ${PUTNAMI_CLOUD_INGEST_URL}/capabilities/install` with body `{}` and the run credential as bearer. 201: the install credential `{bearer, expiresAt, hosts}`. 204: absence. Every other 4xx, 404 included: a refusal with Delivery's `code`, whatever it is (for example `install_capability_denied` or `install_capability_conflict`), else `install_refused`, never retried. 5xx, 408, 429 or network error: one retry after 0.5 s, then `install_capability_unavailable`. No usable ingest base: `install_capability_unconfigured`. |
| `credential`, purpose `read`, developer machine | One bearer for the signed-in user of a linked checkout: client `distribution`, scope `go npm oci put`, for the sorted hosts of the four registry endpoints. It is cached in `~/.putnami/registries.json` under a key no host can take, until shortly before it expires. Not signed in, not linked, or any failure: absence, with one redacted stderr line for a failure. |
| `credential`, purpose `publish` | Absence. Publishing keeps `putnami cloud token`. |
| `shutdown` | Acknowledged; the process exits. It also exits on stdin EOF. |

The provider denies inspection of itself before it reads its first line, and
keeps the run credential in memory only: never in its environment, a file, an
argument, a child process or a log. Every answer comes within the engine's 30 s
op timeout: two Delivery attempts of at most 12 s each, 24.5 s with the wait
between them, and 25 s for the developer-machine mint. When the mint runs out
of time, the provider answers absence and tells the mint to stop: a pending key
creation or exchange ends, and the mint stores nothing. Two steps finish
anyway. A sign-in refresh in progress completes and is saved, because the auth
server has already rotated the refresh token. A key that was created is
revoked, with its own 5 s limit. A later read waits for that mint to return,
so two mints never run at once, and shutdown waits for it too.
Concurrent reads share one Delivery call. Stderr lines
are at most 512 bytes, and the run credential and anything shaped like a token
are replaced with `<redacted>`.

delivery-api serves both capability routes under the ingest base. The provider
calls them through delivery-api's generated client.

## Configuration

Defaults can be overridden with flags or environment variables:

| Setting | Flag | Environment | Default |
| --- | --- | --- | --- |
| Auth URL | `--auth-url` | `PUTNAMI_AUTH_URL`, `PUTNAMI_AUTH_ISSUER` | `https://auth.putnami.cloud` |
| Control-plane URL | `--control-plane-url` | `PUTNAMI_CLOUD_API_URL` | `https://api.putnami.cloud` |
| OAuth client ID | `--client-id` | - | `putnami-cli` |
| Build cache URL | `--cache-url` | `PUTNAMI_CACHE_URL` | `https://cache.putnami.cloud` |
| Build cache mode | `--cache-mode` | `PUTNAMI_CACHE_MODE` | `full` |

All commands avoid direct Google Cloud calls and use the control-plane API for
cloud workspace and status data.

Platform-operator commands are outside this extension. `@putnami/cloud` owns
workspace-facing report submission and portable `audit-attest`.

The Cloud SQL Auth Proxy wrapper is `putnami operator sql-proxy`. It needs
Google Cloud IAM credentials, so it is an emergency operator path;
`putnami cloud db connect` is the everyday database bridge.

## Service clients

The extension calls Putnami Cloud services through 14 generated Go clients.
Each one lives under `../clients/<service>/go` and is its own Putnami project,
`go.putnami.dev/cloud/clients/<service>`. `./putnamiw clientgen`
generates it from `../clients/<service>/schema/openapi.json`. Never edit
`client.gen.go` by hand; regenerate it.

The services are auth-server, cache-server, config-api, control-api, data-api,
db-gateway, delivery-api, distribution-api, identity-api, observability-api,
oci-server, put-server, runtime-api and source-api.

The schemas come from the Put package `cloud/doc-contents-cloud-cli-schema`,
channel `canary`, version `0.0.0-20261010124957-95ec3b1f6`. Its file layout is
`cloud-cli-schema/services/<service>/openapi.json`. Each file is copied byte
for byte, except four `info.description` strings that lose their issue
numbers; [`../clients/README.md`](../clients/README.md) lists them. To update
the schemas:

1. Fetch a newer version of `cloud/doc-contents-cloud-cli-schema`.
2. Copy each `services/<service>/openapi.json` over
   `../clients/<service>/schema/openapi.json`.
3. Run `./putnamiw clientgen --projects <the client projects>`, for example
   `--projects go.putnami.dev/cloud/clients/put-server`.
4. Adapt the call sites to the regenerated clients.

A call with no declared contract stays hand-written. Two inventories list
every such call site, and `@putnami/clientgen:validate` fails on a call site
that neither claims:

- [`clientgen.framework.json`](clientgen.framework.json) lists the Putnami
  calls with no contract yet: the session and log chunk ingest of the CI
  reporters, and the OCI registry fast path. They wait on contract
  declarations in delivery-api and oci-server. It also lists the transport
  primitives of the remote cache client.
- [`clientgen.external.json`](clientgen.external.json) lists third-party calls:
  OAuth and OpenID Connect (OIDC), the Google Compute Engine (GCE) metadata
  server, Google Cloud Storage (GCS) signed URLs, the OCI distribution API, and
  the go.dev toolchain index.

[`../clients/README.md`](../clients/README.md) repeats the update procedure.

## Development

```bash
./putnamiw test --projects @putnami/cloud-extension
./putnamiw build --projects @putnami/cloud-extension
```

### Source layout

The published `@putnami/cloud` extension is **one manifest and one binary**.
The framework discovers exactly one `putnami.extension.json` per extension and
dispatches every subcommand through a single binary built from `cmd/putnami-cloud`. The
implementation behind that binary is split by domain into internal packages;
[`cmd/putnami-cloud`](cmd/putnami-cloud) and
[`internal/cloudcli`](internal/cloudcli) are the thin aggregator that bundles
and exposes them.

| Package | Owns |
|---------|------|
| [`internal/clicore`](internal/clicore) | Shared toolkit: IO contract, exit-coded errors, control-plane HTTP client, flag and positional parsing, workspace and app resolution, OAuth auth and token layer, OIDC endpoints, result writers, and the binding that reaches Cloud providers through their generated Go clients (it links the framework client runtime). Every domain package depends on it. |
| [`internal/deliverycli`](internal/deliverycli) | `cache`, `cache-provider` (and the remote build-cache client), the `credential-provider` RPC server and its Delivery capability calls, `ci report` (run conclusions and tee'd JSONL test, coverage and build report batches), `ci init/validate/fmt/explain`, `channels follow` |
| [`internal/sourcecli`](internal/sourcecli) | `source connect/status/disconnect` self-service GitHub onboarding |
| [`internal/configcli`](internal/configcli) | `config`, `secrets`, `config publish` |
| [`internal/distributioncli`](internal/distributioncli) | `registries`, `registry-token`, `channels set/status`, the developer-machine read credential of `credential-provider`, `packages`, `db migrations publish`, and the site-content packaging and publishing that the operator CLI's `publish-doc` calls |
| [`internal/datacli`](internal/datacli) | `db` (`status`, `list`, `info`, `grant`, `connect`) and the Data side of migration publication |
| [`internal/runtimecli`](internal/runtimecli) | `env`, `deploy` |
| [`internal/identitycli`](internal/identitycli) | `login` (OAuth device flow), `whoami` |
| [`internal/observabilitycli`](internal/observabilitycli) | `logs` (query and `--follow` SSE live tail), `traces` (`--slow`), `metrics` (`--window`), all with `--output=jsonl` as the agent surface |
| [`internal/cloudcli`](internal/cloudcli) | The aggregator: `RunMain`/`RunCommand` dispatch, the `putnami.extension.json` manifest and the extension binary, and the cross-cutting commands that orchestrate several domains (`login`/`logout`/`setup`/`install`/`status`/`token`) plus workspace-link and CLI-runtime plumbing. |

SSE means server-sent events.

Each domain owns the CLI commands for its bounded context; `RunCommand`
dispatches each verb to its domain handler (for example `deliverycli.Cache`,
`runtimecli.Deploy`). Commands that span domains stay in the aggregator as thin
orchestration. For example, `login` runs `identitycli.Login` for the auth
mechanism, then `distributioncli.WriteRegistryTokenRecipes` to provision
per-registry credentials. A domain package never imports another domain
(`identitycli` knows nothing of registries), keeping auth a generic subdomain.

All packages live in one Go module, `go.putnami.dev/cloud/extension`. The
workspace `go.work` resolves it for normal builds. The module is also compiled
under `GOWORK=off` for its packaged build and by `bin/prepare-runtime`, which
prepares the runtime in a source checkout. Its `go.mod` therefore carries a
filesystem `replace` for each framework module, protocol module and generated
client it uses. Filesystem replaces need no `go.sum` entry.

**Adding or moving a command:**

1. Implement the handler in the owning `internal/<domain>cli` package, with
   the signature `func(params map[string]any, args []string, workspaceRoot
   string, env map[string]string, ioctx clicore.IO) error`. Call `clicore.*`
   for shared concerns.
2. Add its declaration to `putnami.extension.json`. The manifest is the single
   source of truth, and the parser and manifest **drift test** guards that flag
   types stay in sync.
3. Register any value-less flags with `clicore.RegisterBooleanFlags`, in the
   aggregator's init or in the domain package's `flags.go`.
4. Wire the `RunCommand` switch case to the domain handler.
