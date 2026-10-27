package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchainpins"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/useragent"
)

// installWritesWorkspace reports whether an install may write the workspace's
// own files: putnami.lock.json and the assistant content (AGENTS.md,
// .mcp.json, agent workflows). A hosted install runs from the committed files
// and writes none of them (ADR 0055): its toolchain pins stay in memory
// (toolchainpins.Options.Hosted), it refreshes no lock metadata, even a CLI
// pin that a CLI of another version left without its protocol version, and it
// generates no assistant content. The extensions phase refuses a lock change
// on its own (extensions.ExtensionsInstallWithOptions).
func installWritesWorkspace() bool {
	return !runcredential.Hosted()
}

// toolchainPinOptions are the options of the toolchain pins phase for the
// command ctx belongs to: the first-use install is implicit, a hosted run
// writes no lock and asks offline, and so does a run whose dependencies are
// offline (extensionproto.OfflineDependenciesEnv).
func toolchainPinOptions(ctx context.Context) toolchainpins.Options {
	hosted := !installWritesWorkspace()
	return toolchainpins.Options{
		Implicit: shared.IsImplicitInstall(ctx),
		Hosted:   hosted,
		Offline:  hosted || os.Getenv(extensionproto.OfflineDependenciesEnv) == "1",
	}
}

// resolveToolchainPins runs the toolchain pins phase for the command ctx
// belongs to: env.pinToolchains when it is set. Otherwise it runs
// toolchainpins.Resolve, then installs the pinned releases the machine lacks
// (installToolchainPins).
func resolveToolchainPins(ctx context.Context, wsRoot string, env LifecycleEnv) (toolchainpins.Result, error) {
	opts := toolchainPinOptions(ctx)
	if env.pinToolchains != nil {
		return env.pinToolchains(ctx, wsRoot, opts)
	}
	pins, err := toolchainpins.Resolve(ctx, wsRoot, opts)
	if err != nil {
		return pins, err
	}
	return pins, installToolchainPins(ctx, wsRoot, pins, opts, env)
}

// installToolchainPins installs, in identity order, the pinned release of each
// identity an extension answered for, unless the machine already holds it
// (toolchain.Provision). A hosted run, or one whose dependencies are offline,
// installs nothing: it runs the toolchains the machine holds. It reports each
// release it installed to env.OnAction; with no OnAction the same line goes
// to env.Out. It stops at the first failure.
func installToolchainPins(ctx context.Context, wsRoot string, pins toolchainpins.Result, opts toolchainpins.Options, env LifecycleEnv) error {
	if opts.Hosted || opts.Offline {
		return nil
	}
	record := env.OnAction
	if record == nil {
		out := env.out()
		record = func(action LifecycleAction) { iox.Fprintf(out, "  ✓ %s\n", action.Description) }
	}
	for _, identity := range slices.Sorted(maps.Keys(pins.Answers)) {
		entry, pinned := pins.Toolchains[identity]
		if !pinned {
			continue
		}
		_, installed, err := toolchain.Provision(ctx, wsRoot, toolchain.ProvisionTarget{
			Identity:  identity,
			Entry:     entry,
			Toolchain: pins.Answers[identity].Toolchain,
		}, toolchain.ProvisionOptions{UserAgent: useragent.String()})
		if err != nil {
			return err
		}
		if installed {
			record(LifecycleAction{Kind: "toolchain", Name: identity,
				Description: fmt.Sprintf("Installed %s %s under the Putnami home", identity, entry.Version)})
		}
	}
	return nil
}

