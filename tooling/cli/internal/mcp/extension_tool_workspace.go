package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The two things a manifest-declared MCP tool needs that its manifest cannot
// state: which executable {extensionRuntime} resolves to, and what the
// workspace looks like.
//
// Both exist for one reason. An extension has no workspace loader and no
// prepared-runtime resolver, and it must never grow either: two derivations of
// the same fact is how a tool and the orchestrator start disagreeing about what
// the workspace contains and about which binary is the extension. So the
// orchestrator — which holds both answers already — resolves them here and
// publishes them, exactly as it does for a job subprocess.

// referencesExtensionRuntime reports whether a manifest string names the
// extension's prepared runtime.
func referencesExtensionRuntime(value string) bool {
	return strings.Contains(value, "{"+proto.TemplateVarExtensionRuntime+"}")
}

// extensionToolTemplateVars resolves the variables one tool invocation expands,
// preparing the extension's runtime on FIRST USE when the tool names it.
//
// Lazy rather than eager, and the timing is the contract. Registration runs no
// extension code — it reads manifests, so a workspace with a dozen extensions
// still starts an MCP session instantly — while a local extension's runtime is
// COMPILED, which is work no agent asked for until it calls the tool. The call's
// own context carries the deadline, so a runtime that will not build fails the
// call it belongs to instead of the session.
//
// The resolved path is cached on the description, so the second call in a
// session pays nothing. Same shape as `projects sync`'s syncRuntimeResolver.
func extensionToolTemplateVars(
	ctx context.Context, workspaceRoot string, candidate extensionToolCandidate,
) (map[string]string, error) {
	vars := extension.BuildTemplateVars(
		workspaceRoot, workspaceRoot, candidate.extensionRoot(), "")
	if !referencesExtensionRuntime(candidate.def.Command) && !toolArgsReferenceRuntime(candidate.def) {
		return vars, nil
	}
	executable, err := extensionToolRuntime(ctx, workspaceRoot, candidate)
	if err != nil {
		return nil, err
	}
	vars[proto.TemplateVarExtensionRuntime] = executable
	return vars, nil
}

// toolArgsReferenceRuntime reports whether the args, the cwd or an env value of
// def name the prepared runtime; each is expanded with the same variables.
func toolArgsReferenceRuntime(def extension.ToolDefinition) bool {
	for _, arg := range def.Args {
		if referencesExtensionRuntime(arg) {
			return true
		}
	}
	for _, value := range def.Env {
		if referencesExtensionRuntime(value) {
			return true
		}
	}
	return referencesExtensionRuntime(def.Cwd)
}

func extensionToolRuntime(
	ctx context.Context, workspaceRoot string, candidate extensionToolCandidate,
) (string, error) {
	ext := candidate.ext
	if ext == nil || ext.Runtime == nil {
		return "", fmt.Errorf("extension tool %s names {%s}, but extension %q declares no runtime",
			candidate.name, proto.TemplateVarExtensionRuntime, candidate.name)
	}
	if ext.RuntimeExecutable != "" {
		return ext.RuntimeExecutable, nil
	}
	ws, err := workspace.Load(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("load workspace: %w", err)
	}
	if err := jobs.SynchronizeExtensionRuntimes(
		ctx, ws, []*extension.ExtensionDescription{ext}, nil); err != nil {
		return "", fmt.Errorf("prepare the %s runtime for tool %s: %w", ext.Name, candidate.name, err)
	}
	if ext.RuntimeExecutable == "" {
		return "", fmt.Errorf("the %s runtime prepared no executable for tool %s", ext.Name, candidate.name)
	}
	return ext.RuntimeExecutable, nil
}

// toolSelectionArgs is the canonical selection vocabulary an MCP tool narrows
// with — the same three argument names `run_jobs` and the core catalog tools
// take, which is what makes this a contract rather than a convention.
//
// It is decoded LENIENTLY, unlike a core tool's own arguments: the tool owns the
// rest of its argument object and the orchestrator has no business rejecting a
// member it does not recognize. A member that is present but ill-typed is still
// an error — that is the tool's own vocabulary being misused.
type toolSelectionArgs struct {
	Projects []string `json:"projects"`
	Impacted bool     `json:"impacted"`
	Baseline string   `json:"baseline"`
}

// attachToolWorkspaceView fills the members a tool that declared
// WorkspaceSelection is entitled to: the complete resolved membership and the
// projection its arguments resolve to.
//
// The workspace is loaded ONCE for both. Resolution comes first because it is
// the half that can legitimately fail — an unknown selector, an unresolvable
// baseline — and publishing a membership beside a selection nobody could resolve
// would hand the tool a view it must not answer from.
func (s *Server) attachToolWorkspaceView(ctx context.Context, request *proto.ToolCallRequest, args json.RawMessage) error {
	var parsed toolSelectionArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return fmt.Errorf("invalid arguments: %w", err)
		}
	}
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	if _, err := s.recordedView(ws); err != nil {
		return err
	}
	return AttachWorkspaceView(ws, request, args, s.opts.ResolveSelection)
}

