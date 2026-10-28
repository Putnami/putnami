package mapgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
)

// Fragment is one project's map contribution.
type Fragment struct {
	// Project is the project-scoped half of a map entry.
	Project FragmentProject `json:"project"`
}

// FragmentProject is everything about a project that is derivable WITHOUT
// looking at any other project. The reduce turns it into a ProjectEntry by
// resolving Dependencies against the live set and adding the reverse edge.
type FragmentProject struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Readme      string   `json:"readme,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Extensions  []string `json:"extensions,omitempty"`
	// Dependencies are the declared dependency NAMES, not ids: resolving a name
	// to a project is a workspace-wide question and belongs to the reduce.
	Dependencies []string    `json:"dependencies,omitempty"`
	Endpoints    []Endpoint  `json:"endpoints,omitempty"`
	ConfigKeys   []ConfigKey `json:"configKeys,omitempty"`
	// Schemas are the project-relative paths of the JSON artifacts committed
	// directly under SchemaDir, sorted.
	Schemas []string `json:"schemas,omitempty"`
}

// path3 joins slash-formed path segments without touching the OS separator.
func path3(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.Trim(filepath.ToSlash(p), "/"); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "/")
}

// readInputs reads the declared project-scoped inputs once, returning the file
// contents keyed by project-relative path (absent files are missing from the
// map) and the schema listing.
func readInputs(projDir string) (map[string][]byte, []string, error) {
	contents := make(map[string][]byte, len(ProjectInputFiles))
	for _, rel := range ProjectInputFiles {
		data, err := os.ReadFile(filepath.Join(projDir, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("read map input %s: %w", rel, err)
		}
		contents[rel] = data
	}

	schemas, err := readSchemaListing(projDir)
	if err != nil {
		return nil, nil, err
	}
	return contents, schemas, nil
}

// readSchemaListing lists the JSON artifacts committed directly under
// <project>/schema/ (non-recursive: every such directory in this tree is flat)
// and returns them as sorted project-relative paths. A missing (or
// non-directory) schema/ lists nothing.
func readSchemaListing(projDir string) ([]string, error) {
	dir := filepath.Join(projDir, filepath.FromSlash(SchemaDir))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil //nolint:nilerr // no schema/ directory lists nothing, it is not a failure
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read map input %s/: %w", SchemaDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	rels := make([]string, 0, len(names))
	for _, name := range names {
		rels = append(rels, SchemaDir+"/"+name)
	}
	if len(rels) == 0 {
		rels = nil
	}
	return rels, nil
}

// identityView is the normalized projection of the workspace-resolved project
// identity the fragment consumes.
type identityView struct {
	ID           string
	Path         string
	Name         string
	Type         string
	Description  string
	Tags         []string
	Extensions   []string
	Dependencies []string
}

// projectIdentity normalizes a workspace project into the identity view: sorted,
// deduped, slash-formed, with the authored description folded in.
func projectIdentity(p *workspace.Project) identityView {
	description := ""
	if p.Config != nil {
		description = strings.TrimSpace(p.Config.Description)
	}
	return identityView{
		ID:           p.ID,
		Path:         filepath.ToSlash(p.Path),
		Name:         p.Name,
		Type:         p.Type,
		Description:  description,
		Tags:         sortedSet(p.Tags),
		Extensions:   sortedSet(p.Extensions),
		Dependencies: sortedSet(p.Dependencies),
	}
}

// BuildFragment computes a project's fragment from its four declared inputs plus
// its workspace-resolved identity. It is a pure function of those inputs: no
// timestamps, no absolute paths, no CLI version.
func BuildFragment(wsRoot string, p *workspace.Project) (*Fragment, error) {
	projDir := filepath.Join(wsRoot, filepath.FromSlash(p.Path))
	contents, schemas, err := readInputs(projDir)
	if err != nil {
		return nil, err
	}
	view := projectIdentity(p)

	projPath := filepath.ToSlash(p.Path)
	project := FragmentProject{
		ID:           view.ID,
		Path:         view.Path,
		Name:         view.Name,
		Type:         view.Type,
		Description:  view.Description,
		Tags:         view.Tags,
		Extensions:   view.Extensions,
		Dependencies: view.Dependencies,
		Schemas:      schemas,
	}
	if readme, ok := contents["README.md"]; ok {
		project.Readme = path3(projPath, "README.md")
		project.Summary = readmeSummary(readme)
	}
	if openapi, ok := contents["schema/openapi.json"]; ok {
		endpoints, err := parseOpenAPI(openapi)
		if err != nil {
			return nil, fmt.Errorf("parse %s/schema/openapi.json: %w", projPath, err)
		}
		project.Endpoints = endpoints
	}
	if schema, ok := contents["schema/config.jsonschema.json"]; ok {
		keys, err := parseConfigSchema(schema)
		if err != nil {
			return nil, fmt.Errorf("parse %s/schema/config.jsonschema.json: %w", projPath, err)
		}
		project.ConfigKeys = keys
	}

	return &Fragment{Project: project}, nil
}

// sortedSet returns the sorted, deduped, blank-free copy of items, or nil when
// nothing survives — the one normalization every emitted string list uses, so
// declaration order can never reach the output bytes.
func sortedSet(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
