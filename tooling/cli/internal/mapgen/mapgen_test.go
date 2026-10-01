package mapgen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// fixtureOpenAPI exercises the endpoint extractor's whole surface: two methods
// on one path, a second path, and a non-operation key ("parameters") that must
// not become an endpoint.
const fixtureOpenAPI = `{
  "openapi": "3.1.0",
  "paths": {
    "/tasks": {
      "get": {"operationId": "listTasks", "summary": "List tasks"},
      "post": {"operationId": "createTask"},
      "parameters": [{"name": "limit", "in": "query"}]
    },
    "/health": {
      "get": {"operationId": "health"}
    }
  }
}`

// fixtureConfigSchema exercises the config-key flattener: nested objects, a
// typed leaf, and a typed array.
const fixtureConfigSchema = `{
  "type": "object",
  "properties": {
    "server": {
      "type": "object",
      "properties": {
        "port": {"type": "integer"},
        "host": {"type": "string"}
      }
    },
    "features": {"type": "array", "items": {"type": "string"}}
  }
}`

const fixtureReadme = `# Task API

The task API owns tasks.

More prose that must not be the summary.
`

// fixtureWorkspace writes a controlled two-project workspace under a temp root
// and returns the loaded workspace plus the two projects (api, lib). api carries
// all four inputs; lib carries none, so the "typed absence" path is exercised in
// every test that uses it.
func fixtureWorkspace(t *testing.T) (*workspace.Workspace, *workspace.Project, *workspace.Project) {
	t.Helper()
	root := t.TempDir()

	writeFile(t, root, "svc/api/README.md", fixtureReadme)
	writeFile(t, root, "svc/api/schema/openapi.json", fixtureOpenAPI)
	writeFile(t, root, "svc/api/schema/config.jsonschema.json", fixtureConfigSchema)
	writeFile(t, root, "svc/api/putnami.json", `{"name":"example/api","description":"Task API service"}`)
	writeFile(t, root, "svc/lib/putnami.json", `{"name":"example/lib"}`)

	api := &workspace.Project{
		ID:           "/svc/api",
		Name:         "example/api",
		Path:         "svc/api",
		Type:         "application",
		Tags:         []string{"go", "api"},
		Extensions:   []string{"@putnami/go"},
		Dependencies: []string{"example/lib", "third-party/not-a-project"},
		Config:       &wsproto.ProjectConfig{Description: "Task API service"},
	}
	lib := &workspace.Project{
		ID:   "/svc/lib",
		Name: "example/lib",
		Path: "svc/lib",
		Type: "library",
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "example"}, []*workspace.Project{api, lib})
	return ws, api, lib
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestBuildFragment_ExtractsProjectScopedFacts pins what a fragment carries and
// that its inputs record covers exactly the four declared inputs, with absent
// ones present as a typed absence rather than omitted.
func TestBuildFragment_ExtractsProjectScopedFacts(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)

	fragment, err := BuildFragment(ws.Root, api)
	if err != nil {
		t.Fatalf("BuildFragment: %v", err)
	}

	// The four file inputs plus the schema/ directory LISTING input.
	if got := len(fragment.Inputs.Files); got != len(ProjectInputFiles)+1 {
		t.Fatalf("inputs recorded %d files, want %d", got, len(ProjectInputFiles)+1)
	}
	for i, f := range fragment.Inputs.Files {
		if i > 0 && fragment.Inputs.Files[i-1].Path >= f.Path {
			t.Errorf("input files are not sorted: %v", fragment.Inputs.Files)
		}
		if f.Digest == "" {
			t.Errorf("input %s has an empty digest", f.Path)
		}
	}
	if fragment.Inputs.Digest == "" || fragment.Inputs.Identity == "" {
		t.Fatalf("inputs digest/identity must be populated: %+v", fragment.Inputs)
	}

	p := fragment.Project
	if p.Summary != "The task API owns tasks." {
		t.Errorf("summary = %q, want the README's first prose line", p.Summary)
	}
	if p.Readme != "svc/api/README.md" {
		t.Errorf("readme = %q, want the workspace-relative README path", p.Readme)
	}
	if p.Description != "Task API service" {
		t.Errorf("description = %q, want the authored putnami.json description", p.Description)
	}
	// Endpoints: sorted by (path, method), "parameters" excluded.
	want := []Endpoint{
		{Method: "GET", Path: "/health", OperationID: "health"},
		{Method: "GET", Path: "/tasks", OperationID: "listTasks", Summary: "List tasks"},
		{Method: "POST", Path: "/tasks", OperationID: "createTask"},
	}
	if len(p.Endpoints) != len(want) {
		t.Fatalf("endpoints = %+v, want %+v", p.Endpoints, want)
	}
	for i := range want {
		if p.Endpoints[i] != want[i] {
			t.Errorf("endpoint %d = %+v, want %+v", i, p.Endpoints[i], want[i])
		}
	}
	wantKeys := []ConfigKey{
		{Key: "features", Type: "array<string>"},
		{Key: "server.host", Type: "string"},
		{Key: "server.port", Type: "integer"},
	}
	if len(p.ConfigKeys) != len(wantKeys) {
		t.Fatalf("configKeys = %+v, want %+v", p.ConfigKeys, wantKeys)
	}
	for i := range wantKeys {
		if p.ConfigKeys[i] != wantKeys[i] {
			t.Errorf("configKey %d = %+v, want %+v", i, p.ConfigKeys[i], wantKeys[i])
		}
	}
	wantSchemas := []string{"schema/config.jsonschema.json", "schema/openapi.json"}
	if strings.Join(p.Schemas, ",") != strings.Join(wantSchemas, ",") {
		t.Errorf("schemas = %v, want %v", p.Schemas, wantSchemas)
	}

	// The library has none of the artifacts except putnami.json: every other
	// input — the three files AND the schema/ listing — must be recorded as a
	// typed absence, not dropped.
	libFragment, err := BuildFragment(ws.Root, lib)
	if err != nil {
		t.Fatalf("BuildFragment(lib): %v", err)
	}
	absent := 0
	for _, f := range libFragment.Inputs.Files {
		if f.Digest == AbsentDigest {
			absent++
		}
	}
	if absent != 4 {
		t.Errorf("lib recorded %d absent inputs, want 4: %+v", absent, libFragment.Inputs.Files)
	}
	if len(libFragment.Project.Schemas) != 0 {
		t.Errorf("lib fragment must carry no schemas: %v", libFragment.Project.Schemas)
	}
	if libFragment.Project.Readme != "" || len(libFragment.Project.Endpoints) != 0 {
		t.Errorf("lib fragment must carry no README/endpoints: %+v", libFragment.Project)
	}
}

