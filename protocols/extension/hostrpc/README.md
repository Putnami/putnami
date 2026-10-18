# Host RPC protocol

`go.putnami.dev/protocol/extension/hostrpc` is the wire contract of the host
RPC channel. Through it, an extension command calls into the `putnami` process
that launched it. It does not start a second `putnami` process.

## Why

An extension command sometimes needs an answer that only the core has: the
envelope of a collaboration contract operation, or a verification report the
cache can corroborate. Before this channel, the only way to get one was to run
`putnami` again as a subprocess. That second process loads the workspace again,
resolves the extensions again and can disagree with its parent. The channel
lets the extension ask the process that already holds that state.

## Who receives it

The host opens one channel for each interactive command-group subcommand it
launches, and closes it when that process exits. The child's environment
carries two variables:

| Variable | Value |
|---|---|
| `PUTNAMI_HOST_RPC` (`SocketEnv`) | The path of a Unix socket in a fresh directory that only the user can open (mode 0700). |
| `PUTNAMI_HOST_RPC_TOKEN` (`TokenEnv`) | 32 random bytes (`TokenBytes`), hex-encoded. |

The host sets neither variable for these processes:

- a planned job, such as a build job or a non-interactive subcommand, whose
  result the cache keys and whose answer must not depend on a call the key
  does not cover;
- a provider tool the core calls;
- any process of a hosted run (`--credential-fd`).

The host also removes an inherited value from these processes, so a nested
`putnami` run never passes its parent's channel on. If the host cannot open
the channel, the child receives no variable and the host prints a warning.

## Frames

- JSON-RPC 2.0. Each frame is one JSON object followed by a newline (`\n`).
- A frame is at most 32 MiB (`MaxFrameBytes`) without its newline, in both
  directions. The host answers a longer frame with `-32600` and closes the
  connection.
- One connection serves its requests in order, one at a time. The host
  serializes calls across connections too.
- Every request carries the token in a top-level `token` member. The host
  compares it in constant time. A request with any other token gets `-32001`,
  and the host closes the connection.
- A request without an `id` is a notification. No method accepts one, so the
  host answers nothing. A notification with any other token gets no answer
  either, and the host closes the connection.

```json
{"jsonrpc":"2.0","id":1,"method":"host.hello","token":"<64 hex characters>"}
{"jsonrpc":"2.0","id":1,"result":{"protocol":1,"methods":["collaboration.call","host.hello","verification.report","workspace.owners"]}}
```

## Methods

| Method | Params | Result |
|---|---|---|
| `host.hello` | none | `{"protocol": 1, "methods": [...]}` (`HelloResult`) |
| `collaboration.call` | `{"contract", "operation", "input"}` (`CollaborationCallParams`) | The envelope `putnami <contract> <operation> --output=json` prints for the same request, byte for byte. |
| `verification.report` | `{"project", "task", "path", "digest"}` (`VerificationReportParams`) | `{"report": "<base64>"}` (`VerificationReportResult`): the bytes a successful cached test run produced, after the cache corroborated them. |
| `workspace.owners` | `{"paths": [...]}` (`WorkspaceOwnersParams`) | `{"owners": [{"path", "projects": [{"id", "path"}]}]}` (`WorkspaceOwnersResult`): the projects that own each path. |

Params are strict: an unknown member is `-32602`. `collaboration.call` routes
contract operations only. A `contract` that is not a collaboration contract,
and `proposals finalize`, which is a command, not an operation, are refused
with `-32602`.

`workspace.owners` answers which workspace projects own each path. Each path
is workspace-relative and in `io/fs` form (`ValidWorkspacePath`): no leading
or trailing slash, no empty, `.` or `..` element, and not `.` itself. The
result holds one entry for each requested path, in request order, and a path
that repeats gets an entry each time. A project owns a path when impact
analysis treats it as an owner: the nearest project whose directory holds the
path (a project rooted at the workspace root owns what no deeper project
holds), every project that claims the path as an asset, and, for a file at the
workspace root such as `go.work`, every project whose provider lists that file
as watched. The CLI applies this rule to a changed file before it follows the
dependency graph, so a root file can have many owners. A backslash is an
ordinary character of a name, not a separator. An entry lists its owners
sorted by project ID, with the project's canonical ID and its
workspace-relative directory. A path that no project owns gets an empty
`projects` array. An empty `paths` array, or a path
that is not in `io/fs` form, is `-32602`. Outside a workspace the method
answers `-32002`.

The method is additive, so the protocol version stays 1. A client finds it in
the methods `host.hello` lists.

## Errors

| Code | Meaning |
|---|---|
| `-32700` | The frame is not JSON. |
| `-32600` | The frame is not a request object, or exceeds the frame limit. |
| `-32601` | The host does not serve this method. `host.hello` lists the ones it does. |
| `-32602` | The params are not the method's. |
| `-32603` | The host failed. |
| `-32001` | The token is not this launch's. |
| `-32002` | This launch cannot serve the method, for example `collaboration.call` outside a workspace that declares `options.collaboration`, or `workspace.owners` outside any workspace. |
| `-32003` | The cache does not corroborate the requested verification report. |

An operation that fails at the provider is not an error: the envelope reports
it, exactly as the CLI prints it.

## Implementations

| Side | Package |
|---|---|
| Extension (client) | [`go.putnami.dev/sdk/extension/hostrpc`](../../../tooling/extension-sdk/hostrpc) |
| Host (server) | `tooling/cli/internal/hostrpc`, started by `tooling/cli/internal/cli` |

The decision and its alternatives are recorded in
[ADR 0071 of `@putnami/cli`](../../../tooling/cli/doc/adr/0071-an-extension-command-calls-the-core-over-a-host-rpc-channel.md).
