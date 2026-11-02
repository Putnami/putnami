<!-- putnami-observe:summary -->
### putnami observe

**1 changed, 1 dependent components; changed lines 80.0% covered; 1 dependency change**

Commit `aaaaaaaaaaaa`, measured from `bbbbbbbbbbbb`, [CI run](https://github.com/acme/shop/actions/runs/987).

| Component | Impact | Tests | Coverage |
|---|---|---|---|
| `packages/pricing` | changed | 4 passed, 1 failed, 0 skipped | 80.0% of 50 lines |
| `apps/web` | dependent | no tests | no coverage |

**Failing tests** (1)

- `Calculator > applies the discount`: `expected 90, got 100`

**Changed lines**: 8 of 10 covered (80.0%)

| File | Covered | Uncovered lines |
|---|---|---|
| `packages/pricing/src/discount.ts` | 6 of 8 | 12-13 |
| `packages/pricing/src/line\|item.ts` | 2 of 2 | none |

**Dependency changes** (1)

| Package | Change | Version | Lockfile |
|---|---|---|---|
| `lodash` | upgraded | `4.17.20 → 4.17.21` | `package-lock.json` |

1 diagnostic in the job log: `observe.report_missing`.
