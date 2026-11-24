# Changelog — rust

## 0.5.0 — 2026-11-24

### Features

- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(stacks,integrations): shared core helpers, one command surface, pinned SDKs and the Android SDK opt-in (de53a45)
- feat(putnami.dev): publish the stack and monorepo tool references (f59d1d8)
- feat(cli): putnami init declares and detects every language (6377670)
- feat(rust): fetch, install and upgrade crates, run cargo offline, filter tests and check doc links (38d64ce)
- feat(rust): run, serve, package and publish a crate (fd82747)
- feat(stacks): @putnami/dart runs Dart and Flutter packages as Putnami tasks (9feea7b)
- feat(stacks): @putnami/java runs Java and Kotlin with Gradle and Maven components (b67430d)
- feat(php): add the workspace, serve, run, upgrade, package and publish commands (8646ee6)
- feat(stacks): @putnami/rust covers a typical Rust repository with no override (5802aab)
- feat(stacks): a shared stack core, and @putnami/rust runs Cargo crates as Putnami tasks (26f9803)

### Fixes

- fix: specs, ADRs and docs say what the code does, and the projects set aside are declared (b3178a1)
- fix(cli): a command output directory never holds another project's outputs (27565d9)
- fix(extensions): package --port declares no default, so the extensions' flags stay compatible (86fb618)
- fix(stacks): every stack extension packages a lowercase image name (0c64bd1)

### Other

- test(extensions): an image test never runs the developer's Docker credential helper (540e9a4)
- test(java,rust): resolve the fixture roots so the stub-tool tests pass on macOS (9ca8c37)
- build(stacks): publish the stack extensions and tool integrations as archives (886d834)
- docs: correct the adoption and stack-core docs after the review (53a767a)
