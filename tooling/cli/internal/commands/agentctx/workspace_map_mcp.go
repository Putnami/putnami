// Package agentctx: the read-only MCP `workspace_map` aggregator.
//
// BuildWorkspaceMapResult renders the workspace orientation map entirely in
// memory — reusing every fragment the session's cache holds whose recorded
// inputs digest still matches the working tree and rebuilding the rest, so a
// warm call is O(changed) and a cold one O(projects). It reads and writes no
// map file. It shares mapgen.ResolveMap and scopeWorkspaceMap with `context
// map`, so the served document and the printed one are the same by
// construction rather than by agreement.
//
// The MCP server reaches this builder through a dependency-injected closure on
// mcp.Options, wired by the CLI shell (internal/cli/mcp_serve.go). internal/mcp
// must never import internal/commands: MCP is an adapter
// over engine.Run, and internal/engine imports this package for the production
// preflight gate, so an mcp → commands edge would close a cycle. The closure
// returns the already-shaped result as `any`, so the mcp package needs no
// commands type.
package agentctx

import (
	"fmt"
	"strings"

	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/mapgen"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Map sections a caller can ask for. They exist so an agent pays a few KB for a
// scoped question instead of the whole map (~233 KB in a large workspace), which
// is the difference between a tool it calls and a tool it avoids.
const (
	MapSectionAll          = "all"
	MapSectionProjects     = "projects"
	MapSectionDependencies = "dependencies"
	MapSectionIntercalls   = "intercalls"
	MapSectionAPIs         = "apis"
	MapSectionConfigKeys   = "config-keys"
	MapSectionSchemas      = "schemas"
	MapSectionDocs         = "docs"
)

// MapSections is the accepted section list, in the order the error message and
// the tool schema advertise them.
var MapSections = []string{
	MapSectionProjects,
	MapSectionDependencies,
	MapSectionIntercalls,
	MapSectionAPIs,
	MapSectionConfigKeys,
	MapSectionSchemas,
	MapSectionDocs,
	MapSectionAll,
}

// WorkspaceMapResult is the MCP workspace_map payload: the freshly rendered map
// (scoped to the requested section/project) and how the answer was produced.
type WorkspaceMapResult struct {
	Document   *mapgen.RepoMap        `json:"document"`
	Provenance WorkspaceMapProvenance `json:"provenance"`
}

// WorkspaceMapProvenance says what this answer covers and what it cost, so a
// caller can tell a scoped view from the whole map without diffing it, and can
// tell a warm reduce from a cold one.
type WorkspaceMapProvenance struct {
	// Workspace is the workspace name; Revision is the git HEAD the working tree
	// sits on ("unknown" outside a repository).
	Workspace string `json:"workspace,omitempty"`
	Revision  string `json:"revision"`
	// SchemaVersion is the document shape version — a wire contract since the
	// map became MCP-served.
	SchemaVersion int `json:"schemaVersion"`
	// Section and Project echo the applied scope ("all" and "" when unscoped).
	Section string `json:"section"`
	Project string `json:"project,omitempty"`
	// LiveProjects is the workspace's project count; ReturnedProjects is how
	// many survived the scope.
	LiveProjects     int `json:"liveProjects"`
	ReturnedProjects int `json:"returnedProjects"`
	// FragmentsReused / FragmentsBuilt describe the reduce: reused fragments
	// came from the session's cache after their inputs digest matched the
	// working tree, built ones were recomputed.
	FragmentsReused int `json:"fragmentsReused"`
	FragmentsBuilt  int `json:"fragmentsBuilt"`
}

// BuildWorkspaceMapResult loads the workspace, renders the complete map in
// memory, and returns it scoped to section (default "all") and, optionally, to
// one project (id or name). fragments is the session's fragment cache; a nil
// cache builds every fragment.
func BuildWorkspaceMapResult(wsRoot, section, projectSelector string, fragments *mapgen.FragmentCache) (WorkspaceMapResult, error) {
	// workspace.Load memoizes the workspace for the process lifetime. The CLI is
	// a one-shot process, but the MCP server is long-lived: without invalidating,
	// the first request's project set would be served for the rest of the
	// session, so a project added after the first call would be missing from the
	// map.
	workspace.InvalidateLoadCache(wsRoot)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return WorkspaceMapResult{}, fmt.Errorf("load workspace: %w", err)
	}
	// The same refusal `putnami context map` gives: a workspace that declares
	// extensions but has adopted no provider view resolves every project from
	// authored config alone, so the map would carry zero provider-derived edges.
	// Serving that to an agent as "the workspace map" is a confident wrong
	// answer; the actionable `putnami projects sync` is the right one.
	if err := requireResolvedIdentity(ws); err != nil {
		return WorkspaceMapResult{}, err
	}
	section, err = normalizeMapSection(section)
	if err != nil {
		return WorkspaceMapResult{}, err
	}

	projectID := ""
	var keep map[string]bool
	if selector := strings.TrimSpace(projectSelector); selector != "" {
		proj := shared.ResolveProjectSelector(ws, selector)
		if proj == nil {
			return WorkspaceMapResult{}, cmderr.NotFoundf("project not found: %s", selector)
		}
		projectID = proj.ID
		keep = map[string]bool{proj.ID: true}
	}

	resolved, err := mapgen.ResolveMap(ws, fragments)
	if err != nil {
		return WorkspaceMapResult{}, err
	}
	scoped := scopeWorkspaceMap(resolved.Map, section, keep)

	return WorkspaceMapResult{
		Document: scoped,
		Provenance: WorkspaceMapProvenance{
			Workspace:        resolved.Map.Workspace,
			Revision:         resolveWorkspaceRevision(wsRoot),
			SchemaVersion:    resolved.Map.SchemaVersion,
			Section:          section,
			Project:          projectID,
			LiveProjects:     len(resolved.Map.Projects),
			ReturnedProjects: len(scoped.Projects),
			FragmentsReused:  resolved.FragmentsReused,
			FragmentsBuilt:   resolved.FragmentsBuilt,
		},
	}, nil
}

