# Changelog — typescript

## 0.4.0 — 2026-11-04

### Breaking

- feat(typescript)!: the test command type-checks every project file (00a0b0b)
- feat(extension)!: providers replace the extension tool contract (74741e9)
- feat(go)!: the race detector runs on the packages a change touches (51620dc)
- feat!: compose requests each member as a serve run and moves to @putnami/dev with qualify (1655830)
- feat!: an extension manifest names its extension (fe4e1d3)
- feat!: extensions answer toolchain pins and the core installs them (6d40476)
- feat(typescript)!: delete the lint --config-path flag (15c6ac6)
- feat!: commit page and gRPC service inventories (050c162)
- fix(cache)!: a clientgen key reads no file a build of the run writes (bb6dece)
- feat(cli): reporters receive their credential over the protocol under credential custody (6be6aba)

### Features

- feat(web): sitemap, translated paths, locale domains and the language suggestion (fc4b3d6)
- feat(web): agent translation and human validation of catalogs (0a9fed9)
- feat(web): per-namespace catalogs, inline tags and a footer switcher on putnami.dev (ca965fa)
- feat(web): locale routes, typed messages and catalogs, proven on putnami.dev (88d5ce1)
- feat(evidence-readers): read JUnit, LCOV, Cobertura, Clover and Go cover reports (373c552)
- feat(sdd): libraries and generated clients answer feature coverage without a copied reason (6caa78b)
- feat(protocols): past the per-task bound, keep the test cases that prove a check and the slowest ones, and report the dropped time (93b9187)
- feat(application): five TypeScript features state what authentication, caching, HTTP, gRPC and telemetry do (8225583)
- feat(protocols): the last protocols own the features of their contracts (95ef1b4)
- feat(protocols): nine protocol slices own the features of their contracts (b1bfb06)
- feat(protocols): the qualify and template protocols own the features of their contracts (a60fe8d)
- feat(samples): the two client-matrix samples enforce a spec of their own (3a400e1)
- feat(protocols): the runtime protocol owns the feature of its event stream (c507b77)
- feat(protocols): the features protocol owns the feature of its contract (8f3edc5)
- feat(protocols): the operational protocols own the features of their contracts (d6954bf)
- feat(client): the TypeScript client generator writes a putnami.json when the client has none (a24c05e)
- feat(templates): each TypeScript template declares its own feature (088ae7a)
- feat(sdd): every project carries a feature, a spec or a featureAuthority (64d15b8)
- feat(cli): the result envelope carries workspace load warnings (4fbd500)
- feat(typescript): the TypeScript extension answers toolchain pins for bun (1e18c6b)
- feat(typescript): workspace install creates the root package.json it lists the members in (f15f674)
- feat(protocols): the workspace adapter declares its install inputs and its language (fed2589)
- feat(extension): a publish task declares the publish channels it serves (5f41e90)
- feat(extension): contract 11 declares command kinds, workloads and effects (d41461e)
- feat(dev): @putnami/dev declares the development verbs (b2b22a5)
- feat(application): drainMetrics merges every telemetry bucket for a test (408f582)
- feat(cli): an extension declares a verb vocabulary (2739e9f)
- feat(cli): a publication reuses a member another channel already published with the same fingerprint (1340535)
- feat(infra): a secret requirement is named by its config key (1a9071f)
- feat(cli): an extension installs and loads its extension dependencies (d465c1b)
- feat(protocol/infra): the infra protocol accepts a config key as a secret name (67c330c)
- feat(cli): a recorded session names the failed check's code, the declarations and the launching agent (d1baec7)
- feat(cli): sessions report the spec gate's record, and protocol/ci defines the run verdict (224d7e5)
- feat(typescript): validate holds the breaking marker for stable TypeScript packages (8d66fd7)
- feat(intelligence): add public agent-readiness extension (7b407e5)
- feat(cli): stream plan.json as a session reporting artifact (c8c9e8f)
- feat(database): the test provider provisions only the datasources a suite asks for (4da6833)
- feat(protocol): a workload's deployment declaration as a release-set member (6cdaf53)
- feat(cli): publication-v1 uploads put members in-process (2e1c87d)
- feat(cli): publish through provider operations, with every upload in-process (00650c0)

### Fixes

