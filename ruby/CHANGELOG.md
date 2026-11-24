# Changelog — ruby

## 0.5.0 — 2026-11-24

### Features

- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(stacks,integrations): shared core helpers, one command surface, pinned SDKs and the Android SDK opt-in (de53a45)
- feat(putnami.dev): publish the stack and monorepo tool references (f59d1d8)
- feat(cli): putnami init declares and detects every language (6377670)
- feat(ruby): @putnami/ruby runs, installs, packages and publishes Ruby and Rails components (76c6f00)

### Fixes

- fix(cli): a command output directory never holds another project's outputs (27565d9)
- fix(extensions): package --port declares no default, so the extensions' flags stay compatible (86fb618)
- fix(stacks): every stack extension packages a lowercase image name (0c64bd1)

### Other

- test(extensions): an image test never runs the developer's Docker credential helper (540e9a4)
- build(stacks): publish the stack extensions and tool integrations as archives (886d834)
- docs: correct the adoption and stack-core docs after the review (53a767a)
