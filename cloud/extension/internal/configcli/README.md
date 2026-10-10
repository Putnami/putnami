# Config CLI

The Config domain's commands for the `@putnami/cloud` CLI extension, in
package `configcli` (`go.putnami.dev/cloud/extension/internal/configcli`). It
implements `putnami cloud config` and `putnami cloud secrets`. It also supplies
the Config logic behind `config publish`, `config validate` and
`package-config-member`. It builds on the shared toolkit in
[`internal/clicore`](../clicore) and on the authored-member format in
[`go.putnami.dev/protocol/config/authoredmember`](../../../../protocols/config/authoredmember).

## Commands

- `putnami cloud config status [<project>]` and
  `putnami cloud secrets status [<project>]`: compare what each project's
  published schema declares with what each environment sets, and print one
  status report (`--strict`, `--output json`). They list the projects from
  `GET /v1/workspaces/{ws}/config/apps`. A required key or secret that is not
  set fails the status; secrets status never reads a value. Without a project,
  one status reads at most 240 calls' worth of projects and reports the rest as
  not read. `ConfigStatusNode` and `SecretsStatusNode` serve the same nodes to
  `putnami cloud status`.
- `putnami cloud config show [<project>]`: shows the resolved config. One
  option picks another view: `--schema` (the config schema), `--key <key>`
  (one key), `--secret-keys` (secret-key status), `--reveal-secrets`
  (plaintext secrets after confirmation), `--declared` (every declared config
  and secret key and whether it is set, with no value), `--keys` (the resolved
  key names) or `--metadata` (the merge metadata). The aggregator maps each
  view to the handler here; the older forms `config <project>`, `config list`
  and `config resolve` still run.
- `putnami cloud config put | drift`: force-writes non-secret values from
  YAML, or diffs the committed `conf/env*.yaml` against the published config
  plane. `drift` exits non-zero on drift.
- `putnami cloud secrets set | get | reveal | list | delete`: manages
  workspace and project secrets.
- `putnami cloud config publish`: outside a release set, the compatibility
  path that publishes a project's config and schema through `/api/configs` and
  `/api/schemas`. A release set is the immutable record of what one publishing
  run released. When the command runs with a release-set context, it takes the
  authored-member path instead: it prepares and verifies the authored Config
  member and publishes it to Distribution.

Run `putnami cloud config help` or `putnami cloud secrets help` for the full
flag list.

## How the CLI mounts it

The extension binary ([`cmd/putnami-cloud`](../../cmd/putnami-cloud))
runs the aggregator in [`internal/cloudcli`](../cloudcli). `cli.go` dispatches
each command name to this package:

- `config` calls `Config`, and `secrets` calls `Secrets`.
- `config publish`, `config validate`, and `package-config-member` go through
  `internal/cloudcli/publish_config.go`. It calls `PrepareAuthoredConfig`,
  `PackageAuthoredConfig`, `VerifyPackagedAuthoredConfig`, or `PublishConfig`.
- `internal/cloudcli/workspace_probe.go` reads a project's declared Config
  namespace with `ConfigNamespaceFromOptions` and `AuthoredConfigCoordinate`.

## Layout

The package has no sub-packages:

- `config.go`, `config_inspect.go`: the `config` family and the inspect view.
- `secrets.go`: the `secrets` family.
- `status.go`: `config status` and `secrets status`, and the nodes
  `putnami cloud status` reads.
- `publish.go`: `config publish` and the sync publish path.
- `authored_publish.go`, `authored_schema_fields.go`: prepares, packages, and
  verifies the authored Config member. Publish checks that a rebuild from the
  current inputs matches the packaged bytes exactly.
- `configapi.go`: binds config-api's generated client
  ([`cloud/clients/config-api`](../../../clients/config-api)) through the control-plane
  host the workspace link names, and forwards the caller's bearer per call.
  The config, secrets and publish-config commands share it.
- `drift.go`, `metadata_token.go`: the drift check. Without a user token, it
  authenticates with the runner's Google metadata ID token.
- `flags.go`: registers the domain's value-less flags with `clicore`.

## Consumers

- [`internal/cloudcli`](../cloudcli), the aggregator.

## Test

```sh
./putnamiw test --projects @putnami/cloud-extension
./putnamiw lint --projects @putnami/cloud-extension
```

The tests call the exported handlers directly, with a temporary workspace and
fake HTTP clients. They need no Putnami Cloud account, database, or
environment variables.

## Read next

- [CLI extension README](../../README.md), including the full command list.
