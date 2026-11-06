package cli

import (
	"context"
	"fmt"
	"os"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/progress"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CommandEnv is the standardized input passed to every structured command
// handler: handlers read what they need from a single value instead of each
// carrying a bespoke signature.
type CommandEnv struct {
	// App is the running CLI application.
	App *App
	// Ctx is the invocation context (cancellation, deadlines).
	Ctx context.Context
	// Cfg is the loaded, alias-resolved configuration.
	Cfg *wsproto.Config
	// WsRoot is the workspace root, or "" when no workspace was found.
	WsRoot string
	// Sub is the resolved subcommand (e.g. "install" for "extensions install").
	Sub string
	// Args are the remaining raw arguments after the command and subcommand.
	Args []string
	// OutputFormat is the --output value ("json", "jsonl", "cloud-logging", or
	// ""). A structured command writes its success result in this mode.
	OutputFormat string
	// Global holds the parsed global flags.
	Global GlobalFlags

	// warnings keeps the workspace load warnings the command reported.
	// WorkspaceWarnings creates it on first use.
	warnings *workspace.WarningReport
}

// WorkspaceWarnings returns the report a command that reads the workspace hands
// its load warnings to. The report writes them where warningWriter says and
// keeps them, so a structured command's result envelope carries them in
// workspaceWarnings, under --quiet too.
func (e *CommandEnv) WorkspaceWarnings() *workspace.WarningReport {
	if e.warnings == nil {
		e.warnings = workspace.NewWarningReport(warningWriter(e))
	}
	return e.warnings
}

// envelopeWarnings returns the reported workspace warnings in their result
// envelope shape, or nil when the command reported none.
func (e *CommandEnv) envelopeWarnings() []protocolcli.WorkspaceWarning {
	if e == nil {
		return nil
	}
	return e.warnings.Envelope()
}

// Progress returns a progress.Reporter that writes human-readable phase/step
// updates to stderr, already gated for this command's output mode. When a
// structured output mode is active ("json", "jsonl", or "cloud-logging"),
// the returned Reporter is inert, so a handler can report progress
// unconditionally without risking corruption of a machine stdout stream.
func (e *CommandEnv) Progress() *progress.Reporter {
	return progress.New(os.Stderr, protocolcli.OutputMode(e.OutputFormat).IsMachineReadable())
}

// requireWorkspace returns cmderr.ErrNoWorkspace when no workspace was found.
// Handlers do: if err := env.requireWorkspace(); err != nil { return err }.
// The error carries the exact recovery command via protocolcli.WithNext, so the
// dispatcher prints a "Try: putnami workspace init" line (human mode) and the
// result envelope's error.next field surfaces it to agents (structured mode);
// classification and message are unchanged, so errors.Is(ErrNoWorkspace) holds.
func (e *CommandEnv) requireWorkspace() error {
	if e.WsRoot == "" {
		return protocolcli.WithNext(cmderr.ErrNoWorkspace, "putnami workspace init")
	}
	return nil
}

// CommandFunc is the uniform entrypoint signature for a structured command.
// Workspace, config, and output handling all flow through CommandEnv, and the
// returned error is mapped to a process exit code by exitCodeForError, so
// handlers never deal with exit codes directly.
type CommandFunc func(env *CommandEnv) error

// command is a registered structured command group.
type command struct {
	name string
	run  CommandFunc
}

// commandRegistry binds each structured command NAME to its handler. It is a
// dispatch table, not a command table: the vocabulary — which names exist, what
// they do, their subcommands, flags and workspace requirements — is the command
// catalog (internal/commandmeta, ADR 0001 §1), and isStructuredCommand reads the
// catalog. This map exists only because a handler is a Go function value the
// catalog package cannot reference without importing this one.
//
// Slice A6a took the vocabulary role away, which is what dropped the
// pinned command-table count in structural_baseline_test.go to zero.
var commandRegistry = map[string]*command{}

// registerCommand binds a handler to a catalog command. Intended to be called
// from package init functions; both failure modes are programming errors and
// panic so they surface at startup rather than silently shadowing:
//
//   - a duplicate name would shadow an already-bound handler;
//   - a name the catalog does not declare as a structured root would be a
//     command with no help, no completion and no declared workspace
//     requirement — the drift class ADR 0001 §1 closes. Registering it here
//     instead of adding it to the catalog is how a second, hand-maintained
//     command vocabulary grows back.
func registerCommand(name string, run CommandFunc) {
	if _, exists := commandRegistry[name]; exists {
		panic(fmt.Sprintf("cli: duplicate command registration %q", name))
	}
	if !commandmeta.IsStructuredRoot(name) {
		panic(fmt.Sprintf("cli: %q is not a structured command in the catalog "+
			"(internal/commandmeta/catalog_data.go) — add it there first", name))
	}
	commandRegistry[name] = &command{name: name, run: run}
}

// lookupCommand returns the registered command for name, if any.
func lookupCommand(name string) (*command, bool) {
	c, ok := commandRegistry[name]
	return c, ok
}

// exitCodeForError maps a command error to the process exit code. The mapping
// is owned by go.putnami.dev/protocol/cli so every CLI surface classifies the
// same way: usage/no-match/validation → Usage (2), auth → 3, api → 4,
// unclassified → Failure (1), nil → Success.
func exitCodeForError(err error) int {
	return protocolcli.ExitCodeForError(err)
}

// usageErrorf builds an ErrUsage-classified error with a formatted message.
// The sentinel's own text is never appended, so usage hints read exactly as
// written while still matching errors.Is. Embed multi-line hints with
// "\n  ...".
func usageErrorf(format string, a ...any) error {
	return cmderr.Classify(fmt.Errorf(format, a...), cmderr.ErrUsage)
}
