# Runtime CLI

This package holds the Runtime commands of the `@putnami/cloud` extension:
`deploy status`, the operator `deploy publish-v2` request, environment status,
and the continuous-delivery (CD) environment checks. The import path is
`go.putnami.dev/cloud/extension/internal/runtimecli`, package `runtimecli`.

The package has no entrypoint of its own. The aggregator in
[`internal/cloudcli`](../cloudcli) imports it, and `internal/cloudcli/cli.go`
maps each command name to one exported function here.
[`putnami.extension.json`](../../putnami.extension.json) declares the
commands. There is no top-level `putnami deploy` verb: workloads deploy
through environments that follow channels.

## Commands

- `putnami cloud deploy status <release-id>`: prints one release as a status node, with one check per workload. It exits 1 when the release failed or ended `partial` or `skipped`; a release still converging reads `degraded` and exits 0.
- `putnami cloud deploy publish-v2 --request-file <path>`: accepts the environment definition at the request's source revision, then submits an exact release-set selection through Control's ordinary publish v2 path. A release set is the immutable record of what one publishing run released. An operator rolls one workload back with a request that names an older set.
- `putnami cloud env status [<env>]`: prints a status node: each environment `putnami.ci.json` declares, or the one named, against what the workspace runs there, with its channel and each workload. It exits 1 when a workload failed, or ended `partial` or `skipped`. `--health` and `--provenance` print the deployment table instead: the CD header of one environment, then project, environment, revision, and health. `--health` adds the last 10 minutes of errors and whether the served commit is on main, and `--provenance` shows the commit-to-revision chain. The root `putnami cloud status` is a one-line-per-entry summary that the aggregator builds; its `env` line comes from here.
- `putnami cloud env doctor [<env>]`: prints every CD prerequisite of one environment as a status node. A missing prerequisite reads `failing` and exits 1; a check that could not run reads `unknown` and exits 1 only under `--strict`.
- `putnami cloud env enable [<env>]`: applies the fix for each missing prerequisite the workspace owns, then prints the prerequisites as a table. It always asks Control to accept the environment definition at `--source-revision`, or at the default-branch head: Control replays a source revision it already accepted and writes nothing, so a moved head is accepted even when the row already reads ok.

`Env` takes a function argument that the aggregator supplies, so this
package does not import the Data CLI package.

## Layout

The package has no sub-packages. Files group by concern:

- **Package doc:** `doc.go`.
- **Deploy:** `deploy.go` (`Deploy`, `DeployStatus`, the submit and poll calls), `deploy_publish_v2.go` (the request file and the acceptance), `deploy_release.go` (how a release and its `--wait` poll print), `deploy_control.go` (the control-api client of the deploy calls), and `deploy_reply.go` (the output types the deploy replies are read into).
- **Status:** `env_status.go` (the `env status` node), `status.go` (the deployment table of `--health` and `--provenance`), `status_header.go` (the channel-follow header), and `status_health.go` (`--health`).
- **Environment checks:** `env_doctor.go`, `env_doctor_checkout.go` (the rows the checkout answers), and `env_enable.go`.
- **Status nodes:** `status_node.go` (the `env doctor` node) and `deploy_status_node.go` (the `deploy status` node). `env_status.go` also answers the `env` line of `putnami cloud status`.
- **Clients:** `control_client.go` binds control-api's generated client for reads such as `status`.

The Cloud SQL Auth Proxy runner is not in this package. It is
`putnami operator sql-proxy`: it needs Google Cloud IAM credentials, so it is
not a public command.

## Dependencies

It calls Cloud services through generated Go clients:
[`cloud/clients/control-api`](../../../clients/control-api),
[`cloud/clients/distribution-api`](../../../clients/distribution-api),
[`cloud/clients/runtime-api`](../../../clients/runtime-api), and
[`cloud/clients/observability-api`](../../../clients/observability-api) (the error
logs that `status` reads). The deploy calls in `deploy.go` (`POST /deploy` and
`GET /deploy/{id}`) bind their own control-api client per call
(`deploy_control.go`). Wire types come from those generated clients. The
package reads each reply once into its own output types, which keep the
member order of the structured output. IO, auth, and flag handling come from
[`internal/clicore`](../clicore).

## Consumers

- [`internal/cloudcli`](../cloudcli), the aggregator, is the only package that
  imports it.

## Test

```sh
./putnamiw test --projects @putnami/cloud-extension
./putnamiw lint --projects @putnami/cloud-extension
```

The tests use `httptest` servers in place of the control plane, so they need no
database and no credentials. The `status --health` tests build a Git checkout
and fail without `git` on `PATH`.

## Read next

- [`@putnami/cloud` CLI source layout](../../README.md#source-layout).
- [CLI extension README](../../README.md#commands) for the full command list.
