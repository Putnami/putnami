# Recorded Go registry responses

Each `.http` file is one response from the production Go module registry,
written by `curl -si --http1.1` and served back by
`go.putnami.dev/sdk/extension/recorded`. The dry-run probe's tests answer with
these bytes. They do not author a refusal of their own. See
[`tooling/cli/doc/23-testing-at-production-boundaries.md`](../../../../../../../../tooling/cli/doc/23-testing-at-production-boundaries.md).

| File | Request | Recorded |
| --- | --- | --- |
| `module-version-not-found.404.http` | `GET https://go.putnami.dev/go.putnami.dev/sdk/extension/@v/v0.0.0-20260101000000-000000000000.zip`, no `Authorization`: a module the registry serves, at a version it does not | 2026-10-01 |
| `module-not-found.404.http` | `GET https://go.putnami.dev/go.putnami.dev/no-such-module-probe/@v/v1.0.0.zip`, no `Authorization` | 2026-10-01 |

## Redactions

No request carried a credential, and no response carries one or personal data.
Trace and request identifiers are kept as recorded.

## What is not recorded

- A response for a version the registry serves is not recorded: it is the
  module zip itself. The tests build that answer from the bytes they stage.
- A `401` or a `403` is not recorded. On 2026-10-01 every anonymous `GET` tried
  was answered `200` or `404`, a private module path included: the zip, `.mod`
  and `@v/list` paths of a served module and of `go.putnami.dev/cloud`, the
  version and release paths, the blob upload path and `/`. A refused credential
  needs a live bearer. The probe's handling of a status outside the protocol is
  tested with a `502` the test builds.

## Recording a new response

1. Run `curl -si --http1.1 '<url>' > <what-it-shows>.<status>.http`.
2. Check that the file holds no credential: `grep -i -E 'authorization|bearer|eyJ' <file>`.
3. Add a row to the table above.
