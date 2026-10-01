package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// runStructuredCommand dispatches a structured command (e.g. "extensions
// install", "projects list") by looking it up in the command registry and
// invoking its handler with a uniform CommandEnv. The handler returns an
// error, which exitCodeForError maps to the process exit code via the cmderr
// sentinel taxonomy.
func (a *App) runStructuredCommand(ctx context.Context, parsed *ParsedArgs, cfg *wsproto.Config, wsRoot string) int {
	cmd := parsed.Commands[0]
	sub := parsed.Subcommand
	outputFormat := parsed.Global.Output

	if code, rejected := rejectStructuredIfUnsupported(outputFormat, cmd, sub); rejected {
		return code
	}

	c, _ := lookupCommand(cmd)

	// Structured commands emit a single result object, so --output=json and
	// --output=jsonl are equivalent for them (only streaming job commands
	// distinguish the two). Route both through the handlers' existing "jsonl"
	// branch so no per-handler change is needed. A raw-output command is the
	// exception a second time: its stdout is the recorded document itself, and
	// the two modes differ there in FRAMING — pretty bytes under json, one
	// compact line under jsonl — so it must see the mode that was asked.
	if protocolcli.OutputMode(outputFormat).IsStructured() && !c.rawOutput(sub) {
		outputFormat = string(protocolcli.OutputJSONL)
	}

	env := &CommandEnv{
		App:          a,
		Ctx:          ctx,
		Cfg:          cfg,
		WsRoot:       wsRoot,
		Sub:          sub,
		Args:         parsed.RawJobArgs,
		OutputFormat: outputFormat,
		Global:       parsed.Global,
	}

	if !protocolcli.OutputMode(parsed.Global.Output).IsStructured() {
		if err := c.run(env); err != nil {
			cmdPath := cmd
			if sub != "" {
				cmdPath = cmd + " " + sub
			}
			return writeStructuredFailure(os.Stdout, os.Stderr, protocolcli.OutputMode(parsed.Global.Output), cmdPath, err)
		}
		return ExitSuccess
	}

	// A raw-output command writes its own contract document (registerRawCommand),
	// so its stdout is forwarded untouched instead of being captured and wrapped:
	// wrapping a document a consumer binds to directly would make every consumer
	// unwrap it first. Its FAILURES still take the shared envelope below.
	if c.rawOutput(sub) {
		if err := c.run(env); err != nil {
			return writeStructuredFailure(os.Stdout, os.Stderr, protocolcli.OutputMode(parsed.Global.Output),
				commandmeta.CanonicalPath(cmd, sub), err)
		}
		return ExitSuccess
	}

	// Several long-lived commands still produce their data through JSONL
	// helpers. Collect those documents here, where every structured command
	// already passes, then put the original payload under one ResultV2.data.
	// Commands that already produce a v2 envelope are passed through unchanged
	// so this migration does not create a nested envelope.
	captured, err := iox.CaptureStdout(func() error { return c.run(env) })
	cmdPath := cmd
	if sub != "" {
		cmdPath = cmd + " " + sub
	}
	if err != nil {
		return writeStructuredFailure(os.Stdout, os.Stderr, protocolcli.OutputMode(parsed.Global.Output), cmdPath, err)
	}
	if envelope, ok := capturedResultEnvelope(captured); ok {
		code, _ := protocolcli.WriteResultV2(os.Stdout, protocolcli.OutputMode(parsed.Global.Output), envelope)
		return code
	}
	data, err := structuredPayload(captured)
	if err != nil {
		return writeStructuredFailure(os.Stdout, os.Stderr, protocolcli.OutputMode(parsed.Global.Output), cmdPath, err)
	}
	code, writeErr := protocolcli.WriteResultV2(os.Stdout, protocolcli.OutputMode(parsed.Global.Output),
		protocolcli.NewResultV2(cmdPath, data, nil))
	if writeErr != nil {
		return code
	}
	return code
}

// capturedResultEnvelope reports whether captured is one complete v2 result
// envelope emitted by a command that had already adopted the shared writer.
// It deliberately requires the complete discriminating set: a payload may
// happen to contain protocolVersion but must remain payload data unless it is
// actually an envelope.
func capturedResultEnvelope(captured string) (protocolcli.ResultV2, bool) {
	var result protocolcli.ResultV2
	decoder := json.NewDecoder(strings.NewReader(captured))
	if err := decoder.Decode(&result); err != nil {
		return protocolcli.ResultV2{}, false
	}
	if err := ensureJSONEOF(decoder); err != nil ||
		result.ProtocolVersion != protocolcli.ResultProtocolVersion ||
		result.Command == "" || result.Status == "" {
		return protocolcli.ResultV2{}, false
	}
	return result, true
}

// structuredPayload decodes the legacy JSONL success output of a structured
// command. One document retains its former object shape; multiple records are
// preserved, in order, as an array. An empty former stream becomes data: [] so
// list commands can distinguish it from commands with no payload.
func structuredPayload(captured string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(captured))
	var documents []any
	for {
		var document any
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("structured command emitted invalid JSON: %w", err)
		}
		documents = append(documents, document)
	}
	if len(documents) == 1 {
		return documents[0], nil
	}
	return documents, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON documents")
		}
		return err
	}
	return nil
}

