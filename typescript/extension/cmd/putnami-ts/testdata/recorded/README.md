# Recorded npm exchanges

The dry-run npm probe's tests answer with what the other side sent. They do not
author a refusal of their own. See
[`tooling/cli/doc/23-testing-at-production-boundaries.md`](../../../../../../tooling/cli/doc/23-testing-at-production-boundaries.md).

## `npm-registry/`: HTTP responses

Each `.http` file is one response from the production npm registry of Putnami,
written by `curl -si --http1.1` and served back by
`go.putnami.dev/sdk/extension/recorded`. The managed probe reads them.

| File | Request | Recorded |
| --- | --- | --- |
| `tarball-version-not-found.404.http` | `GET https://npm.putnami.dev/@putnami/runtime/-/runtime-0.0.0-20260101000000-00000000.tgz`, no `Authorization`: a package the registry serves, at a version it does not | 2026-10-01 |
| `tarball-package-not-found.404.http` | `GET https://npm.putnami.dev/@putnami/no-such-package-probe/-/no-such-package-probe-1.0.0.tgz`, no `Authorization` | 2026-10-01 |

## `npm-cli/`: command exchanges

Each directory is one `npm` invocation: `stdout`, `stderr` and `exit`, replayed
through `recorded.Command`. The unmanaged probe reads them. They were recorded
with npm 10.2.4, in a directory holding only a `package.json`, with an empty
user and global configuration (`NPM_CONFIG_USERCONFIG` and
`NPM_CONFIG_GLOBALCONFIG` pointing at empty files), so no request carried a
credential.

| Directory | Command | Recorded |
| --- | --- | --- |
| `view-version-not-found` | `npm view lodash@99.99.99 dist --json --fetch-retries 0` | 2026-10-01 |
| `view-package-not-found` | `npm view @putnami-probe-test/nothing@1.0.0 dist --json --fetch-retries 0` | 2026-10-01 |
| `view-registry-unreachable` | `npm view lodash@4.17.21 dist --json --fetch-retries 0 --registry http://127.0.0.1:9`: a port nothing listens on | 2026-10-01 |
| `view-version-held` | `npm view lodash@4.17.21 dist --json --fetch-retries 0`: the public registry, which advertises a `sha512` integrity | 2026-10-01 |
| `view-private-registry` | `npm view @putnami/runtime@0.2.0 dist --json --fetch-retries 0 --registry https://npm.putnami.dev`: a registry that advertises a `sha256` integrity | 2026-10-01 |
| `config-get-registry` | `npm config get @putnami-probe-test:registry registry` | 2026-10-01 |

## Redactions

- No request carried a credential, and no recording holds one.
- npm prints the path of its debug log on `stderr`. The home directory in that
  path and in the stack trace of `view-registry-unreachable` is replaced with
  `/Users/person/`, a name of the same length.
- Trace and request identifiers are kept as recorded.

## What is not recorded

- A response for a tarball the registry serves is not recorded: it is the
  package itself. The tests build that answer from the bytes they stage.
- An `npm view` answer whose integrity equals the staged package is built by the
  test, from the tarball its fake `npm pack` writes.
- A `401` or a `403` is not recorded: the registry answers an anonymous read of
  the packages it serves, and a refused credential needs a live bearer.

## Recording a new exchange

1. For a response, run `curl -si --http1.1 '<url>' > <what-it-shows>.<status>.http`.
2. For a command, run `mkdir <what-it-shows> && <command> > <what-it-shows>/stdout 2> <what-it-shows>/stderr; echo $? > <what-it-shows>/exit`.
3. Check that the recording holds no credential and no personal data: `grep -r -i -E 'authorization|bearer|eyJ' <path>`.
4. Add a row to the matching table above.