// AttachWorkspaceView fills the members a tool that declared
// WorkspaceSelection is entitled to, from an already-loaded workspace: the
// projection its canonical projects, impacted and baseline arguments resolve
// to through resolve, and the complete resolved membership. Every entry path
// that calls such a tool attaches its view through this one function.
func AttachWorkspaceView(
	ws *workspace.Workspace, request *proto.ToolCallRequest, args json.RawMessage,
	resolve func(*workspace.Workspace, ProjectSelection) (*proto.ToolSelection, error),
) error {
	var parsed toolSelectionArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return fmt.Errorf("invalid arguments: %w", err)
		}
	}
	selection, err := catalogSelectionIn(ws, parsed.Projects, parsed.Impacted, parsed.Baseline)
	if err != nil {
		return err
	}
	if resolve == nil {
		return fmt.Errorf("extension tool %s declares workspaceSelection, but this server has no selection resolver",
			request.Name)
	}
	resolved, err := resolve(ws, selection)
	if err != nil {
		return err
	}
	request.Selection = resolved
	request.WorkspaceProjects = toolWorkspaceProjects(ws)
	return nil
}

// catalogSelectionIn resolves a tool call's project narrowing against an
// already-loaded workspace, so one tool call reads the tree once.
//
// The two guards were the built-in catalog tools' own, and outlived them: those
// tools moved to @putnami/sdd, and their guards stayed here because they are
// the ORCHESTRATOR's, not the tool's. `projects` and
// `impacted` are two ways to say the same thing, and an unresolvable selector
// must answer "projects not found" instead of silently narrowing to a subset of
// what was asked for — neither of which an extension can enforce, because it
// has no workspace to resolve a selector against.
func catalogSelectionIn(
	ws *workspace.Workspace, projects []string, impacted bool, baseline string,
) (ProjectSelection, error) {
	selection := ProjectSelection{Impacted: impacted, Baseline: baseline}
	if impacted && len(projects) > 0 {
		return selection, errors.New("projects and impacted select the same thing two ways: pass one")
	}
	if len(projects) == 0 {
		return selection, nil
	}
	ids := make([]string, 0, len(projects))
	var missing []string
	for _, selector := range projects {
		if p := resolveProject(ws, selector); p != nil {
			ids = append(ids, p.ID)
		} else {
			missing = append(missing, selector)
		}
	}
	if len(missing) > 0 {
		return selection, fmt.Errorf("projects not found: %s", strings.Join(missing, ", "))
	}
	selection.Projects = strings.Join(ids, ",")
	return selection, nil
}

// toolWorkspaceProjects projects the loaded workspace onto the tool wire, in
// canonical project-id order.
//
// It is the same projection jobs.workspaceProjectsContext makes for a job
// subprocess, plus the authored project config — which a job never needs,
// because a job acts on its own project, and a workspace-answering tool always
// does, because it reports ON other projects.
//
// The one member the two wires answer differently is Version, deliberately. A
// job carries the version of its project's LINE, derived from the git tags the
// run resolved; this tool describes a workspace without running anything, so it
// reports the package version the probe read from the project's own manifest.
// Reporting a line version here would mean resolving git per call, and reading
// a run's stamp out of a read-only description is exactly the kind of answer
// that goes stale between two calls.
func toolWorkspaceProjects(ws *workspace.Workspace) []proto.ToolProjectRef {
	if ws == nil {
		return nil
	}
	members := make([]*workspace.Project, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project != nil {
			members = append(members, project)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })

	refs := make([]proto.ToolProjectRef, 0, len(members))
	for _, project := range members {
		refs = append(refs, proto.ToolProjectRef{
			ID:           project.ID,
			Name:         project.Name,
			SourceName:   project.SourceName,
			Version:      project.Version,
			Type:         project.Type,
			Path:         filepath.ToSlash(project.Path),
			Tags:         project.Tags,
			Publish:      project.Publish,
			Extensions:   project.Extensions,
			RunsWith:     project.RunsWith,
			Dependencies: jobs.DirectDependencyIDs(ws, project),
			Config:       encodedProjectConfig(project),
		})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// encodedProjectConfig re-encodes the authored putnami.json the loader parsed.
//
// A config that will not re-encode is omitted rather than reported: the members
// a tool reads out of it are optional refinements (`bin`, `featureAuthority`,
// `options`), and failing a whole tool call over one project's config would turn
// a slightly poorer answer into no answer at all.
func encodedProjectConfig(project *workspace.Project) json.RawMessage {
	if project == nil || project.Config == nil {
		return nil
	}
	encoded, err := json.Marshal(project.Config)
	if err != nil {
		return nil
	}
	return encoded
}
