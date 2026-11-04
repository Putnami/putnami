# Changelog

## 0.4.0 — <release date>

Every version line moves to 0.4.0: the `putnami` CLI and its extensions, the Go,
TypeScript and Python frameworks, the protocols and the sites. The `cloud` and
`intelligence` lines publish for the first time. Each line's own changelog lists
its commits.

- `@putnami/observe` is on the stable channel and stays `experimental`. In a
  repository that keeps its own CI and has no Putnami workspace, `putnami
  observe` reports the changed components, the tests and the changed-line
  coverage, and the dependency changes with their licences and known
  vulnerabilities. It reads Nx, npm, pnpm, yarn, Go, Composer and Python
  repositories. `--publish` posts a commit status and a summary note on the pull
  or merge request. `--upload` sends the evidence to Putnami Cloud.
- `@putnami/web` localizes an application: locale routes, typed messages,
  per-namespace catalogs, translated paths, locale domains, a localized sitemap,
  and `putnami i18n` to keep the catalogs in step with the messages.
- `@putnami/jira-collaboration` is a new tasks provider over Jira Cloud,
  `preview`.
- `putnami projects owner|deps|why`, `putnami context map|pack` and
  `putnami <verb> --impacted --dry-run` answer what the MCP server answered.
  Putnami no longer serves one.
- `putnami test` type-checks every TypeScript file of a project, tests included.
  On a change, it runs `go test -race` on the Go packages the change touches.
- The contributor workflows run one `build` loop. Workflow profiles replace
  tiers, `putnami change estimate` sizes a plan, and `epic` runs the issues of
  one outcome in waves.
- Go: the framework's 401, 403, 404 and 500 answers carry the canonical error
  envelope, a caller that leaves answers 499 below ERROR, a route can declare its
  own write timeout, and `go.putnami.dev/events` publishes to Google Cloud
  Pub/Sub with the framework alone.
- `@putnami/web`: client navigation follows links, scroll and redirects as a page
  load does.
- A task's cache key no longer changes with the commit or the checkout directory.
  `build` and `test` share one generate and one describe per Go project.

### What you have to change

Run `putnami upgrade`, then `putnami install`. Upgrade the CLI and the extensions
together: a Go, TypeScript or Python extension of this release does not load in
an older CLI, and an older collaboration provider loses its binding.

#### Every workspace

- **The CLI writes lock format 5.** Only `putnami install` rewrites a lock below
  5 that pins toolchains. Run it once and commit `putnami.lock.json`.
- **Only `putnami.workspace.json` marks a workspace root.** A `package.json` with
  `workspaces` no longer does. Run `putnami init` there.
- **Extensions no longer load from `package.json` `devDependencies` or
  `node_modules`.** Declare them under `extensions` in `putnami.workspace.json`,
  then run `putnami extensions install`.
- **`build`, `test`, `lint`, `serve`, `run`, `format`, `package`, `publish`,
  `deploy`, `generate`, `validate`, their aliases, `version`, `compose` and
  `qualify` exist only where a loaded extension declares them.** Elsewhere they
  exit 2. The language extensions bring `@putnami/dev`. A hosted run refuses
  `version`, `compose` and `qualify`.
- **A flag goes after the full command.** `putnami -v projects list`,
  `putnami projects -v describe web` and `putnami help -v projects` exit 2.
- **A core command refuses a job-only flag it does not read.** For example,
  `putnami install --plan` exits 2. A bare `--` is refused for every built-in
  command.
- **`putnami init` installs less.** It writes no root `package.json`: the
  TypeScript install creates it, so a Go-only or Python-only workspace has none.
  It no longer installs `@putnami/contributor`. To add the skills and agents, run
  `putnami extensions install @putnami/contributor`.
- **Workspace map.** `build` no longer writes `.putnami/context-map/`, and
  `PUTNAMI_CONTEXT_MAP` is gone. `context map` and `context pack` print their
  result and write no file. `context map --print` and `context pack --check` are
  gone. `context pack` takes languages from the workspace providers, so a project
  with only a `setup.py` no longer reports `python`.
