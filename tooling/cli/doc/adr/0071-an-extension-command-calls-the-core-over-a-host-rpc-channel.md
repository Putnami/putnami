# ADR 0071 — An extension command calls the core over a host RPC channel

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/hostrpc`, `internal/cli`,
  `internal/jobs`), `go.putnami.dev/protocol/extension` (`hostrpc`) and
  `go.putnami.dev/sdk/extension` (`hostrpc`, `scratch`)
- **Decisions**: D-014 of the root `decisions.json`

## Context

D-014 settles that an extension command calls the collaboration providers
through an RPC call to the core, made with an SDK client. Commands that leave
the core for an extension still need answers only the core has: the envelope
of a collaboration contract operation, routed to the provider the workspace
binds, and a verification report the cache corroborates.

Until now, the only way to get one was to run `putnami` again as a
subprocess. That second process loads the workspace again, discovers the
extensions again and prepares their releases again. It can resolve another
provider set than its parent, and it costs a process start on every call.

This decision builds the channel and its first two methods. It moves no
command.

## Decision

1. **One channel per interactive launch.** The CLI opens a channel for each
   launch of a command-group subcommand declared `interactive: true`, inside
   or outside a workspace, and stops it when the process exits, whatever its
   exit: the stop is deferred in `runInteractiveExtensionCommand`, so the
   error path and a first interrupt, which cancels the run, close it too. A
   forced exit, on a second interrupt or when the 10-second shutdown timer
   fires, calls `os.Exit` and skips the deferred stop: it leaves the
   directory behind until the next channel's sweep reclaims it (decision 3).
   A planned job gets no channel. Its result is cached, and a call its cache
   key does not cover must not decide it. A provider tool the core calls gets
   none, and neither does any process of a hosted run (`--credential-fd`):
   `hostrpc.Start` returns `ErrHosted` there.
2. **The process receives two variables.** `PUTNAMI_HOST_RPC` names the
   socket, and `PUTNAMI_HOST_RPC_TOKEN` carries 32 random bytes, hex-encoded.
   `buildJobInvocation` removes both from the inherited environment and from
   the task's own `Env`, then restates them only from the launch's `extraEnv`,
   like the parent session id. `enterProcess` removes both before anything
   else runs, so a nested `putnami` never passes its parent's channel on, and
   the tools and jobs the core starts never inherit one.
3. **A Unix socket in a private directory.** The socket lives in a fresh
   scratch directory (`scratch.NewIn`) restricted to the user
   (`ownerperm.Restrict`, mode 0700, a protected access list on Windows). A
   CLI killed before the stop leaves the directory behind, and the next
   channel's sweep removes it. macOS holds at most 104 bytes in a socket path,
   its terminating NUL included, so a path over 103 bytes moves the directory
   to `/tmp`. Windows 10 version 1803 and later serve the same `AF_UNIX`
   socket, under the same 103-byte limit but with no `/tmp` fallback: a
   temporary directory whose socket path is too long means no channel and a
   warning. When no directory works, the CLI prints a warning and starts the
   process without the variables; the client's `NoHostError` says why they
   can be absent.
4. **Every request carries the token.** The token is a top-level `token`
   member of each request, compared in constant time. A request with another
   token gets `-32001`, and the host closes the connection. A notification
   with another token gets no answer, as JSON-RPC 2.0 requires, and the host
   closes the connection too. The 0700 directory keeps other users out. A
   process of the same user can read the launched process's environment
   (`ps -E` on macOS, `/proc/<pid>/environ` on Linux), so the token does not
   keep the user's own processes out: it is defense in depth against one that
   finds the socket but not the environment.
5. **JSON-RPC 2.0 in line frames.** A frame is one JSON object and a newline,
   at most 32 MiB without the newline, in both directions. The host reads
   with a bounded reader, answers a longer frame with `-32600` and closes the
   connection. Params are strict: an unknown member is `-32602`. The wire
   contract is a protocol package, `go.putnami.dev/protocol/extension/hostrpc`,
   so the host and every client share one set of types and limits.
6. **Three methods.**
   - `host.hello` returns the protocol version and the sorted method list.
   - `collaboration.call` routes through `collaborationEnvelope`, the code
     `putnami <contract> <operation>` runs, and returns the envelope that
     command prints under `--output=json`, byte for byte. Where the command
     prints no envelope, the method refuses with the reason: `-32602` for a
     word that is not a collaboration contract, which the CLI does not route
     to that command; `-32002` outside a workspace, without
     `options.collaboration`, or when an extension command owns the contract
     word. An operation the contract does not define, such as `finalize` on
     `proposals`, is answered like any other: an envelope whose outcome is
     `invalid`.
   - `verification.report` returns what `engine.VerifyCachedVerificationReport`
     returns, as base64 bytes, and its refusal as `-32003` with the engine's
     message. Outside a workspace it answers `-32002`.
7. **One call at a time.** The core state these methods read is not built for
   concurrent use, so one mutex serializes calls across every connection of
   the channel. Stopping the channel cancels the context of the call in
   flight, answers it `-32002`, closes every connection and removes the
   directory.

## Alternatives

- **Run the CLI again**, as `$PUTNAMI_CLI_EXECUTABLE <contract> <operation>
  --input-file - --output=json`. Rejected by D-014: a second process loads
  the workspace again, can disagree with its parent and costs a process start
  per call.
- **A channel for every job.** Rejected: a planned job's cache key would have
  to cover every answer the job could ask for.
- **Loopback TCP.** Rejected: any local user can connect to a port, so the
  token would be the only guard. A socket in a 0700 directory adds the file
  system's.
- **Authenticate the connection once.** Rejected: a token on each request
  keeps the server stateless per request, and costs 64 bytes.
- **Concurrent calls.** Rejected until a method needs them and the state it
  reads is safe to share.
- **Return the envelope as a decoded object.** Rejected: the raw bytes are
  what guarantees the answer equals the CLI's output.

## Consequences

- An extension command can call `host.hello`, `collaboration.call` and
  `verification.report`. A new method is additive: `host.hello` lists it, and
  the protocol version does not move. A change to an existing method's params
  or result moves the version.
- A non-interactive subcommand that needs a core answer cannot use the
  channel. It keeps its current path until a decision covers planned jobs.
- The SDK gains `scratch.NewIn`, which creates a scratch directory under
  another root than the temporary directory and sweeps that root.
- This decision deletes nothing.
