package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/mcp"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// mcpServe runs the Putnami MCP server over stdio (stdin/stdout), blocking
// until the client disconnects or the context is canceled. It is the entry
// point behind `putnami mcp`: a spawned, single-session JSON-RPC server that
// exposes the workspace to AI agent harnesses. See internal/mcp for the
// protocol implementation and tool set.
//
// It lives in the CLI shell rather than in internal/commands (where it sat
// until an earlier change) for a structural reason, not a stylistic one. MCP is
// an ADAPTER over engine.Run, and the shell is the one package that already
// depends on every adapter — so it is where an adapter's command-subtree
// dependencies get injected (AgentContext, Preflight) instead of becoming
// internal/mcp imports. Every other `putnami mcp` subcommand still delegates to
// internal/commands/agentctx.
//
// There used to be a harder reason: internal/engine imported
// internal/commands for the production preflight gate, so a `commands → mcp`
// edge would have closed the cycle mcp → engine → commands → mcp. That edge is
// gone (engine.PreflightGate is injected), and the injection style above is
// what keeps it gone.
func mcpServe(ctx context.Context, wsRoot string, cfg *wsproto.Config, version string) error {
	return serveMCPSession(ctx, wsRoot, cfg, version, agentctx.ReconcileAgentWorkflows, os.Stdin, os.Stdout, os.Stderr)
}

// agentWorkflowReconciler is the session-start pass's signature.
type agentWorkflowReconciler func(ctx context.Context, wsRoot string, cfg *wsproto.Config) error

// serveMCPSession is mcpServe with the session-start reconcile and the three
// stdio streams passed in, so a test drives the real sequence — reconcile,
// then serve — without replacing process-wide state.
func serveMCPSession(ctx context.Context, wsRoot string, cfg *wsproto.Config, version string, reconcile agentWorkflowReconciler, in io.Reader, out, stderr io.Writer) error {
	reconcileAgentWorkflowsAtSessionStart(ctx, wsRoot, cfg, reconcile, stderr)
	return newMCPServerWithContext(ctx, wsRoot, cfg, version).Serve(ctx, in, out)
}

// agentWorkflowReconcileTimeout bounds the session-start reconcile. The pass is
// local reads plus, on a version mismatch, a copy out of the machine-global
// store; the bound only matters when another process holds that store's
// exclusive lock, and the server must start regardless.
const agentWorkflowReconcileTimeout = 5 * time.Second

// reconcileAgentWorkflowsAtSessionStart brings the materialized agent workflows
// to the locked version before the server answers its first request (ADR
// 0040). A host starts this server when an agent session starts, so this is
// the one moment that precedes the agent reading a skill. Every failure is a
// warning on stderr — never stdout, which carries the protocol — and the
// server starts either way.
func reconcileAgentWorkflowsAtSessionStart(ctx context.Context, wsRoot string, cfg *wsproto.Config, reconcile agentWorkflowReconciler, stderr io.Writer) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, agentWorkflowReconcileTimeout)
	defer cancel()
	if err := reconcile(ctx, wsRoot, cfg); err != nil {
		iox.Fprintf(stderr, "putnami: warning: agent workflows were not reconciled with %s: %v. The MCP server starts anyway; `putnami install` materializes the locked version once nothing blocks it.\n",
			lockfile.LockFilename, err)
	}
}

// newMCPServer is mcpServe minus the process's own streams: the wiring, and
// only the wiring.
//
// It is separate so a test can drive the REAL server — the real tool set, the
// real injected builders, the real extension discovery — over a pair of buffers
// instead of stdio. A test that rebuilt this Options literal would be asserting
// against its own copy of the wiring, which is exactly the drift an extraction
// must not hide.
func newMCPServer(wsRoot string, cfg *wsproto.Config, version string) *mcp.Server {
	return newMCPServerWithContext(context.Background(), wsRoot, cfg, version)
}

