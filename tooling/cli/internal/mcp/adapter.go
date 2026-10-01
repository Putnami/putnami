package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The recorded workspace view, on every graph answer.
//
// Project identity and dependency edges are a PROJECTION: the provider
// extensions own them, and `.putnami/workspace-index.json` is the CLI's local
// copy. The tools below answer from that copy without paying for a probe, and
// that fast path is deliberate. What changes here is only the honesty of the
// answer.
//
// The contract (`cli.workspace-probe-view.v1`) declares `onMissing:
// fail-closed` and `onStale: use-stale`, and this is where those stop being
// sentences in a manifest:
//
//   - ABSENT — no usable copy. The tool returns its partial answer AND an
//     error, so the caller receives an isError result whose payload names the
//     command that rebuilds the copy. Answering an empty-but-well-formed graph
//     would read as "there is nothing here" rather than "nobody has told me
//     yet", so the tool fails closed instead.
//   - STALE — past the declared bound. The copy IS answered from, and the block
//     says so, which is what use-stale means.
//   - FRESH — the ordinary case, still reported, so a consumer never has to
//     infer freshness from the absence of a warning.

// errRecordedViewUnavailable is the fail-closed verdict. It carries no detail:
// the remedy is in the payload's workspaceView block, beside the partial answer
// it applies to.
var errRecordedViewUnavailable = errors.New(
	"the workspace view these facts are projected from is unavailable; see workspaceView in the payload")

// recordedView reads the copy this answer is derived from and returns the
// fail-closed verdict alongside it. Returning both together is what keeps a tool
// from reporting the view and then forgetting to act on it.
func recordedView(ws *workspace.Workspace) (workspace.RecordedView, error) {
	view := workspace.RecordedIndexView(ws.Root, time.Now())
	if !view.Usable() {
		return view, errRecordedViewUnavailable
	}
	return view, nil
}

// graphAnswer frames one graph tool's map payload with the recorded view it was
// derived from. Tools with a typed reply set the field themselves; both paths go
// through recordedView, so the verdict has one source.
func (s *Server) graphAnswer(ws *workspace.Workspace, payload map[string]any) (any, error) {
	view, err := s.recordedView(ws)
	payload["workspaceView"] = view
	return payload, err
}

// --- list_projects ---

type projectSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version,omitempty"`
	Path         string   `json:"path"`
	Type         string   `json:"type,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

func (s *Server) toolListProjects(ctx context.Context, _ json.RawMessage) (any, error) {
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	projects := append([]*workspace.Project(nil), ws.Projects...)
	sort.Slice(projects, func(i, j int) bool { return projects[i].Path < projects[j].Path })

	out := make([]projectSummary, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectSummary{
			ID:           p.ID,
			Name:         p.Name,
			Version:      p.Version,
			Path:         p.Path,
			Type:         p.Type,
			Tags:         p.Tags,
			Dependencies: p.Dependencies,
		})
	}
	return s.graphAnswer(ws, map[string]any{"count": len(out), "projects": out})
}

// --- describe_project ---

type describeArgs struct {
	Project string `json:"project"`
}

type projectDetail struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version,omitempty"`
	Path         string   `json:"path"`
	Type         string   `json:"type,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	Dependents   []string `json:"dependents,omitempty"`
	Extensions   []string `json:"extensions,omitempty"`
	Publish      []string `json:"publish,omitempty"`
	// WorkspaceView reports the recorded copy this project's identity and edges
	// were projected from, and how old it is.
	WorkspaceView workspace.RecordedView `json:"workspaceView"`
}

func (s *Server) toolDescribeProject(ctx context.Context, args json.RawMessage) (any, error) {
	var a describeArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Project == "" {
		return nil, errors.New("project is required (id or name)")
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	proj := resolveProject(ws, a.Project)
	if proj == nil {
		return nil, fmt.Errorf("project not found: %s", a.Project)
	}
	view, viewErr := s.recordedView(ws)
	return projectDetail{
		ID:            proj.ID,
		Name:          proj.Name,
		Version:       proj.Version,
		Path:          proj.Path,
		Type:          proj.Type,
		Tags:          proj.Tags,
		Dependencies:  proj.Dependencies,
		Dependents:    ws.Graph.DependentsOf(proj.ID),
		Extensions:    proj.Extensions,
		Publish:       proj.Publish,
		WorkspaceView: view,
	}, viewErr
}

// --- agent_context ---

type agentContextArgs struct {
	Project string `json:"project"`
}

// toolAgentContext returns one project's freshly aggregated agent-context result
// via the injected closure. It never reaches into internal/commands: the
// dependency-injected s.opts.AgentContext owns the aggregation and returns the
// already-shaped result, so this package keeps no edge into the command subtree
// at all (Options.Preflight is injected for the same reason).
func (s *Server) toolAgentContext(ctx context.Context, args json.RawMessage) (any, error) {
	var a agentContextArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Project) == "" {
		return nil, errors.New("project is required (id or name)")
	}
	if s.opts.AgentContext == nil {
		return nil, errors.New("agent_context is not available")
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	if _, err := s.recordedView(ws); err != nil {
		return nil, err
	}
	return s.opts.AgentContext(ctx, a.Project)
}

// --- workspace_map ---

type workspaceMapArgs struct {
	Section string `json:"section"`
	Project string `json:"project"`
}

// toolWorkspaceMap returns the freshly rendered workspace orientation map via
// the injected closure, scoped to the requested section and optional project.
// Both arguments are optional — an agent that knows nothing yet asks for the
// whole map — so validation of the section name belongs to the builder, which
// owns the section vocabulary and answers with the valid set.
func (s *Server) toolWorkspaceMap(ctx context.Context, args json.RawMessage) (any, error) {
	var a workspaceMapArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if s.opts.WorkspaceMap == nil {
		return nil, errors.New("workspace_map is not available")
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	if _, err := s.recordedView(ws); err != nil {
		return nil, err
	}
	return s.opts.WorkspaceMap(ctx, a.Section, a.Project)
}

// --- project refs and graph tools ---

type impactedArgs struct {
	Baseline string `json:"baseline"`
}

type projectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// impactEdgeRef is one hop of a why_impacted path: the edge the impact walk
// crossed from one project to the next, and what relation it is.
type impactEdgeRef struct {
	// From is the project ID the hop starts at.
	From string `json:"from"`
	// To is the project ID the hop reaches.
	To string `json:"to"`
	// Kind is dependency | contract | extension-consumer.
	Kind string `json:"kind"`
}

// --- deps ---

type depsArgs struct {
	Project    string `json:"project"`
	Direction  string `json:"direction"`
	Transitive bool   `json:"transitive"`
}

func (s *Server) toolDeps(ctx context.Context, args json.RawMessage) (any, error) {
	var a depsArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Project == "" {
		return nil, errors.New("project is required (id or name)")
	}
	direction := a.Direction
	if direction == "" {
		direction = "dependencies"
	}
	if direction != "dependencies" && direction != "dependents" {
		return nil, fmt.Errorf("direction must be one of: dependencies, dependents")
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	proj := resolveProject(ws, a.Project)
	if proj == nil {
		return nil, fmt.Errorf("project not found: %s", a.Project)
	}

	var ids []string
	switch {
	case direction == "dependencies" && a.Transitive:
		ids = ws.Graph.TransitiveDependenciesOf([]string{proj.ID})
	case direction == "dependencies":
		ids = append([]string(nil), ws.Graph.DependenciesOf(proj.ID)...)
	case direction == "dependents" && a.Transitive:
		ids = ws.Graph.TransitiveDependentsOf([]string{proj.ID})
	default:
		ids = append([]string(nil), ws.Graph.DependentsOf(proj.ID)...)
	}
	ids = removeProjectID(ids, proj.ID)
	sort.Strings(ids)
	refs := refsForIDs(ws, ids)
	return s.graphAnswer(ws, map[string]any{
		"project":    refForProject(proj),
		"direction":  direction,
		"transitive": a.Transitive,
		"count":      len(refs),
		"projects":   refs,
	})
}

// --- find_owner ---

type findOwnerArgs struct {
	Path string `json:"path"`
}

func (s *Server) toolFindOwner(ctx context.Context, args json.RawMessage) (any, error) {
	var a findOwnerArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Path) == "" {
		return nil, errors.New("path is required")
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	owners := workspace.ProjectOwnersForPath(ws, a.Path)
	refs := make([]projectRef, 0, len(owners))
	for _, p := range owners {
		refs = append(refs, refForProject(p))
	}
	return s.graphAnswer(ws, map[string]any{"path": a.Path, "count": len(refs), "projects": refs})
}

// --- why_impacted ---

type whyImpactedArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) toolWhyImpacted(ctx context.Context, args json.RawMessage) (any, error) {
	var a whyImpactedArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if a.From == "" || a.To == "" {
		return nil, errors.New("from and to are required (project id or name)")
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	from := resolveProject(ws, a.From)
	if from == nil {
		return nil, fmt.Errorf("project not found: %s", a.From)
	}
	to := resolveProject(ws, a.To)
	if to == nil {
		return nil, fmt.Errorf("project not found: %s", a.To)
	}
	if from.ID == to.ID {
		return s.graphAnswer(ws, map[string]any{
			"from":     refForProject(from),
			"to":       refForProject(to),
			"impacted": true,
			"self":     true,
			"path":     refsForIDs(ws, []string{from.ID}),
			"edges":    []impactEdgeRef{},
			"message":  fmt.Sprintf("%s is the same project as %s; no dependency path is needed", from.Name, to.Name),
		})
	}

	// The walk is the impact union — dependency, contract and
	// extension-consumer edges — through the same neighbor function `impacted`
	// widens with, and over the same task index, so the two tools cannot
	// disagree about a pair or about its task scope. DependentPath, which used
	// to answer here, has no extension-consumer edges and denied pairs
	// `impacted` had selected.
	reach := workspace.ImpactReachWithTasks(ws, from.ID, s.taskIndex(ws))
	steps := reach.Trace.PathTo(to.ID)
	if len(steps) == 0 {
		return s.graphAnswer(ws, map[string]any{
			"from":     refForProject(from),
			"to":       refForProject(to),
			"impacted": false,
			"path":     []projectRef{},
			"edges":    []impactEdgeRef{},
			"message":  fmt.Sprintf("%s does not transitively impact %s", from.Name, to.Name),
		})
	}
	path := make([]string, 0, len(steps))
	edges := make([]impactEdgeRef, 0, len(steps)-1)
	for i, step := range steps {
		path = append(path, step.Project)
		if i > 0 {
			edges = append(edges, impactEdgeRef{From: steps[i-1].Project, To: step.Project, Kind: string(step.Kind)})
		}
	}
	// The scope is the reach's, not the path's: a project the walk reached
	// task-scoped runs those tasks only, and every task that reached it is
	// listed, whichever edge the path names.
	taskScope := reach.Trace.ScopeOf(to.ID)
	if taskScope == nil {
		taskScope = []string{}
	}
	return s.graphAnswer(ws, map[string]any{
		"from":      refForProject(from),
		"to":        refForProject(to),
		"impacted":  true,
		"path":      refsForIDs(ws, path),
		"edges":     edges,
		"taskScope": taskScope,
	})
}

// --- topo_sort ---

func (s *Server) toolTopoSort(ctx context.Context, _ json.RawMessage) (any, error) {
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	order, ok := ws.Graph.TopologicalOrder()
	if !ok {
		return nil, errors.New("dependency graph contains a cycle")
	}
	refs := refsForIDs(ws, order)
	return s.graphAnswer(ws, map[string]any{"count": len(refs), "projects": refs})
}

func removeProjectID(ids []string, remove string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != remove {
			out = append(out, id)
		}
	}
	return out
}

func refForProject(p *workspace.Project) projectRef {
	if p == nil {
		return projectRef{}
	}
	return projectRef{ID: p.ID, Name: p.Name, Path: p.Path}
}

func refsForIDs(ws *workspace.Workspace, ids []string) []projectRef {
	refs := make([]projectRef, 0, len(ids))
	for _, id := range ids {
		if p := ws.ProjectByID(id); p != nil {
			refs = append(refs, refForProject(p))
		}
	}
	return refs
}

func (s *Server) toolImpacted(ctx context.Context, args json.RawMessage) (any, error) {
	var a impactedArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	baseline := a.Baseline
	selection, err := workspace.ImpactedSelectionForBaselineWithTasks(ws, baseline, s.taskIndex(ws))
	if err != nil {
		return nil, impactedError(baseline, err)
	}
	refs := make([]projectRef, 0, len(selection.Projects))
	for _, p := range selection.Projects {
		refs = append(refs, projectRef{ID: p.ID, Name: p.Name, Path: p.Path})
	}
	// The impacted set is a walk over the projected dependency edges, so it
	// carries the same honesty block and the same fail-closed verdict as the
	// other graph answers.
	view, viewErr := s.recordedView(ws)
	return impactedAnswer{
		Baseline:         selection.Baseline,
		BaselineSource:   string(selection.BaselineSource),
		Count:            len(refs),
		Projects:         refs,
		UnownedRootFiles: selection.UnownedRootFiles,
		Reasons:          impactedReasons(selection),
		WorkspaceView:    view,
	}, viewErr
}

// impactedReasons projects the selection's trace onto the wire, one entry per
// selected project in selection order. It reads the record; it computes no
// answer of its own.
func impactedReasons(selection workspace.ImpactedSelection) []impactedReason {
	if len(selection.Projects) == 0 {
		return nil
	}
	reasons := make([]impactedReason, 0, len(selection.Projects))
	for _, p := range selection.Projects {
		reason := impactedReason{Project: p.ID}
		if seeds, ok := selection.Trace.Seeds[p.ID]; ok {
			reason.Seeds = make([]impactSeedRef, 0, len(seeds))
			for _, seed := range seeds {
				reason.Seeds = append(reason.Seeds, impactSeedRef{File: seed.File, Kind: string(seed.Kind), Via: seed.Via})
			}
		} else if edge, ok := selection.Trace.Edges[p.ID]; ok {
			reason.From = edge.From
			reason.Kind = string(edge.Kind)
			reason.Via = edge.Via
			reason.ContractSHA256 = edge.ContractSHA256
		}
		reason.TaskScope = selection.Trace.ScopeOf(p.ID)
		reasons = append(reasons, reason)
	}
	return reasons
}

// impactedAnswer is the impacted tool's reply.
//
// It is a typed record rather than the map the other read-only tools return
// because one of its fields is CONDITIONAL: UnownedRootFiles is the evidence
// for a count an agent would otherwise have to take on faith — changed
// workspace-root paths that reached no project, which is how a count of 0 can
// sit over a non-empty diff — and it must be absent, not empty, when everything
// was attributed. `omitempty` states that once, in the shape, instead of at a
// call site that has to remember.
type impactedAnswer struct {
	Baseline string `json:"baseline"`
	// BaselineSource is the resolution tier that produced Baseline (explicit,
	// epic, trunk, local-trunk, upstream) — the same evidence the CLI prints
	// when a fallback tier resolved, so an agent can distrust a stale ref too.
	BaselineSource   string       `json:"baselineSource,omitempty"`
	Count            int          `json:"count"`
	Projects         []projectRef `json:"projects"`
	UnownedRootFiles []string     `json:"unownedRootFiles,omitempty"`
	// Reasons explains every entry of Projects, in the same order. A seeded
	// project lists the changed files that claimed it; a propagated project
	// names the project and the edge kind that reached it first. Absent when
	// Count is 0.
	Reasons []impactedReason `json:"reasons,omitempty"`
	// WorkspaceView reports the recorded copy the impacted walk read its
	// dependency edges from, and how old it is.
	WorkspaceView workspace.RecordedView `json:"workspaceView"`
}

// impactedReason is one project's entry in impactedAnswer.Reasons. Exactly one
// of Seeds or From/Kind is present: a project a changed file claimed directly
// has seeds and no edge.
type impactedReason struct {
	// Project is the project ID; equals Projects[i].ID at the same index.
	Project string `json:"project"`
	// Seeds is present on directly affected projects: the changed files that
	// claimed the project, in diff order.
	Seeds []impactSeedRef `json:"seeds,omitempty"`
	// From is present on propagated projects: the project ID already in the
	// set that the walk reached this one from.
	From string `json:"from,omitempty"`
	// Kind is present on propagated projects: dependency, contract or
	// extension-consumer.
	Kind string `json:"kind,omitempty"`
	// Via is present on a project a contract edge reached: the provider's
	// committed contract that changed, which is the only reason a contract
	// edge fires.
	Via string `json:"via,omitempty"`
	// ContractSHA256 is present beside Via: the sha256 of that contract in the
	// tree the selection read.
	ContractSHA256 string `json:"contractSha256,omitempty"`
	// TaskScope is present on a task-scoped project: the tasks of it the
	// change reaches — `<command>~<step>` — and nothing else of it. Absent on
	// a project that runs every task.
	TaskScope []string `json:"taskScope,omitempty"`
}

// impactSeedRef is one changed file's claim on a directly affected project.
type impactSeedRef struct {
	// File is the workspace-relative changed path.
	File string `json:"file"`
	// Kind is path-owner | root-watched-file | declared-input | workspace-input
	// | scope-config.
	Kind string `json:"kind"`
	// Via is the path, watched entry, pattern set, or scope directory that
	// claimed the file.
	Via string `json:"via"`
}

// --- run_jobs ---

type runJobsArgs struct {
	Commands []string `json:"commands"`
	Projects []string `json:"projects"`
	Impacted bool     `json:"impacted"`
	Baseline string   `json:"baseline"`
	DryRun   bool     `json:"dryRun"`
}

type diagnostic struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Severity string `json:"severity,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	Project  string `json:"project,omitempty"`
	Job      string `json:"job,omitempty"`
}