// TestBuildFragment_ByteStable is the determinism floor: repeated builds over an
// unchanged tree serialize identically, so nothing ambient (map order,
// timestamps, absolute paths) leaks into a fragment.
func TestBuildFragment_ByteStable(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)

	first, err := BuildFragment(ws.Root, api)
	if err != nil {
		t.Fatalf("BuildFragment: %v", err)
	}
	firstBytes, err := CanonicalFragment(first)
	if err != nil {
		t.Fatalf("CanonicalFragment: %v", err)
	}
	if !strings.HasSuffix(string(firstBytes), "\n") {
		t.Error("canonical fragment bytes must end with a trailing newline")
	}
	if strings.Contains(string(firstBytes), ws.Root) {
		t.Error("canonical fragment bytes leak the absolute workspace root")
	}
	for i := range 4 {
		again, err := BuildFragment(ws.Root, api)
		if err != nil {
			t.Fatalf("BuildFragment iteration %d: %v", i, err)
		}
		gotBytes, err := CanonicalFragment(again)
		if err != nil {
			t.Fatalf("CanonicalFragment iteration %d: %v", i, err)
		}
		if string(gotBytes) != string(firstBytes) {
			t.Fatalf("iteration %d: fragment not byte-stable\n--- first:\n%s\n--- got:\n%s", i, firstBytes, gotBytes)
		}
	}
}

