# Changelog — sites

## 0.5.0 — 2026-11-24

### Breaking

- feat(telemetry)!: services keep a trace budget with a per-minute floor and count each decision (e1fc98d)
- feat(telemetry)!: metrics leave every 60 s and spans in 30 s batches (53f125d)
- fix(typescript)!: HTTP telemetry records http.server.* metrics like the Go runtime (d40cedd)
- feat(clientgen)!: a generated client is built, not committed (3ad93a2)
- refactor(tooling)!: prune multi-version paths, guards and lint escapes (wave 2) (3cd6ecb)

### Features

- feat(observe): a GitLab CI/CD catalog component runs observe in one include line (7c10762)
- feat(cloud): @putnami/cloud exchanges a GitLab CI ID token, and the GitLab job needs no secret (4d4ad0a)
- feat(observe): putnami observe --history analyzes the recent past on a laptop (7f90ace)
- feat(observe): upload from Jenkins, CircleCI, Buildkite, Azure Pipelines, Bitbucket Pipelines and TeamCity (f74114c)
- feat(cli): init links the recipe that builds a Postgres API in Go or TypeScript (37b3460)
- feat(observe): read bun.lock and the declared versions of a Maven build (abd79b7)
- feat(observe): read an .xcresult bundle on a macOS runner (8a1c343)
- feat(observe): a pull request from a fork gets its upload, status and note (e663eff)
- feat(evidence): move protocol/evidence and @putnami/observe to preview (a021312)
- feat(cloud): the CI upload token is the organization ingest token, created from the CLI (4d1a0fb)
- feat(docs): type-check the code examples of the documentation (2ed6002)
- feat(typescript): putnami deps add and deps remove for TypeScript projects (d16d177)
- feat(database): read DATABASE_URL and migrate provisioned test databases (2bd11da)
- feat(typescript): serve client and bidirectional Connect streams over HTTP/2 (4b8a760)
- feat(cloud): putnami cloud telemetry set takes --parent and --for (aad8045)
- feat(telemetry): trace settings set from Cloud win, a service decides behind an outside parent, and putnami cloud telemetry (e63b24e)
- feat(cloud): putnami observe uploads from GitHub Actions with the job's ID token (9036e5c)
- feat(cloud): @putnami/cloud hosts the agency commands and the hosted memory and missions providers (a75432f)
- feat(cloud): Cloud remote config sources refresh on an opt-in period (afc56da)
- feat(telemetry): a published event carries its trace, and its consumer continues it (c44d9e0)
- feat(telemetry): services apply the export directives and 429 delays their OTLP endpoint returns (cacad72)
- feat(contributor): change screenshots captures only the pages that prove a touched requirement (a78e12e)
- feat(telemetry): logs follow the environment and the token source like metrics and traces (9097357)
- feat(telemetry): TypeScript workloads export to the collector their environment names (d59509e)
- feat(protocols): publish the screenshots manifest as protocol/screenshots v1 (f684eee)
- feat(contributor): change finalize uploads the screenshots it verifies to the proposals provider (3d849e4)
- feat(cloud): whoami and status recognise a delegated session, and a refused route names the MCP read tool (9fd85fd)
- feat(test): a Postgres suite fails instead of skipping, and runs on a native Postgres without Docker (87b4940)
- feat(contributor): screenshots are the proof of a web change (c7e3690)
- feat(cloud): the CLI works in a hosted agent session (01c8b36)
- feat(nx): a named target runs as the task the workspace maps it to (75e52eb)
- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(integrations): one import rule for Nx, Turborepo, Rush and moon (bb0f2aa)
- feat(typescript): build compiles a library with tsc when its build script does (654f519)
- feat(typescript): serve, run and package a TypeScript workload on Node (b9bdeda)
- feat(typescript): lint runs a project's ESLint configuration (b24b609)
- feat(typescript): test runs a project's Vitest or Jest suite with its JUnit and coverage reports (47fd2e3)
- feat(typescript): install with the repository's npm, pnpm or yarn and keep its lockfile (7de6c9a)
- feat(nx,turbo): a delegated workspace imports its projects one at a time (65d12fa)
- feat(putnami.dev): publish the stack and monorepo tool references (f59d1d8)
- feat(rush): putnami init --from rush turns a Rush repository into a workspace (af16977)
- feat(turbo): putnami init --from turbo turns a Turborepo repository into a workspace (acbf235)
- feat(moon): putnami init --from moon turns a moon repository into a workspace (c8e44f3)
- feat(nx): putnami init --from nx turns a Nx repository into a workspace (df34317)
- feat(bazel): putnami init --from bazel turns a Bazel repository into a workspace (ecf716b)
- feat(pants): putnami init --from pants delegates a Pants repository to Pants (2dabc44)
- feat(sdd): import a Structurizr workspace as architecture manifests (b8cb3c6)
- feat(observe): a short summary in the log, and --open signs in and opens the full report (2aff020)
- feat(observe): ArchUnit, deptrac and import-linter results per component (5e9195a)
- feat(observe): the MADR decisions that govern the files a change touches (f89e57d)
- feat(observe): the pull or merge request note links to the full report and needs an upload (428d2d9)
- feat(contributor): agent skills work on a repository with no Putnami workspace (5786c4a)
- feat(protocols): the collaboration and architecture contracts get a compatibility promise (e3c5026)
- feat(observe): a GitHub Marketplace action, Putnami/observe-action (7ab8645)
- feat(observe): a Rust repository gets its crates and Cargo.lock changes (5ef0dbc)
- feat(observe): a Ruby or Rails repository gets its gems, packs and Gemfile.lock changes (5b212ca)
- feat(observe): a .NET repository gets its projects, TRX results and NuGet changes (e0931d8)
- feat(observe): a Dart or Flutter repository gets its packages, coverage and pubspec.lock changes (b0db3bb)
- feat(observe): an iOS or Swift repository gets its packages, Xcode projects and dependency changes (f3a92c9)
- feat(observe): a Gradle or Maven repository, Android included, gets its components, JaCoCo coverage and dependency changes (46fa6ce)
- feat(observe): a moon workspace gets its projects and declared edges (a47f6ef)
- feat(observe): a Bazel repository gets its packages and their edges from a query export (a66cbd3)
- feat(observe): a Pants repository gets its components and inferred edges from a peek export (1c5bfde)
- feat(sdd): specs list and inspect show the source format and what a native rewrite adds (df7ec47)
- feat(sdd): specs init --from drafts a native spec from an external one (c785bbd)
- feat(features): read OpenSpec specs (cb0e1d8)