// runRecord is what one run_jobs call leaves behind for get_diagnostics. The
// tool ANSWERS with the v2 document (protocolcli.MCPResult, built by
// machine.Run) — the v1 runResult/planResult wire shapes were deleted with the
// rest of the v1 emitters.
//
// The diagnostic list is kept apart from the answer on purpose: the v2 run
// summary carries a failed task's diagnostics, while this holds EVERY
// diagnostic the run produced, warnings on succeeding tasks included. That list
// is deliberately off-wire — an agent asks for it with get_diagnostics instead
// of receiving it on every run.
type runRecord struct {
	result         *protocolcli.MCPResult
	allDiagnostics []diagnostic
}

// toolRunJobs is the MCP ADAPTER over engine.Run. Before it,
// run_jobs re-implemented the whole job pipeline — its own workspace load,
// non-Detailed extension discovery, project selection, planning without the
// missing-extension and starved-command guards, its own SchedulerConfig with no
// remote cache/session/profiler, and a bespoke reducer that duplicated the
// output package's. All of it is gone: this builds a Request, calls the engine,
// and translates the canonical SessionResult onto the (unchanged) tool contract.
//
// Three seams are deliberately NOT the terminal path's:
//
//   - The telemetry observer stays nil (ADR 0001 §4). An MCP tool call is not a
//     user-initiated session, no consent notice has been shown in this process,
//     and the design settled on "never send before notice". internal/engine's
//     seam_test.go enforces module-wide that only the terminal adapter injects
//     one.
//   - EventSink is discardSink: the scheduler's rendering would land on the
//     JSON-RPC frame stream.
//   - Request.Stdout is a buffer for the same reason. The engine's own notices
//     ("No jobs matched. Nothing to do.") are captured and folded into the tool
//     error instead of written to the transport.
//
// Hooks stay nil: an agent's tool call must not fire the workspace's before/after
// lifecycle hooks, which are declared for interactive/CI runs.
func (s *Server) toolRunJobs(ctx context.Context, args json.RawMessage) (any, error) {
	var a runJobsArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if len(a.Commands) == 0 {
		return nil, errors.New(`commands is required, e.g. ["lint","test","build"]`)
	}
	if a.DryRun {
		// Planning is a pure read: it never schedules anything and mutates
		// nothing outside the workspace, so neither refusal below applies here. An
		// agent can still ask what `serve` or `deploy` would run.
		return s.planJobs(ctx, a)
	}
	if err := rejectLongLivedCommands(a.Commands); err != nil {
		return nil, err
	}
	if err := s.rejectSideEffectingCommands(a.Commands); err != nil {
		return nil, err
	}
	res, err := s.runJobs(ctx, a)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.lastRun = res
	s.mu.Unlock()
	return res.result, nil
}

