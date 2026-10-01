package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// installWritesWorkspace reports whether an install may write the workspace's
// own files: putnami.lock.json and the assistant content (AGENTS.md,
// .mcp.json, agent workflows). A hosted install runs from the committed files
// and writes none of them (ADR 0055): it pins no toolchain, refreshes no lock
// metadata, even a CLI pin that a CLI of another version left without its
// protocol version, and generates no assistant content. The extensions phase
// refuses a lock change on its own (extensions.ExtensionsInstallWithOptions).
func installWritesWorkspace() bool {
	return !runcredential.Hosted()
}

// fillImplicitToolchainPins is the implicit install's lock step, and the step
// `projects create` takes before it installs a missing go command. It is a seam
// for tests.
var fillImplicitToolchainPins = versioncmd.FillMissingToolchainPins

// pinExplicitToolchains is the explicit install's lock step before the
// workspace installers. It is a seam for tests.
var pinExplicitToolchains = versioncmd.PinDeclaredToolchains

// recordMissingToolchainPins runs the implicit install's lock step and records
// the lock action when it pinned a toolchain.
func recordMissingToolchainPins(ctx context.Context, wsRoot string, record func(LifecycleAction)) error {
	if !installWritesWorkspace() {
		return nil
	}
	changed, err := fillImplicitToolchainPins(ctx, wsRoot)
	if err != nil {
		return fmt.Errorf("pin missing toolchains: %w", err)
	}
	if changed {
		record(LifecycleAction{Kind: "lock", Name: "putnami.lock.json", Description: "Missing toolchain pins recorded"})
	}
	return nil
}

// pinToolchainsBeforeInstallers pins the toolchains go.work and
// package.json#packageManager declare before the workspace installers run:
// they install the release the lock pins, and the tasks run only that
// release. An explicit install pins the declared release where the lock pins
// none or another one. The implicit bootstrap restores the committed lock and
// only fills a missing pin.
func pinToolchainsBeforeInstallers(ctx context.Context, wsRoot string, record func(LifecycleAction)) error {
	if !installWritesWorkspace() {
		return nil
	}
	if shared.IsImplicitInstall(ctx) {
		return recordMissingToolchainPins(ctx, wsRoot, record)
	}
	changed, err := pinExplicitToolchains(ctx, wsRoot)
	if err != nil {
		return fmt.Errorf("pin declared toolchains: %w", err)
	}
	if changed {
		record(LifecycleAction{Kind: "lock", Name: "putnami.lock.json", Description: "Declared toolchain pins recorded"})
	}
	return nil
}

// runWorkspaceInstallers is Install's installer phase: it pins the declared
// toolchains, then runs the workspace installers, which install the pinned
// releases.
func runWorkspaceInstallers(ctx context.Context, wsRoot string, cfg *wsproto.Config, phase installPhaseIO) error {
	phase.display.phase("toolchain pins")
	if err := pinToolchainsBeforeInstallers(ctx, wsRoot, phase.record); err != nil {
		return err
	}
	if phase.env.Display.Verbose || phase.env.Display.Debug {
		iox.Fprintln(phase.env.out(), "\n  Running workspace installers...")
	}
	phase.display.clear()
	if err := DepsInstall(ctx, wsRoot, cfg, "", "", phase.env); err != nil {
		return fmt.Errorf("deps install: %w", err)
	}
	return nil
}

// runDeferredInstallHooks runs the onInstall hooks a hosted install deferred
// until after its workspace-fetch. It is a seam for tests.
var runDeferredInstallHooks = extensions.RunExtensionInstallHooksTo

// installExtensions is Install's extensions phase: it installs the extension
// artifacts and runs their onInstall hooks. A hosted install
// (runcredential.Hosted) runs every workspace-fetch, then
// env.BeforeRepositoryCode, between the two (ADR 0055): the fetch receives the
// run credential, and a hosted run hands its credential to no process that
// starts after repository code, which an onInstall hook is. The workspace
// installers then run without a second fetch (LifecycleEnv.fetched).
func installExtensions(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, phase *installPhaseIO) error {
	if phase.env.Display.Verbose || phase.env.Display.Debug {
		iox.Fprintln(phase.env.out(), "\n  Installing extensions...")
	}
	phase.display.phase("extensions")
	extensionOut, extensionDetails := phase.phaseWriter()
	hosted := runcredential.Hosted()
	if err := extensions.ExtensionsInstallWithOptions(ctx, wsRoot, cfg, args, extensions.InstallOptions{
		Out:               extensionOut,
		DeferInstallHooks: hosted,
		OnAction: func(action extensions.InstallAction) {
			if action.Status == extensions.InstallStatusFailed {
				phase.display.clear()
			}
			if lifecycleAction, ok := artifactLifecycleAction(action); ok {
				phase.record(lifecycleAction)
			}
		},
	}); err != nil {
		phase.flush(extensionDetails)
		return fmt.Errorf("extensions install: %w", err)
	}
	if !hosted {
		return nil
	}
	phase.display.clear()
	if err := phase.env.fetchBeforeRepositoryCode(ctx, wsRoot, cfg, "", ""); err != nil {
		return fmt.Errorf("deps install: %w", err)
	}
	phase.env.fetched = true
	if err := runDeferredInstallHooks(ctx, wsRoot, cfg, args, extensionOut); err != nil {
		phase.flush(extensionDetails)
		return fmt.Errorf("extensions install: %w", err)
	}
	return nil
}

