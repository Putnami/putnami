# @putnami/spectest

Binds `bun:test` tests to the executable-spec checks they prove, so the spec gate can read their verdicts.

```ts
import { specTest } from '@putnami/spectest';

specTest('a broken link fails the gate', { feature: 'docs/links', requirement: 'broken-links-fail', check: 'names-the-link' }, () => {
  // ...
});
```

`specTest` registers an ordinary test. `observeMeasurement` registers a test that returns the number a threshold check measures. Outside a Putnami test run, both register the test unchanged and write nothing.

Most projects import the same functions from `@putnami/runtime/spectest`, which re-exports this package.

## Commands

```bash
putnami lint,test,build @putnami/spectest  # check it the way CI does
```

## Documentation

- [The TypeScript test job](../../extension/doc/03-test.md#spec-verification-report) explains how the fragments become the project's verification report.
- `src/index.ts` documents the wire format and why a fragment can never turn a failing test green.
