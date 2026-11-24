# Changelog — cloud

## 0.5.0 — 2026-11-24

### Breaking

- feat(clientgen)!: a generated client is built, not committed (3ad93a2)
- refactor(tooling)!: prune multi-version paths, guards and lint escapes (wave 2) (3cd6ecb)
- refactor(tooling)!: prune dead code, dual paths, duplicates, comments and config knobs (fa16476)

### Features

- feat(contributor): a session publishes its web address with its sync (5417d8c)
- feat(cloud): the evidence upload names its CI and links its run (0164779)
- feat(observe): the pull request note says what level 3 would have held and the next step (f3e9b44)
- feat(cloud): @putnami/cloud exchanges a GitLab CI ID token, and the GitLab job needs no secret (4d4ad0a)
- feat(cloud): the evidence upload sends the test cases once Delivery accepts them (6d55e45)
- feat(evidence): the evidence names each test case that ran (8069279)
- feat(cloud): the CI upload token is the organization ingest token, created from the CLI (4d1a0fb)
- feat(cloud): putnami cloud telemetry set takes --parent and --for (aad8045)
- feat(cloud): the proposals provider answers the putnami.app change page of a pull request (7e2ac7d)
- feat(telemetry): trace settings set from Cloud win, a service decides behind an outside parent, and putnami cloud telemetry (e63b24e)
- feat(cloud): putnami observe uploads from GitHub Actions with the job's ID token (9036e5c)
- feat(cloud): @putnami/cloud ships the routine skills (putnami-routine, rebase, routine-build) (d2eec44)
- feat(cloud): @putnami/cloud hosts the agency commands and the hosted memory and missions providers (a75432f)
- feat(cloud): Cloud remote config sources refresh on an opt-in period (afc56da)
- feat(telemetry): a published event carries its trace, and its consumer continues it (c44d9e0)
- feat(web): publish @putnami/web as an extension archive so putnami i18n runs outside the monorepo (220fec3)
- feat(cloud): the cache token recipe prints a delegated session's own access token (600367a)
- feat(telemetry): TypeScript workloads export to the collector their environment names (d59509e)
- feat(contributor): change finalize uploads the screenshots it verifies to the proposals provider (3d849e4)
- feat(cloud): putnami cloud ci queue lists its entries, and pauses and resumes the queue (948a3e2)
- feat(cloud): whoami and status recognise a delegated session, and a refused route names the MCP read tool (9fd85fd)
- feat(cloud): publish pull requests through Putnami Source (210caf8)
- feat(cloud): the CLI works in a hosted agent session (01c8b36)
- feat(cloud): serve the tasks and proposals contracts through Putnami Source (d9a4829)
- feat(observe): a workspace change carries its declarations and spec verification record (85f52ce)
- feat(cloud): a site-content section may be a declared generated output (db5189b)
- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(ci): verdict policy files state what each gap and each declaration change costs, by directory and by feature maturity (638311d)
- feat(telemetry): Go services record request metrics and traces when the environment names a collector (3d93329)
- feat(cloud): putnami cloud ci queue turns the merge queue on in one command (1f519cc)
- feat(observe): a short summary in the log, and --open signs in and opens the full report (2aff020)
- feat(observe): the pull or merge request note links to the full report and needs an upload (428d2d9)
- feat(cloud): the evidence reporters send the provenance external for a CI other than GitHub or GitLab (0547b54)
- feat(protocols): one protocol module owns the code-host slugs and their vocabulary (83922ec)

### Fixes

- fix(cloud): the GitLab ID token check accepts only the audience the exchange accepts (2cfdb1a)
- fix(cloud): traces prints no root-span-only span count and reads one trace whole with --trace (7eea39f)
- fix(cloud): serialize the CLI session refresh across processes (72ab837)
- fix(cloud): a delegated CLI session mints no registry token (fa762bc)
- fix(cloud): routine-build attaches from the branch it pushes, and audit names prune-triage's middle tier (42e21bc)
- fix(cloud): the Config member accepts a map entry whose sensitive field is omitted (8ec6181)
- fix(observe): a declared change that touches no declaration carries an empty declarations list (5dd7b04)
- fix(publication): a retry of the same commit passes publish, and uploads renew their bearer (794248f)
- fix: specs, ADRs and docs say what the code does, and the projects set aside are declared (b3178a1)
- fix(cloud): an upload with a token names its repository (3ed9db1)
- fix(cli): a command output directory never holds another project's outputs (27565d9)
- fix(cloud): resolve projects by their workspace name (8b2fa6a)
- fix(cloud): the Config publisher versions its member from the job context, not from the shared stamp (7545408)
- fix(sdd): the specs ratchet refuses a stale baseline (449a2fc)
- fix(cloud): a Config member mismatch names the selected and prepared version and source revision (a541c23)

### Other

- chore(contributor): run review on the fix-heavy tier at high effort (8f237f2)
- test(cloud): env enable and env status tests run in a user home they own (71aaa1a)
- test(cloud): clear every token-discovery variable in remote source tests (cae177f)
- test(tooling): every test fixture runs git without the caller's git environment (f2075f8)
- refactor(cloud): ON MAIN tests run git without the 5 s status deadline (c85ee96)