- **Without a `putnami.ci.json`, the generated agent gate is `lint,test,build`.**
  To keep `validate`, declare it in that file.

Commands that moved. The old spellings exit 2, with no alias.

| Old | New | Served by |
|---|---|---|
| `putnami channel set\|status` | `putnami cloud channels set\|status` | `@putnami/cloud` |
| `putnami ci init\|validate\|fmt\|explain` | `putnami cloud ci …` | `@putnami/cloud` |
| `putnami infra plan` | `putnami cloud infra plan` | `@putnami/cloud` |
| `putnami change-plan` | `putnami impact-plan <commands> --base <commit>` | core |
| `putnami version list\|use` | `putnami cli list\|use` | core |
| `putnami dev extension validate` | `putnami extensions validate` | core |
| `putnami dev template validate\|test\|package` | `putnami templates validate\|test\|package` | core |
| `putnami tree verify` | `putnami change verify` | `@putnami/contributor` |
| `putnami proposals finalize` | `putnami change finalize` | `@putnami/contributor` |
| `putnami contributor language` | `putnami change language` | `@putnami/contributor` |
| `putnami contributor machine-load` | `putnami doctor --machine` | core |
| `putnami migrate agent-content` | Removed. The refusal names each successor. | — |

#### Agents

- **`putnami mcp` and `putnami mcp install` are gone.** Agents use the CLI:
  `putnami projects owner|deps|why`, `putnami context map|pack`,
  `putnami <verb> --impacted --dry-run`.
- **An explicit `putnami install` or `upgrade` removes the `putnami` entry from
  `.mcp.json`** when it is the one Putnami wrote, and keeps a changed one with a
  warning. The CLI installer removes the user-scope Claude Code and Codex
  registration it wrote.
- **The result vocabulary drops `mcpResult` and `sessionFile.agent`,** and the
  report `origin` value `mcp` becomes `ephemeral`.
- **`@putnami/contributor`:** `build` replaces `plan`, `execute`, `fix`,
  `code-review`, `fix-loop` and the `putnami-*` skills. `design` replaces `adr`
  and `prototype`. In the contributor policy, `profiles` and `tasks.profileLabel`
  replace `tasks.tiers` and `workers`.

#### TypeScript

- **A type error in a test now fails `putnami test`.** Fix the error.
- **`putnami lint --config-path` is gone.** Place a `biome.json` in the project
  or the workspace root.
- **Commit the route inventories.** Upgrade the TypeScript extension and your
  `preBuild` hook providers together. A custom hook must report complete route
  evaluation.

#### Go

- **The race step needs a C compiler on Linux and Windows.** Install one, or pass
  `--no-race-changed`.
- **A file committed under `.gen/clientgen` or `.gen/schema` fails the build.**
  `git mv` it to `clientgen.json` or `schema/openapi.json`.
- **gRPC registrars must only register services:** the build reads the inventory
  without opening a listener.
- **`ParseLineTag` leaves `go.putnami.dev/protocol/workspace`.** Use
  `linetag.Version` of the extension SDK with the line's pattern.

#### Putnami Cloud and publishing

- **Nine `putnami cloud` aliases are gone.**

  | Old | New |
  |---|---|
  | `cloud tokens` | `cloud token create\|list\|revoke` |
  | `cloud distribution` | `cloud packages namespaces\|grants\|mirrors` |
  | `cloud oci` | `cloud packages copy\|retag\|revert` |
  | `cloud publish-archives` | `cloud packages publish` |
  | `cloud track` | `cloud channels follow` |
  | `cloud report` | `cloud ci report` |
  | `cloud validate-config` | `cloud config validate` |
  | `cloud publish-config` | `cloud config publish` |
  | `cloud publish-migration` | `cloud db migrations publish` |

- **`--skip-publish-doc` is gone.** To leave docs out of a run, do not select the
  docs project.
- **A hosted publish needs a credential provider that serves `uploader-v1`.**
  Upgrade `@putnami/cloud`.
