# Agent-readiness CLI

`@putnami/agent-readiness` is a separately installable Putnami extension. Its
command and repository collector are public source. The existing anonymous
service scores the payload and returns the report link.

```sh
putnami extensions install --user --latest @putnami/agent-readiness
putnami agent-readiness
putnami agent-readiness --print-payload
putnami agent-readiness --timeout 300 --output=json
```

Inside a Putnami workspace, declare `@putnami/agent-readiness` in that
workspace's extensions to make the command available there. It needs Git with
`--no-lazy-fetch` support and at least one commit. It needs no account or cloud setup.

See [the CLI contract](doc/01-cli.md) for privacy, output and configuration.

The module owns `areas`, `history`, `inventory`, `markers` and `payload`, the
schema v1 wire types in `contract`, and the embedded public schemas in `schema`.
`internal/command` collects, validates, submits and renders; `cmd` provides the
extension runtime. Server-side scoring and report storage remain separate.

## Support status

- **Status**: `preview`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json).
- **Owner**: `@putnami/agent-readiness` (`intelligence/agent-readiness`).
- **What may still change**: extension installation and command interfaces.
  Stable support requires release-matrix coverage and a compatibility budget
  for persisted CLI contracts.
- **Evidence**: the [CLI specification](specs/agent-readiness.json), collector
  fixtures, anonymous generated-client submission tests, runtime contract
  tests, and the CLI installer's readiness command-map consumer.

Run the repository gate from the repository root:

```sh
./putnamiw lint,test,build,validate --projects @putnami/agent-readiness --enforce-coverage
```