// runJobs executes the run and translates the canonical reduction onto the v2
// MCP document, keeping the run's full diagnostic list for get_diagnostics.
func (s *Server) runJobs(ctx context.Context, a runJobsArgs) (*runRecord, error) {
	result, err := s.runThroughEngine(ctx, a, false)
	if err != nil {
		return nil, err
	}
	// No scheduler ran: selection or planning ended the run before execution.
	// Exit success there is the legitimate empty case (nothing impacted), which
	// has always answered with a zero-count success; anything else is a stage
	// that refused to run, and the agent gets that as an explicit error rather
	// than a green summary of a build that never happened.
	if result.Session == nil {
		if result.ExitCode == engine.ExitSuccess {
			return &runRecord{result: machine.Run{}.MCPRun(a.Commands)}, nil
		}
		return nil, s.abortedRunError(a, result.ExitCode)
	}
	// Built from the canonical reduction plus the plan and results the engine
	// hands back — v2 names every task, and only the plan node holds a task's
	// typed identity.
	return &runRecord{
		result:         machine.RunFrom(result.Session, result.Plan, result.Results).MCPRun(a.Commands),
		allDiagnostics: diagnosticsFrom(result.Session.Diagnostics),
	}, nil
}

// planJobs answers dryRun=true. It is the engine's --plan stage: plan, report,
// execute nothing. Global.DryRun is deliberately NOT used — that flag makes the
// engine print a preview to the human stream and is what the extension-alias
// adapter forwards to jobs as a parameter, whereas this tool promises "return
// the DAG without executing subprocesses".
func (s *Server) planJobs(ctx context.Context, a runJobsArgs) (*protocolcli.MCPResult, error) {
	result, err := s.runThroughEngine(ctx, a, true)
	if err != nil {
		return nil, err
	}
	if result.Plan == nil && result.ExitCode != engine.ExitSuccess {
		return nil, s.abortedRunError(a, result.ExitCode)
	}
	return machine.MCPPlan(a.Commands, result.Plan), nil
}