func newMCPServerWithContext(ctx context.Context, wsRoot string, cfg *wsproto.Config, version string) *mcp.Server {
	prepared := lifecycle.PrepareReadOnly(ctx, wsRoot, cfg)
	guidance := make([]mcp.ExtensionGuidance, 0, len(prepared.Extensions))
	issues := make([]mcp.GuidanceIssue, 0, len(prepared.Issues))
	preparedRoots, unavailable := preparedExtensionSelection(prepared)
	for _, item := range prepared.Extensions {
		content, err := agentctx.ReadExtensionGuidance(item.Root)
		switch {
		case err != nil:
			issues = append(issues, mcp.GuidanceIssue{Name: item.Name, Version: item.Version, Reason: "read exact AI.md: " + err.Error()})
		case content == "":
			issues = append(issues, mcp.GuidanceIssue{Name: item.Name, Version: item.Version, Reason: "the exact extension artifact does not contain AI.md"})
		default:
			guidance = append(guidance, mcp.ExtensionGuidance{Name: item.Name, Version: item.Version, Content: content})
		}
	}
	for _, issue := range prepared.Issues {
		issues = append(issues, mcp.GuidanceIssue{Name: issue.Name, Version: issue.Version, Reason: issue.Reason})
	}

	return mcp.NewServer(mcp.Options{
		WorkspaceRoot:         wsRoot,
		Config:                cfg,
		ServerVersion:         version,
		ExtensionGuidance:     guidance,
		GuidanceIssues:        issues,
		PreparedExtensions:    preparedRoots,
		UnavailableExtensions: unavailable,
		PrepareWorkspaceView: func(callCtx context.Context) error {
			return prepareMCPWorkspaceView(callCtx, wsRoot, cfg)
		},
		PropagateAgentIdentity: mcpAgentIdentityEnabled(os.Getenv(mcp.AgentIdentityEnabledEnv)),
		AgentModel:             os.Getenv(mcp.AgentModelEnv),
		// Dependency injection avoids an internal/mcp → internal/commands import
		// cycle (commands already imports the agent-context builder). version is
		// threaded through so the in-memory build stamps the same provenance
		// version `context pack` did, keeping the freshness comparison honest.
		AgentContext: func(_ context.Context, sel string) (any, error) {
			return agentctx.BuildAgentContextResult(wsRoot, version, sel)
		},
		// The workspace map is served from an in-memory reduce over the working
		// tree, so it needs no version: it carries no provenance stamp that a
		// build could disagree with.
		WorkspaceMap: func(_ context.Context, section, sel string) (any, error) {
			return agentctx.BuildWorkspaceMapResult(wsRoot, section, sel)
		},
		// The selection an EXTENSION-contributed tool receives on its request.
		// It is the same resolver, through the same seam, for the
		// same reason: an extension has no workspace loader and must never grow
		// one, so a tool that declares `workspaceSelection` is handed the answer
		// the CLI already resolved rather than deriving a second one.
		ResolveSelection: func(ws *workspace.Workspace, selection mcp.ProjectSelection) (*extproto.ToolSelection, error) {
			resolved, err := shared.ResolveProjectSelection(ws, catalogProjectSelection(selection))
			if err != nil {
				return nil, err
			}
			return toolSelectionWire(resolved), nil
		},
		// The same production doctor gate every other adapter wires, injected
		// here for the same reason: internal/mcp holds no edge into the command
		// subtree.
		Preflight: doctor.DoctorPreflight,
	})
}

// preparedExtensionSelection turns one read-preparation pass into the
// extension selection an entry path serves from: the prepared root of every
// lock-pinned registry extension, an empty root for one whose exact release
// was not prepared, and the names of the latter, which must not serve.
// extension.SelectPreparedExtensions drops an empty root, so an unprepared
// release is never loaded from a stale stable link. The MCP server and the
// collaboration commands both select through it.
func preparedExtensionSelection(report lifecycle.ReadPreparationReport) (map[string]string, []string) {
	roots := make(map[string]string, len(report.Extensions)+len(report.Issues))
	unavailable := make([]string, 0, len(report.Issues))
	for _, item := range report.Extensions {
		roots[item.Name] = item.Root
	}
	for _, issue := range report.Issues {
		if issue.Name == "" {
			continue
		}
		roots[issue.Name] = ""
		unavailable = append(unavailable, issue.Name)
	}
	return roots, unavailable
}

// mcpGraphPreparationTimeout bounds the complete first graph request: exact
// lock-pinned artifact selection, local runtime preparation, provider probes
// and the atomic index publish. Individual probes have their own tighter
// transport bound; this deadline also caps lock contention with another CLI.
const mcpGraphPreparationTimeout = 60 * time.Second