// The classification a provider derives is a fact ABOUT the project, so it must
// reach the map entry an agent reads and it must key the fragment: the Go
// probe derives Type from the module's sources, and a module that gains its
// first package main flips library → application without
// any of the four file inputs moving. A fragment that did not digest the
// identity would keep serving the stale classification.
func TestFragment_CarriesTheProjectClassification(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)

	fragments := map[string]*Fragment{}
	for _, p := range []*workspace.Project{api, lib} {
		f, err := BuildFragment(ws.Root, p)
		if err != nil {
			t.Fatalf("BuildFragment(%s): %v", p.ID, err)
		}
		if f.Project.Type != p.Type {
			t.Errorf("%s fragment type = %q, want %q", p.ID, f.Project.Type, p.Type)
		}
		fragments[p.ID] = f
	}

	repoMap, err := Reduce(ws, fragments)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	for _, entry := range repoMap.Projects {
		want := map[string]string{"/svc/api": "application", "/svc/lib": "library"}[entry.ID]
		if entry.Type != want {
			t.Errorf("%s entry type = %q, want %q", entry.ID, entry.Type, want)
		}
	}

	// Reclassifying the same tree must invalidate the fragment: nothing on disk
	// changed, so only the identity digest can carry the difference.
	before := fragments["/svc/lib"].Inputs.Digest
	promoted := *lib
	promoted.Type = "application"
	after, err := BuildFragment(ws.Root, &promoted)
	if err != nil {
		t.Fatalf("BuildFragment(promoted): %v", err)
	}
	if after.Inputs.Digest == before {
		t.Error("reclassifying a project left the fragment inputs digest unchanged; a stale type would survive")
	}
}

// TestResolveFragment_InputsDigestInvalidatesAStalePersistedFragment is the
// acceptance test for "drift is unrepresentable, not detected-after-the-fact":
// a persisted fragment is REUSED while its inputs are unchanged, and silently
// discarded the moment any declared input — a file OR the workspace-resolved
// identity — moves.
func TestResolveFragment_InputsDigestInvalidatesAStalePersistedFragment(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)

	// Persist the current fragment.
	fragment, err := BuildFragment(ws.Root, api)
	if err != nil {
		t.Fatalf("BuildFragment: %v", err)
	}
	data, err := CanonicalFragment(fragment)
	if err != nil {
		t.Fatalf("CanonicalFragment: %v", err)
	}
	fragPath := filepath.Join(ws.Root, filepath.FromSlash(FragmentPath(api.Path)))
	if err := atomicWriteFile(fragPath, data); err != nil {
		t.Fatalf("persist fragment: %v", err)
	}

	// Unchanged inputs: reused.
	got, reused, err := ResolveFragment(ws.Root, api)
	if err != nil || !reused {
		t.Fatalf("ResolveFragment on unchanged inputs = (reused=%v, err=%v), want reused", reused, err)
	}
	if got.Inputs.Digest != fragment.Inputs.Digest {
		t.Errorf("reused fragment digest = %q, want %q", got.Inputs.Digest, fragment.Inputs.Digest)
	}

	// A changed FILE input invalidates it.
	writeFile(t, ws.Root, "svc/api/schema/openapi.json",
		strings.Replace(fixtureOpenAPI, `"summary": "List tasks"`, `"summary": "Enumerate tasks"`, 1))
	got, reused, err = ResolveFragment(ws.Root, api)
	if err != nil {
		t.Fatalf("ResolveFragment after edit: %v", err)
	}
	if reused {
		t.Fatal("a fragment whose openapi input changed was reused — the inputs digest is not covering it")
	}
	if len(got.Project.Endpoints) == 0 || got.Project.Endpoints[1].Summary != "Enumerate tasks" {
		t.Errorf("rebuilt fragment did not pick the new summary up: %+v", got.Project.Endpoints)
	}

	// Re-persist, then change only the workspace-resolved IDENTITY (no file
	// moved): the fragment must still be invalidated.
	data, err = CanonicalFragment(got)
	if err != nil {
		t.Fatalf("CanonicalFragment: %v", err)
	}
	if err := atomicWriteFile(fragPath, data); err != nil {
		t.Fatalf("persist fragment: %v", err)
	}
	if _, reused, err = ResolveFragment(ws.Root, api); err != nil || !reused {
		t.Fatalf("ResolveFragment after re-persist = (reused=%v, err=%v), want reused", reused, err)
	}
	api.Dependencies = append(api.Dependencies, "example/newdep")
	if _, reused, err = ResolveFragment(ws.Root, api); err != nil {
		t.Fatalf("ResolveFragment after identity change: %v", err)
	} else if reused {
		t.Fatal("a fragment whose project identity changed was reused — the identity digest is not covering it")
	}
}