## 0.4.0 — 2026-11-04

### Breaking

- feat(typescript)!: the test command type-checks every project file (00a0b0b)
- feat(extension)!: providers replace the extension tool contract (74741e9)
- feat(cloud)!: delete the --skip-publish-doc publish flag (9b02970)
- feat(cloud)!: delete the nine Cloud alias subcommands (431d3cf)
- fix(cache)!: a clientgen key reads no file a build of the run writes (bb6dece)

### Features

- feat(cloud): evidence-report sends the tested tree and merge base (95cb017)
- feat(cloud): upload a change evidence document from putnami observe to Putnami Cloud (932a242)
- feat(cloud): the session and log reporters gzip each chunk once the receiver advertises it (ea913b0)
- feat(cloud): a staged run releases its set onto the staging channel alone (538dc9e)
- feat(cloud): ci usage prints the merges, deploys and verdicts; ci view prints the run's verdict (61583c5)
- feat(protocols): the last protocols own the features of their contracts (95ef1b4)
- feat(protocols): nine protocol slices own the features of their contracts (b1bfb06)
- feat(extension): a publish task declares the publish channels it serves (5f41e90)
- feat(cloud): the credential provider uploads publication members to the Putnami registries (3188728)
- feat(cli): the cache provider owns the remote cache configuration (f47c38d)
- feat(cloud): ci view shows the members a run reused from its reuse channel (e9f4902)
- feat(extension): contract 11 declares command kinds, workloads and effects (d41461e)
- feat(cloud): cloud infra plan lists the workspace workloads from the job context (99c8e1c)
- feat(contributor): the contributor extension ships a compiled executable (079f47f)
- feat(cli): a publication reuses a member another channel already published with the same fingerprint (1340535)
- feat(infra): a secret requirement is named by its config key (1a9071f)
- feat(cloud): the session reporter uploads a local gate session under the developer's login (711d2a7)
- feat(cli): sessions report the spec gate's record, and protocol/ci defines the run verdict (224d7e5)
- feat(cloud): align the Cloud extension with its source at 9c1fbf2bd (451b85a)
- feat(cloud): host the Cloud CLI extension (f5c9546)
- feat(cloud): the Go runtime destination for config, secrets and workload identity (31f2675)
- feat(cloud): the TypeScript runtime destination for config, secrets and events (476a338)

### Fixes

- fix(cloud): a hosted run negotiates minimal when no cache mode is requested (51b84e6)
- fix(cloud): evidence-report uploads the licenses and vulnerabilities of each dependency change (6401c7f)
- fix(cloud): ci report folds the complete session record and batch members (66ead82)
- fix: four tests that failed under load now hold whatever the CPU speed (4ef0389)
- fix(cloud): prepare-runtime builds one binary for one runtime digest (b7125b9)
- fix: a read-only machine token runs without refusal warnings (1a28e26)
- fix(cloud): a session with no signed-in user is not told to sign in when a token from its environment serves it (e8b01d8)
- fix(cli): help shows only what a workspace's extensions provide, and empty lists emit valid JSON (f27f34c)
- fix(cloud): env status names env enable for a workload the accepted definition omits (0b6a402)
- fix(cloud): env doctor and env enable read a degraded environment-definition row (a2926cf)
- fix(cloud): secrets set refuses a requirement name and names the config field (f043b2b)
- fix(cloud): no generate rewrites .gen while the migration publication reads its bundle (02c22b9)
- fix: a verdict names any project path, and concurrent reads share one install call (03ea680)
- fix(cloud): one file that fails its check no longer costs a download-batch answer (5eed1bd)
- fix(python): Putnami pins and installs uv like Bun and Go (163268a)
- fix(cloud): the runner image layer restores the Cloud CLI from the pinned extension (ea0f37b)
- fix(cloud): site-content packaging reads the tree validation checks (1b4e084)
- fix(cloud): the Cloud extension's build cache uploads by batch and reads storage tokens (5cfaed6)
- fix(cloud): cache the two pure-read Cloud checks and delete the two dead help tasks (1cfe71d)
- fix(cache): a task with no effect to replay is keyed and shared whatever its duration (ecc4d5a)

### Performance

- perf(cli): a hosted run serves a hit whose files no job reads (4bf426b)
- perf(cloud): late restores share one negotiate and uploads go in larger windows (bc6ba01)
- perf(cloud): the build cache fetches the small files of every hit in a few requests (78fcc9a)

### Other

- chore(release): prepare the 0.4.0 release (1217a7a)
- chore(sdd): the Cloud runtimes and the area detector declare their features (b7e3c46)
- chore: each expected check lives with the project whose tests prove it (1e49e0b)
- docs: describe the publication uploaders (c20f816)
- docs: the cache provider owns the remote cache configuration (d0496e4)
- docs: comments name the scheduler, planner, cachekey and publication packages (aeed0ff)
- docs(cloud): the infra plan texts name the core command as deleted (2ce2b4b)
- refactor: first-party extensions declare the traits of their serve, run, package and publish commands (25c7eb9)
