# CLI core

This package is the shared toolkit of the `@putnami/cloud` CLI. It holds
what every command needs regardless of its domain: the IO contract,
exit-coded errors, stored credentials and session refresh, workspace and app
resolution, flag and output handling, and the binding that reaches Cloud
services through their generated Go clients. The import path is
`go.putnami.dev/cloud/extension/internal/clicore`, package `clicore`.

It contributes no command. Each domain package (for example
[`internal/distributioncli`](../distributioncli) or
[`internal/runtimecli`](../runtimecli)) implements its commands on top of it,
and the aggregator in [`internal/cloudcli`](../cloudcli) passes the same `IO`
value and parsed flags to every command. Domain packages depend on this one
neutral package instead of on the aggregator.

## Layout

The package has one sub-package, a test helper. Files group by concern:

- **Contract:** `clicore.go` (default endpoints, the `.putnami/auth.json` and `.putnami/cloud-link.json` paths, and `ExitError` with its exit codes) and `io.go` (the `IO` struct: output and event callbacks, HTTP client, environment, clock, and prompts).
- **Credentials and sessions:** `auth.go` (active auth, workspace-scoped token minting, refresh with retry), `credentials.go` (reads and writes the stored token under the Putnami home directory), `oauth_token.go`, `oidc.go` (auth and control-plane base URLs), `jwt.go`, `browser.go`, and `apikeys.go` (auth-server's generated client, bound to the issuer of the signed-in session, for API key calls).
- **Workspace resolution:** `workspace.go` (`WorkspaceContext`, the per-invocation seam with the workspace id, base URL, and bearer), `resolve.go` (the cloud link, workspace config, and app lookup), and `project_id.go` (the project ID the framework derives from a project's workspace-relative path).
- **Root manifest edits:** `manifestjson.go`. `ManifestPath` finds the root manifest a setup command edits, and `UpsertJSONField` sets one field and leaves every other byte as written.
- **Generated clients:** `services.go`. `NewServiceClient` resolves a provider's generated client over any `client.ServiceBinding`. `WorkspaceContext.ServiceBinding` binds the control-plane host through `ServiceBindingFor`, which takes any base URL and forwards the signed-in user's bearer per call under the `user` profile. `ForwardedServiceBinding` also takes a base URL and forwards the bearer under another profile, such as put-server's `reader`. `CallWithSession` re-mints the bearer once on a 401. Every client that `NewServiceClient` builds refuses HTTP redirects. `context_client.go` makes a client's requests end when a context ends.
- **Status contract:** `statusnode.go`. Every `putnami cloud <entry> status` command and the `putnami cloud status` synthesis build a `StatusNode`: a title, a state (`ok`, `degraded`, `failing` or `unknown`), a detail, a fix, metrics, and child checks. A `StatusMetric` has a value, a unit (`count`, `bytes`, `percent`, `seconds`, `eur`), an optional limit and window, and a kind: `usage` for what a bill or a quota would read, `health` for how well the entry works. `RenderStatusReport` prints one entry (header, metrics table, checks table); `RenderStatusTable` prints the synthesis. `WriteStatus` and `WriteStatusSynthesis` apply the one exit rule: success unless the state is failing, or, with `--strict`, unless it is ok. Structured output holds one envelope whose data is the node.
- **Arguments:** `argv.go` (`Positionals`, `DropFirstPositional` and `ReplaceFirstPositional`). With `FirstPositional` in `flags.go`, they route a verb such as `cloud packages copy` to the handler of the older command.
- **Flags and output:** `flags.go`, `params.go`, and `output.go` (the text, JSON, JSON Lines, and Cloud Logging output modes from [`go.putnami.dev/protocol/cli`](../../../../protocols/cli)).
- **HTTP helpers:** `http.go` (JSON requests and `APIError`) and `useragent.go` (the unified CLI User-Agent).
- **Utilities:** `atomicfile.go`, `remote.go` (repository key from a Git remote), and `util.go`.
- **`hometest`:** points a test's user home at a temporary directory on Unix and Windows. Import it only from `_test.go` files.

## Dependencies

It builds on the framework `app`, `client`, `errors`, and `inject` modules.
`workspace.go` also calls identity-api's generated client
([`cloud/clients/identity-api`](../../../clients/identity-api)) to look up a workspace
id from its slug, and `apikeys.go` calls auth-server's generated client
([`cloud/clients/auth-server`](../../../clients/auth-server)).

## Consumers

Every other package of this module depends on it: the domain packages
(`configcli`, `datacli`, `deliverycli`, `distributioncli`, `identitycli`,
`observabilitycli`, `runtimecli` and `sourcecli`) and the aggregator
`cloudcli`.

## Test

```sh
./putnamiw test --projects @putnami/cloud-extension
./putnamiw lint --projects @putnami/cloud-extension
```

The tests need no database and no network.

## Read next

- [`@putnami/cloud` CLI source layout](../../README.md#source-layout).
- [Service clients](../../README.md#service-clients): how the generated clients are produced and updated.
- `oidc.go` for the endpoint variables (`PUTNAMI_AUTH_URL`, `PUTNAMI_AUTH_ISSUER`, `PUTNAMI_CLOUD_API_URL`, `PUTNAMI_CONTROL_PLANE_URL`) it reads.
