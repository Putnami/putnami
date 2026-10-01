# Recorded registry-token exchanges

Each directory is one run of the credential seam's command,
`putnami cloud registry-token --host <host>`, as its caller observes it:
`stdout`, `stderr`, and the `exit` status. `go.putnami.dev/sdk/extension/recorded`
replays them. The seam's tests answer with these bytes. They do not author
their own.

| Directory | What ran | Recorded |
| --- | --- | --- |
| `settled/` | `putnami cloud registry-token --host put.putnami.dev`, signed in, lock settled. CLI built from this repository; `@putnami/cloud` `0.0.0-20260914013011-2aa9b861-c3db1d2` | 2026-09-17 |
| `not-authenticated/` | Same command with `PUTNAMI_HOME` pointing at a home that holds the installed artifacts and no `auth.json` | 2026-09-17 |
| `not-authenticated-materialize/` | The `--materialize` form of the same run, for `npm.putnami.dev` | 2026-09-17 |
| `core-only-cli/` | Same command in a workspace with no extension and an empty `PUTNAMI_HOME`. The released CLI `0.0.0-20260916061023-7194fc35-02f32ca` and a CLI built from this repository printed identical bytes | 2026-09-17 |
| `core-only-cli-materialize/` | The `--materialize` form of the same core-only run | 2026-09-17 |
| `lock-changed/` | The first call after `putnami.lock.json` changed, captured by a `PATH` shim during the incident the fix addressed | 2026-09-15 |

## Redactions

- Runs through `./putnamiw` also print the wrapper's own "Building putnami CLI" lines on stderr. Those lines come from the wrapper, not from `putnami`, and are removed.
- `settled/stdout` and `lock-changed/stdout` carry a bearer. It is not the recorded one. It keeps the recorded shape: an RS256 JWT of 1011 bytes whose three segments are 123, 544 and 342 bytes, with an 8-hour lifetime (`exp - iat = 28800`). Every claim value, the key id and the signature are replaced.
- `lock-changed/stdout` keeps the three setup lines exactly as the shim logged them.

## Recording a new exchange

1. Run `mkdir <name> && <command> > <name>/stdout 2> <name>/stderr; echo $? > <name>/exit`.
2. Replace any bearer with a value of the same length and segment sizes.
3. Add a row to the table above.
