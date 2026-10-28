package collaboration

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The workspace view a provider tool that declares workspaceSelection
// receives on its request.
//
// An extension has no workspace loader and must never grow one: two
// derivations of the same fact is how a tool and the orchestrator start
// disagreeing about what the workspace contains. So the orchestrator, which
// holds the answer already, resolves it here and publishes it, exactly as it
// does for a job subprocess.

// ProjectSelection is the project narrowing a request carries, already
// resolved into canonical project ids. It is declared here — not imported from
// the command subtree — because this package holds no edge into
// internal/commands, so the CLI shell maps it onto the selection value the
// shared resolver takes.
type ProjectSelection struct {
	// Projects is a comma-separated list of canonical project ids.
	Projects string
	// Impacted asks for the projects the current change reaches.
	Impacted bool
	// Baseline is the git ref backing Impacted.
	Baseline string
}

// toolSelectionArgs is the canonical selection vocabulary a request narrows
// with: the projects, impacted and baseline members.
//
// It is decoded LENIENTLY: the tool owns the rest of its argument object and
// the orchestrator has no business rejecting a member it does not recognize. A
// member that is present but ill-typed is still an error — that is the tool's
// own vocabulary being misused.
type toolSelectionArgs struct {
	Projects []string `json:"projects"`
	Impacted bool     `json:"impacted"`
	Baseline string   `json:"baseline"`
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
		return fmt.Errorf("extension tool %s declares workspaceSelection, but this entry path has no selection resolver",
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

// catalogSelectionIn resolves a request's project narrowing against an
// already-loaded workspace, so one call reads the tree once.
//
// The two guards are the ORCHESTRATOR's, not the tool's. `projects` and
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

// resolveProject looks up a project by canonical id first, then by name.
func resolveProject(ws *workspace.Workspace, selector string) *workspace.Project {
	if p := ws.ProjectByID(selector); p != nil {
		return p
	}
	return ws.ProjectByName(selector)
}

// toolWorkspaceProjects projects the loaded workspace onto the tool wire, in
// canonical project-id order.
//
// It is the same projection cachekey.WorkspaceProjectsContext makes for a job
// subprocess, plus the authored project config — which a job never needs,
// because a job acts on its own project, and a workspace-answering tool always
// does, because it reports ON other projects.
//
// Two members differ between the two wires, deliberately. The first is
// Version. A job carries the version of its project's LINE, derived from the
// git tags the run resolved; this tool describes a workspace without running
// anything, so it reports the package version the probe read from the
// project's own manifest.
// Reporting a line version here would mean resolving git per call, and reading
// a run's stamp out of a read-only description is exactly the kind of answer
// that goes stale between two calls.
//
// The second is Type. This tool describes, so it leaves an unclassified
// project's type empty and a consumer applies the workspace default. A job acts
// on the membership, so its wire always names the type and an absent one means
// an older producer the job must decline.
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
			Dependencies: cachekey.DirectDependencyIDs(ws, project),
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
