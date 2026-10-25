package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

const (
	workspaceContextURI = "workspace://context"

	// workspaceGuidanceURI serves the Putnami block body that `putnami context
	// generate` writes into AGENTS.md and CLAUDE.md.
	workspaceGuidanceURI = "putnami://workspace/guidance"
	// workspaceMapURI serves the whole workspace map; workspaceMapSectionPrefix
	// plus a section name serves one slice of it.
	workspaceMapURI           = "putnami://workspace/map"
	workspaceMapSectionPrefix = workspaceMapURI + "/"
	// projectContextPrefix and projectContextSuffix frame the escaped canonical
	// project id of a project's agent-context resource.
	projectContextPrefix = "putnami://projects/"
	projectContextSuffix = "/context"
)

// workspaceMapSections are the sections the workspace_map tool accepts, in the
// tool schema's order, minus "all": the whole map has its own URI. A test pins
// this list to the tool's schema enum and to the builder's section list.
var workspaceMapSections = []string{"projects", "dependencies", "intercalls", "apis", "config-keys", "schemas", "docs"}

// projectContextURI returns the URI of one project's agent-context resource.
// The id is the canonical project id (for example "/tooling/cli"), the same
// value list_projects reports. It contains slashes, so it is escaped as one
// path segment: "/tooling/cli" becomes "%2Ftooling%2Fcli".
func projectContextURI(id string) string {
	return projectContextPrefix + url.PathEscape(id) + projectContextSuffix
}

// projectIDFromContextURI returns the canonical project id a project-context
// URI names, or false when the URI has another shape.
func projectIDFromContextURI(uri string) (string, bool) {
	if !strings.HasPrefix(uri, projectContextPrefix) || !strings.HasSuffix(uri, projectContextSuffix) {
		return "", false
	}
	escaped := strings.TrimSuffix(strings.TrimPrefix(uri, projectContextPrefix), projectContextSuffix)
	if escaped == "" || strings.Contains(escaped, "/") {
		return "", false
	}
	id, err := url.PathUnescape(escaped)
	if err != nil || id == "" {
		return "", false
	}
	return id, true
}

// resourceDefs lists the resources. It stays cheap: it prepares no workspace
// graph. A resource whose builder is not injected is left out, so the list
// never offers a URI that a read would refuse.
func (s *Server) resourceDefs() []Resource {
	resources := []Resource{{
		URI:         workspaceContextURI,
		Name:        "Putnami workspace context",
		Description: "Live workspace graph, layout, publish channels, and pinned versions derived from the same workspace load path as the CLI.",
		MimeType:    "application/json",
	}}
	if s.opts.WorkspaceGuidance != nil {
		resources = append(resources, Resource{
			URI:         workspaceGuidanceURI,
			Name:        "Putnami workspace guidance",
			Description: "The Putnami guidance block of AGENTS.md as `putnami context generate` renders it now (the rules, the gate line, and the line that names the decisions.json files), then the settled decisions.",
			MimeType:    "text/markdown",
		})
	}
	if s.opts.WorkspaceMap != nil {
		resources = append(resources, Resource{
			URI:         workspaceMapURI,
			Name:        "Putnami workspace map",
			Description: "The whole workspace map, as the workspace_map tool returns it for section all.",
			MimeType:    "application/json",
		})
		for _, section := range workspaceMapSections {
			resources = append(resources, Resource{
				URI:         workspaceMapSectionPrefix + section,
				Name:        "Putnami workspace map: " + section,
				Description: "The " + section + " section of the workspace map, as the workspace_map tool returns it.",
				MimeType:    "application/json",
			})
		}
	}
	if s.opts.AgentContext != nil {
		for _, project := range s.listedProjects() {
			resources = append(resources, Resource{
				URI:         projectContextURI(project.ID),
				Name:        "Putnami project context: " + project.ID,
				Description: "The agent-context document of project " + project.ID + ", as the agent_context tool returns it.",
				MimeType:    "application/json",
			})
		}
	}
	for _, entry := range s.guidanceResources() {
		resources = append(resources, entry.resource)
	}
	return resources
}

// listedProjects reads the workspace's projects for the resource list and for
// the project check of a read. It skips PrepareWorkspaceView, so it never
// waits on graph preparation. It drops the load memo first, as the builders
// do: the server is long-lived, and a project added to or removed from the
// workspace after the first request must change the list and the check the
// way it changes the agent_context tool. A workspace that does not load lists
// no projects; the read of a project resource reports the load failure.
func (s *Server) listedProjects() []*workspace.Project {
	workspace.InvalidateLoadCache(s.opts.WorkspaceRoot)
	ws, err := workspace.Load(s.opts.WorkspaceRoot)
	if err != nil {
		return nil
	}
	s.noteWorkspaceLoad(ws)
	projects := append([]*workspace.Project(nil), ws.Projects...)
	sort.Slice(projects, func(i, j int) bool { return projects[i].Path < projects[j].Path })
	return projects
}

func (s *Server) handleResourcesRead(ctx context.Context, msg *rpcMessage) *rpcResponse {
	var p readResourceParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return s.fail(msg.ID, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if resp, handled := s.readWorkspaceResource(ctx, msg.ID, p.URI); handled {
		return resp
	}
	if p.URI != workspaceContextURI {
		for _, entry := range s.guidanceResources() {
			if entry.resource.URI != p.URI {
				continue
			}
			return s.ok(msg.ID, readResourceResult{Contents: []resourceContent{{
				URI: p.URI, MimeType: entry.resource.MimeType, Text: entry.content,
			}}})
		}
		return s.fail(msg.ID, codeInvalidParams, "unknown resource: "+p.URI)
	}
	data, err := s.workspaceContextResource(ctx)
	if err != nil {
		return s.fail(msg.ID, codeInternalError, fmt.Sprintf("read %s: %v", p.URI, err))
	}
	text, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return s.fail(msg.ID, codeInternalError, "encode resource: "+err.Error())
	}
	return s.ok(msg.ID, readResourceResult{
		Contents: []resourceContent{{
			URI:      p.URI,
			MimeType: "application/json",
			Text:     string(text),
		}},
	})
}