// TestResolveFragment_SchemaListingInvalidatesOnAddAndRemove is the acceptance
// test for gap 3: the fragment EMITS the schema file names, so the
// inputs digest must move when one is added or removed — and must NOT move when
// an unparsed schema's CONTENT changes, because the emitted bytes do not depend
// on it and a digest that moves for nothing is a cache miss for nothing.
func TestResolveFragment_SchemaListingInvalidatesOnAddAndRemove(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)
	persist := func() *Fragment {
		t.Helper()
		fragment, err := BuildFragment(ws.Root, api)
		if err != nil {
			t.Fatalf("BuildFragment: %v", err)
		}
		data, err := CanonicalFragment(fragment)
		if err != nil {
			t.Fatalf("CanonicalFragment: %v", err)
		}
		if err := atomicWriteFile(filepath.Join(ws.Root, filepath.FromSlash(FragmentPath(api.Path))), data); err != nil {
			t.Fatalf("persist fragment: %v", err)
		}
		return fragment
	}
	reused := func() bool {
		t.Helper()
		_, ok, err := ResolveFragment(ws.Root, api)
		if err != nil {
			t.Fatalf("ResolveFragment: %v", err)
		}
		return ok
	}

	// The listing input is recorded as a directory entry, distinct from the files.
	fragment := persist()
	var listing *InputFile
	for i, f := range fragment.Inputs.Files {
		if f.Path == SchemaDirInput {
			listing = &fragment.Inputs.Files[i]
		}
	}
	if listing == nil {
		t.Fatalf("inputs do not record the schema listing: %+v", fragment.Inputs.Files)
	}
	if listing.Digest == AbsentDigest {
		t.Errorf("schema listing digest = %q for a project that has a schema/ directory", listing.Digest)
	}
	if !reused() {
		t.Fatal("an unchanged tree did not reuse its fragment")
	}

	// A schema the fragment does not parse is ADDED: the listing changes, so the
	// fragment must be rebuilt and must emit the new name.
	writeFile(t, ws.Root, "svc/api/schema/capabilities.json", `{"capabilities":[]}`)
	if reused() {
		t.Fatal("a fragment was reused after a schema file was added — the listing digest is not covering it")
	}
	fragment = persist()
	want := []string{"schema/capabilities.json", "schema/config.jsonschema.json", "schema/openapi.json"}
	if strings.Join(fragment.Project.Schemas, ",") != strings.Join(want, ",") {
		t.Errorf("schemas = %v, want %v (sorted, project-relative, non-recursive)", fragment.Project.Schemas, want)
	}

	// The same schema's CONTENT changes: only names are emitted, so the fragment
	// stays valid.
	writeFile(t, ws.Root, "svc/api/schema/capabilities.json", `{"capabilities":["a","b"]}`)
	if !reused() {
		t.Error("editing an unparsed schema's content invalidated the fragment; only the name list is an input")
	}

	// A non-JSON file is not listed and is not an input either.
	writeFile(t, ws.Root, "svc/api/schema/notes.txt", "ignored")
	if !reused() {
		t.Error("a non-JSON file under schema/ moved the listing digest")
	}

	// It is REMOVED: the listing changes back and the fragment is invalidated.
	if err := os.Remove(filepath.Join(ws.Root, filepath.FromSlash("svc/api/schema/capabilities.json"))); err != nil {
		t.Fatalf("remove schema: %v", err)
	}
	if reused() {
		t.Fatal("a fragment was reused after a schema file was removed")
	}

	// An absent schema/ directory is a typed ABSENCE, not an empty listing: the
	// two must not hash alike.
	empty := t.TempDir()
	writeFile(t, empty, "svc/api/putnami.json", `{"name":"example/api"}`)
	if err := os.MkdirAll(filepath.Join(empty, "svc", "api", "schema"), 0o755); err != nil {
		t.Fatalf("mkdir schema: %v", err)
	}
	emptyDir, err := BuildFragment(empty, api)
	if err != nil {
		t.Fatalf("BuildFragment(empty schema dir): %v", err)
	}
	missing := t.TempDir()
	writeFile(t, missing, "svc/api/putnami.json", `{"name":"example/api"}`)
	noDir, err := BuildFragment(missing, api)
	if err != nil {
		t.Fatalf("BuildFragment(no schema dir): %v", err)
	}
	if emptyDir.Inputs.Digest == noDir.Inputs.Digest {
		t.Error("an empty schema/ directory and a missing one produced the same inputs digest")
	}
}