// runThroughEngine is the single Engine.Run call behind run_jobs. planOnly
// selects the engine's --plan stage.
func (s *Server) runThroughEngine(ctx context.Context, a runJobsArgs, planOnly bool) (engine.SessionResult, error) {
	global, err := s.selectionFlags(ctx, a)
	if err != nil {
		return engine.SessionResult{}, err
	}
	global.Plan = planOnly

	s.notices.Reset()
	result, runErr := engine.New().Run(ctx, engine.Request{
		WorkspaceRoot: s.opts.WorkspaceRoot,
		Config:        s.opts.Config,
		Commands:      a.Commands,
		Global:        global,
		// Empty, non-nil, exactly as the pre-A4 code passed to jobs.Plan and
		// jobs.NewScheduler. A param value's Go type is part of every cache key
		// (store.hashParams) and of every run-marker key
		// (workspace_state.LastBuildParamsHash), so MCP-initiated runs keep
		// producing byte-identical keys to the ones they produced before.
		CommandParams: map[string]any{},
		// An agent tool call is not the user's build, so its session must not
		// become `latest`, the session every `--session latest` reader opens.
		// The session is still written and still listed by `putnami sessions`.
		EphemeralSession: true,
		// An agent's run executes real jobs, so it meets the same production
		// doctor bar a human's does; nil fails a production run closed.
		Preflight: s.opts.Preflight,
		Stdout:    &s.notices,
	}, discardSink{})
	return result, runErr
}

// selectionFlags maps the tool's three selection shapes onto the engine's
// project-selection flags. Two properties are load-bearing:
//
//   - No shape sets All or leaves the selection bare, so isBareProjectSelection
//     is never true and shouldRecordSuccessfulBuild is never satisfied: an MCP
//     run publishes NO successful-run marker, locally or remotely. Markers are
//     keyed by (branch, commands, params) with no provenance, so an agent's run
//     of "lint,test,build" would otherwise silently narrow what a later human or
//     CI `--impacted` run considers changed. Auto-selection stays dormant for the
//     same reason it always has: the tool schema exposes no way to ask for it.
//   - "*" (rather than All) also keeps DirectTarget true in the engine's filter,
//     which is what preserves the pre-A4 default of literally every project,
//     unfiltered by the workspace's disable.tags.
func (s *Server) selectionFlags(ctx context.Context, a runJobsArgs) (engine.GlobalFlags, error) {
	var global engine.GlobalFlags
	// Local cache on, remote cache off — which is exactly what the pre-A4 fork
	// did (it built a local CacheManager and never called SetRemoteCache). The
	// value must be stated rather than left empty: the engine fails CLOSED on an
	// unresolved trust policy, degrading to local-only WITH a per-run warning on
	// stderr, and the resolution chain that would produce a real policy
	// (--cache-trust > PUTNAMI_CACHE_TRUST > workspace options > ci/any default)
	// lives in the CLI shell. Granting an agent surface read/write access to the
	// team's shared cache is a decision of its own, not a refactor side effect.
	global.CacheTrust = string(store.CacheTrustNone)
	switch {
	case a.Impacted:
		global.Impacted = true
		global.Baseline = a.Baseline
		// The agent asked for the impacted set. The engine's lenient default
		// falls back to running EVERY project when the baseline cannot be
		// resolved; for a tool call that is an unbounded run nobody requested, so
		// MCP fails closed exactly as the pre-A4 code did.
		global.ImpactedStrict = true
	case len(a.Projects) > 0:
		ids, err := s.resolveProjectIDs(ctx, a.Projects, !a.DryRun)
		if err != nil {
			return global, err
		}
		global.Projects = strings.Join(ids, ",")
	default:
		global.Projects = "*"
	}
	return global, nil
}

