# @putnami/capabilities-protocol-proof

Proves the requirements of `capabilities/manifest-contract`, the feature `go.putnami.dev/protocol/capabilities` declares in [`../putnami.features.json`](../putnami.features.json), against the protocol's fixtures and schemas. Each test is bound with `spectest.Proves` to the check the feature declares for it, and uses only the protocol's exported API. The Go framework, `@putnami/application` and `@putnami/typescript` prove the same feature under checks of their own.

The protocol cannot bind these tests itself. `go.putnami.dev/protocol/features`, which provides `spectest`, requires the capabilities module, so a test of the protocol that imported `spectest` would make the two projects depend on each other. This module requires the protocol, `go.putnami.dev/protocol/diagnostic` and `go.putnami.dev/protocol/features`, and nothing publishes it. This project owns no feature: its `featureAuthority` names `capabilities/manifest-contract`.

`version_test.go` also reads the TypeScript application producer, `typescript/framework/application/src/capabilities/capabilities.producer.ts`, to check the version it stamps; the file is a declared test input.

## Commands

```bash
putnami test @putnami/capabilities-protocol-proof
```

## Documentation

The contract is described in the protocol's [README](../README.md) and specified in [`../specs/manifest-contract.json`](../specs/manifest-contract.json).
