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
	"go.putnami.dev/tooling/cli/internal/jobs"
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
	})
	for _, failure := range failures {
		iox.Fprintf(os.Stderr, "putnami: %v\n", failure.Err)
	}
	return reporters
}

func finishSessionReporting(reporters *sessionreporter.Runs, sessionID string, finalizeErr error) {
	if finalizeErr != nil {
		iox.Fprintln(os.Stderr, "putnami: finalized session could not be persisted")
	}
	for _, failure := range reporters.Finish() {
		iox.Fprintf(os.Stderr, "putnami: %s incomplete: %v; replay with putnami sessions replay --session %s\n", failure.Capability.Activity, failure.Err, sessionID)
	}
}

func reportingResolver(ctx context.Context, capability sessionreporter.Capability, ws *workspace.Workspace, discovered *extension.DiscoveryResult) sessionreporter.Resolve {
	return func(setupCtx context.Context) (sessionreporter.LaunchSpec, error) {
		var selected []*extension.ExtensionDescription
		for _, ext := range discovered.Extensions {
			if ext != nil && ext.Name == capability.Provider(ctx) {
				selected = append(selected, ext)
			}
		}
		provider, err := extension.ResolveReservedProvider(selected, capability.Name)
		if err != nil {
			return sessionreporter.LaunchSpec{}, err
		}
		if provider == nil || provider.ExtensionName != capability.Provider(ctx) {
			return sessionreporter.LaunchSpec{}, fmt.Errorf("selected reporter is not available")
		}
		for _, ext := range discovered.Extensions {
			if ext.Name != provider.ExtensionName {
				continue
			}
			job := ext.Jobs[capability.Name]
			if job == nil || strings.TrimSpace(job.Command) == "" || len(job.PipelineSteps) != 1 {
				return sessionreporter.LaunchSpec{}, fmt.Errorf("reporter must resolve to one executable command")
			}
			launch, err := jobs.PrepareProviderLaunch(setupCtx, ws.Root, ext, capability.Name)
			if err != nil {
				return sessionreporter.LaunchSpec{}, err
			}
			// The reporter runs the extension's code. An extension installed
			// from the artifact store is registry code; any other one is
			// repository code.
			if !extension.InArtifactStore(ws.Root, ext) {
				runcredential.MarkRepositoryCodeStarted("session reporter " + ext.Name)
			}
			return sessionreporter.LaunchSpec(launch), nil
		}
		return sessionreporter.LaunchSpec{}, fmt.Errorf("selected reporter was not discovered")
	}
}

// ReplaySession sends only retained artifacts, for every selected reporting
// capability whose evidence is not delivered. Discovery and each selected
// reporter runtime use their normal mechanisms; no workload DAG or hook runs.
// Normal reporter runtime preparation may build its provider executable.
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
	})
}
