package agentctx

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/mapgen"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestBuildWorkspaceMapResult_FullDocumentMatchesTheCLIRender is the verification
// criterion the tool exists to satisfy: the unscoped document served over MCP is
// byte-identical to what `putnami context map` writes, so an agent and a human
// can never be looking at two different maps. It also pins that the answer comes
// from the working tree, not from the file: the map is served before any build
// has written one.
func TestBuildWorkspaceMapResult_FullDocumentMatchesTheCLIRender(t *testing.T) {
	root := mapCommandWorkspace(t)

	cold, err := BuildWorkspaceMapResult(root, "", "")
	if err != nil {
		t.Fatalf("workspace_map on a cold workspace: %v", err)
	}
	if cold.DiskArtifact.Present || cold.DiskArtifact.Fresh {
		t.Errorf("no build has run here, yet the artifact reports %+v", cold.DiskArtifact)
	}
	if len(cold.Document.Projects) != 2 {
		t.Fatalf("cold document carries %d projects, want 2", len(cold.Document.Projects))
	}
	if cold.Provenance.FragmentsBuilt != 2 || cold.Provenance.FragmentsReused != 0 {
		t.Errorf("provenance = %+v, want a cold reduce that built every fragment", cold.Provenance)
	}

	if err := ContextMapCommand(root, ContextMapOptions{}); err != nil {
		t.Fatalf("context map: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(mapgen.JSONPath)))
	if err != nil {
		t.Fatalf("read map: %v", err)
	}

	warm, err := BuildWorkspaceMapResult(root, MapSectionAll, "")
	if err != nil {
		t.Fatalf("workspace_map: %v", err)
	}
	served, err := mapgen.RenderJSON(warm.Document)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if string(served) != string(written) {
		t.Errorf("the served document is not byte-identical to the CLI render:\n--- CLI:\n%s\n--- MCP:\n%s",
			written, served)
	}
	if !warm.DiskArtifact.Present || !warm.DiskArtifact.Fresh {
		t.Errorf("the artifact the CLI just wrote is reported %+v", warm.DiskArtifact)
	}
	if warm.DiskArtifact.Path != mapgen.JSONPath {
		t.Errorf("artifact path = %q, want %q", warm.DiskArtifact.Path, mapgen.JSONPath)
	}
	if warm.Provenance.FragmentsReused != 2 || warm.Provenance.FragmentsBuilt != 0 {
		t.Errorf("provenance = %+v, want a warm reduce that reused every fragment", warm.Provenance)
	}
	if warm.Provenance.Section != MapSectionAll || warm.Provenance.Project != "" {
		t.Errorf("provenance scope = %+v, want the unscoped default", warm.Provenance)
	}
	if warm.Provenance.SchemaVersion != mapgen.SchemaVersion || warm.Provenance.Workspace != "example" {
		t.Errorf("provenance = %+v, want the workspace name and schema version", warm.Provenance)
	}
}

// TestBuildWorkspaceMapResult_StaleArtifactIsReportedNotServed pins the
// separation the whole design rests on: an out-of-date file on disk changes the
// artifact STATUS, never the served document.
func TestBuildWorkspaceMapResult_StaleArtifactIsReportedNotServed(t *testing.T) {
	root := mapCommandWorkspace(t)
	if err := ContextMapCommand(root, ContextMapOptions{}); err != nil {
		t.Fatalf("context map: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash("svc/lib/README.md")),
		[]byte("# Lib\n\nA shared library.\n"), 0o644); err != nil {
		t.Fatalf("edit lib README: %v", err)
	}

	result, err := BuildWorkspaceMapResult(root, "", "")
	if err != nil {
		t.Fatalf("workspace_map: %v", err)
	}
	if !result.DiskArtifact.Present || result.DiskArtifact.Fresh {
		t.Errorf("artifact = %+v, want present but stale", result.DiskArtifact)
	}
	served, err := mapgen.RenderJSON(result.Document)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(string(served), "A shared library.") {
		t.Error("the served document did not reflect the working tree")
	}
	onDisk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(mapgen.JSONPath)))
	if err != nil {
		t.Fatalf("read map: %v", err)
	}
	if strings.Contains(string(onDisk), "A shared library.") {
		t.Error("a read-only tool rewrote the on-disk map")
	}
}

// TestBuildWorkspaceMapResult_ScopingReturnsStrictSubsets is the other half of
// the verification criterion: every section and project scope drops entries and
// fields from the full document and never adds, renames, or rewrites one.
func TestBuildWorkspaceMapResult_ScopingReturnsStrictSubsets(t *testing.T) {
	root := mapCommandWorkspace(t)
	full, err := BuildWorkspaceMapResult(root, MapSectionAll, "")
	if err != nil {
		t.Fatalf("workspace_map (all): %v", err)
	}
	fullEntries := entriesByID(full.Document)
	fullSize := len(renderForSize(t, full.Document))

	for _, section := range MapSections {
		t.Run(section, func(t *testing.T) {
			scoped, err := BuildWorkspaceMapResult(root, section, "")
			if err != nil {
				t.Fatalf("workspace_map (%s): %v", section, err)
			}
			assertSubsetOfFull(t, scoped.Document, fullEntries)
			if scoped.Provenance.Section != section {
				t.Errorf("provenance section = %q, want %q", scoped.Provenance.Section, section)
			}
			if scoped.Provenance.LiveProjects != len(full.Document.Projects) {
				t.Errorf("liveProjects = %d, want the whole workspace (%d)",
					scoped.Provenance.LiveProjects, len(full.Document.Projects))
			}
			if scoped.Provenance.ReturnedProjects != len(scoped.Document.Projects) {
				t.Errorf("returnedProjects = %d, but the document carries %d",
					scoped.Provenance.ReturnedProjects, len(scoped.Document.Projects))
			}
			if section == MapSectionAll {
				return
			}
			// A scope is only worth having if it is cheaper than the whole map.
			if size := len(renderForSize(t, scoped.Document)); size >= fullSize {
				t.Errorf("section %q rendered %d bytes, not smaller than the full map (%d)", section, size, fullSize)
			}
			if section != MapSectionDocs && len(scoped.Document.Docs) != 0 {
				t.Errorf("section %q carried the workspace docs index", section)
			}
		})
	}

	// Sections that have no content for a project drop it entirely; "projects"
	// keeps every live project because existing IS the answer there.
	byName := map[string]int{}
	for _, section := range []string{MapSectionProjects, MapSectionAPIs, MapSectionConfigKeys, MapSectionDependencies} {
		scoped, err := BuildWorkspaceMapResult(root, section, "")
		if err != nil {
			t.Fatalf("workspace_map (%s): %v", section, err)
		}
		byName[section] = len(scoped.Document.Projects)
	}
	if byName[MapSectionProjects] != 2 {
		t.Errorf("section projects returned %d projects, want every live one", byName[MapSectionProjects])
	}
	if byName[MapSectionAPIs] != 1 {
		t.Errorf("section apis returned %d projects, want only the one with endpoints", byName[MapSectionAPIs])
	}
	if byName[MapSectionConfigKeys] != 1 {
		t.Errorf("section config-keys returned %d projects, want only the one with a config schema", byName[MapSectionConfigKeys])
	}
	if byName[MapSectionDependencies] != 2 {
		t.Errorf("section dependencies returned %d projects, want both ends of the edge", byName[MapSectionDependencies])
	}

	// docs is the workspace-scoped section: no project entries at all.
	docs, err := BuildWorkspaceMapResult(root, MapSectionDocs, "")
	if err != nil {
		t.Fatalf("workspace_map (docs): %v", err)
	}
	if len(docs.Document.Projects) != 0 {
		t.Errorf("section docs returned %d project entries", len(docs.Document.Projects))
	}
}

// TestBuildWorkspaceMapResult_ProjectFilterNarrowsToOneEntry pins the project
// scope: one entry, byte-equal to that entry in the full map, and the header
// still there so the fragment describes itself.
func TestBuildWorkspaceMapResult_ProjectFilterNarrowsToOneEntry(t *testing.T) {
	root := mapCommandWorkspace(t)
	full, err := BuildWorkspaceMapResult(root, MapSectionAll, "")
	if err != nil {
		t.Fatalf("workspace_map (all): %v", err)
	}
	fullEntries := entriesByID(full.Document)

	// Selectable by name and by id — the same selector vocabulary every other
	// project-scoped tool accepts.
	for _, selector := range []string{"example/api", "/svc/api"} {
		scoped, err := BuildWorkspaceMapResult(root, MapSectionAll, selector)
		if err != nil {
			t.Fatalf("workspace_map (%s): %v", selector, err)
		}
		if len(scoped.Document.Projects) != 1 || scoped.Document.Projects[0].ID != "/svc/api" {
			t.Fatalf("selector %q returned %+v, want only /svc/api", selector, scoped.Document.Projects)
		}
		if scoped.Provenance.Project != "/svc/api" {
			t.Errorf("provenance project = %q, want the resolved id", scoped.Provenance.Project)
		}
		if scoped.Document.Workspace != full.Document.Workspace || scoped.Document.SchemaVersion != full.Document.SchemaVersion {
			t.Error("the scoped document lost its self-describing header")
		}
		assertSubsetOfFull(t, scoped.Document, fullEntries)
	}

	// Section and project compose: one project, one section.
	scoped, err := BuildWorkspaceMapResult(root, MapSectionAPIs, "example/api")
	if err != nil {
		t.Fatalf("workspace_map (apis, example/api): %v", err)
	}
	if len(scoped.Document.Projects) != 1 || len(scoped.Document.Projects[0].Endpoints) != 1 {
		t.Fatalf("scoped document = %+v, want one project's endpoint table", scoped.Document.Projects)
	}
	if len(scoped.Document.Projects[0].ConfigKeys) != 0 {
		t.Error("the apis section leaked config keys")
	}
	assertSubsetOfFull(t, scoped.Document, fullEntries)
}

// TestBuildWorkspaceMapResult_RejectsUnknownInputs keeps the tool's failures
// actionable: an unknown section answers with the valid set, and an unresolvable
// project is a not-found rather than an empty map that reads like "no such
// dependencies".
func TestBuildWorkspaceMapResult_RejectsUnknownInputs(t *testing.T) {
	root := mapCommandWorkspace(t)

	_, err := BuildWorkspaceMapResult(root, "endpoints", "")
	if err == nil || !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("unknown section error = %v, want ErrInvalidConfig", err)
	}
	for _, want := range MapSections {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name the %q section: %v", want, err)
		}
	}

	_, err = BuildWorkspaceMapResult(root, "", "example/nope")
	if err == nil || !errors.Is(err, cmderr.ErrNotFound) {
		t.Fatalf("unknown project error = %v, want ErrNotFound", err)
	}
}