// writeStructuredFailure renders a structured command's failure and returns the
// process exit code. In a machine-readable output mode (--output=json|jsonl) it
// writes a version-2 failure envelope to stdout — including error.next when the
// error carries a protocolcli.WithNext suggestion — so an agent receives the
// actionable next command programmatically, and echoes the plain message to
// stderr for a watching human. Structured commands treat json ≡ jsonl (they
// emit a single object), so the envelope uses the selected structured mode just
// as their success output does. In human mode it prints "putnami: <message>"
// plus, when a suggestion is present, an indented "  Try: <next>" line to
// stderr.
//
// This is the single failure envelope of EVERY structured command, which is why
// converting it (B1b) is what took the last versionless documents off the
// CLI's stdout. One behavior follows from the v2 contract rather than from this
// function: a signal-classified failure now reports status "aborted" with exit
// code 130 instead of being folded into a plain failure.
//
// A reportedFailure is the one exception: its command already wrote the
// failure on stdout as its own document, so this writes nothing and returns the
// exit code alone.
func writeStructuredFailure(stdout, stderr io.Writer, mode protocolcli.OutputMode, cmdPath string, err error) int {
	if errors.As(err, new(reportedFailure)) {
		return exitCodeForError(err)
	}
	if !mode.IsStructured() {
		printCommandError(stderr, err)
		return exitCodeForError(err)
	}
	code, _ := protocolcli.WriteResultV2(stdout, mode, protocolcli.NewResultV2(cmdPath, shared.ResultData(err), err))
	iox.Fprintf(stderr, "putnami: %v\n", err)
	return code
}

// reportedFailure is a command failure the command already reported on stdout
// in its own machine document, such as the not-verified verdict of `tree
// verify`. The dispatcher exits with the code of the wrapped error and writes
// nothing else: no "putnami:" line on stderr, no result envelope on stdout.
type reportedFailure struct{ err error }

func (f reportedFailure) Error() string { return f.err.Error() }

func (f reportedFailure) Unwrap() error { return f.err }

// printCommandError writes a structured command's failure to w in human mode:
// the standard "putnami: <message>" line, followed — when the error carries a
// suggestion via protocolcli.WithNext — by an indented "  Try: <next>" line so
// the user sees the exact next command to run. In a machine-readable mode the
// same suggestion travels in the result envelope's error.next field instead
// (see writeStructuredFailure), so this human line stays out of the machine
// stream.
func printCommandError(w io.Writer, err error) {
	iox.Fprintf(w, "putnami: %v\n", err)
	if next := protocolcli.SuggestedNext(err); next != "" {
		iox.Fprintf(w, "  Try: %s\n", next)
	}
}

// regenerateAIContext refreshes the shared AI context after an extensions
// install/update. When the command is streaming JSONL on stdout it uses the
// quiet form so the human-readable context summary does not corrupt the
// machine-readable stream.
func regenerateAIContext(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	if outputFormat == "jsonl" {
		return agentctx.ContextGenerateQuiet(wsRoot, cfg)
	}
	return agentctx.ContextGenerate(wsRoot, cfg, nil)
}

// resolveBinDir returns the .putnami/bin/ directory, using ~/.putnami/bin/ if
// --global is present.
func resolveBinDir(wsRoot string, args []string) string {
	if suppliedFlag(args, "--global", "-g") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".putnami", "bin")
		}
	}
	return filepath.Join(wsRoot, ".putnami", "bin")
}

// parseUpgradeFlags binds `putnami upgrade`'s arguments through the command
// catalog: which flags exist, which take a value, and which values --channel
// accepts all come from commandmeta rather than from the second flag table this
// replaces (flagTakesValue).
//
// Version resolution is canonical: --version, then the first positional
// argument.
func parseUpgradeFlags(args []string) (lifecycle.UpgradeFlags, error) {
	parsed, err := parseCatalogCommandFlags("upgrade", args)
	if err != nil {
		return lifecycle.UpgradeFlags{}, err
	}

	version := parsed.value("--version")
	if version == "" && len(parsed.positionals) > 0 {
		version = parsed.positionals[0]
	}

	return lifecycle.UpgradeFlags{
		CLI:        parsed.has("--cli"),
		Extensions: parsed.has("--extensions"),
		Deps:       parsed.has("--deps"),
		Channel:    parsed.value("--channel"),
		Release:    parsed.value("--release"),
		Namespace:  parsed.value("--namespace"),
		Version:    version,
		DryRun:     parsed.has("--dry-run"),
		FromSource: parsed.has("--from-source"),
	}, nil
}

// rejectStructuredIfUnsupported returns (ExitUsage, true) when the user asked
// for structured output (--output=json or --output=jsonl, including the --json
// shorthand) on a subcommand that does not emit it, so the flag fails loudly
// instead of being silently ignored. Callers spread the result:
// `if code, rejected := ...; rejected { return code }`.
//
// Capability is Command.StructuredOutput in the command catalog, and the bare
// spelling of a command resolves through Command.DefaultSub — so a root whose
// handler treats "" like a subcommand answers exactly as that subcommand does.
// Before slice A1b this was a second hand-maintained table plus a hand-keyed
// switch, and they disagreed: `putnami scopes list --json` worked while
// `putnami scopes --json` exited 2 (ADR 0001).
func rejectStructuredIfUnsupported(outputFormat, cmd, sub string) (int, bool) {
	if !protocolcli.OutputMode(outputFormat).IsStructured() {
		return ExitSuccess, false
	}
	fullCmd := commandmeta.CanonicalPath(cmd, sub)
	if commandmeta.EmitsStructuredOutput(fullCmd) {
		return ExitSuccess, false
	}
	iox.Fprintf(os.Stderr, "putnami: --output=%s is not supported by %q\n", outputFormat, fullCmd)
	iox.Fprintf(os.Stderr, "  structured-output commands: %s\n", strings.Join(commandmeta.StructuredOutputPaths(), ", "))
	return ExitUsage, true
}