// Install runs a combined install: extensions install + install hooks + deps install.
// This is the shorthand for ensuring everything is set up after cloning or
// updating a workspace.
//
// Every human progress line goes to env.Out, never to os.Stdout directly. That
// is what lets the first-use bootstrap run this whole sequence with its output on
// stderr under --output=json|jsonl (slice A5a); before it, the bootstrap
// reassigned the process-global os.Stdout for the duration of the call.
func Install(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, env LifecycleEnv) (retErr error) {
	out := env.out()
	display := newInstallDisplay(env)
	actions := make([]LifecycleAction, 0, 8)
	outerAction := env.OnAction
	recordAction := func(action LifecycleAction) { actions = append(actions, action) }
	env.OnAction = recordAction
	defer func() {
		display.clear()
		if outerAction != nil {
			for _, action := range actions {
				outerAction(action)
			}
			return
		}
		display.finish(actions)
	}()
	// An explicit install answers for the committed files it rewrites
	// (installRewriteGuard); the snapshot is taken before any phase runs.
	trackedBefore := installRewriteSnapshot(ctx, wsRoot)
	detailed := env.Display.Verbose || env.Display.Debug
	phaseWriter := func() (io.Writer, *bytes.Buffer) {
		if detailed {
			return out, nil
		}
		buffer := &bytes.Buffer{}
		return buffer, buffer
	}
	flushFailureDetails := func(buffer *bytes.Buffer) {
		if buffer == nil || buffer.Len() == 0 {
			return
		}
		display.clear()
		iox.Fprint(out, buffer.String())
	}

	// `install` is the combined lifecycle — extensions, templates, deps, AI
	// context, lock refresh — every step of which assumes it is setting THIS
	// workspace up. Materialization flags belong to the narrow command
	// that owns their guarantees; honoring them here would materialize foreign
	// artifacts and then regenerate the AI context and refresh the lock from
	// them anyway. The CLI's flag validator already rejects them for this path;
	// this closes the same hole for in-process callers.
	if extensions.ExtensionsInstallMaterializes(args) {
		return cmderr.Usagef(
			"`putnami install` cannot materialize for another platform or destination; use `putnami extensions install --platform <os>/<arch> [--dest <dir>]`")
	}

	phaseIO := installPhaseIO{env: env, display: display, phaseWriter: phaseWriter, flush: flushFailureDetails, record: recordAction}
	if err := installExtensions(ctx, wsRoot, cfg, args, &phaseIO); err != nil {
		return err
	}

	// Regenerate shared AI context after extensions install. A hosted install
	// writes no assistant content (installWritesWorkspace).
	if installWritesWorkspace() {
		display.phase("assistant context")
		updatedCfg := wsproto.Load(wsRoot)
		contextOut, contextDetails := phaseWriter()
		contextResult, err := agentctx.ContextGenerateWithResult(wsRoot, updatedCfg, contextOut)
		if err != nil {
			flushFailureDetails(contextDetails)
			return fmt.Errorf("generate AI context: %w", err)
		}
		if contextResult.GuidanceChanged {
			recordAction(LifecycleAction{Kind: "assistant-context", Name: "AGENTS.md", Description: "Assistant guidance updated"})
		}
		// Register the putnami MCP server so the next agent session in this
		// workspace reaches it with no manual step (ADR 0040). The first-use
		// bootstrap is not that moment: it restores a checkout on behalf of an
		// unrelated command, and must not change a committed file as a side effect.
		if !shared.IsImplicitInstall(ctx) && agentctx.RegisterMCPServer(wsRoot, os.Stderr) {
			recordAction(LifecycleAction{Kind: "assistant-context", Name: ".mcp.json", Description: "Putnami MCP server registered in .mcp.json"})
		}
	}

	if len(cfg.Templates) > 0 {
		if env.Display.Verbose || env.Display.Debug {
			iox.Fprintln(out, "\n  Installing templates...")
		}
		display.phase("templates")
		templateOut, templateDetails := phaseWriter()
		if err := extensions.TemplatesInstallWithOptions(ctx, wsRoot, cfg, args, extensions.InstallOptions{
			Out: templateOut,
			OnAction: func(action extensions.InstallAction) {
				if action.Status == extensions.InstallStatusFailed {
					display.clear()
				}
				if lifecycleAction, ok := artifactLifecycleAction(action); ok {
					recordAction(lifecycleAction)
				}
			},
		}); err != nil {
			flushFailureDetails(templateDetails)
			return fmt.Errorf("templates install: %w", err)
		}
	}

	// Agent content is materialized from the extension release the COMMITTED
	// lock pins, or from the npm package the package manager installs —
	// install never resolves a version and never rewrites the lock for it, so a
	// clone reproduces the exact files the locks describe.
	agentPhase, err := agentWorkflowInstallPhase(wsRoot, cfg)
	if err != nil {
		return fmt.Errorf("agent workflows install: %w", err)
	}
	if agentPhase == agentPhaseBeforeInstallers {
		if err := installAgentWorkflows(ctx, wsRoot, cfg, phaseIO); err != nil {
			return err
		}
	}

	if err := runWorkspaceInstallers(ctx, wsRoot, cfg, phaseIO); err != nil {
		return err
	}
	if agentPhase == agentPhaseAfterInstallers {
		if err := installAgentWorkflows(ctx, wsRoot, cfg, phaseIO); err != nil {
			return err
		}
	}

	// The lock is the machine preflight contract as well as the artifact lock:
	// resolve exact Go/Bun releases and their vendor-published archive
	// digests after installers have succeeded. This is a deliberate v3 write;
	// ordinary v2 lock updates remain v2 until install (or pin) derives the new
	// vocabulary.
	// An implicit bootstrap restores a checkout from its committed lock. It must
	// not upgrade that contract as a side effect of an unrelated command, so it
	// only pins a declared toolchain the lock has no pin for yet: without that
	// pin, every command that needs the toolchain refuses to run. It runs again
	// here because the installers can write the declaration a pin derives from.
	if !shared.IsImplicitInstall(ctx) {
		display.phase("lock metadata")
		changed, err := refreshLockMetadataGuarded(ctx, wsRoot, env.CLIVersion, out, display, trackedBefore)
		if err != nil {
			return err
		}
		if changed {
			recordAction(LifecycleAction{Kind: "lock", Name: "putnami.lock.json", Description: "Workspace lock metadata refreshed"})
		}
	} else if err := recordMissingToolchainPins(ctx, wsRoot, recordAction); err != nil {
		return err
	}

	// Record the post-install workspace lock state so the first-use bootstrap
	// guard (EnsureWorkspaceBootstrap) treats the workspace as current and does
	// not re-install on the next command. Best-effort: a write failure only
	// costs a redundant bootstrap later, never correctness.
	if err := writeInstallState(wsRoot); err != nil {
		slog.Debug("write install state", "error", err)
	}

	return nil
}

