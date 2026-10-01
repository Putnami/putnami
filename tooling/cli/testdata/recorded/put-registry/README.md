# Recorded Put registry responses

Each `.http` file is one response from the production Put registry, written by
`curl -si --http1.1` and served back by `go.putnami.dev/sdk/extension/recorded`.
Tests at the registry boundary answer with these bytes. They do not author a
status and a body of their own. See
[`doc/23-testing-at-production-boundaries.md`](../../../doc/23-testing-at-production-boundaries.md).

| File | Request | Recorded |
| --- | --- | --- |
| `download-anonymous.401.http` | `GET https://put.putnami.dev/putnami/go/download?channel=canary&os=linux&arch=amd64`, no `Authorization` | 2026-09-17 |
| `download-invalid-credential.401.http` | Same request, `Authorization: Bearer pkt_invalid_recording` | 2026-09-17 |
| `download-token-for-another-registry.403.http` | Same request, carrying a valid bearer minted for `npm.putnami.dev` | 2026-09-17 |
| `download-version-not-found.404.http` | `GET https://put.putnami.dev/putnami/cloud/download?channel=0.0.0-20260101000000-00000000&os=linux&arch=amd64`, no `Authorization` | 2026-09-17 |
| `cli-download-private-anonymous.404.http` | `GET https://put.putnami.dev/putnami/cli/download?arch=amd64&channel=0.0.0-20260928071846-ca6955c76&os=linux`, no `Authorization`: a private CLI build that the same request with a credential downloads | 2026-09-28 |
| `gateway-reset-before-headers.502.http` | An archive blob upload by the `@putnami/cloud` publish task on a `main` run, 2026-09-15 | see below |

## Redactions

- The 403 request carried a live bearer. The response does not contain it, and the file holds only the response.
- No response carries a credential or personal data. Trace and request identifiers are kept as recorded.

## The 502 is partly reconstructed, and its host is unknown

The 502 was answered to an upload, not to a download, and the log does not name
the host that answered. It is filed here because the tests replay it on
download requests to the registry. The run log kept the status line and the
body verbatim:
`upload archive blob: 502 Bad Gateway: upstream connect error or disconnect/reset before headers. reset reason: protocol error`.
It did not keep the headers. The file declares only the two headers that frame
the body: `content-type: text/plain`, which the gateway's local reply uses, and
`content-length`. Replace this file with a full recording the next time the
gateway answers this way.

## Recording a new response

1. Run `curl -si --http1.1 '<url>' > <what-it-shows>.<status>.http`.
2. Check that the file holds no credential: `grep -i -E 'authorization|bearer|eyJ' <file>`.
3. Add a row to the table above.