// readWorkspaceResource answers the guidance, map and project-context URIs. It
// reports handled=false for any other URI, and for a URI of these shapes whose
// builder is not injected, so those fall through to "unknown resource".
func (s *Server) readWorkspaceResource(ctx context.Context, id json.RawMessage, uri string) (*rpcResponse, bool) {
	switch {
	case uri == workspaceGuidanceURI:
		if s.opts.WorkspaceGuidance == nil {
			return nil, false
		}
		return s.ok(id, readResourceResult{Contents: []resourceContent{{
			URI: uri, MimeType: "text/markdown", Text: s.opts.WorkspaceGuidance(ctx),
		}}}), true
	case uri == workspaceMapURI || strings.HasPrefix(uri, workspaceMapSectionPrefix):
		section := "all"
		if uri != workspaceMapURI {
			section = strings.TrimPrefix(uri, workspaceMapSectionPrefix)
			if !containsString(workspaceMapSections, section) {
				return nil, false
			}
		}
		if s.opts.WorkspaceMap == nil {
			return nil, false
		}
		return s.readGraphResource(ctx, id, uri, func() (any, error) {
			return s.opts.WorkspaceMap(ctx, section, "")
		}), true
	}
	projectID, ok := projectIDFromContextURI(uri)
	if !ok || s.opts.AgentContext == nil {
		return nil, false
	}
	// Check the id before graph preparation, which can take up to a minute.
	known := false
	for _, project := range s.listedProjects() {
		known = known || project.ID == projectID
	}
	if !known {
		return nil, false
	}
	return s.readGraphResource(ctx, id, uri, func() (any, error) {
		return s.opts.AgentContext(ctx, projectID)
	}), true
}

// readGraphResource reads a resource that the matching tool builds from the
// workspace graph. It takes the tool's path: prepare the graph view, require a
// recorded view, then call the injected builder, and it encodes the result the
// way tools/call does, so a resource and its tool return the same bytes.
func (s *Server) readGraphResource(ctx context.Context, id json.RawMessage, uri string, build func() (any, error)) *rpcResponse {
	ws, err := s.loadWorkspace(ctx)
	if err != nil {
		return s.fail(id, codeInternalError, fmt.Sprintf("read %s: load workspace: %v", uri, err))
	}
	if _, err := s.recordedView(ws); err != nil {
		return s.fail(id, codeInternalError, fmt.Sprintf("read %s: %v", uri, err))
	}
	data, err := build()
	if err != nil {
		return s.fail(id, codeInternalError, fmt.Sprintf("read %s: %v", uri, err))
	}
	text, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return s.fail(id, codeInternalError, "encode resource: "+err.Error())
	}
	return s.ok(id, readResourceResult{Contents: []resourceContent{{
		URI: uri, MimeType: "application/json", Text: string(text),
	}}})
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

type guidanceResource struct {
	resource Resource
	content  string
}

func (s *Server) guidanceResources() []guidanceResource {
	selection := s.currentSelection()
	entries := make([]guidanceResource, 0, len(selection.ExtensionGuidance)+len(selection.GuidanceIssues))
	available := make(map[string]bool, len(selection.ExtensionGuidance))
	for _, guide := range selection.ExtensionGuidance {
		if guide.Name == "" || guide.Version == "" || guide.Content == "" {
			continue
		}
		available[guide.Name+"\x00"+guide.Version] = true
		entries = append(entries, guidanceResource{
			resource: Resource{
				URI:         extensionGuidanceURI(guide.Name, guide.Version),
				Name:        guide.Name + "@" + guide.Version + " AI guidance",
				Description: "Exact AI.md bytes from the lock-pinned extension artifact.",
				MimeType:    "text/markdown",
			},
			content: guide.Content,
		})
	}
	for _, issue := range selection.GuidanceIssues {
		if issue.Name == "" || available[issue.Name+"\x00"+issue.Version] {
			continue
		}
		content, _ := json.Marshal(map[string]string{
			"status":    "unavailable",
			"extension": issue.Name,
			"version":   issue.Version,
			"reason":    issue.Reason,
			"fallback":  "continue with Putnami local workspace tools; do not substitute guidance from latest",
		})
		entries = append(entries, guidanceResource{
			resource: Resource{
				URI:         extensionGuidanceURI(issue.Name, issue.Version),
				Name:        guidanceStatusResourceName(issue),
				Description: "The exact lock-pinned AI.md is unavailable; read for the bounded failure reason.",
				MimeType:    "application/json",
			},
			content: string(content),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].resource.URI < entries[j].resource.URI })
	return entries
}

// guidanceStatusResourceName avoids rendering a dangling "@" for an extension
// whose lock pin is absent: there is no version to report, and naming one that
// does not exist is exactly the substitution this resource exists to refuse.
func guidanceStatusResourceName(issue GuidanceIssue) string {
	if issue.Version == "" {
		return issue.Name + " AI guidance status (no lock pin)"
	}
	return issue.Name + "@" + issue.Version + " AI guidance status"
}

func extensionGuidanceURI(name, version string) string {
	values := url.Values{}
	values.Set("name", name)
	values.Set("version", version)
	return "putnami://extension-guidance/AI.md?" + values.Encode()
}