// agentWorkflowPhase is where Install runs its agent phase, if anywhere.
type agentWorkflowPhase int

const (
	// agentPhaseNone: the workspace opts into no agent content.
	agentPhaseNone agentWorkflowPhase = iota
	// agentPhaseBeforeInstallers: every opted-in extension's content is read
	// from a release the extensions phase installed, or from a path.
	agentPhaseBeforeInstallers
	// agentPhaseAfterInstallers: an opted-in extension is an npm package. Its
	// content is read from the package the package manager installs, so the
	// phase waits for the workspace installers: before them, node_modules can
	// still hold the release the next command no longer loads.
	agentPhaseAfterInstallers
)

// agentWorkflowInstallPhase places Install's agent phase. A workspace that opts
// into none skips it entirely, which is what keeps `agentArtifacts` an opt-in.
func agentWorkflowInstallPhase(wsRoot string, cfg *wsproto.Config) (agentWorkflowPhase, error) {
	declared, err := agentctx.DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil || len(declared) == 0 {
		return agentPhaseNone, err
	}
	if agentctx.AgentContentFollowsPackageManager(wsRoot, cfg) {
		return agentPhaseAfterInstallers, nil
	}
	return agentPhaseBeforeInstallers, nil
}

// installPhaseIO is the progress and summary plumbing one phase of Install
// reports through.
type installPhaseIO struct {
	env         LifecycleEnv
	display     *installDisplay
	phaseWriter func() (io.Writer, *bytes.Buffer)
	flush       func(*bytes.Buffer)
	record      func(LifecycleAction)
}

