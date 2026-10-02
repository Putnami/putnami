# Recorded Put registry responses

Each `.http` file is one response from the production Put registry, written by
`curl -si --http1.1` and served back by `go.putnami.dev/sdk/extension/recorded`.
The archive probe's tests answer with these bytes. They do not author a refusal
of their own. See
[`tooling/cli/doc/23-testing-at-production-boundaries.md`](../../../../../cli/doc/23-testing-at-production-boundaries.md).

The probe sends `HEAD`. Each response is recorded with `GET` on the same URL,
because a `HEAD` response declares a body length and carries no body, which the
replay rejects as truncated. The registry answers both methods with the same
status and headers.

| File | Request | Recorded |
| --- | --- | --- |
| `archive-version-not-found.404.http` | `GET https://put.putnami.dev/putnami/cli/download?channel=0.0.0-20260101000000-00000000&os=linux&arch=amd64`, no `Authorization` | 2026-10-01 |
| `archive-package-not-found.404.http` | `GET https://put.putnami.dev/putnami/no-such-package-probe/download?channel=1.0.0&os=linux&arch=amd64`, no `Authorization` | 2026-10-01 |
| `archive-private-anonymous.404.http` | `GET https://put.putnami.dev/putnami/cli/download?arch=amd64&channel=0.0.0-20260928071846-ca6955c76&os=linux`, no `Authorization`: a private build that the same request with a credential downloads | 2026-09-28 |
| `archive-anonymous.401.http` | `GET https://put.putnami.dev/putnami/go/download?channel=canary&os=linux&arch=amd64`, no `Authorization` | 2026-09-17 |
| `archive-token-for-another-registry.403.http` | Same request, carrying a valid bearer minted for `npm.putnami.dev` | 2026-09-17 |

The last three files are copies of the recordings in
`tooling/cli/testdata/recorded/put-registry/`
(`cli-download-private-anonymous.404.http`, `download-anonymous.401.http` and
`download-token-for-another-registry.403.http`). The registry no longer answers
the 401 request that way without a credential, and the 403 needs a live bearer.
A project's tests read only its own recordings, so the bytes are copied.

## Redactions

- The 403 request carried a live bearer. The response does not contain it, and the file holds only the response.
- No response carries a credential or personal data. Trace and request identifiers are kept as recorded.

## What is not recorded

A response for a version the registry holds is not recorded: it declares a body
of tens of megabytes. The tests build that answer, with the two headers the
registry sends on it, `x-integrity` and `x-resolved-version`.

## Recording a new response

1. Run `curl -si --http1.1 '<url>' > <what-it-shows>.<status>.http`.
2. Check that the file holds no credential: `grep -i -E 'authorization|bearer|eyJ' <file>`.
3. Add a row to the table above.