// TestResolveFragment_RejectsForeignFragmentShapes pins the fail-safe direction:
// a corrupt, empty, or older-version fragment is a cache MISS, never an error
// and never a trusted entry.
func TestResolveFragment_RejectsForeignFragmentShapes(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)
	fragPath := filepath.Join(ws.Root, filepath.FromSlash(FragmentPath(api.Path)))

	fragment, err := BuildFragment(ws.Root, api)
	if err != nil {
		t.Fatalf("BuildFragment: %v", err)
	}

	cases := []struct {
		name    string
		content func() []byte
	}{
		{"malformed", func() []byte { return []byte("{not json") }},
		{"empty", func() []byte { return nil }},
		{"old version", func() []byte {
			stale := *fragment
			stale.FragmentVersion = FragmentVersion - 1
			data, err := CanonicalFragment(&stale)
			if err != nil {
				t.Fatalf("CanonicalFragment: %v", err)
			}
			return data
		}},
		{"digest cleared", func() []byte {
			stale := *fragment
			stale.Inputs.Digest = ""
			data, err := CanonicalFragment(&stale)
			if err != nil {
				t.Fatalf("CanonicalFragment: %v", err)
			}
			return data
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := atomicWriteFile(fragPath, tc.content()); err != nil {
				t.Fatalf("write fragment: %v", err)
			}
			got, reused, err := ResolveFragment(ws.Root, api)
			if err != nil {
				t.Fatalf("ResolveFragment: %v", err)
			}
			if reused {
				t.Fatalf("a %s fragment was reused", tc.name)
			}
			if got == nil || got.Inputs.Digest != fragment.Inputs.Digest {
				t.Errorf("rebuilt fragment digest = %v, want the fresh one", got)
			}
		})
	}
}