// resolveProjectIDs turns the tool's project selectors into canonical ids. This
// is argument validation, not a selection stage: it is the same resolveProject
// lookup the eight read-only tools do, and it exists so a typo answers "projects
// not found: x" instead of silently running a SUBSET of what was asked for —
// which is what handing unknown names to the engine's filter would do.
func (s *Server) resolveProjectIDs(ctx context.Context, selectors []string, prepare bool) ([]string, error) {
	var (
		ws  *workspace.Workspace
		err error
	)
	if prepare {
		ws, err = s.loadWorkspace(ctx)
	} else {
		// An MCP dry run is a preview-only engine request. Resolving its explicit
		// selectors must not turn the preview into a persistent cold-workspace
		// bootstrap before the engine has applied its no-write policy.
		ws, err = workspace.Load(s.opts.WorkspaceRoot)
	}
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	ids := make([]string, 0, len(selectors))
	var missing []string
	for _, selector := range selectors {
		if p := resolveProject(ws, selector); p != nil {
			ids = append(ids, p.ID)
		} else {
			missing = append(missing, selector)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("projects not found: %s", strings.Join(missing, ", "))
	}
	return ids, nil
}

// abortedRunError reports a run the engine stopped before any job executed. The
// reason is whatever the engine wrote to its human stream, which MCP captured
// instead of letting it reach the JSON-RPC transport; stderr carries the rest.
func (s *Server) abortedRunError(a runJobsArgs, exitCode int) error {
	detail := strings.TrimSpace(s.notices.String())
	if detail == "" {
		detail = "see the server's stderr for the reason"
	}
	return fmt.Errorf("run_jobs %s: no jobs ran (exit %d): %s",
		strings.Join(a.Commands, ","), exitCode, strings.Join(strings.Fields(detail), " "))
}

// longLivedCommands names the commands routing through the engine makes
// UNTERMINATABLE over MCP, so the refusal is scoped to the hazard A4 actually
// introduces rather than to blocking calls in general.
//
// `serve` qualifies: the engine promotes it to watch mode
// (internal/engine/execute.go:110), so it no longer ends even when its child
// exits. The MCP transport is synchronous — one tool call at a time on a single
// stdio loop — so that wedges the WHOLE session with no way out.
//
// `run` deliberately does NOT qualify. It is not promoted to watch, so it
// terminates exactly as it did on MCP's pre-A4 fork, and forwarding a workload's
// exit code (a migration returning 3) is a legitimate agent use. Refusing it
// would narrow a published tool's accepted input for a hazard this slice does
// not create.
var longLivedCommands = map[string]string{
	"serve": "is promoted to watch mode by the engine, so it never returns — not even when its child exits",
	// `compose` is a structured command rather than a job verb, so run_jobs
	// cannot reach it today. It is named here anyway: it supervises serve
	// processes until a signal, and a future surface that forwards command
	// names must inherit the refusal instead of rediscovering the hazard.
	"compose": "supervises its workloads until it receives a signal, so it never returns on its own",
	// `qualify` waits on a live workload for up to its readiness deadline and
	// every request deadline, and with a local target it also composes and
	// supervises that workload; one MCP tool call would block the session for
	// that whole time.
	"qualify": "waits on a live workload (and composes one for a local target) for as long as its readiness and request deadlines allow",
}

func rejectLongLivedCommands(commands []string) error {
	for _, command := range commands {
		if why, blocked := longLivedCommands[command]; blocked {
			return fmt.Errorf("run_jobs cannot run %q: it %s, and one MCP tool call blocks the whole session. "+
				"Run it in a terminal instead", command, why)
		}
	}
	return nil
}

// rejectSideEffectingCommands refuses a run whose commands mutate state OUTSIDE
// the workspace. The principle is the one every other seam on this surface
// already applies — the telemetry observer stays nil, hooks stay nil, CacheTrust
// is none, `--impacted` fails closed: an agent's tool call may spend CPU on the
// workspace, because that is reversible and local, but pushing a release to a
// registry or converging a cloud environment is neither, and no confirmation
// step exists between a tool call and the effect. So the refusal names the
// terminal as the remedy rather than pretending the run is unavailable.
//
// The classification is NOT a second list of names. The extension protocol
// already types it as CommandTraits.SideEffects ("registry" for publish, "cloud"
// for deploy), resolved per command by proto.EffectiveTraits, so an extension's
// own release verb is covered by declaring the trait — which a hardcoded pair of
// well-known names would let walk straight through.
//
// Only EXECUTION is gated: dryRun returns before this, exactly as it does for
// rejectLongLivedCommands.
func (s *Server) rejectSideEffectingCommands(commands []string) error {
	declared := s.declaredSideEffects()
	for _, command := range commands {
		name := proto.BaseCommandName(command)
		effects, isDeclared := declared[name]
		if !isDeclared {
			// Nothing this workspace loaded declares the command, so the
			// well-known verb table is the whole classification — the same answer
			// EffectiveTraits gives a manifest that declares no traits of its own,
			// and the one that keeps `deploy` refused when the extension owning it
			// is not installed here.
			effects = proto.EffectiveTraits(name, nil).SideEffects
		}
		if effects != proto.SideEffectsNone {
			return fmt.Errorf("run_jobs cannot run %q: it mutates %s outside the workspace, "+
				"which is not a decision an agent tool call can make. Run it in a terminal instead",
				command, effects)
		}
	}
	return nil
}

// declaredSideEffects maps each command the workspace's extensions declare to
// its resolved SideEffects trait. Resolution is the protocol's own
// (extension.Resolve applies proto.EffectiveTraits at load), so a
// manifest-declared `sideEffects` on a custom verb counts exactly as publish's
// default does, and a manifest that overrides a well-known verb's traits owns
// the answer for it — the same resolved traits the planner schedules by.
//
// A command several extensions declare keeps the side-effecting classification
// if ANY of them carries one: run_jobs would schedule all of them.
//
// Discovery failure yields no entries rather than an error, which leaves the
// caller on the well-known verb table: an unreadable manifest must not open the
// gate for `publish` and `deploy`.
func (s *Server) declaredSideEffects() map[string]string {
	discovered, err := s.discoverExtensions()
	if err != nil {
		return nil
	}
	declared := make(map[string]string)
	for _, ext := range discovered.Extensions {
		if ext == nil {
			continue
		}
		for name, job := range ext.Jobs {
			if job == nil {
				continue
			}
			base := proto.BaseCommandName(name)
			if effects, seen := declared[base]; seen && effects != proto.SideEffectsNone {
				continue
			}
			declared[base] = job.Traits.SideEffects
		}
	}
	return declared
}

// diagnosticsFrom projects canonical task diagnostics onto the wire shape. The
// project/job provenance is already stamped by the reducer, so nothing is
// re-derived here.
func diagnosticsFrom(in []jobs.TaskDiagnostic) []diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]diagnostic, 0, len(in))
	for _, d := range in {
		out = append(out, diagnostic{
			File:     d.File,
			Line:     d.Line,
			Column:   d.Column,
			Severity: d.Severity,
			Code:     d.Code,
			Message:  d.Message,
			Project:  d.Project,
			Job:      d.Job,
		})
	}
	return out
}

