package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/sessionreporter"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// startSessionReporting starts every selected reporting capability as its own
// declared subscriber of the session's one event stream; the session stays the
// stream's only producer. Each capability reports at most one diagnostic here
// and never changes the run.
func startSessionReporting(ctx context.Context, req *Request, ws *workspace.Workspace, discovered *extension.DiscoveryResult, session *workspace_state.Session) *sessionreporter.Runs {
	if session == nil {
		for _, capability := range sessionreporter.Selected(ctx) {
			if !req.WorkspaceLifecycle {
				iox.Fprintf(os.Stderr, "putnami: %s unavailable: session could not be recorded\n", capability.Label)
			}
		}
		return nil
	}
	reporters, failures := sessionreporter.StartSelected(ctx, session.Events(), session.ID, func(capability sessionreporter.Capability) sessionreporter.Resolve {
		return reportingResolver(ctx, capability, ws, discovered)
	}, req.HostedReporters.holders())
	for _, failure := range failures {
		printReportingFailure(failure)
	}
	return reporters
}

func printReportingFailure(failure sessionreporter.Failure) {
	iox.Fprintf(os.Stderr, "putnami: %v\n", failure.Err)
}

// HostedReporters are the reporters of a hosted invocation, started once,
// before its first repository code, so that each can hold the run credential
// (sessionreporter.Holders): a reporter starts at its first frame, after the
// hooks (the session reporter's plan.json while the first jobs start, the log
// reporter's first events chunk after them), and a hosted run hands its
// credential to no process started after repository code. The first-use
// bootstrap starts them before the implicit install's first repository code,
// beside the remote cache (HostedRemoteCache), and the run that follows adopts
// them (Request.HostedReporters). The zero value is ready to use. Whoever creates
// it closes it, after the last run that adopts from it.
type HostedReporters struct {
	held sessionreporter.Holders
}

// Start starts the reporters the context selects, once, for a hosted request
// that records a session, and prints each one that does not hold the run
// credential. It never fails the request: a reporter that did not start is
// started by the session's run, which custody then refuses. Without a run
// credential, or for a request that records no session (a workspace
// lifecycle job, --plan, a dry run), it starts nothing. A workspace that does
// not load starts nothing here: the run reports it after the hooks.
func (h *HostedReporters) Start(ctx context.Context, req *Request) {
	if h == nil || !runcredential.Hosted() || req.WorkspaceLifecycle || req.Global.Plan || req.previewsOnly() {
		return
	}
	selected := sessionreporter.Selected(ctx)
	if len(selected) == 0 {
		return
	}
	ws, err := workspace.Load(req.WorkspaceRoot)
	if err != nil {
		return
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		projectPaths[i] = p.Path
	}
	discovered, err := extension.DiscoverExtensionsDetailed(req.WorkspaceRoot, req.Config, projectPaths)
	if err != nil {
		return
	}
	for _, failure := range h.held.Start(ctx, selected, func(capability sessionreporter.Capability) sessionreporter.Resolve {
		return reportingResolver(ctx, capability, ws, discovered)
	}) {
		printReportingFailure(failure)
	}
}

// Close closes every reporter Start started that no run adopted. A nil
// HostedReporters does nothing.
func (h *HostedReporters) Close() {
	if h == nil {
		return
	}
	h.held.Close()
}

// holders is what a session's run adopts from, or nil.
func (h *HostedReporters) holders() *sessionreporter.Holders {
	if h == nil {
		return nil
	}
	return &h.held
}

// finishSessionReporting ends the session's reporting. A terminal job run
// that may detach its reporting (Request.DetachedReporting,
// sessionreporter.Detaches) stops each worker at its durable
// checkpoint and hands what remains to a detached replay, then starts one for
// each of the newest other sessions a replay still owes (ADR 0115). Any other
// run drains before it returns.
func finishSessionReporting(ctx context.Context, req *Request, reporters *sessionreporter.Runs, session *workspace_state.Session, finalizeErr error) {
	if finalizeErr != nil {
		iox.Fprintln(os.Stderr, "putnami: finalized session could not be persisted")
	}
	if req.DetachedReporting && sessionreporter.Detaches(!reporters.Empty(), finalizeErr == nil) {
		launch := req.startReplay
		if launch == nil {
			launch = sessionreporter.StartDetachedReplay
		}
		if reporters.Stop() {
			if err := launch(ctx, req.WorkspaceRoot, session.ID); err != nil {
				iox.Fprintf(os.Stderr, "putnami: reporting incomplete: its replay did not start; replay with putnami sessions replay --session %s\n", session.ID)
			}
		}
		// Earlier sessions that no replay is delivering any more are named
		// once, then handed to a replay again.
		pending := sessionreporter.PendingSessions(ctx, filepath.Dir(session.Dir()), session.ID, flushedPendingSessions)
		if len(pending) > 0 {
			iox.Fprintf(os.Stderr, "putnami: %d earlier session(s) not delivered by their reporters; replaying them in the background (putnami sessions inspect %s)\n", len(pending), pending[0])
		}
		for _, id := range pending {
			_ = launch(ctx, req.WorkspaceRoot, id)
		}
		return
	}
	for _, failure := range reporters.Finish() {
		iox.Fprintf(os.Stderr, "putnami: %s incomplete: %v; replay with putnami sessions replay --session %s\n", failure.Capability.Activity, failure.Err, session.ID)
	}
}

