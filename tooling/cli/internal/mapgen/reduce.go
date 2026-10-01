package mapgen

import (
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
)

// Reduce renders the complete map from the LIVE project set plus one fragment
// per live project.
//
// It is a total function of its arguments: a project the workspace manifest no
// longer enumerates contributes nothing (deletions and renames are structural,
// not diffed), a live project with no fragment is skipped rather than guessed
// at, and the previously rendered documents are never consulted. Entries are sorted by
// project path, so the emitted bytes are stable regardless of discovery order.
func Reduce(ws *workspace.Workspace, fragments map[string]*Fragment) (*RepoMap, error) {
	entries := make([]ProjectEntry, 0, len(ws.Projects))
	for _, p := range ws.Projects {
		fragment, ok := fragments[p.ID]
		if !ok || fragment == nil {
			continue
		}
		entries = append(entries, entryFromFragment(ws, fragment))
	}
	sortEntries(entries)
	linkDependents(entries)
	linkIntercalls(entries)

	docs, err := indexDocs(ws.Root, projectReadmePaths(ws))
	if err != nil {
		return nil, err
	}

	return &RepoMap{
		SchemaVersion: SchemaVersion,
		Workspace:     ws.Name,
		GeneratedBy:   GeneratedBy,
		Regenerate:    Regenerate,
		Projects:      entries,
		Docs:          docs,
	}, nil
}

// entryFromFragment lifts a fragment into a map entry, resolving the declared
// dependency NAMES to live project ids. A name that resolves to no live project
// is an external dependency (or a dangling reference) and is dropped: the map's
// dependency edges are workspace edges, and emitting an id-shaped value for a
// project that does not exist would make the graph lie.
func entryFromFragment(ws *workspace.Workspace, f *Fragment) ProjectEntry {
	p := f.Project
	return ProjectEntry{
		ID:          p.ID,
		Path:        p.Path,
		Name:        p.Name,
		Type:        p.Type,
		Description: p.Description,
		Summary:     p.Summary,
		Readme:      p.Readme,
		Tags:        append([]string(nil), p.Tags...),
		Extensions:  append([]string(nil), p.Extensions...),
		DependsOn:   resolveDependencyIDs(ws, p.Dependencies),
		Endpoints:   append([]Endpoint(nil), p.Endpoints...),
		ConfigKeys:  append([]ConfigKey(nil), p.ConfigKeys...),
		Schemas:     append([]string(nil), p.Schemas...),
	}
}

// projectReadmePaths is the set of workspace-relative README paths that a
// PROJECT ROOT owns. The document index excludes them because the project entry
// already carries each one as `readme`/`summary`; indexing them again would say
// the same thing twice and bury the scope-level and sub-project READMEs that
// have no other representation in the map.
func projectReadmePaths(ws *workspace.Workspace) map[string]bool {
	owned := make(map[string]bool, len(ws.Projects))
	for _, p := range ws.Projects {
		if p == nil {
			continue
		}
		dir := strings.Trim(filepath.ToSlash(p.Path), "/")
		if dir == "." {
			dir = ""
		}
		owned[path3(dir, readmeFilename)] = true
	}
	return owned
}

// resolveDependencyIDs maps declared dependency names (or already-id-shaped
// references) onto live workspace project ids, deduped and sorted.
func resolveDependencyIDs(ws *workspace.Workspace, refs []string) []string {
	seen := make(map[string]bool, len(refs))
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		var id string
		if p := ws.ProjectByName(ref); p != nil {
			id = p.ID
		} else if p := ws.ProjectByID(ref); p != nil {
			id = p.ID
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	return ids
}

// linkDependents fills each entry's Dependents from the others' DependsOn. It is
// the one field that cannot exist in a fragment: the reverse edge is a property
// of the whole live set, which is exactly why the reduce owns it.
func linkDependents(entries []ProjectEntry) {
	byID := make(map[string]int, len(entries))
	for i := range entries {
		byID[entries[i].ID] = i
	}
	for i := range entries {
		for _, dep := range entries[i].DependsOn {
			if j, ok := byID[dep]; ok {
				entries[j].Dependents = append(entries[j].Dependents, entries[i].ID)
			}
		}
	}
	for i := range entries {
		entries[i].Dependents = sortedSet(entries[i].Dependents)
	}
}

// sortEntries imposes the map's canonical order: by project path, with the id as
// a tiebreak so the ordering is total even if two projects ever share a path.
func sortEntries(entries []ProjectEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].ID < entries[j].ID
	})
}

// LiveProjects returns the workspace's project set in canonical (path, id)
// order — the "live set" the reduce enumerates. Callers use it so the reduce and
// the fragment refresh agree on what a project is.
func LiveProjects(ws *workspace.Workspace) []*workspace.Project {
	live := make([]*workspace.Project, 0, len(ws.Projects))
	live = append(live, ws.Projects...)
	sort.Slice(live, func(i, j int) bool {
		pi, pj := filepath.ToSlash(live[i].Path), filepath.ToSlash(live[j].Path)
		if pi != pj {
			return pi < pj
		}
		return live[i].ID < live[j].ID
	})
	return live
}