// normalizeMapSection accepts an empty/blank section as "all" and rejects
// anything else by NAMING the valid set — an agent that guessed a section name
// must get the list back, not a shrug.
func normalizeMapSection(section string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(section))
	if normalized == "" {
		return MapSectionAll, nil
	}
	for _, known := range MapSections {
		if normalized == known {
			return normalized, nil
		}
	}
	return "", cmderr.InvalidConfigf("unknown section: %s (want one of: %s)",
		section, strings.Join(MapSections, ", "))
}

// scopeWorkspaceMap projects the full map onto the requested section and, when
// keep is non-nil, onto the projects whose id it holds. The result is always a
// SUBSET of the full document: it drops fields and entries, never adds or
// rewrites one, so a scoped answer and the full map can never disagree. The
// MCP tool and `context map` both print through it.
//
// The header (schema version, workspace, provenance strings) always survives —
// it is a handful of bytes and it is what makes a scoped fragment
// self-describing. An entry that carries nothing for the requested section is
// dropped entirely, which is what makes "apis" cheap in a workspace where three
// projects out of ninety expose one.
//
// The docs index rides with `all` and `docs` only, and is unaffected by the
// project filter: docs are workspace-scoped facts, so filtering them by a
// project would mean inventing an ownership rule the map does not have.
func scopeWorkspaceMap(full *mapgen.RepoMap, section string, keep map[string]bool) *mapgen.RepoMap {
	scoped := &mapgen.RepoMap{
		SchemaVersion: full.SchemaVersion,
		Workspace:     full.Workspace,
		GeneratedBy:   full.GeneratedBy,
		Regenerate:    full.Regenerate,
		// Non-nil so a scope that matches nothing serializes as [] rather than
		// null: "no project has an endpoint table" is an answer, "null" is a
		// riddle.
		Projects: []mapgen.ProjectEntry{},
	}
	if section == MapSectionAll || section == MapSectionDocs {
		scoped.Docs = full.Docs
	}
	if section == MapSectionDocs {
		return scoped
	}
	for _, entry := range full.Projects {
		if keep != nil && !keep[entry.ID] {
			continue
		}
		projected, populated := projectEntrySection(entry, section)
		if !populated {
			continue
		}
		scoped.Projects = append(scoped.Projects, projected)
	}
	return scoped
}

// projectEntrySection keeps one entry's identity core plus the fields the
// requested section asks for, and reports whether the section found anything.
// The identity core (id, path, name, type) always stays: an entry the caller
// cannot address is not an answer.
func projectEntrySection(entry mapgen.ProjectEntry, section string) (mapgen.ProjectEntry, bool) {
	if section == MapSectionAll {
		return entry, true
	}
	scoped := mapgen.ProjectEntry{
		ID:   entry.ID,
		Path: entry.Path,
		Name: entry.Name,
		Type: entry.Type,
	}
	switch section {
	case MapSectionProjects:
		scoped.Description = entry.Description
		scoped.Summary = entry.Summary
		scoped.Readme = entry.Readme
		scoped.Tags = entry.Tags
		scoped.Extensions = entry.Extensions
		// Every live project is an answer to "which projects exist", including
		// one that declares nothing beyond its name.
		return scoped, true
	case MapSectionDependencies:
		scoped.DependsOn = entry.DependsOn
		scoped.Dependents = entry.Dependents
		return scoped, len(scoped.DependsOn) > 0 || len(scoped.Dependents) > 0
	case MapSectionIntercalls:
		scoped.Intercalls = entry.Intercalls
		scoped.CalledBy = entry.CalledBy
		return scoped, len(scoped.Intercalls) > 0 || len(scoped.CalledBy) > 0
	case MapSectionAPIs:
		scoped.Endpoints = entry.Endpoints
		return scoped, len(scoped.Endpoints) > 0
	case MapSectionConfigKeys:
		scoped.ConfigKeys = entry.ConfigKeys
		return scoped, len(scoped.ConfigKeys) > 0
	case MapSectionSchemas:
		scoped.Schemas = entry.Schemas
		return scoped, len(scoped.Schemas) > 0
	}
	return scoped, false
}