- fix(typescript): build-generate removes a capability manifest it did not produce (d38fcab)
- fix(typescript): putnami test passes --rerun-each to bun test (6552091)
- fix(typescript): generate tasks show the log events of their hooks (4824f3f)
- fix(events): stop records each delivery it abandons as dropped (6d3b2e4)
- fix(typescript): a compile build writes no temporary file into the project source tree (a489ca9)
- fix: a file a test removes no longer fails the source binding of its project (aa9a2be)
- fix(typescript): 13 TypeScript libraries declare their project type (44352dc)
- fix(web): a fragment link keeps its scroll, and a fragment-only history step keeps the loaded data (22120a6)
- fix(github-collaboration): close tasks with empty labels (8c93b0f)
- fix(schema): preserve nested fields through metadata helpers (9878ad8)
- fix(cli): keep recursive activation within project boundaries (d5d0841)
- fix(go): prepare completes go.mod replaces; the CLI writes no go.mod (6218f1b)
- fix(typescript): a fresh workspace installs the Biome its rules target (42e0f95)
- fix(typescript): delete the version job command that putnami version never reaches (cd990d6)
- fix(web): client navigation follows links, scroll and redirects as a page load does (9437850)
- fix(web): a layout under a dynamic segment loads its data on client navigation (ef59258)
- fix: a read-only machine token runs without refusal warnings (1a28e26)
- fix(cli): two writers of one resource never run at once when a plan drops a job (6a5ccd0)
- fix(typescript): a cached generate never replays evidence of an earlier tree (cfc02e4)
- fix(typescript): a managed npm publish outside a release set retries a transient registry failure (4977804)
- fix(features): a feature, evidence or report diagnostic names the file position of its entry (d23cd4c)
- fix(cli): the declared-edge check judges a project whose provider found no edge (6783063)
- fix(web): a page loader's unexpected error is logged and recorded as the request error (cbf3ba4)
- fix(application): an expired OAuth refresh token logs at WARN, not ERROR (b9f3513)
- fix(cloud): a session with no signed-in user is not told to sign in when a token from its environment serves it (e8b01d8)
- fix(cli): a go test timeout is not recorded in the failure-replay cache (6c9d4d5)
- fix(cloud): secrets set refuses a requirement name and names the config field (f043b2b)
- fix(cli): qualify --target local reads the workload's local config (6de45d2)
- fix(cli): serve ignores Git-ignored changes, and --watch=false stops its restarts (3b1b0a9)
- fix(web): a client navigation keeps its query string on the loader endpoint (db600db)
- fix(web): client navigation follows the redirect a loader returns (7c681f6)
- fix(web): an action that redirects evicts its cache patterns (c31c038)
- fix(web): a client <Form> follows the redirect its action returns (f256526)
- fix(application): the OAuth plugin declares its session and oauth config blocks (31be061)
- fix(application): config-extract writes no default for array, map and object fields (6db9ecd)
- fix(cli): a task that executes starts from an empty declared output directory (5863886)
- fix(typescript): generate fails when a declared dependency is not installed (a8a182e)
- fix(cloud): cache the two pure-read Cloud checks and delete the two dead help tasks (1cfe71d)
- fix(cache): a task with no effect to replay is keyed and shared whatever its duration (ecc4d5a)
- fix(test): a test process does not see the job variables of a hosted run (232c85c)
- fix(cli): a local workload without platform routes qualifies once its application has started (3785578)
- fix(cli): run --args keeps a value that begins with a hyphen (5b4a32a)
- fix(cli): tree verify accepts a CI policy with --fix=false (a664ab9)
- fix(events): open the push handler's DI scope from the context built after warmup (59308c5)
- fix(typescript): upgrade --deps reads large packuments and reports skipped packages (b9bd227)
- fix(cli): publish --dry-run reports a version that already exists on a registry (0b3b7bf)

### Performance

- perf(cli): build and test share one generate and one describe per Go project (9aecda0)
- perf(cli): key installed-extension tasks on the extension's implementation digest (745907a)

### Other

- chore(release): prepare the 0.4.0 release (1217a7a)
- test(web): the browser navigation hooks outlast their browser spawn (277fc21)
- style(samples): format the featureAuthority line of 13-fullstack-app (8f49f98)
- test(client): the raw WebSocket tests wait for the provider's close (1eb8f3e)
- chore: the web compiled-probe test and the packaging bench compile outside the source tree (d051966)
- test(typescript): the workspace-fetch descriptor test does not time out on a loaded host (1177060)
- chore: each expected check lives with the project whose tests prove it (1e49e0b)
- docs: protocol docs match how extensions load and templates test runs (9539e0b)
- docs: describe the publication uploaders (c20f816)
- build: extension projects declare the archives publish channel (cf5b5f7)
- test(client): the WebSocket transport tests wait for the provider instead of fixed sleeps (dff3e70)
- test(web): the router navigation fixture reads each step's scrolls after React renders its page (5a7514b)
- refactor(templates): no template variable names the Go framework version (f7080df)
- refactor(extensions): the Go, TypeScript and Python batchable tasks declare their kind and phase (cb0b943)
- docs: comments name the scheduler, planner, cachekey and publication packages (aeed0ff)
- docs: name extensions validate and templates validate|test|package outside the core (8ef89c5)
- refactor(cli): the planner leaves internal/jobs (37316ff)
- test(application): every PBKDF2 test states its hashing budget (24a762e)
- refactor: first-party extensions declare the traits of their serve, run, package and publish commands (25c7eb9)

## 0.3.0 — 2026-10-02

### Features

- feat(workspace): the first-use path runs on a machine with no prerequisite installed (b3db2dd)

### Fixes

- fix(cli): tasks run in a workspace that is not a git repository (755ae6d)
- fix(cli): init resolves extensions, templates and starter dependencies on one channel (a50a244)
- fix(web): server pages hydrate without React error 418 (0cbee44)

## 0.2.0 — 2026-10-01

First public release. See the root [CHANGELOG.md](../CHANGELOG.md).