// installAgentWorkflows is Install's agent phase: it materializes the content
// of every opted-in extension and records one action per contribution whose
// files changed.
func installAgentWorkflows(ctx context.Context, wsRoot string, cfg *wsproto.Config, phase installPhaseIO) error {
	if !installWritesWorkspace() {
		return nil
	}
	if phase.env.Display.Verbose || phase.env.Display.Debug {
		iox.Fprintln(phase.env.out(), "\n  Installing agent workflows...")
	}
	phase.display.phase("agent workflows")
	workflowOut, workflowDetails := phase.phaseWriter()
	workflowActions, err := agentctx.InstallAgentWorkflowsWithResult(ctx, wsRoot, cfg, workflowOut)
	if err != nil {
		phase.flush(workflowDetails)
		return fmt.Errorf("agent workflows install: %w", err)
	}
	for _, action := range workflowActions {
		phase.record(LifecycleAction{
			Kind: "agent-workflow", Name: action.Name,
			Description: fmt.Sprintf("Agent workflow %s@%s materialized", action.Name, action.Version),
		})
	}
	return nil
}

func artifactLifecycleAction(action extensions.InstallAction) (LifecycleAction, bool) {
	var verb string
	switch action.Status {
	case extensions.InstallStatusInstalled:
		verb = "installed"
	case extensions.InstallStatusUpdated:
		verb = "updated"
	case extensions.InstallStatusRestored:
		verb = "restored from cache"
	default:
		return LifecycleAction{}, false
	}
	kind := "Extension"
	if action.Kind == extensions.InstallKindTemplate {
		kind = "Template"
	}
	name := action.Name
	if action.Version != "" {
		name += "@" + action.Version
	}
	return LifecycleAction{
		Kind: string(action.Kind), Name: action.Name,
		Description: fmt.Sprintf("%s %s %s", kind, name, verb),
	}, true
}

// installRewriteSnapshot is the tracked-file state an explicit install is
// measured against; nil for the implicit bootstrap, which writes nothing and
// is not guarded.
func installRewriteSnapshot(ctx context.Context, wsRoot string) map[string]string {
	if shared.IsImplicitInstall(ctx) {
		return nil
	}
	before, _ := git.TrackedDirtyDigests(wsRoot)
	return before
}

// refreshLockMetadataGuarded is the last phase of an explicit install: the
// lock refresh, then the rewrite guard over everything the install did. The
// guard's failure is the phase's failure, so `Install` has one error path.
func refreshLockMetadataGuarded(ctx context.Context, wsRoot, cliVersion string, out io.Writer, display *installDisplay, trackedBefore map[string]string) (bool, error) {
	changed := false
	if installWritesWorkspace() {
		var err error
		changed, err = versioncmd.RefreshLockMetadataWithResult(ctx, wsRoot, cliVersion)
		if err != nil {
			return false, fmt.Errorf("refresh lock metadata: %w", err)
		}
	}
	trackedAfter, _ := git.TrackedDirtyDigests(wsRoot)
	rewritten := git.RewrittenTrackedFiles(trackedBefore, trackedAfter)
	if len(rewritten) > 0 {
		display.clear()
	}
	installRewriteGuard(out, rewritten)
	return changed, nil
}

// installRewriteGuard names the committed files an explicit `putnami
// install` rewrote, as a warning on the human stream. An install that
// rewrites nothing prints nothing.
//
// It is a warning everywhere, CI included, and not a failure. The hosted
// runner rewrites committed files around the install BY DESIGN: it replaces
// `bun.lock`'s registry URLs with its sealed dependency broker before the
// install and restores the original bytes when the run ends
// (ci-run-native.sh). The CLI cannot tell that rewrite from one it should
// refuse, and a hard failure here failed every hosted bootstrap on the first
// commit that carried it. The run's own record of what it planned
// from is the `selection:impacted` trace's `uncommittedFiles`; the place a
// hard guard can live is the runner, which knows which rewrites are its own.
func installRewriteGuard(out io.Writer, rewritten []string) {
	if len(rewritten) == 0 {
		return
	}
	iox.Fprintf(out, "  warning: putnami install rewrote %d committed file(s): %s — commit or revert them before relying on --impacted\n",
		len(rewritten), strings.Join(rewritten, ", "))
}
