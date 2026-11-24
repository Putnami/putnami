# Changelog — java

## 0.5.0 — 2026-11-24

### Features

- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(stacks,integrations): shared core helpers, one command surface, pinned SDKs and the Android SDK opt-in (de53a45)
- feat(putnami.dev): publish the stack and monorepo tool references (f59d1d8)
- feat(java): build a Gradle or Maven project on a blank machine (3f27e83)
- feat(cli): putnami init declares and detects every language (6377670)
- feat(java): filter the tests and check the documentation links (e91d41d)
- feat(stacks): @putnami/dart runs Dart and Flutter packages as Putnami tasks (9feea7b)
- feat(stacks): @putnami/java runs Java and Kotlin with Gradle and Maven components (b67430d)

### Fixes

- fix: specs, ADRs and docs say what the code does, and the projects set aside are declared (b3178a1)
- fix(cli): a command output directory never holds another project's outputs (27565d9)
- fix(extensions): package --port declares no default, so the extensions' flags stay compatible (86fb618)
- fix(stacks): every stack extension packages a lowercase image name (0c64bd1)
- fix(extension-sdk): publish --docker honors --docker-registry (5fc22d4)
- fix(python,java): plan the image package step of a release-set member (2b2042d)

### Other

- test(java,rust): resolve the fixture roots so the stub-tool tests pass on macOS (9ca8c37)
- build(stacks): publish the stack extensions and tool integrations as archives (886d834)