// flushedPendingSessions is how many other sessions a detaching run hands to
// a detached replay: the newest ones whose delivery is incomplete.
const flushedPendingSessions = 3

// replayLauncher starts the delivery of one retained session's reporting.
type replayLauncher func(ctx context.Context, wsRoot, sessionID string) error

func reportingResolver(ctx context.Context, capability sessionreporter.Capability, ws *workspace.Workspace, discovered *extension.DiscoveryResult) sessionreporter.Resolve {
	return func(setupCtx context.Context) (sessionreporter.LaunchSpec, error) {
		var selected []*extension.ExtensionDescription
		for _, ext := range discovered.Extensions {
			if ext != nil && ext.Name == capability.Provider(ctx) {
				selected = append(selected, ext)
			}
		}
		if len(selected) == 0 {
			return sessionreporter.LaunchSpec{}, sessionreporter.Cause(fmt.Sprintf("%s names %s, which this workspace does not load", capability.SelectorEnv, capability.Provider(ctx)))
		}
		provider, err := extension.ResolveReservedProvider(selected, capability.Name)
		if err != nil {
			return sessionreporter.LaunchSpec{}, err
		}
		if provider == nil || provider.ExtensionName != capability.Provider(ctx) {
			return sessionreporter.LaunchSpec{}, sessionreporter.Cause(fmt.Sprintf("%s serves no %s command in this workspace", capability.Provider(ctx), capability.Name))
		}
		for _, ext := range discovered.Extensions {
			if ext.Name != provider.ExtensionName {
				continue
			}
			job := ext.Jobs[capability.Name]
			if job == nil || strings.TrimSpace(job.Command) == "" || len(job.PipelineSteps) != 1 {
				return sessionreporter.LaunchSpec{}, fmt.Errorf("reporter must resolve to one executable command")
			}
			launch, err := scheduler.PrepareProviderLaunch(setupCtx, ws.Root, ext, capability.Name)
			if err != nil {
				return sessionreporter.LaunchSpec{}, err
			}
			// The reporter runs the extension's code. An extension installed
			// from the artifact store is registry code, and a runtime the
			// source build prepared is the engine's own build; any other one
			// is repository code.
			if extension.StartsRepositoryCode(ws.Root, ext) {
				runcredential.MarkRepositoryCodeStarted("session reporter " + ext.Name)
			}
			return sessionreporter.LaunchSpec{
				Command: launch.Command, Args: launch.Args, Dir: launch.Dir, Env: launch.Env,
				Runtime: ext.RuntimeExecutable,
			}, nil
		}
		return sessionreporter.LaunchSpec{}, fmt.Errorf("selected reporter was not discovered")
	}
}

// ReplaySession sends only retained artifacts, for every selected reporting
// capability whose evidence is not delivered. Discovery and each selected
// reporter runtime use their normal mechanisms; no workload DAG or hook runs.
// Normal reporter runtime preparation may build its provider executable. A
// hosted replay hands its reporters the run credential as a run does, and
// prints each one that does not hold it.
func (e *Engine) ReplaySession(ctx context.Context, wsRoot string, cfg *wsproto.Config, sessionID string) error {
	ctx = sessionreporter.Capture(ctx)
	if sessionID == "" || sessionID == "latest" || filepath.Base(sessionID) != sessionID {
		return fmt.Errorf("sessions replay requires an exact session id")
	}
	identity := protocolcli.NewSessionReportingChunk(sessionID, "session.json", 0, 0, nil, true)
	if err := identity.Validate(); err != nil {
		return fmt.Errorf("invalid session id")
	}
	store := workspace_state.NewSessionStore(wsRoot)
	dir := filepath.Join(store.Root(), sessionID)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("recorded session directory not found")
	}
	ws, discovered, code := loadWorkspaceAndExtensions(&Request{WorkspaceRoot: wsRoot, Config: cfg})
	if code != ExitSuccess {
		return fmt.Errorf("could not discover reporter extension")
	}
	return sessionreporter.ReplaySelected(ctx, dir, sessionID, func(capability sessionreporter.Capability) sessionreporter.Resolve {
		return reportingResolver(ctx, capability, ws, discovered)
	}, printReportingFailure)
}
