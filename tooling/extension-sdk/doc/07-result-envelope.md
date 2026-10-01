# Result Envelopes

The `resultv2` package writes the CLI's result envelope from an extension binary, so an interactive extension subcommand produces the bytes a built-in structured command produces.

## Why an extension writes the envelope itself

An extension subcommand declared `interactive: true` is the CLI's one deliberate scheduler bypass. The subprocess inherits the terminal's stdin/stdout, no renderer runs, and nothing wraps what the process prints. Whatever it writes to stdout **is** the command's machine-readable output.

That makes the envelope a contract. A consumer bound to `putnami features list --output=json` must not be able to tell that the command moved from the CLI into an extension. The reference implementation is `App.runStructuredCommand` in `tooling/cli/internal/cli/commands.go`; this package reproduces it.

## Usage

```go
import (
    protocolcli "go.putnami.dev/protocol/cli"
    pctx "go.putnami.dev/sdk/extension/context"
    "go.putnami.dev/sdk/extension/resultv2"
)

func runFeaturesList(ctx *pctx.Context, args []string) int {
    // The mode the user selected. An extension subcommand may not declare an
    // `output` flag — it is a reserved global — so the CLI forwards the
    // resolved mode as a job param.
    mode := protocolcli.OutputMode(ctx.Params.String("output"))

    report, err := listFeatures(ctx, args)
    if err == nil && !mode.IsStructured() {
        printHuman(report) // structured modes print nothing but the envelope
    }
    return resultv2.Emit(os.Stdout, os.Stderr, mode, "features", "list", report, err)
}
```

| Function | Returns |
|----------|---------|
| `Path(command, subcommand)` | `"features list"`, or `"features"` when the group is invoked bare |
| `Envelope(command, subcommand, data, err)` | the `protocolcli.ResultV2` value, for a caller that wants to inspect or embed it |
| `Emit(stdout, stderr, mode, command, subcommand, data, err)` | the process exit code, after writing whatever the mode calls for |

## What `Emit` writes

| Mode | Success | Failure |
|------|---------|---------|
| `json` | indented envelope on stdout | indented envelope on stdout, `putnami: <error>` on stderr |
| `jsonl` | one compact envelope line on stdout | one compact line on stdout, `putnami: <error>` on stderr |
| anything else | nothing — your human rendering owns stdout | `putnami: <error>` and, when the error carries one, `  Try: <next>` on stderr |

The exit code always agrees with the envelope's `exitCode`, whether or not the envelope was written, so the same failure exits the same way in every mode.

Failures go through the same envelope: `status`, `exitCode`, `error.code` and `error.next` are derived from the error's classification (`protocolcli.Usagef`, `Authf`, `APIf`, `Classify`, `WithNext`). A failure may carry `data` too — a check report that explains *why* the command failed belongs in the envelope, not only in the message.

## The mode is a value, and it is the selected one

`Emit` takes the mode as a parameter and never re-derives it from the environment. That is deliberate, and it is the one thing in this contract that is easy to get wrong.

The CLI rewrites the output mode to `jsonl` **for the handler** (`commands.go:44-46`), because every structured command's handler emits through the same jsonl branch. It then writes the envelope with the mode the user actually selected (`commands.go:96`, `:103`). So:

- `--output=json` → `json.MarshalIndent`, a pretty document;
- `--output=jsonl` → `json.Marshal`, one compact line.

A helper that normalized the mode before writing would emit the compact line under `--output=json` and be byte-different from every built-in command. `TestEmit_JSONStaysIndented` in this package pins that.
