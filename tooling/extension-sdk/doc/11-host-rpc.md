# Host RPC

The `hostrpc` package lets an interactive extension subcommand call the
`putnami` process that started it. The call goes to that running process over
a Unix socket. It does not start `putnami` again.

## What an extension can call

| Method | Client call | Returns |
|---|---|---|
| `host.hello` | `Hello(ctx)` | The protocol version and the methods the host serves. |
| `collaboration.call` | `CallCollaboration(ctx, contract, operation, input)` | The envelope `putnami <contract> <operation> --output=json` prints for the same input, byte for byte. |
| `verification.report` | `VerificationReport(ctx, ref)` | The bytes of a verification report a successful cached test run produced, once the host's cache corroborates them. |

That is the whole list. The host answers any other method with
method-not-found (`-32601`). `Hello` tells you what a given host serves, so
check it before you call a method a later protocol version adds.

`collaboration.call` routes contract operations through the same code as the
CLI command, so the answer matches what the user sees. An operation that fails
at the provider is not an error: the envelope reports it. `proposals finalize`
is a command, not an operation, and the host refuses it.

## Who receives a channel

The CLI opens one channel for each launch of a command-group subcommand
declared `interactive: true`, and closes it when the process exits. The
process receives two variables:

| Variable | Value |
|---|---|
| `PUTNAMI_HOST_RPC` | The socket path. |
| `PUTNAMI_HOST_RPC_TOKEN` | The launch's token: 32 random bytes, hex-encoded. |

These processes receive no channel:

- a planned job, such as a build job or a subcommand that is not interactive.
  Its result is cached, and a call its cache key does not cover must not
  decide it;
- a provider tool the CLI calls;
- every process of a hosted run (`--credential-fd`);
- a process that `putnami` did not start. A nested `putnami` removes the
  variables it inherited, so its own children never reach their grandparent.

## Usage

```go
import (
    "context"
    "encoding/json"
    "errors"

    "go.putnami.dev/sdk/extension/hostrpc"
)

func findTasks(ctx context.Context) (json.RawMessage, error) {
    client, err := hostrpc.Dial()
    if err != nil {
        var none *hostrpc.NoHostError
        if errors.As(err, &none) {
            // putnami opened no channel: name what the user should run instead.
        }
        return nil, err
    }
    defer client.Close()
    return client.CallCollaboration(ctx, "tasks", "find", json.RawMessage(`{}`))
}
```

`Dial` reads both variables. When one is missing it returns a `*NoHostError`
that names the missing variables and why `putnami` sets none for this process.
When the CLI could not open the channel, it printed a warning on stderr and
started the process without the variables, so the extension gets the same
error.

`Connect(socket, token)` connects to a channel you name yourself, for example
in a test that starts its own host.

## Errors

| Error | Meaning | The client stays usable |
|---|---|---|
| `*hostrpc.NoHostError` | The process received no channel. | No client exists. |
| `*Error` of the protocol package `go.putnami.dev/protocol/extension/hostrpc` | The host refused the call. `Code` says why, for example `-32002` when this launch cannot serve the method, or `-32003` when the cache does not corroborate a report. | Yes. |
| Any other error | The connection broke, the answer was not one the client can trust (another id, a frame over 32 MiB, not JSON), or the context ended before the answer. | No. Every later call returns the same error. Dial again. |

## Concurrency

A `Client` is safe for concurrent use. The host serves one call at a time,
across every connection of the launch, so a second call waits for the first.
When the subcommand's process exits, the CLI closes the channel and cancels
the call in flight.

The wire contract, its frames and its error codes are in
[`protocols/extension/hostrpc`](../../../protocols/extension/hostrpc/README.md).