// prepareMCPWorkspaceView creates only the recorded provider graph a cold MCP
// read needs. It deliberately re-runs exact read preparation on every missing-
// index attempt, so a transiently unavailable registry artifact or a repair by
// another process is visible to the next request rather than being frozen into
// the server's startup state.
func prepareMCPWorkspaceView(ctx context.Context, wsRoot string, cfg *wsproto.Config) error {
	if workspace.RecordedIndexView(wsRoot, time.Now()).Usable() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, mcpGraphPreparationTimeout)
	defer cancel()

	workspace.InvalidateLoadCache(wsRoot)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load cold workspace: %w", err)
	}
	if cfg == nil {
		cfg = ws.Config
	}
	prepared := lifecycle.PrepareReadOnly(ctx, wsRoot, cfg)
	if err := requireCompleteMCPReadPreparation(prepared); err != nil {
		return err
	}
	preparedRoots := make(map[string]string, len(prepared.Extensions)+len(prepared.Issues))
	for _, item := range prepared.Extensions {
		preparedRoots[item.Name] = item.Root
	}

	projectPaths := make([]string, len(ws.Projects))
	for i, project := range ws.Projects {
		projectPaths[i] = project.Path
	}
	discovered, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return fmt.Errorf("discover workspace providers: %w", err)
	}
	discovered.Extensions = extension.SelectPreparedExtensions(discovered.Extensions, preparedRoots)
	if err := requireCompleteMCPProviderDiscovery(prepared, discovered); err != nil {
		return err
	}
	if _, err := engine.PrepareWorkspaceGraph(ctx, ws, discovered); err != nil {
		return fmt.Errorf("prepare workspace graph: %w", err)
	}

	workspace.InvalidateLoadCache(wsRoot)
	if view := workspace.RecordedIndexView(wsRoot, time.Now()); !view.Usable() {
		return fmt.Errorf("workspace graph preparation produced no usable index: %s", view.Message)
	}
	return nil
}

func requireCompleteMCPReadPreparation(prepared lifecycle.ReadPreparationReport) error {
	if len(prepared.Issues) == 0 {
		return nil
	}
	failures := make([]string, 0, len(prepared.Issues))
	for _, issue := range prepared.Issues {
		identity := issue.Name
		if issue.Version != "" {
			identity += "@" + issue.Version
		}
		failures = append(failures, fmt.Sprintf("%s: %s", identity, issue.Reason))
	}
	slices.Sort(failures)
	return fmt.Errorf("exact workspace extension preparation is incomplete (%s); run `putnami install` to repair it, then retry",
		strings.Join(failures, "; "))
}

func requireCompleteMCPProviderDiscovery(
	prepared lifecycle.ReadPreparationReport,
	discovered *extension.DiscoveryResult,
) error {
	failures := make([]string, 0)
	if discovered == nil {
		failures = append(failures, "extension discovery returned no result")
	} else {
		for _, skip := range discovered.Skipped {
			identity := skip.Name
			if identity == "" {
				identity = skip.Ref
			}
			failures = append(failures, fmt.Sprintf("%s: %v", identity, skip.Reason))
		}
		for _, item := range prepared.Extensions {
			if extension.FindExtensionByName(discovered.Extensions, item.Name) == nil {
				failures = append(failures, fmt.Sprintf("%s@%s: exact manifest did not load", item.Name, item.Version))
			}
		}
	}
	if len(failures) == 0 {
		return nil
	}
	slices.Sort(failures)
	return fmt.Errorf("workspace provider discovery is incomplete (%s); repair the named extension or upgrade Putnami, then retry",
		strings.Join(failures, "; "))
}

// catalogProjectSelection maps the MCP catalog tools' already-resolved project
// narrowing onto the selection value the shared catalog builders take. The two
// surfaces therefore run the SAME resolution — canonical filtering, canonical
// impact baseline, canonical ordering — instead of agreeing by convention.
//
// Tag and exclusion filters are deliberately absent from the tool schema: an
// agent composes selections from ids it already resolved with list_projects or
// impacted, and adding a second filter vocabulary to the wire would let the two
// surfaces disagree about what a tag means.
func catalogProjectSelection(selection mcp.ProjectSelection) shared.ProjectSelection {
	return shared.ProjectSelection{
		Projects: selection.Projects,
		Impacted: selection.Impacted,
		Baseline: selection.Baseline,
	}
}

// toolSelectionWire projects the resolved selection onto the EXTENSION
// contract's selection block.
//
// It goes through ResolvedSelection.Wire() rather than reading the fields
// directly, so this mapping can never drift from the one a job subprocess gets:
// the job wire is the definition, and the tool wire is that definition's members
// under the tool contract's own type. protocols/job's
// TestToolSelectionMarshalsLikeTheJobSelection pins that the two encode to the
// same bytes; this function is what makes the claim true for one invocation.
func toolSelectionWire(resolved shared.ResolvedSelection) *extproto.ToolSelection {
	wire := resolved.Wire()
	return &extproto.ToolSelection{
		Mode:           wire.Mode,
		Scoped:         wire.Scoped,
		Baseline:       wire.Baseline,
		BaselineSource: wire.BaselineSource,
		ProjectIDs:     wire.ProjectIDs,
		EmptyImpact:    wire.EmptyImpact,
	}
}

func mcpAgentIdentityEnabled(raw string) bool {
	enabled, err := strconv.ParseBool(raw)
	return err == nil && enabled
}
