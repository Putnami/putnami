# @putnami/diagnostic-protocol-proof

Proves the requirements of `diagnostic/finding-record`, the feature `go.putnami.dev/protocol/diagnostic` declares in [`../putnami.features.json`](../putnami.features.json). Each test is bound with `spectest.Proves` to the check the feature declares for it, and uses only the protocol's exported API.

The protocol cannot bind these tests itself. `go.putnami.dev/protocol/features`, which provides `spectest`, requires the diagnostic module, so a test of the protocol that imported `spectest` would make the two projects depend on each other. This module requires both and nothing else, and nothing publishes it. This project owns no feature: its `featureAuthority` names `diagnostic/finding-record`.

## Commands

```bash
putnami test @putnami/diagnostic-protocol-proof
```

## Documentation

The contract is described in the protocol's [README](../README.md) and specified in [`../specs/finding-record.json`](../specs/finding-record.json).
