package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"

	registryproto "go.putnami.dev/protocol/registry"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/output"
)

const (
	// workspaceBootstrappedEnv marks the workspace a parent putnami has already
	// bootstrapped, so nested invocations (an install job that shells out to
	// putnami) skip the redundant first-use check. It rides the standard
	// PUTNAMI_* subprocess inheritance, mirroring artifactsEnsuredEnv.
	workspaceBootstrappedEnv = "PUTNAMI_WORKSPACE_BOOTSTRAPPED"

	// noAutoInstallEnv opts out of first-use auto-install entirely — for CI
	// pipelines that prefer to run `putnami install` as an explicit, isolated
	// step rather than have it triggered by the first command.
	noAutoInstallEnv = "PUTNAMI_NO_AUTO_INSTALL"
)

// bootstrappedWorkspaces dedupes EnsureWorkspaceBootstrap to once per workspace
// per process.
var bootstrappedWorkspaces sync.Map // wsRoot -> struct{}

// installRunner is the install action invoked when the workspace is stale. It is
// a package var so tests can substitute a spy without running a real install.
var installRunner = Install

// BootstrapOptions controls the first-use workspace bootstrap.
type BootstrapOptions struct {
	// Command is the command name being run, used to skip install-family
	// commands (which do this work themselves) and for messaging.
	Command string
	// Subcommand is the second command word, when any. It identifies the
	// credential seam (`cloud registry-token`), whose stdout is a bearer and
	// nothing else.
	Subcommand string
	// Output is the requested output mode. Structured modes keep stdout reserved
	// for the original command by routing implicit install chatter to stderr.
	Output string
	// Plan/DryRun mark read-only intent: the workspace is never mutated, but a
	// note is emitted when it is out of date so --plan stays honest.
	Plan   bool
	DryRun bool
	// RunJob runs the implicit install's workspace-level jobs through the CLI
	// engine. It travels on the options rather than being resolved here because
	// internal/engine imports this package (see WorkspaceJobRunner); the caller in
	// internal/cli owns the binding.
	RunJob WorkspaceJobRunner
	// Display carries the invoking command's resolved human-output preferences.
	Display LifecycleDisplay
	// BeforeRepositoryCode is the implicit install's
	// LifecycleEnv.BeforeRepositoryCode: on a hosted run it runs after the
	// workspace-fetch and before the install's first repository code.
	BeforeRepositoryCode func(context.Context) error
}

