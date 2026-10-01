package wsview

import (
	"encoding/json"

	proto "go.putnami.dev/protocol/extension"
	workspaceproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
)

// The MCP tool half of this view.
//
// An agent's tool call reaches this extension over a DIFFERENT wire from a job's:
// one ToolCallRequest on stdin instead of a job-context document, with no
// project, no task identity and no staging tree — because a tool call is not a
// job. What it does share with a job is the question it asks, so it carries the
// same two answers under the same two names: `workspaceProjects`, the complete
// resolved membership, and `selection`, the projection the caller's arguments
// resolved to.
//
// Both are the ORCHESTRATOR's answers. That is the whole point: resolving
// `impacted` means diffing a baseline, mapping paths to owners and walking the
// dependent graph, and resolving `projects` means matching selectors against the
// workspace's own identities. An extension that did either would be a second
// definition of what the workspace contains.
//
// The tool wire carries more RESOLVED authored members directly (`tags`,
// `publish`, and friends), while both wires carry the raw project config. A
// workspace-answering surface can therefore read a sibling's `bin`,
// `featureAuthority`, and `options` without rediscovering its manifest.

// FromToolRequest builds the view an MCP tool call is entitled to, from the
// request and nothing else: no directory scan, no `putnami` subprocess.
//
// The membership may legitimately be EMPTY — a workspace with no projects is a
// workspace — so emptiness is not an error here. What distinguishes "no members"
// from "nobody answered" is ToolSelection below, which is why every caller
// checks that first.
func FromToolRequest(request proto.ToolCallRequest) *Workspace {
	projects := make([]*Project, 0, len(request.WorkspaceProjects))
	for _, ref := range request.WorkspaceProjects {
		id := ref.ID
		if id == "" {
			id = ProjectIDFromPath(ref.Path)
		}
		projects = append(projects, &Project{
			ID:         id,
			Name:       ref.Name,
			SourceName: ref.SourceName,
			Version:    ref.Version,
			Type:       ref.Type,
			Path:       CleanWorkspacePath(ref.Path),
			Tags:       append([]string(nil), ref.Tags...),
			Publish:    append([]string(nil), ref.Publish...),
			Extensions: append([]string(nil), ref.Extensions...),
			RunsWith:   append([]string(nil), ref.RunsWith...),
			// The RESOLVED direct edges, already translated from declared names
			// to workspace ids, so BuildGraph indexes core's graph rather than a
			// second reading of anybody's manifest.
			Dependencies: append([]string(nil), ref.Dependencies...),
			Config:       decodeProjectConfig(ref.Config),
		})
	}
	return NewWorkspace(request.WorkspaceRoot, nil, projects)
}

// ToolSelection reports the projection this call runs over, and whether the
// orchestrator resolved one at all.
//
// The boolean is the fail-closed marker. A CLI older than the member sends no
// `selection`, and a tool that read that absence as "the whole workspace" would
// answer a narrowed question with an unnarrowed answer — quietly, and with a
// well-formed payload. Every caller refuses instead.
func ToolSelection(request proto.ToolCallRequest) (selection pctx.Selection, resolved bool) {
	if request.Selection == nil {
		return pctx.Selection{}, false
	}
	return pctx.Selection{
		Mode:           request.Selection.Mode,
		Scoped:         request.Selection.Scoped,
		Baseline:       request.Selection.Baseline,
		BaselineSource: request.Selection.BaselineSource,
		ProjectIDs:     request.Selection.ProjectIDs,
		EmptyImpact:    request.Selection.EmptyImpact,
	}, true
}

// decodeProjectConfig parses the authored putnami.json the wire carries raw.
//
// A config that will not decode is dropped rather than reported: its members are
// refinements a consumer reads when present (`bin`, `featureAuthority`,
// `options`), and failing a whole answer over one project's config would trade
// a slightly poorer answer for no answer at all — the same choice both
// producers make when they decline to encode one.
func decodeProjectConfig(raw json.RawMessage) *workspaceproto.ProjectConfig {
	if len(raw) == 0 {
		return nil
	}
	var config workspaceproto.ProjectConfig
	if json.Unmarshal(raw, &config) != nil {
		return nil
	}
	return &config
}
