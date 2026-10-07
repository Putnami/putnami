# Agent-readiness CLI

Install the extension independently of `@putnami/intelligence`:

```sh
putnami extensions install --user --latest @putnami/agent-readiness
putnami agent-readiness
```

The command runs from any committed Git repository using a Git version that
supports `--no-lazy-fetch`. In a Putnami workspace,
add `@putnami/agent-readiness` to its extensions; user installations serve
commands outside a workspace. The `run` subcommand is the default.

## Collection and privacy

The collector reads committed files and Git history using read-only Git
commands. It measures the markers of method 0.4. It changes no repository
file, runs no project code and sends no file content, author email, remote URL
or absolute path. It sends counts, repository-relative paths, area names,
per-run author pseudonyms and commit identifiers. A privacy check and the
embedded schema v1 validation run before output or submission.

`putnami agent-readiness --print-payload` prints the exact JSON payload and
sends nothing. Inspect it before submitting if needed. A normal run sends
those bytes once through the generated client of
`POST /v1/intelligence/agent-readiness/reports`, then prints
what it read, the returned verdict, what it sent and the report link. The
service owns scoring and the report; the collector does not calculate levels.

## Flags and output

| Flag | Contract |
| --- | --- |
| `--print-payload` | Print the payload as JSON without contacting the service. |
| `--timeout <seconds>` | Bound collection and submission; a whole number from 1 to 3600, default 120. |
| `--output=json` or `--json` | Use the shared CLI result envelope for a submission. |

Success exits 0, invalid arguments or missing Git history exit 2, collection
or privacy failures exit 1, and submission failures exit 4. No failed send is
retried automatically.

The API origin defaults to `https://api.putnami.cloud`.
`PUTNAMI_CLOUD_API_URL`, then `PUTNAMI_CONTROL_PLANE_URL`, overrides it. No
credential is loaded or sent for this anonymous operation.

The [public method](https://putnami.dev/agent-readiness/method) defines the
markers and levels. The published payload schema is
[version 1](https://putnami.dev/schemas/agent-readiness/payload.v1.json).