func impactedError(baseline string, err error) error {
	if strings.TrimSpace(baseline) == "" {
		return fmt.Errorf("compute impacted: %w", err)
	}
	return fmt.Errorf("compute impacted vs %s: %w", baseline, err)
}

// --- get_diagnostics ---

type getDiagnosticsArgs struct {
	Severity string `json:"severity"`
}

func (s *Server) toolGetDiagnostics(_ context.Context, args json.RawMessage) (any, error) {
	var a getDiagnosticsArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	s.mu.Lock()
	last := s.lastRun
	s.mu.Unlock()
	if last == nil {
		return map[string]any{
			"count":       0,
			"diagnostics": []diagnostic{},
			"message":     "no run_jobs call yet in this session",
		}, nil
	}

	out := make([]diagnostic, 0, len(last.allDiagnostics))
	for _, d := range last.allDiagnostics {
		if a.Severity != "" && d.Severity != a.Severity {
			continue
		}
		out = append(out, d)
	}
	return map[string]any{"count": len(out), "diagnostics": out}, nil
}

// --- workspace://context ---

type workspaceContextData struct {
	Name             string                  `json:"name,omitempty"`
	Root             string                  `json:"root"`
	Baseline         string                  `json:"baseline,omitempty"`
	Layout           workspaceLayoutContext  `json:"layout"`
	Projects         []workspaceProject      `json:"projects"`
	Graph            map[string]graphContext `json:"graph"`
	TopologicalOrder []string                `json:"topologicalOrder,omitempty"`
	PublishChannels  map[string][]projectRef `json:"publishChannels,omitempty"`
	PinnedVersions   pinnedVersionsContext   `json:"pinnedVersions,omitempty"`
	Warnings         []string                `json:"warnings,omitempty"`
}

type workspaceLayoutContext struct {
	Includes       []string          `json:"includes,omitempty"`
	Extensions     []string          `json:"extensions,omitempty"`
	Aliases        map[string]string `json:"aliases,omitempty"`
	ProjectAliases map[string]string `json:"projectAliases,omitempty"`
	Groups         map[string]string `json:"groups,omitempty"`
}

type workspaceProject struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	Type         string   `json:"type,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Publish      []string `json:"publish,omitempty"`
	Extensions   []string `json:"extensions,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	Dependents   []string `json:"dependents,omitempty"`
}

type graphContext struct {
	Dependencies []string `json:"dependencies,omitempty"`
	Dependents   []string `json:"dependents,omitempty"`
}

