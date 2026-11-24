# Changelog — php

## 0.5.0 — 2026-11-24

### Features

- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(stacks,integrations): shared core helpers, one command surface, pinned SDKs and the Android SDK opt-in (de53a45)
- feat(putnami.dev): publish the stack and monorepo tool references (f59d1d8)
- feat(cli): putnami init declares and detects every language (6377670)
- feat(stacks): @putnami/dart runs Dart and Flutter packages as Putnami tasks (9feea7b)
- feat(php): add the documentation link check, the test filter and the forced install (42b7ba2)
- feat(php): add the workspace, serve, run, upgrade, package and publish commands (8646ee6)
- feat(php): a Composer repository needs no PHP or Composer on the machine (b4d739a)
- feat(stacks): @putnami/php runs PHP and Symfony components as Putnami tasks (4ae45f1)

### Fixes

- fix: specs, ADRs and docs say what the code does, and the projects set aside are declared (b3178a1)
- fix(cli): a command output directory never holds another project's outputs (27565d9)
- fix(extensions): package --port declares no default, so the extensions' flags stay compatible (86fb618)
- fix(stacks): every stack extension packages a lowercase image name (0c64bd1)
- fix(cli): an explicit install on a host with no archive keeps the committed pin (af9f83b)
- fix(cli): a flag's short alias reaches the task under its long name (3949d6b)
- fix(extension-sdk): publish --docker honors --docker-registry (5fc22d4)

### Other

- test(extensions): an image test never runs the developer's Docker credential helper (540e9a4)
- build(stacks): publish the stack extensions and tool integrations as archives (886d834)
- test(php): a sample application and library run the extension with real PHP (c6a1af0)
