package engine

import (
	"context"
	"errors"
	"os"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Workspace probe synchronization — the run's SNAPSHOT-FIRST load path.
//
// The invariant this phase exists to hold, stated as a cost:
//
//	unchanged workspace  → 0 extension processes, 0 runtime preparations
//	changed manifest     → exactly 1 process per provider that OWNS the change
//	ordinary source edit → 0 processes unless a provider declared that source
//	                       shape as workspace metadata; then exactly its owner runs
//
// Everything below is arranged around it. Providers are built LAZILY (the
// runtime is prepared inside the first Probe call, not before it), so a valid
// snapshot skips both the probe and the compile that would precede it. That is
// what keeps `putnami build` on an unchanged tree from paying for the
// TypeScript toolchain it does not need — and it is why this phase does not
// simply reuse the command-scoped runtime synchronization above it: workspace
// ownership is not command-scoped, but its COST must stay proportional to
// whether anything actually moved.
//
// The provider set comes from the FULL discovered extension set, never from the
// plan-narrowed one. `putnami ts build` narrows the plan to one extension; if it
// also narrowed the workspace view, the same tree would resolve to different
// projects depending on how the user spelled the command.

// synchronizeWorkspaceProbe resolves the provider view for this run.
//
// It returns an exit code rather than an error because it sits in the run's
// staged pipeline beside the other phases; ExitSuccess means "the run may
// continue", which includes the case where a probe failed but this command is
// one of the recovery commands allowed to survive it.
func synchronizeWorkspaceProbe(
	ctx context.Context,
	req *Request,
	ws *workspace.Workspace,
	discovered *extension.DiscoveryResult,
) int {
	bindings := workspaceProviderBindings(ctx, ws, discovered, req.preparation)
	if len(bindings) == 0 {
		return ExitSuccess
	}

	outcome, err := workspace.Synchronize(workspace.SyncRequest{
		Context: ctx,
		OnLockWait: func(message string) {
			if !req.Global.Quiet {
				iox.Fprintf(os.Stderr, "putnami: %s\n", message)
			}
		},
		Workspace: ws,
		Providers: bindings,
		Reason:    probeReasonFor(req),
		// `--plan` and a PREVIEW-ONLY `--dry-run` may probe in memory and must
		// never persist: a speculative view must not become the workspace's
		// recorded identity. The dry-run half goes through previewsOnly rather
		// than reading the flag, which is the rule every other "does this run
		// execute?" decision follows — an extension alias that forwards
		// --dry-run as a job param really does run over the real tree, and what
		// it recorded about that tree is not speculative.
		Policy: workspace.SnapshotWritePolicy{
			Plan:   req.Global.Plan,
			DryRun: req.previewsOnly(),
		},
	})
	if err != nil {
		return reportProbeFailure(req, err)
	}

	ws.AdoptProbeView(outcome.Merged)
	reportProbeDiagnostics(req, ws, outcome)
	return ExitSuccess
}

// PrepareWorkspaceGraph is the read-only adapter's minimal cold-workspace
// bootstrap. It prepares only provider runtimes and asks their workspace
// adapters for identity and dependency facts; it runs no lifecycle hooks,
// dependency installers, Cloud setup or context generation.
//
// Synchronize owns cross-process exclusion and snapshot-first rechecking, so a
// CLI command racing an MCP request performs one refresh and the loser adopts
// the recorded answer without starting another provider.
func PrepareWorkspaceGraph(
	ctx context.Context,
	ws *workspace.Workspace,
	discovered *extension.DiscoveryResult,
) (*workspace.SyncOutcome, error) {
	if ws == nil {
		return nil, errors.New("engine: cannot prepare the graph of a nil workspace")
	}
	outcome, err := workspace.Synchronize(workspace.SyncRequest{
		Context: ctx,
		OnLockWait: func(message string) {
			iox.Fprintf(os.Stderr, "putnami: %s\n", message)
		},
		Workspace: ws,
		Providers: workspaceProviderBindings(ctx, ws, discovered, nil),
		Reason:    wsproto.ProbeReasonLoad,
	})
	if err != nil {
		// A reader serves a graph the workspace refuses to plan over.
		return workspace.ReadableRefusal(err)
	}
	ws.AdoptProbeView(outcome.Merged)
	return outcome, nil
}

// probeReasonFor tells providers why they are being asked. It is advisory — a
// provider must answer the same facts regardless — but `plan` additionally
// documents in the exchange itself that this run's view is speculative and will
// not be persisted.
func probeReasonFor(req *Request) wsproto.ProbeReason {
	if req.Global.Plan || req.previewsOnly() {
		return wsproto.ProbeReasonPlan
	}
	return wsproto.ProbeReasonLoad
}

// workspaceProviderBindings builds one lazily-resolved provider per
// adapter-declaring extension.
//
// Runtime preparation is deferred into the provider's own Resolve hook. A
// workspace whose snapshot validates therefore never enters
// SynchronizeExtensionRuntimes at all — which matters because preparation for a
// local extension compiles it, and paying a compile to discover that nothing
// changed is precisely the cost the snapshot exists to avoid.
func workspaceProviderBindings(
	ctx context.Context,
	ws *workspace.Workspace,
	discovered *extension.DiscoveryResult,
	preparation *jobs.PreparationReport,
) []workspace.ProviderBinding {
	if discovered == nil {
		return nil
	}
	bindings := make([]workspace.ProviderBinding, 0, len(discovered.Extensions))
	for _, ext := range discovered.Extensions {
		if ext == nil || ext.Workspace == nil {
			continue
		}
		scope, ok := workspace.NewProviderScope(ext.Name, ext.Workspace)
		if !ok {
			continue
		}
		bindings = append(bindings, workspace.ProviderBinding{
			Scope:          scope,
			Implementation: jobs.ExtensionImplementationIdentity(ext),
			Provider: &workspace.ExecProbeProvider{
				Extension:         ext.Name,
				Dir:               ws.Root,
				Context:           ctx,
				Resolve:           runtimeResolverFor(ctx, ws, ext, preparation),
				FromArtifactStore: extension.InArtifactStore(ws.Root, ext),
			},
		})
	}
	sort.SliceStable(bindings, func(i, j int) bool {
		return bindings[i].Scope.Extension < bindings[j].Scope.Extension
	})
	return bindings
}

// runtimeResolverFor prepares one extension's runtime on first use and returns
// its executable. Preparation failure is returned verbatim: the runtime
// primitive already classifies it, and re-wrapping it here would lose the
// distinction between "this extension has no runtime" and "its prepare command
// failed" — the two failures with different remedies.
func runtimeResolverFor(
	ctx context.Context,
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
	preparation *jobs.PreparationReport,
) func() (string, error) {
	return func() (string, error) {
		if ext.RuntimeExecutable == "" {
			if err := jobs.SynchronizeExtensionRuntimes(
				ctx, ws, []*extension.ExtensionDescription{ext}, preparation,
			); err != nil {
				return "", err
			}
		}
		return ext.RuntimeExecutable, nil
	}
}

// reportProbeFailure applies the probe failure policy.
//
// A command that plans or executes over the project graph cannot run on a
// half-known workspace: a missing provider drops dependency edges, and a
// dropped edge is a wrong build rather than a slow one. The commands that can
// REPAIR the situation must stay reachable, or one broken extension makes a
// workspace unrecoverable — which is why a workspace lifecycle run (`install`,
// `deps install`, `projects sync`'s install pass) survives here regardless of
// what it happens to spell its job.
func reportProbeFailure(req *Request, err error) int {
	failure := &wsproto.ProbeFailure{}
	ok := errors.As(err, &failure)
	if !ok {
		iox.Fprintf(os.Stderr, "putnami: workspace probe: %v\n", err)
		return ExitError
	}
	if req.WorkspaceLifecycle || runIsRecoveryCommand(req) {
		if !req.Global.Quiet {
			iox.Fprintf(os.Stderr, "putnami: warning: workspace probe failed (%s); continuing so the workspace stays repairable: %s\n",
				failure.Kind, failure.Message)
		}
		return ExitSuccess
	}
	if guardErr := workspace.RequireProbe(primaryCommandPath(req), failure); guardErr == nil {
		return ExitSuccess
	}

	iox.Fprintf(os.Stderr, "putnami: workspace probe failed: %v\n", failure)
	for _, d := range failure.Diagnostics {
		iox.Fprintf(os.Stderr, "putnami:   %s\n", d.String())
	}
	if !workspace.RefusesAUsableGraph(failure) {
		// A refused graph states its own repair in the message above; the
		// recovery commands re-probe, they do not edit an import.
		iox.Fprintf(os.Stderr, "putnami: run `putnami projects sync` or `putnami install` to repair the workspace.\n")
	}
	return ExitError
}

func runIsRecoveryCommand(req *Request) bool {
	for _, command := range req.Commands {
		if commandmeta.IsRecoveryCommand(command) {
			return true
		}
	}
	return false
}

func primaryCommandPath(req *Request) string {
	if len(req.Commands) == 0 {
		return ""
	}
	return req.Commands[0]
}

// reportProbeDiagnostics surfaces provider findings and the name divergences
// `projects sync` exists to align.
//
// Until slice C4b this also ran the old/new equivalence guard, comparing each
// provider's answer against the core parser it was scheduled to replace. That
// guard did its job and deleted itself with its subject: with the parsers gone
// there is nothing left to compare to, and keeping it would be comparing the
// probe to itself. What remains worth reporting is the divergence that made core
// align names in the first place — a manifest on disk spelling the project
// differently from the name the workspace resolved for it — which is a warning,
// never an error, because the resolved name is what this run uses either way.
func reportProbeDiagnostics(req *Request, ws *workspace.Workspace, outcome *workspace.SyncOutcome) {
	if req.Global.Quiet {
		return
	}
	for _, d := range outcome.Diagnostics {
		if d.Severity == diag.Error {
			continue // errors are only produced with a failure, handled above
		}
		iox.Fprintf(os.Stderr, "putnami: warning: %s\n", d.String())
	}
	for _, warning := range workspace.NameDivergences(ws) {
		iox.Fprintf(os.Stderr, "putnami: warning: %s\n", warning)
	}
	if req.Global.Debug {
		iox.Fprintf(os.Stderr, "[debug] workspace probe: fresh=%v probed=%v reason=%q changed=%d\n",
			outcome.Fresh, outcome.Probed, outcome.Reason, len(outcome.Changed))
	}
}