// TestReduce_ResolvesEdgesAndSortsByPath pins the reduce's three
// workspace-level jobs: name→id dependency resolution against the LIVE set (an
// external dependency is dropped), the reverse dependents edge, and the
// sorted-by-path emit order.
func TestReduce_ResolvesEdgesAndSortsByPath(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)

	fragments := map[string]*Fragment{}
	for _, p := range []*workspace.Project{api, lib} {
		f, err := BuildFragment(ws.Root, p)
		if err != nil {
			t.Fatalf("BuildFragment(%s): %v", p.ID, err)
		}
		fragments[p.ID] = f
	}

	repoMap, err := Reduce(ws, fragments)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if repoMap.SchemaVersion != SchemaVersion || repoMap.Workspace != "example" {
		t.Errorf("map header = %+v, want schemaVersion %d / workspace example", repoMap, SchemaVersion)
	}
	if len(repoMap.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(repoMap.Projects))
	}
	if repoMap.Projects[0].Path != "svc/api" || repoMap.Projects[1].Path != "svc/lib" {
		t.Errorf("entries are not sorted by path: %q, %q", repoMap.Projects[0].Path, repoMap.Projects[1].Path)
	}
	if got := repoMap.Projects[0].DependsOn; len(got) != 1 || got[0] != "/svc/lib" {
		t.Errorf("api dependsOn = %v, want [/svc/lib] (the external name must be dropped)", got)
	}
	if got := repoMap.Projects[1].Dependents; len(got) != 1 || got[0] != "/svc/api" {
		t.Errorf("lib dependents = %v, want [/svc/api]", got)
	}

	// A project the live set no longer contains contributes nothing — deletions
	// are structural, with no diffing.
	shrunk := workspace.NewWorkspace(ws.Root, &wsproto.Config{Name: "example"}, []*workspace.Project{api})
	repoMap, err = Reduce(shrunk, fragments)
	if err != nil {
		t.Fatalf("Reduce(shrunk): %v", err)
	}
	if len(repoMap.Projects) != 1 || repoMap.Projects[0].ID != "/svc/api" {
		t.Errorf("a removed project still appears in the map: %+v", repoMap.Projects)
	}
	if len(repoMap.Projects[0].DependsOn) != 0 {
		t.Errorf("api still depends on the removed project: %v", repoMap.Projects[0].DependsOn)
	}
}

// TestReduce_CarriesSchemasAndIndexesNonProjectDocs pins the two reduce-stage
// halves of this feature end to end: a project's committed schema listing
// reaches its entry, and a README that no project root owns reaches the
// document index while the project-root ones stay where they already were
// (the project entry).
func TestReduce_CarriesSchemasAndIndexesNonProjectDocs(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)
	writeFile(t, ws.Root, "svc/README.md", "# Services\n\nThe service scope.\n")

	fragments := map[string]*Fragment{}
	for _, p := range []*workspace.Project{api, lib} {
		f, err := BuildFragment(ws.Root, p)
		if err != nil {
			t.Fatalf("BuildFragment(%s): %v", p.ID, err)
		}
		fragments[p.ID] = f
	}
	repoMap, err := Reduce(ws, fragments)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}

	want := []string{"schema/config.jsonschema.json", "schema/openapi.json"}
	if got := repoMap.Projects[0].Schemas; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("api schemas = %v, want %v", got, want)
	}
	if got := repoMap.Projects[1].Schemas; len(got) != 0 {
		t.Errorf("lib schemas = %v, want none", got)
	}
	if len(repoMap.Docs) != 1 || repoMap.Docs[0].Path != "svc/README.md" || repoMap.Docs[0].Title != "Services" {
		t.Errorf("docs = %+v, want only the scope README (the project-root one is the project's `readme`)", repoMap.Docs)
	}
	if repoMap.Projects[0].Readme != "svc/api/README.md" {
		t.Errorf("api readme = %q, want the project-root README the index excludes", repoMap.Projects[0].Readme)
	}
}

// TestIndexDocs_IsSortedAndTitled pins the one genuinely global file input
// besides the workspace manifest.
func TestIndexDocs_IsSortedAndTitled(t *testing.T) {
	root := t.TempDir()
	if docs, err := indexDocs(root, nil); err != nil || docs != nil {
		t.Fatalf("indexDocs with no documents = (%v, %v), want (nil, nil)", docs, err)
	}
	writeFile(t, root, "docs/z.md", "# Zulu\n")
	writeFile(t, root, "docs/nested/a.md", "```\n# not a heading\n```\n# Alpha\n")
	writeFile(t, root, "docs/notes.txt", "ignored")
	writeFile(t, root, "docs/.hidden/x.md", "# Hidden\n")

	docs, err := indexDocs(root, nil)
	if err != nil {
		t.Fatalf("indexDocs: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %+v, want 2 entries", docs)
	}
	if docs[0].Path != "docs/nested/a.md" || docs[0].Title != "Alpha" {
		t.Errorf("docs[0] = %+v, want docs/nested/a.md titled Alpha (fenced '#' is not a heading)", docs[0])
	}
	if docs[1].Path != "docs/z.md" || docs[1].Title != "Zulu" {
		t.Errorf("docs[1] = %+v, want docs/z.md titled Zulu", docs[1])
	}
}

