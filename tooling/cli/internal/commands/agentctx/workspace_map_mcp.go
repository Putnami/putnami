// Package agentctx: the read-only MCP `workspace_map` aggregator.
//
// BuildWorkspaceMapResult renders the workspace orientation map entirely in
// memory — reusing every fragment whose recorded inputs digest still matches the
// working tree and rebuilding the rest, so a warm call is O(changed) and a cold
// one O(projects) — and reports whether the ephemeral
// `.putnami/context-map/repo-map.json` convenience artifact matches that fresh
// render. It shares mapgen.ResolveMap with `context map --print`, so the served
// document and the CLI's render are the same bytes by construction rather than
// by agreement.
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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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
// (scoped to the requested section/project), the freshness of the on-disk
// convenience artifact, and how the answer was produced.
type WorkspaceMapResult struct {
	Document     *mapgen.RepoMap        `json:"document"`
	DiskArtifact DiskArtifactStatus     `json:"diskArtifact"`
	Provenance   WorkspaceMapProvenance `json:"provenance"`
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
	// were validated against the working tree, built ones were recomputed.
	FragmentsReused int `json:"fragmentsReused"`
	FragmentsBuilt  int `json:"fragmentsBuilt"`
}

// BuildWorkspaceMapResult loads the workspace, renders the complete map in
// memory, compares it byte-for-byte to the on-disk document to report freshness,
// and returns it scoped to section (default "all") and, optionally, to one
// project (id or name).
//
// It deliberately does NOT enforce the migration refusal that `putnami
// build` and `putnami context map` apply: those two PRODUCE the map, and a
// retired committed copy is a producer conflict. This tool only reads, from the
// working tree, so refusing to answer an agent's question over a leftover file
// would withhold a correct answer to punish a state the agent cannot fix.
func BuildWorkspaceMapResult(wsRoot, section, projectSelector string) (WorkspaceMapResult, error) {
	// workspace.Load memoizes the workspace for the process lifetime. The CLI is
	// a one-shot process, but the MCP server is long-lived: without invalidating,
	// the first request's project set would be served for the rest of the
	// session, so a project added after the first call would be missing from the
	// map AND the on-disk artifact would be reported fresh against an equally
	// stale render.
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

	resolved, err := mapgen.ResolveMap(ws)
	if err != nil {
		return WorkspaceMapResult{}, err
	}
	freshBytes, err := mapgen.RenderJSON(resolved.Map)
	if err != nil {
		return WorkspaceMapResult{}, err
	}

	// Freshness is a property of the FULL document, so it is measured before the
	// scope is applied: a scoped answer must not report the on-disk map stale
	// merely because the caller asked for one section of it.
	status := DiskArtifactStatus{Path: mapgen.JSONPath}
	onDisk, readErr := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(mapgen.JSONPath)))
	switch {
	case readErr == nil:
		status.Present = true
		status.Fresh = bytes.Equal(onDisk, freshBytes)
	case os.IsNotExist(readErr):
		// Present/Fresh stay false: no build or `context map` has run here yet.
		// The served document is unaffected — that is the point of the in-memory
		// reduce.
	default:
		return WorkspaceMapResult{}, fmt.Errorf("read workspace map artifact %s: %w", mapgen.JSONPath, readErr)
	}

	projectID := ""
	if selector := strings.TrimSpace(projectSelector); selector != "" {
		proj := shared.ResolveProjectSelector(ws, selector)
		if proj == nil {
			return WorkspaceMapResult{}, cmderr.NotFoundf("project not found: %s", selector)
		}
		projectID = proj.ID
	}
	scoped := scopeWorkspaceMap(resolved.Map, section, projectID)

	return WorkspaceMapResult{
		Document:     scoped,
		DiskArtifact: status,
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
// set, onto one project. The result is always a strict SUBSET of the full
// document: it drops fields and entries, never adds or rewrites one, so a scoped
// answer and the full map can never disagree.
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
func scopeWorkspaceMap(full *mapgen.RepoMap, section, projectID string) *mapgen.RepoMap {
	scoped := &mapgen.RepoMap{
		SchemaVersion: full.SchemaVersion,
		Workspace:     full.Workspace,
		GeneratedBy:   full.GeneratedBy,
		Regenerate:    full.Regenerate,
		// Non-nil so a scope that matches nothing serializes as [] rather than
		// null: "no project has an endpoint table" is an answer, "null" is a
		// riddle. The full render is unaffected — it always has entries, and the
		// document's own encoding is untouched.
		Projects: []mapgen.ProjectEntry{},
	}
	if section == MapSectionAll || section == MapSectionDocs {
		scoped.Docs = full.Docs
	}
	if section == MapSectionDocs {
		return scoped
	}
	for _, entry := range full.Projects {
		if projectID != "" && entry.ID != projectID {
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
