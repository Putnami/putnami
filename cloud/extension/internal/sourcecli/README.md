# Source CLI

`putnami cloud source connect` is the primary way to connect a GitHub
repository to the linked Putnami Cloud workspace. It starts a workspace-scoped
onboarding session, opens GitHub authorization in the default browser, polls at
the server-provided interval, and completes the selected repository binding.
The package is `sourcecli`
(`go.putnami.dev/cloud/extension/internal/sourcecli`). It calls source-api
through its generated client ([`cloud/clients/source-api`](../../../clients/source-api)).

```bash
putnami cloud source connect                         # propose local origin, then prompt
putnami cloud source connect --repo acme/app         # exact non-interactive selection
putnami cloud source connect --repo acme/app --replace
putnami cloud source status --output=jsonl
putnami cloud source disconnect --yes
```

Structured output requires `--repo owner/name`; it never prompts for a
selection. Browser opening defaults to enabled. With human output, use
`--no-open` to print the authorization URL without launching a browser.
On timeout or interruption, the CLI best-effort cancels its actor-owned session.

The break-glass `source bind` and `source unbind` are not in this package.
They are `putnami operator source bind|unbind`, from the `@putnami/operator`
extension. This CLI does not call the source-api binding routes:
`putnami cloud source bind` fails with a usage error that names the operator
command.

`putnami cloud source status` prints one status node with three checks:

| Check | ok | not ok |
| --- | --- | --- |
| connection | a repository is connected | degraded: none connected |
| health | the GitHub App installation is active | failing: suspended, uninstalled, or the repository was removed |
| permissions | the App holds every required permission | degraded: a permission update is required; unknown: not checked |

It also counts the open pull requests, and the stale ones (no update in 14
days), from source-api. That read stops after 5 pages of 100 and never
changes the state. The command exits 1 when the node is failing, and with
`--strict` when it is not ok.

`SourceStatusNode` answers the `source` line of `putnami cloud status`.
