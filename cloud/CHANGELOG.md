# Changelog — cloud

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