// EnsureWorkspaceBootstrap runs `putnami install` once when a fresh checkout, a
// new worktree, or a CI runner has no current install state, so the first
// arbitrary command (`putnami build`, `putnami cloud ...`, ...) finds
// workspace-local state already restored — dependencies plus extension-local
// state such as Cloud's .putnami/cloud-link.json, .putnami/cache.json, and
// registry token-source recipes — without requiring an explicit `putnami
// install` first.
//
// It is the workspace-state counterpart to EnsureArtifacts, which materializes
// extension *binaries*. Properties:
//   - Marker-gated on a content hash of the workspace lock files (see
//     install_state.go): the warm path is a handful of stats (~0.015ms), and it
//     never re-installs when only mtimes moved.
//   - Best-effort: a failed install is logged and the command still runs (it
//     surfaces its own clear error if it truly needs installed state), so a
//     flaky network or missing token can't wedge an offline `putnami build`.
//   - Idempotent and re-entrancy safe: once per workspace per process, and
//     skipped when a parent putnami already bootstrapped this workspace.
//   - Inert under --plan/--dry-run (read-only) and when PUTNAMI_NO_AUTO_INSTALL
//     is set.
//
// It deliberately runs the full `putnami install` rather than a narrower step:
// for the common cases (worktree development, CI) the next command is a
// build/test that needs the full install anyway, and reusing install means an
// extension's existing workspace-install provider (e.g. Cloud's) restores its
// own state with no new protocol.
func EnsureWorkspaceBootstrap(ctx context.Context, wsRoot string, cfg *wsproto.Config, opts BootstrapOptions) {
	if cfg == nil || wsRoot == "" {
		return
	}
	if os.Getenv(noAutoInstallEnv) != "" {
		return
	}
	if isInstallCommand(opts.Command) {
		return // install/deps/upgrade restore workspace state themselves
	}
	if isCredentialSeam(opts.Command, opts.Subcommand) {
		return // stdout is the bearer; the parent command owns workspace state
	}
	if os.Getenv(workspaceBootstrappedEnv) == wsRoot {
		return // a parent putnami already bootstrapped this workspace
	}

	if opts.Plan || opts.DryRun {
		// Read-only intent: never install, but keep --plan honest about state.
		if stale, _ := evalInstall(wsRoot); stale {
			iox.Fprintln(os.Stderr, "  Note: workspace install is out of date; run `putnami install` (skipped under --plan/--dry-run).")
		}
		return
	}

	if _, loaded := bootstrappedWorkspaces.LoadOrStore(wsRoot, struct{}{}); loaded {
		return
	}

	stale, repair := evalInstall(wsRoot)
	if !stale {
		if repair != nil {
			// Fast-path repair: lock mtimes moved with identical content (e.g. a
			// git pull). Rewrite the marker so later commands stat-match again.
			if err := writeInstallStateRaw(wsRoot, repair); err != nil {
				slog.Debug("refresh install state", "error", err)
			}
		}
		return
	}

	// Mark before installing so install jobs that re-invoke putnami skip this pass.
	_ = os.Setenv(workspaceBootstrappedEnv, wsRoot)

	if opts.Display.Verbose || opts.Display.Debug {
		iox.Fprintln(os.Stderr, "  Workspace is out of date — running `putnami install` (set PUTNAMI_NO_AUTO_INSTALL=1 to skip)...")
	}
	if err := runBootstrapInstall(ctx, wsRoot, cfg, opts); err != nil {
		// Best-effort: surface the failure but let the original command proceed.
		// It fails with its own clear error if it genuinely needs installed state.
		slog.Debug("workspace bootstrap install failed", "error", err)
		iox.Fprintf(os.Stderr, "  Workspace install did not complete: %v\n  Continuing; run `putnami install` manually if the command fails.\n", err)
		return
	}
	// A successful Install wrote the marker (see Install in install.go).
}

// runBootstrapInstall runs the implicit install with its human output routed to
// the stream the invoking command can spare.
//
// Under a structured output mode (--output=json|jsonl|cloud-logging) stdout IS
// the machine contract, so the install's progress goes to stderr instead. Until
// slice A5a that was done by reassigning the process-global os.Stdout
// under a mutex for the duration of the call — the only global stdout
// replacement in the tree, and one that silently caught every goroutine and
// every unrelated writer alive at the time. The install family now takes the
// writer (LifecycleEnv.Out), so the redirection is scoped to exactly this call.
func runBootstrapInstall(ctx context.Context, wsRoot string, cfg *wsproto.Config, opts BootstrapOptions) error {
	ctx = shared.WithImplicitInstall(ctx)
	out := io.Writer(os.Stdout)
	if output.StructuredOutput(opts.Output) {
		out = os.Stderr
	}
	// NoCache stays false even when the triggering command typed --no-cache; see
	// LifecycleEnv.NoCache.
	return installRunner(ctx, wsRoot, cfg, nil, LifecycleEnv{
		Out: out, RunJob: opts.RunJob, Display: opts.Display, BeforeRepositoryCode: opts.BeforeRepositoryCode,
	})
}

// isCredentialSeam reports whether the command is `cloud registry-token`, the
// seam every private archive download shells out to for a bearer. Its stdout
// is the bearer and nothing else: an implicit install here would print its
// setup lines ahead of the token (the caller then treats the value as no token
// and the download goes anonymous), and it would run mid-`upgrade`, right after
// the extension phase rewrote the lock. The invoking command owns workspace
// state; the credential child never restores it.
func isCredentialSeam(cmd, sub string) bool {
	return cmd == registryproto.SeamParentCommand && sub == registryproto.SeamSubcommand
}

// isInstallCommand reports whether the command already restores workspace state
// on its own, so the bootstrap guard must not run before it (it would be
// redundant, and for install itself recursive).
func isInstallCommand(cmd string) bool {
	switch cmd {
	case "install", "deps", "workspace-install", "deps-upgrade", "upgrade":
		return true
	}
	return false
}