// TestIndexDocs_IndexesNonProjectReadmesOnly pins gap 2: the index picks
// up every README in the workspace EXCEPT the ones a project root already
// carries as its own `readme`/`summary`, and never descends into a pruned
// directory. A markdown file that is neither under docs/ nor a README is not a
// document.
func TestIndexDocs_IndexesNonProjectReadmesOnly(t *testing.T) {
	root := t.TempDir()

	writeFile(t, root, "README.md", "# Workspace\n")
	writeFile(t, root, "svc/README.md", "# Scope\n")                       // scope-level: indexed
	writeFile(t, root, "svc/api/README.md", "# Api\n")                     // project root: excluded
	writeFile(t, root, "svc/api/internal/store/README.md", "# Store\n")    // nested: indexed
	writeFile(t, root, "svc/api/NOTES.md", "# Notes\n")                    // not a README, not under docs/
	writeFile(t, root, "docs/guide.md", "# Guide\n")                       // docs tree: indexed
	writeFile(t, root, "node_modules/pkg/README.md", "# Dep\n")            // pruned by name
	writeFile(t, root, "svc/api/vendor/dep/README.md", "# Vendored\n")     // pruned at depth
	writeFile(t, root, "svc/api/internal/testdata/ws/README.md", "# Fx\n") // pruned at depth
	writeFile(t, root, ".putnami/context-map/svc/api/README.md", "# St\n") // dot-prefixed
	writeFile(t, root, "svc/api/.gen/README.md", "# Generated\n")          // dot-prefixed at depth

	docs, err := indexDocs(root, map[string]bool{"svc/api/README.md": true, "svc/lib/README.md": true})
	if err != nil {
		t.Fatalf("indexDocs: %v", err)
	}
	got := make([]string, 0, len(docs))
	for _, d := range docs {
		got = append(got, d.Path)
	}
	want := []string{"README.md", "docs/guide.md", "svc/README.md", "svc/api/internal/store/README.md"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("indexed documents =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, d := range docs {
		if d.Title == "" {
			t.Errorf("document %s has no title", d.Path)
		}
	}
}

// TestRenderJSON_OmitsAmbientValues is the "a version bump must not dirty the
// map" guard: the rendered document names no version, no timestamp, and no
// absolute path.
func TestRenderJSON_OmitsAmbientValues(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)
	fragments := map[string]*Fragment{}
	for _, p := range []*workspace.Project{api, lib} {
		f, err := BuildFragment(ws.Root, p)
		if err != nil {
			t.Fatalf("BuildFragment: %v", err)
		}
		fragments[p.ID] = f
	}
	repoMap, err := Reduce(ws, fragments)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	data, err := RenderJSON(repoMap)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("rendered JSON does not parse: %v", err)
	}
	for _, banned := range []string{"version", "generatedAt", "timestamp", "revision", "cliVersion"} {
		if _, present := decoded[banned]; present {
			t.Errorf("rendered map carries an ambient field %q: a CLI release would dirty every committed map", banned)
		}
	}
	if strings.Contains(string(data), ws.Root) {
		t.Error("rendered map leaks the absolute workspace root")
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("rendered map must end with a trailing newline")
	}

	md := string(RenderMarkdown(repoMap))
	if strings.Contains(md, ws.Root) {
		t.Error("rendered markdown leaks the absolute workspace root")
	}
	for _, want := range []string{
		"## Projects", "## Dependencies", "## Intercalls", "## APIs",
		"## Config keys", "## Schemas", "## Docs", "`svc/api`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown is missing %q", want)
		}
	}
}

