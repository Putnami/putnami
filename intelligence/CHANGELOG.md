# Changelog — intelligence

## 0.5.0 — 2026-11-24

### Features

- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)

### Fixes

- fix: specs, ADRs and docs say what the code does, and the projects set aside are declared (b3178a1)

## 0.4.0 — 2026-11-04

### Breaking

- refactor(intelligence)!: remove the agent-readiness extension and keep the area detector (e600515)

### Features

- feat(intelligence): add public agent-readiness extension (7b407e5)

### Fixes

- fix(sdd): sdd.list_features answers one bounded page of short entries (110bc27)

### Other

- chore(sdd): the Cloud runtimes and the area detector declare their features (b7e3c46)
- test(agent-readiness): assert the no-retry rule where a retry can happen (599a738)
