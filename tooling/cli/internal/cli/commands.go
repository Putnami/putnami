package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// runStructuredCommand dispatches a structured command (e.g. "extensions
// install", "projects list") by looking it up in the command registry and
// invoking its handler with a uniform CommandEnv. The handler writes its own
// success output in the selected output mode: under --output=json|jsonl one
// version-2 result envelope (shared.WriteResult), or, for `report`, the
// contract document it forwards. A handler that fails writes nothing to stdout:
// this function writes the failure (writeStructuredFailure), and
// exitCodeForError maps the error to the process exit code via the cmderr
// sentinel taxonomy.
func (a *App) runStructuredCommand(ctx context.Context, parsed *ParsedArgs, cfg *wsproto.Config, wsRoot string) int {
	cmd := parsed.Commands[0]
	sub := parsed.Subcommand
	outputFormat := parsed.Global.Output

	if code, rejected := rejectStructuredIfUnsupported(outputFormat, cmd, sub); rejected {
		return code
	}

	c, _ := lookupCommand(cmd)
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
	if err := c.run(env); err != nil {
		return writeStructuredFailure(os.Stdout, os.Stderr, protocolcli.OutputMode(outputFormat),
			commandmeta.CanonicalPath(cmd, sub), err, env.envelopeWarnings())
	}
	return ExitSuccess
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
// stderr. The envelope carries warnings, the workspace load warnings the
// command reported before it failed, in workspaceWarnings.
//
// This is the single failure envelope of EVERY structured command, which is why
// converting it (B1b) is what took the last versionless documents off the
// CLI's stdout. One behavior follows from the v2 contract rather than from this
// function: a signal-classified failure now reports status "aborted" with exit
// code 130 instead of being folded into a plain failure.
func writeStructuredFailure(stdout, stderr io.Writer, mode protocolcli.OutputMode, cmdPath string, err error,
	warnings []protocolcli.WorkspaceWarning,
) int {
	if !mode.IsStructured() {
		printCommandError(stderr, err)
		return exitCodeForError(err)
	}
	result := protocolcli.NewResultV2(cmdPath, protocolcli.ResultData(err), err)
	result.WorkspaceWarnings = warnings
	code, _ := protocolcli.WriteResultV2(stdout, mode, result)
	iox.Fprintf(stderr, "putnami: %v\n", err)
	return code
}

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
// install/update. Under a structured output mode it uses the quiet form so the
// human-readable context summary does not corrupt the result on stdout.
func regenerateAIContext(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
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
