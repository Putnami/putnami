// Package resultv2 writes the CLI's result envelope from an extension binary.
//
// An interactive extension subcommand — `putnami features list`, the shape a
// command group takes when its subcommand declares `interactive: true` — is the
// one deliberate scheduler bypass: the subprocess inherits the terminal's
// stdout, and nothing wraps what it prints. The bytes it writes ARE the
// command's machine-readable output, so they must be the bytes the CLI would
// have written for a built-in structured command of the same name. A consumer
// that binds to `putnami features list --output=json` cannot be asked to notice
// that the command moved into an extension.
//
// The reference is App.runStructuredCommand and writeStructuredFailure in
// tooling/cli/internal/cli/commands.go. This package reproduces that behavior,
// and the one thing in it that is easy to get wrong is the OUTPUT MODE:
//
//   - The CLI rewrites the mode to "jsonl" for the HANDLER (commands.go:44-46),
//     because a structured command's handlers all emit through the jsonl branch.
//   - It then writes the envelope with the ORIGINALLY selected mode
//     (parsed.Global.Output — commands.go:96, :103), so --output=json is
//     MarshalIndent and --output=jsonl is one compact line.
//
// A helper that normalized the mode before writing would emit a compact line
// under --output=json and be byte-different from every built-in command. Emit
// therefore takes the mode as a VALUE, resolved by the caller, and never
// re-derives it: an extension subcommand may not declare an `output` flag (it is
// a reserved global), so the resolved mode arrives in the job context as
// params["output"], and reading it is the caller's job, not this package's.
package resultv2

import (
	"fmt"
	"io"

	protocolcli "go.putnami.dev/protocol/cli"
)

// Path is the envelope's `command` field: "<command> <subcommand>", or just
// "<command>" when the group is invoked without one. It is the same join the
// CLI does before building the envelope (commands.go:61-63, :88-91), kept here
// so an extension never spells the separator itself.
func Path(command, subcommand string) string {
	if subcommand == "" {
		return command
	}
	return command + " " + subcommand
}

// Envelope builds the result envelope for one subcommand outcome. A nil error
// is a success envelope carrying data; a non-nil error derives status, exit
// code, error code, message and suggested-next from the error's classification,
// exactly as it does for a built-in command — including data, which a failure
// may still carry (a contract-check report explaining WHY the command failed).
func Envelope(command, subcommand string, data any, err error) protocolcli.ResultV2 {
	return protocolcli.NewResultV2(Path(command, subcommand), data, err)
}

// Emit writes the outcome of one interactive subcommand and returns the process
// exit code. It is the whole rendering decision in one call:
//
//   - structured mode (json, jsonl): the envelope goes to stdout in the mode
//     given — indented for json, one compact line for jsonl — and a failure also
//     prints "putnami: <error>" on stderr, where it cannot corrupt the document
//     on stdout;
//   - any other mode: nothing is written to stdout, because the caller's own
//     human rendering owns it. A failure prints "putnami: <error>" and, when the
//     error carries one, the "  Try: <next>" suggestion that travels in
//     error.next for machine consumers.
//
// The exit code always agrees with the envelope's, whether or not the envelope
// was written, so a human-mode run and a machine-mode run of the same failure
// exit the same way.
func Emit(stdout, stderr io.Writer, mode protocolcli.OutputMode, command, subcommand string, data any, err error) int {
	if !mode.IsStructured() {
		if err == nil {
			return protocolcli.ExitSuccess
		}
		fmt.Fprintf(stderr, "putnami: %v\n", err)
		if next := protocolcli.SuggestedNext(err); next != "" {
			fmt.Fprintf(stderr, "  Try: %s\n", next)
		}
		return protocolcli.ExitCodeForError(err)
	}
	code, _ := protocolcli.WriteResultV2(stdout, mode, Envelope(command, subcommand, data, err))
	if err != nil {
		fmt.Fprintf(stderr, "putnami: %v\n", err)
	}
	return code
}
