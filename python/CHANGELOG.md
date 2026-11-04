# Changelog — python

## 0.4.0 — 2026-11-04

### Breaking

- feat!: an extension manifest names its extension (fe4e1d3)
- feat!: extensions answer toolchain pins and the core installs them (6d40476)
- feat(protocols)!: templates declare no workspaceDevDependencies (a60d990)

### Features

- feat(protocols): past the per-task bound, keep the test cases that prove a check and the slowest ones, and report the dropped time (93b9187)
- feat(protocols): nine protocol slices own the features of their contracts (b1bfb06)
- feat(protocols): the features protocol owns the feature of its contract (8f3edc5)
- feat(sdd): every project carries a feature, a spec or a featureAuthority (64d15b8)
- feat(python): the Python extension answers toolchain pins for uv (089a7b0)
- feat(protocols): the workspace adapter declares its install inputs and its language (fed2589)
- feat(extension): contract 11 declares command kinds, workloads and effects (d41461e)
- feat(dev): @putnami/dev declares the development verbs (b2b22a5)
- feat(cli): publication-v1 uploads put members in-process (2e1c87d)
- feat(cli): publish through provider operations, with every upload in-process (00650c0)

### Fixes

- fix(go): prepare completes go.mod replaces; the CLI writes no go.mod (6218f1b)
- fix(python): Putnami pins and installs uv like Bun and Go (163268a)
- fix(python): the runtime builds with the Go the lock pins (e7412f7)
- fix(cache): a task with no effect to replay is keyed and shared whatever its duration (ecc4d5a)
- fix(test): a test process does not see the job variables of a hosted run (232c85c)

### Other

- build: extension projects declare the archives publish channel (cf5b5f7)
- refactor(extensions): the Go, TypeScript and Python batchable tasks declare their kind and phase (cb0b943)
- docs: comments name the scheduler, planner, cachekey and publication packages (aeed0ff)
- docs: name extensions validate and templates validate|test|package outside the core (8ef89c5)
- refactor: first-party extensions declare the traits of their serve, run, package and publish commands (25c7eb9)

## 0.2.1 — 2026-10-02

No change since 0.2.0. The line is republished with Putnami 0.3.0.

## 0.2.0 — 2026-10-01

First public release. See the root [CHANGELOG.md](../CHANGELOG.md).