// TestReadmeSummary_SkipsNonProseOpeners pins the summary heuristic against the
// README shapes this tree actually has: an HTML front-matter comment, a
// blockquote status callout, and a fenced block whose lines start with '#'.
func TestReadmeSummary_SkipsNonProseOpeners(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"plain prose", "# Title\n\nA library.\n", "A library."},
		{"html comment first", "<!-- protocol-version: 1 -->\n\n# Title\n\nA protocol.\n", "A protocol."},
		{"multiline comment", "<!-- The `ProtocolVersion` conformance anchor yet\nanother commentary line\n-->\n\n# Title\n\nA protocol.\n", "A protocol."},
		{"inline comment then prose", "<!-- marker --> Real prose.\n", "Real prose."},
		{"comment closing into prose", "<!-- a\nb --> Tail prose.\n", "Tail prose."},
		{"blockquote callout", "# Title\n\n> **Status: adopted.** Pins the v1 wire contract.\n", "**Status: adopted.** Pins the v1 wire contract."},
		{"fenced hash is not prose", "# Title\n\n```sh\n# not prose\n```\n\nReal prose.\n", "Real prose."},
		{"heading only", "# Title\n", ""},
		{"empty", "", ""},
		{"collapses whitespace", "# T\n\n   spaced    out   line   \n", "spaced out line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readmeSummary([]byte(tc.content)); got != tc.want {
				t.Errorf("readmeSummary = %q, want %q", got, tc.want)
			}
		})
	}

	// The cap is a rune cap, so a long opening paragraph is cut on a rune
	// boundary and marked, deterministically.
	long := "# T\n\n" + strings.Repeat("é", summaryRuneCap+50) + "\n"
	got := readmeSummary([]byte(long))
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a long summary was not marked as truncated: %q", got)
	}
	if runes := len([]rune(got)); runes != summaryRuneCap+1 {
		t.Errorf("truncated summary has %d runes, want %d (cap + the ellipsis)", runes, summaryRuneCap+1)
	}
}

// TestResolveFragment_CacheHitSkipsParsing pins the O(changed) warm path: a
// digest hit must return the stored fragment WITHOUT parsing the inputs. The
// probe is a config schema that is valid JSON bytes for digesting but invalid
// as a schema: parsing it errors, so a warm resolve that still parses cannot
// return (stored, true, nil).
func TestResolveFragment_CacheHitSkipsParsing(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)

	// Make the config schema unparseable, then record a fragment whose inputs
	// digest matches those exact bytes — the shape a warm cache presents.
	writeFile(t, ws.Root, "svc/api/schema/config.jsonschema.json", `{"type": ["broken"`)
	inputs, _, _, _, err := digestFragmentInputs(ws.Root, api)
	if err != nil {
		t.Fatalf("digestFragmentInputs: %v", err)
	}
	stored := &Fragment{
		FragmentVersion: FragmentVersion,
		Inputs:          inputs,
		Project:         FragmentProject{ID: api.ID, Path: filepath.ToSlash(api.Path), Summary: "from cache"},
	}
	data, err := CanonicalFragment(stored)
	if err != nil {
		t.Fatalf("CanonicalFragment: %v", err)
	}
	if err := atomicWriteFile(filepath.Join(ws.Root, filepath.FromSlash(FragmentPath(api.Path))), data); err != nil {
		t.Fatalf("persist fragment: %v", err)
	}

	got, reused, err := ResolveFragment(ws.Root, api)
	if err != nil {
		t.Fatalf("warm resolve parsed its inputs (err = %v), want a digest-only cache hit", err)
	}
	if !reused || got.Project.Summary != "from cache" {
		t.Fatalf("warm resolve = (reused=%v, summary=%q), want the stored fragment untouched", reused, got.Project.Summary)
	}

	// A MISS over the same unparseable input must still surface the parse
	// error — the digest gate must never swallow a real rebuild failure.
	writeFile(t, ws.Root, "svc/api/schema/config.jsonschema.json", `{"type": ["still broken"`)
	if _, _, err := ResolveFragment(ws.Root, api); err == nil {
		t.Fatal("a cache miss over an unparseable schema resolved without error")
	}
}
