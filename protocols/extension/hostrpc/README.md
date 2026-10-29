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
- a collaboration provider the core calls;
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
{"jsonrpc":"2.0","id":1,"result":{"protocol":1,"methods":["collaboration.call","host.hello","verification.report","version.lines","workspace.owners"]}}
```

## Methods

| Method | Params | Result |
|---|---|---|
| `host.hello` | none | `{"protocol": 1, "methods": [...]}` (`HelloResult`) |
| `collaboration.call` | `{"contract", "operation", "input"}` (`CollaborationCallParams`) | The envelope `putnami <contract> <operation> --output=json` prints for the same request, byte for byte. |
| `verification.report` | `{"project", "task", "path", "digest"}` (`VerificationReportParams`) | `{"report": "<base64>"}` (`VerificationReportResult`): the bytes a successful cached test run produced, after the cache corroborated them. |
| `workspace.owners` | `{"paths": [...]}` (`WorkspaceOwnersParams`) | `{"owners": [{"path", "projects": [{"id", "path"}]}]}` (`WorkspaceOwnersResult`): the projects that own each path. |
| `version.lines` | `{"scope"}` (`VersionLinesParams`), `scope` optional | `{"sourceRevision", "recordedGraph" (optional), "lines": [{"line", "tagPattern", "version", "tagged", "tag", "release": {"version", "tag"}, "commits": [{"sha", "subject", "body"}]}]}` (`VersionLinesResult`): each version line as the core computes it. |

Params are strict: an unknown member is `-32602`. `collaboration.call` routes
contract operations only. A `contract` that is not a collaboration contract is
refused with `-32602`. An operation the contract does not define gets the
envelope the command prints for it, whose outcome is `invalid`.

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

`version.lines` reports the workspace's version lines as the core computes
them. The `putnami version get` and `putnami version tag` commands of
`@putnami/dev` read them. The params
object is required; `{}` asks for every line, sorted by scope path. A `scope`
names one line by its scope path, as the `--scope` flag of
`putnami version get` does: leading and trailing slashes are ignored, and an
empty scope asks for every line. Each entry carries:

- `line`, the line's scope path (`""` is the root line), and `tagPattern`,
  how its release tags are spelled, such as `tooling/v{version}`;
- `version`, what `putnami version get` prints for the line;
- `tagged`, whether HEAD carries one of the line's release tags, and `tag`,
  that tag;
- `release`, the version and tag `putnami version tag` gives the line's next
  release when no tag is named, absent when `tagged` is true;
- `commits`, the commits that touch the line since its last release, newest
  first, which that release's changelog lists. It is `[]` when there are
  none.

`sourceRevision` is the full commit ID the run is bound to:
`PUTNAMI_SOURCE_REVISION` when the host's environment sets it, else HEAD's.
A release goes on HEAD, so a caller that releases refuses when the two
differ. When the workspace has a support catalog, the host first refreshes the
dependency graph the catalog reads. If the refresh fails, the host computes
the versions from the graph it recorded earlier and adds `recordedGraph`:
`observedAt`, when it recorded that graph (RFC 3339), and `refreshError`, why
the refresh failed. A caller that releases refuses then. With no recorded
graph either, the method answers `-32603`.

A scope that names no line is `-32602`, and its message lists the lines.
Outside a workspace the method answers `-32002`. A support catalog the host
cannot parse, and a `PUTNAMI_SOURCE_REVISION` that is not a full lowercase
commit ID, are `-32004`. A Git failure, such as a shallow clone, which holds
too little history to compute a version, is `-32603`.

`workspace.owners`, `version.lines` and the code `-32004` are additive, so the
protocol version stays 1. A client finds the methods in the list `host.hello`
returns.

## Errors

| Code | Meaning |
|---|---|
| `-32700` | The frame is not JSON. |
| `-32600` | The frame is not a request object, or exceeds the frame limit. |
| `-32601` | The host does not serve this method. `host.hello` lists the ones it does. |
| `-32602` | The params are not the method's. |
| `-32603` | The host failed. |
| `-32001` | The token is not this launch's. |
| `-32002` | This launch cannot serve the method, for example `collaboration.call` outside a workspace that declares `options.collaboration`, or `workspace.owners` and `version.lines` outside any workspace. |
| `-32003` | The cache does not corroborate the requested verification report. |
| `-32004` | The workspace's configuration or the host's environment is invalid, for example `version.lines` with an unparsable `putnami.support.json` or a malformed `PUTNAMI_SOURCE_REVISION`. The message is the one the core prints, and the core exits 2 for it where it exits 1 for `-32603`. |

An operation that fails at the provider is not an error: the envelope reports
it, exactly as the CLI prints it.

## Implementations

| Side | Package |
|---|---|
| Extension (client) | [`go.putnami.dev/sdk/extension/hostrpc`](../../../tooling/extension-sdk/hostrpc) |
| Host (server) | `tooling/cli/internal/hostrpc`, started by `tooling/cli/internal/cli` |

The decision and its alternatives are recorded in
[ADR 0071 of `@putnami/cli`](../../../tooling/cli/doc/adr/0071-an-extension-command-calls-the-core-over-a-host-rpc-channel.md).