type pinnedVersionsContext struct {
	CLI        string            `json:"cli,omitempty"`
	Extensions map[string]string `json:"extensions,omitempty"`
	Templates  map[string]string `json:"templates,omitempty"`
}

func (s *Server) workspaceContextResource(ctx context.Context) (workspaceContextData, error) {
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return workspaceContextData{}, fmt.Errorf("load workspace: %w", err)
	}
	if _, err := s.recordedView(ws); err != nil {
		return workspaceContextData{}, err
	}
	data := workspaceContextData{
		Name:            ws.Name,
		Root:            ws.Root,
		Warnings:        append([]string(nil), ws.Warnings...),
		Layout:          workspaceLayoutContext{},
		Projects:        make([]workspaceProject, 0, len(ws.Projects)),
		Graph:           make(map[string]graphContext, len(ws.Projects)),
		PublishChannels: make(map[string][]projectRef),
	}
	if ws.Config != nil {
		data.Baseline = ws.Config.Baseline
		data.Layout.Includes = append([]string(nil), ws.Config.Includes...)
		data.Layout.Extensions = ws.Config.Extensions.Names()
		data.Layout.Aliases = copyStringMap(ws.Config.Aliases)
		data.Layout.ProjectAliases = copyStringMap(ws.Config.ProjectAliases)
		data.Layout.Groups = copyStringMap(ws.Config.Groups)
	}

	projects := append([]*workspace.Project(nil), ws.Projects...)
	sort.Slice(projects, func(i, j int) bool { return projects[i].Path < projects[j].Path })
	if order, ok := ws.Graph.TopologicalOrder(); ok {
		data.TopologicalOrder = order
	}
	for _, p := range projects {
		deps := sortedIDs(ws.Graph.DependenciesOf(p.ID))
		dependents := sortedIDs(ws.Graph.DependentsOf(p.ID))
		data.Projects = append(data.Projects, workspaceProject{
			ID:           p.ID,
			Name:         p.Name,
			Path:         p.Path,
			Type:         p.Type,
			Tags:         append([]string(nil), p.Tags...),
			Publish:      append([]string(nil), p.Publish...),
			Extensions:   append([]string(nil), p.Extensions...),
			Dependencies: deps,
			Dependents:   dependents,
		})
		data.Graph[p.ID] = graphContext{Dependencies: deps, Dependents: dependents}
		for _, channel := range p.Publish {
			data.PublishChannels[channel] = append(data.PublishChannels[channel], refForProject(p))
		}
	}
	if len(data.PublishChannels) == 0 {
		data.PublishChannels = nil
	}

	pins, err := lockedVersions(s.opts.WorkspaceRoot)
	if err != nil {
		return workspaceContextData{}, err
	}
	data.PinnedVersions = pins
	return data, nil
}

func lockedVersions(root string) (pinnedVersionsContext, error) {
	lf, err := lockfile.ReadLockFile(root)
	if err != nil {
		return pinnedVersionsContext{}, err
	}
	if lf == nil {
		return pinnedVersionsContext{}, nil
	}
	pins := pinnedVersionsContext{
		Extensions: make(map[string]string, len(lf.Extensions)),
		Templates:  make(map[string]string, len(lf.Templates)),
	}
	if cli, ok := lf.GetCLI(); ok {
		// A source workspace has no published version. Reporting cli.Version
		// would hand an agent an empty string that reads as "unpinned" — the
		// opposite of the truth, since a source workspace is the strictest
		// declaration there is. Report the sentinel verbatim instead.
		if cli.IsWorkspaceSource() {
			pins.CLI = lockfile.SourceWorkspace
		} else {
			pins.CLI = cli.Version
		}
	}
	for name, entry := range lf.Extensions {
		pins.Extensions[name] = entry.Version
	}
	for name, entry := range lf.Templates {
		pins.Templates[name] = entry.Version
	}
	if len(pins.Extensions) == 0 {
		pins.Extensions = nil
	}
	if len(pins.Templates) == 0 {
		pins.Templates = nil
	}
	return pins, nil
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// --- shared helpers ---

// resolveProject looks up a project by canonical id first, then by name.
func resolveProject(ws *workspace.Workspace, selector string) *workspace.Project {
	if p := ws.ProjectByID(selector); p != nil {
		return p
	}
	return ws.ProjectByName(selector)
}

// decodeArgs unmarshals tool arguments, tolerating absent/empty arguments.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// discardSink is MCP's engine.EventSink (the scheduler's renderer contract): it
// discards every task event, because stdout here is the JSON-RPC frame stream
// and a single rendered line desynchronizes the session's framing. Nothing is
// lost — the canonical SessionResult carries the counts, failures and
// diagnostics the tool reports, and the run's structured records reach
// get_diagnostics through it.
type discardSink struct{}

func (discardSink) Start([]*jobs.ScheduledJob)                             {}
func (discardSink) JobStart(*jobs.ScheduledJob)                            {}
func (discardSink) JobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)          {}
func (discardSink) JobComplete(*jobs.ScheduledJob, *jobs.JobResult)        {}
func (discardSink) Finish(map[string]*jobs.JobResult, jobs.SessionOutcome) {}