### Fixes

- fix(cli): init --from completes an existing .gitignore and gives the right ESLint advice (819d61b)
- fix(cli): init reports only real errors and creates an API project on request (eb21e2e)
- fix(application): answer 204 for empty results, 400 for malformed JSON and match */* routes (4515a58)
- fix(cloud): a delegated CLI session mints no registry token (fa762bc)
- fix(putnami.dev): the docs hub renders in the deployed image (fa16541)
- fix(telemetry): each exporting process names its own service.instance.id (d3844b5)
- fix(putnami.dev): the site's telemetry pushes carry a token the collector admits (3827fc3)
- fix(cli): the generated gate and the init hint name only declared verbs and flags (ebadc3a)
- fix(cli): a file the sync writes keeps the repository's layout (b328ca3)
- fix(typescript): an import writes biome.json, tsconfig.json and @types/bun only for a task that reads them (117a647)
- fix(pants): bound the sync's memory, replace the coverage list, resolve symlinked directories (3d26275)
- fix(cli,rush,turbo,moon): a task waits for the builds its tool runs before it (e909636)
- fix(turbo): a delegated task runs the repository's own turbo from node_modules/.bin (d71a23d)
- fix: specs, ADRs and docs say what the code does, and the projects set aside are declared (b3178a1)
- fix(import): a moved lint that only checked only checks (4bcac0b)
- fix(rush,bazel): a delegated lint or test runs its project alone, and every bazel call keeps one instrumentation filter (f4cfe30)
- fix(stacks): a moved TypeScript task does what its script did (b89a51d)
- fix(cli): declared_edge, --channel and the ESLint advice state what is true (0b3c10d)
- fix(sdd): the specs ratchet refuses a stale baseline (449a2fc)
- fix(features): a requirement name keeps its accented Latin letters in its ID (83d3925)

### Other

- chore(release): prepare the 0.5.0 release (63e1f17)
- docs(putnami.dev): a starting path for a team with an existing repository (08efdc7)
- refactor: move the monorepo tool integrations to integrations/ (0198add)
- docs: correct the adoption and stack-core docs after the review (53a767a)
- docs(observe): putnami.dev publishes the observe reference and every tool it reads (4f8ad96)
- docs(sites): bring your Spec Kit, OpenSpec, Kiro or Gherkin specs (7315940)
- docs(sdd): a spec may live in an external market format (2966183)

## 0.4.0 — 2026-11-04

### Breaking

- feat(typescript)!: the test command type-checks every project file (00a0b0b)
- feat(extension)!: providers replace the extension tool contract (74741e9)
- feat(contributor)!: build runs one loop from a short core, and workflow profiles replace tiers (df74424)
- feat(cli)!: Putnami serves no MCP server; agents reach it through the CLI (ffc8df3)
- feat!: compose requests each member as a serve run and moves to @putnami/dev with qualify (1655830)
- feat(cli)!: context map and context pack write no file (1cb09ea)
- feat!: extensions answer toolchain pins and the core installs them (6d40476)
- feat!: commit page and gRPC service inventories (050c162)
- feat(cli)!: init installs @putnami/contributor only on request (1fc95d0)
- feat(cli)!: delete putnami migrate agent-content (58a97c3)
- feat(tooling)!: consolidate canonical contributor workflows (76ebfd4)
- refactor(intelligence)!: remove the agent-readiness extension and keep the area detector (e600515)
- refactor(cli)!: move the channel and ci commands out of the core CLI (b7e3220)

### Features

- feat(observe): a Python repository gets its components, tests and lockfile changes (660669a)
- feat(contributor): an epic session plans issues and launches them in waves (66cb2cf)
- feat(web): sitemap, translated paths, locale domains and the language suggestion (fc4b3d6)
- feat(web): agent translation and human validation of catalogs (0a9fed9)
- feat(web): per-namespace catalogs, inline tags and a footer switcher on putnami.dev (ca965fa)
- feat(observe): licences and known vulnerabilities per changed dependency from CycloneDX or SPDX (e3a37af)
- feat(observe): publish a commit status and a summary note on the merge or pull request (71084e2)
- feat(web): locale routes, typed messages and catalogs, proven on putnami.dev (88d5ce1)
- feat(protocols): past the per-task bound, keep the test cases that prove a check and the slowest ones, and report the dropped time (93b9187)
- feat(protocols): the last protocols own the features of their contracts (95ef1b4)
- feat(protocols): the operational protocols own the features of their contracts (d6954bf)
- feat(sdd): every project carries a feature, a spec or a featureAuthority (64d15b8)
- feat(cli): the MCP instructions list the settled decisions; the block names their files (4042761)
- feat(cli): the MCP notices a context change between requests, tells the next tool result, and reloads extension guides and tools (e1644d9)
- feat(cli): the MCP serves the workspace guidance, the map and each project's context as resources (0c249fb)
- feat(cli): extensions install opts into the agent content of the extension it names (b3a46ec)
- feat(telemetry): the command vocabulary names first-party extension commands (225696e)
- feat(cli): a recorded session names the failed check's code, the declarations and the launching agent (d1baec7)
- feat(intelligence): add public agent-readiness extension (7b407e5)
- feat(database): the test provider provisions only the datasources a suite asks for (4da6833)
- feat(protocol): a workload's deployment declaration as a release-set member (6cdaf53)
- feat(cli): publication-v1 uploads put members in-process (2e1c87d)
- feat(cli): publish through provider operations, with every upload in-process (00650c0)

### Fixes

- fix(go): framework refusals carry the error code of their status (365d0a5)
- fix(protocol/telemetry): a session start without a known command keeps its record (24cc5ff)
- fix(cli): mcp install help and docs name Claude Code as the only host that reads .mcp.json (29fb57a)
- fix(cli): a source workspace registers the MCP server through ./putnamiw (a9c6978)
- fix(cli): qualify --target local reads the workload's local config (6de45d2)
- fix(python): Putnami pins and installs uv like Bun and Go (163268a)
- fix(cli): a local workload without platform routes qualifies once its application has started (3785578)
- fix(go): include embedded files in lifecycle cache inputs (7a7868c)
- fix(typescript): upgrade --deps reads large packuments and reports skipped packages (b9bd227)

### Performance

- perf(cli): the install and smoke script tests run only when their inputs change (068d8de)
- perf(tooling): the Windows compile gate runs only when a Go source changes (4145ff9)

### Other

- chore(release): prepare the 0.4.0 release (1217a7a)
- docs(observe): observe a change in GitHub Actions and GitLab CI from the canary channel (51ca7ac)
- docs: a build no longer refreshes the workspace map (c8d122c)
- docs: name the dev extension's version commands (d50bc14)
- docs: name change finalize instead of proposals finalize (f0c3337)
- docs: the AI how-to and the report protocol name change verify (d6eb878)
- docs: name change language and doctor --machine instead of the contributor commands (61f7598)
- chore(sites): publish the two sites' deployment declarations (66c9748)

## 0.3.0 — 2026-10-02

### Features

- feat(workspace): the first-use path runs on a machine with no prerequisite installed (b3db2dd)

### Fixes

- fix(site): production follows latest and the footer shows that release (f74dd63)
- fix(go): a route plugin added to an application mounts itself on the server (e3fcb4e)

### Other

- docs(workspace): the README says what Putnami brings, for whom, and what is free (ad4030c)

## 0.2.0 — 2026-10-01

First public release. See the root [CHANGELOG.md](../CHANGELOG.md).