- **A publish fails when no planned task serves a declared channel.** A publish
  task declares its channels in `declares.publishChannels`.

#### Extension and template authors

- **Providers replace the `tools` section.** An extension that declares `tools`
  moves to `providers`, which needs CLI contract 19. An extension with other
  tools loses them: the extension tool contract is gone. Update
  `@putnami/github-collaboration`, `@putnami/local-collaboration`,
  `@putnami/memory-store` and `@putnami/intelligence`.
- **A manifest names its extension.** A manifest without `name` fails
  `putnami extensions validate` and packaging.
- **The core no longer reads command names.** Declare `kind`, `workload`,
  `rewritesSources`, `consumesReleaseSet` and side effects at contract 11, or
  load `@putnami/dev`. Without them, a third-party `test`, `validate`, `serve`,
  `run`, `publish` or `deploy` loses its spec gate, watch, release-set or
  dry-run behavior.
- **Toolchains:** the Go, TypeScript and Python extensions require CLI contract
  13. A third-party extension lists its pins in `runtime.resolves` and answers
  `__putnami toolchain-resolve`.
- **Input selectors:** a manifest that lists or names one must be stamped contract
  14. Run `putnami extensions update`.
- **Install inputs:** the lockfiles of an extension that declares no
  `installInputs` no longer trigger the first-use install.
- **Go steps:** `projects create` and `deps add|remove` run the extension's
  `project-setup` and `deps-edit` jobs.
- **`@putnami/go` requires CLI contract 16.** The `from: "command"` input binding
  is gone.
- **A template that declares `workspaceDevDependencies` is refused** by
  `templates validate` and `templates test`. Delete the field.

Rollback: `putnami pin 0.3.0` in a workspace, or
`putnami upgrade --version 0.3.0` for the machine-wide CLI.

## 0.3.0 — 2026-10-02

The `putnami` CLI, its extensions, the Go and TypeScript frameworks and the
sites move to 0.3.0. The protocols and Python lines move to 0.2.1. Each line's
own changelog lists its commits.

- The documented first-use path runs on a machine that holds only Bash, `curl`,
  `tar` and a SHA-256 tool. The TypeScript extension installs its pinned Bun and
  the Go extension its pinned Go under `~/.putnami/toolchains/`, once per
  machine. Lint no longer needs Node.js.
- A workspace that Git does not manage serves, tests and builds. `version`,
  `publish`, `deploy`, `--impacted` and `--baseline` refuse there with one line
  that names git.
- `putnami init` resolves the extensions, the template and the starter's
  dependencies on one channel: `--channel`, `PUTNAMI_CHANNEL`, or the channel
  the CLI was installed from.
- Go: the health, platform and push-delivery events plugins mount their routes
  on the application's HTTP server. The go-server starter answers 200 on
  `/_/health`.
- `@putnami/web`: server pages hydrate without React error 418, and pages
  above 12,800 bytes hydrate under the default content security policy.

### What you have to change

- **Go, an application that adds its own `/_/health` route** while it keeps the
  health plugin stops at configure on a duplicate route. Remove your route, or
  remove the plugin.
- **Go, an application with route plugins and zero or several HTTP server
  plugins** stops at configure with an error that names `RegisterOn`. Call
  `RegisterOn(server)` on each route plugin before configure.
- **The Go extension no longer installs Go per workspace.** The link
  `.putnami/extensions/@putnami-go/bin/go` is gone; Go lives under
  `~/.putnami/toolchains/go/go-<version>/`. Update any script that called the
  old path.

Rollback: `putnami pin 0.2.0` in a workspace, or
`putnami upgrade --version 0.2.0` for the machine-wide CLI.

## 0.2.0 — 2026-10-01

First public release. Every version line starts at 0.2.0: the `putnami` CLI and
its language extensions, the protocols, the Go and TypeScript frameworks, and
the project templates.

- [Install the CLI](tooling/cli/doc/22-installing-the-cli.md)
- [Release contract](RELEASE.md): supported platforms, channels, and
  compatibility before 1.0