// runWorkspaceInstallers is Install's installer phase: the toolchain pins
// phase records the pins the extensions answer, then the workspace installers
// run with the toolchains the lock pins.
func runWorkspaceInstallers(ctx context.Context, wsRoot string, cfg *wsproto.Config, phase installPhaseIO) error {
	phase.display.phase("toolchain pins")
	pins, err := resolveToolchainPins(ctx, wsRoot, phase.env)
	if err != nil {
		return err
	}
	if pins.Written {
		phase.record(LifecycleAction{Kind: "lock", Name: lockfile.LockFilename, Description: "Toolchain pins recorded"})
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
// (runcredential.Hosted) runs the fetch steps between the two
// (LifecycleEnv.fetchBeforeRepositoryCode, ADR 0055): the store extensions'
// workspace-fetch, env.BeforeRepositoryCode, then the path extensions'
// workspace-fetch. The first two hand out the run credential, and a hosted run
// hands its credential to no process that starts after repository code, which
// a path extension's fetch and an onInstall hook are. The workspace installers
// then run without a second fetch (LifecycleEnv.fetched).
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

	if err := refuseHostedAgentArtifactPins(ctx, wsRoot); err != nil {
		return err
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
		// Write the host hooks the opted-in extensions declare, and remove the
		// ones that no longer apply (ADR 0081). Only an explicit install does:
		// the bootstrap must not change a committed file either.
		if !shared.IsImplicitInstall(ctx) {
			for _, change := range agentctx.RegisterHostHooks(wsRoot, updatedCfg, os.Stderr) {
				recordAction(LifecycleAction{Kind: "assistant-context", Name: change.File, Description: change.Description()})
			}
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
	// lock pins — install never resolves a version and never rewrites the lock
	// for it, so a clone reproduces the exact files the lock describes.
	optedIn, err := agentWorkflowsOptedIn(wsRoot, cfg)
	if err != nil {
		return fmt.Errorf("agent workflows install: %w", err)
	}
	if optedIn {
		if err := installAgentWorkflows(ctx, wsRoot, cfg, phaseIO); err != nil {
			return err
		}
	}

	if err := runWorkspaceInstallers(ctx, wsRoot, cfg, phaseIO); err != nil {
		return err
	}

	// An explicit install ends by refreshing the lock's metadata: it drops the
	// agent artifact pins no declaration uses and stamps the CLI pin's output
	// protocol. An implicit bootstrap restores a checkout from its committed
	// lock and does neither.
	if !shared.IsImplicitInstall(ctx) {
		display.phase("lock metadata")
		if err := dropAgentArtifactPins(wsRoot, recordAction); err != nil {
			return err
		}
		changed, err := refreshLockMetadataGuarded(ctx, wsRoot, env.CLIVersion, out, display, trackedBefore)
		if err != nil {
			return err
		}
		if changed {
			recordAction(LifecycleAction{Kind: "lock", Name: "putnami.lock.json", Description: "Workspace lock metadata refreshed"})
		}
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

// agentWorkflowsOptedIn reports whether the workspace opts into agent content.
// Install skips the agent phase entirely when it does not, which is what keeps
// `agentArtifacts` an opt-in.
func agentWorkflowsOptedIn(wsRoot string, cfg *wsproto.Config) (bool, error) {
	declared, err := agentctx.DeclaredAgentArtifacts(wsRoot, cfg)
	return len(declared) > 0, err
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
		phase.record(agentWorkflowLifecycleAction(action))
	}
	return nil
}

// agentWorkflowLifecycleAction is the record of one contribution whose files
// changed, shared by every command that materializes agent content.
func agentWorkflowLifecycleAction(action agentctx.AgentWorkflowInstallAction) LifecycleAction {
	return LifecycleAction{
		Kind: "agent-workflow", Name: action.Name,
		Description: fmt.Sprintf("Agent workflow %s@%s materialized", action.Name, action.Version),
	}
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

// refuseHostedAgentArtifactPins fails a hosted explicit install, before any
// phase runs, when the committed lock still holds a pin under agentArtifacts.
// No declaration can use such a pin, so an install drops it
// (dropAgentArtifactPins); a hosted install writes no lock (ADR 0055), so it
// fails instead, as it does for an extension the lock does not pin
// (extensions.refuseHostedLockChange).
func refuseHostedAgentArtifactPins(ctx context.Context, wsRoot string) error {
	if installWritesWorkspace() || shared.IsImplicitInstall(ctx) {
		return nil
	}
	pins, err := agentctx.AgentArtifactPins(wsRoot)
	if err != nil || len(pins) == 0 {
		return err
	}
	return fmt.Errorf("%s: the committed %s pins agent artifacts no declaration can use: %s; a hosted run writes no lock, so run `putnami install` without %s and commit the lock",
		runcredential.Flag, lockfile.LockFilename, strings.Join(pins, ", "), runcredential.Flag)
}

// dropAgentArtifactPins is the explicit install's step that drops every
// agentArtifacts lock pin: an earlier release pinned agent artifacts there,
// and no declaration can use such a pin now. A hosted install was refused
// before its first phase (refuseHostedAgentArtifactPins) and writes nothing
// here. The implicit bootstrap restores the committed lock and never runs it.
func dropAgentArtifactPins(wsRoot string, record func(LifecycleAction)) error {
	if !installWritesWorkspace() {
		return nil
	}
	dropped, err := agentctx.DropAgentArtifactPins(wsRoot)
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		record(LifecycleAction{Kind: "lock", Name: lockfile.LockFilename,
			Description: "Agent artifact pins no declaration uses dropped: " + strings.Join(dropped, ", ")})
	}
	return nil
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