// TestBuildWorkspaceMapResult_RefusesADegradedIdentity reuses the CLI's guard
// rather than re-deriving it: a workspace with no resolved provider view would
// serve a map with no provider-derived edges, and an agent would read that
// absence as fact.
func TestBuildWorkspaceMapResult_RefusesADegradedIdentity(t *testing.T) {
	root := mapCommandWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"name":"example","version":"0.1.0","includes":["svc/api","svc/lib"],"extensions":["@putnami/go"]}`),
		0o644); err != nil {
		t.Fatalf("rewrite workspace manifest: %v", err)
	}
	workspace.InvalidateLoadCache(root)

	_, err := BuildWorkspaceMapResult(root, "", "")
	if err == nil || !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "projects sync") {
		t.Errorf("the error does not name the repair command: %v", err)
	}
}

// entriesByID indexes the full document's project entries so a scoped answer can
// be checked field-by-field against the map it claims to be a subset of.
func entriesByID(doc *mapgen.RepoMap) map[string]mapgen.ProjectEntry {
	index := make(map[string]mapgen.ProjectEntry, len(doc.Projects))
	for _, entry := range doc.Projects {
		index[entry.ID] = entry
	}
	return index
}

func renderForSize(t *testing.T, doc *mapgen.RepoMap) []byte {
	t.Helper()
	data, err := mapgen.RenderJSON(doc)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	return data
}

// assertSubsetOfFull checks that every entry a scoped document carries exists in
// the full map, and that every field the scope KEPT is identical there. It walks
// the struct reflectively rather than naming fields, so a field added to
// ProjectEntry is covered the day it is added; a zero field is what "this scope
// dropped it" looks like, which is why zero fields are skipped instead of
// compared.
func assertSubsetOfFull(t *testing.T, scoped *mapgen.RepoMap, fullEntries map[string]mapgen.ProjectEntry) {
	t.Helper()
	for _, entry := range scoped.Projects {
		full, ok := fullEntries[entry.ID]
		if !ok {
			t.Errorf("scoped document carries %q, which is not in the full map", entry.ID)
			continue
		}
		got, want := reflect.ValueOf(entry), reflect.ValueOf(full)
		for i := range got.NumField() {
			if got.Field(i).IsZero() {
				continue
			}
			if !reflect.DeepEqual(got.Field(i).Interface(), want.Field(i).Interface()) {
				t.Errorf("%s: scoped field %s = %v, full map has %v",
					entry.ID, got.Type().Field(i).Name, got.Field(i).Interface(), want.Field(i).Interface())
			}
		}
	}
}
