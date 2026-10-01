package openapi

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/codegen"
)

// generationWith parses inline Go source into a single-file Generation,
// sufficient for visitor tests. The runner is bypassed entirely so the
// test stays focused on AST extraction logic.
func generationWith(t *testing.T, src string) *codegen.Generation {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing source: %v", err)
	}
	return &codegen.Generation{
		ProjectRoot: t.TempDir(),
		Mode:        "build",
		// Version is the workspace version and DeclaredVersion the project's own:
		// the two differ here on purpose, because the committed spec must follow
		// the DECLARED one.
		Project: codegen.ProjectInfo{Name: "test-api", Version: "9.9.9", DeclaredVersion: "1.2.3"},
		Fset:    fset,
		Files:   map[string]*ast.File{"/tmp/main.go": f},
	}
}

func TestVisitProducesSpecForServerCalls(t *testing.T) {
	g := generationWith(t, `package main

import "go.putnami.dev/http"

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	server.GET("/tasks", nil)
	server.POST("/tasks", nil)
	server.GET("/tasks/{id}", nil)
}
`)

	v := &visitor{}
	res, err := v.Visit(g)
	if err != nil {
		t.Fatalf("visit: %v", err)
	}
	if res.IsEmpty() {
		t.Fatal("expected non-empty result")
	}
	if len(res.SchemaFiles) != 1 {
		t.Fatalf("expected 1 schema file, got %d", len(res.SchemaFiles))
	}
	sf := res.SchemaFiles[0]
	if sf.RelPath != "schema/openapi.json" {
		t.Errorf("RelPath = %q, want schema/openapi.json", sf.RelPath)
	}

	var doc map[string]any
	if err := json.Unmarshal(sf.Content, &doc); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Errorf("openapi = %v, want 3.0.3", doc["openapi"])
	}
	info := doc["info"].(map[string]any)
	if info["title"] != "test-api" {
		t.Errorf("title = %v, want test-api", info["title"])
	}
	if info["version"] != "1.2.3" {
		t.Errorf("version = %v, want the project's declared 1.2.3", info["version"])
	}

	paths := doc["paths"].(map[string]any)
	if _, ok := paths["/tasks"]; !ok {
		t.Error("missing /tasks path")
	}
	if _, ok := paths["/tasks/{id}"]; !ok {
		t.Error("missing /tasks/{id} path")
	}
	tasks := paths["/tasks"].(map[string]any)
	if _, ok := tasks["get"]; !ok {
		t.Error("missing get on /tasks")
	}
	if _, ok := tasks["post"]; !ok {
		t.Error("missing post on /tasks")
	}
}

func TestVisitSelfGatesWithoutHTTPImport(t *testing.T) {
	g := generationWith(t, `package main

type fake struct{}
func (f *fake) GET(p string, h any) {}

func main() {
	f := &fake{}
	f.GET("/should-be-ignored", nil)
}
`)

	v := &visitor{}
	res, err := v.Visit(g)
	if err != nil {
		t.Fatalf("visit: %v", err)
	}
	if !res.IsEmpty() {
		t.Errorf("expected empty result when http isn't imported, got %d schema files", len(res.SchemaFiles))
	}
}

func TestPathParametersTranslatesBracketSyntax(t *testing.T) {
	g := generationWith(t, `package main

import "go.putnami.dev/http"

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	server.GET("/users/[id]/posts/[postId]", nil)
}
`)

	v := &visitor{}
	res, _ := v.Visit(g)
	body := string(res.SchemaFiles[0].Content)
	if !strings.Contains(body, "/users/{id}/posts/{postId}") {
		t.Errorf("bracket syntax not normalized to OpenAPI braces:\n%s", body)
	}
	if !strings.Contains(body, `"name": "id"`) || !strings.Contains(body, `"name": "postId"`) {
		t.Errorf("path params not extracted:\n%s", body)
	}
}

func TestVisitSkipsCallsWithoutStringLiteralPath(t *testing.T) {
	g := generationWith(t, `package main

import "go.putnami.dev/http"

const tasksPath = "/tasks"

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	server.GET(tasksPath, nil) // const, not a string literal
}
`)

	v := &visitor{}
	res, _ := v.Visit(g)
	if !res.IsEmpty() {
		t.Errorf("expected empty result for non-literal paths, got: %s", res.SchemaFiles[0].Content)
	}
}

func TestVisitRejectsCallsOnUnrelatedReceivers(t *testing.T) {
	// File imports go.putnami.dev/http but the .GET call is on a value
	// of an unrelated type (e.g. an HTTP client). Without receiver
	// resolution, the previous heuristic would have swept this in.
	g := generationWith(t, `package main

import "go.putnami.dev/http"

type Client struct{}
func (Client) GET(path string, body any) {}

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	server.GET("/legit", nil)

	var c Client
	c.GET("/should-be-ignored", nil)
}
`)
	v := &visitor{}
	res, _ := v.Visit(g)
	if res.IsEmpty() {
		t.Fatal("expected /legit route")
	}
	body := string(res.SchemaFiles[0].Content)
	if !strings.Contains(body, "/legit") {
		t.Errorf("expected /legit in output:\n%s", body)
	}
	if strings.Contains(body, "should-be-ignored") {
		t.Errorf("client.GET leaked into spec:\n%s", body)
	}
}

func TestVisitTracksParameterReceivers(t *testing.T) {
	// Routes registered inside a helper function that takes a server
	// parameter must still be discovered.
	g := generationWith(t, `package main

import "go.putnami.dev/http"

func registerRoutes(s *http.ServerPlugin) {
	s.GET("/from-helper", nil)
}

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	registerRoutes(server)
}
`)
	v := &visitor{}
	res, _ := v.Visit(g)
	body := string(res.SchemaFiles[0].Content)
	if !strings.Contains(body, "/from-helper") {
		t.Errorf("expected /from-helper:\n%s", body)
	}
}

func TestVisitTracksDirectConstructorChain(t *testing.T) {
	// Chained registration on the constructor result.
	g := generationWith(t, `package main

import "go.putnami.dev/http"

func main() {
	http.NewServerPlugin(http.ServerConfig{}).GET("/chained", nil)
}
`)
	v := &visitor{}
	res, _ := v.Visit(g)
	body := string(res.SchemaFiles[0].Content)
	if !strings.Contains(body, "/chained") {
		t.Errorf("expected /chained route from direct constructor chain:\n%s", body)
	}
}

// TestVisitNeverStampsTheWorkspaceVersion pins the single-project stability
// contract for the committed spec: with no declared version the spec
// falls back to a CONSTANT, never to the workspace version. Otherwise one
// workspace bump re-stamps every committed spec in the repo, and the failure
// lands on whichever gate later noticed the dirty worktree.
func TestVisitNeverStampsTheWorkspaceVersion(t *testing.T) {
	const src = `package main

import "go.putnami.dev/http"

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	server.GET("/x", nil)
}
`
	version := func(t *testing.T, workspaceVersion string) string {
		t.Helper()
		g := generationWith(t, src)
		g.Project.Version = workspaceVersion
		g.Project.DeclaredVersion = ""
		res, err := (&visitor{}).Visit(g)
		if err != nil {
			t.Fatalf("visit: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(res.SchemaFiles[0].Content, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return doc["info"].(map[string]any)["version"].(string)
	}

	before := version(t, "1.2.3")
	if before != "0.0.0" {
		t.Errorf("undeclared version = %q, want the 0.0.0 default", before)
	}
	if after := version(t, "2.0.0"); after != before {
		t.Errorf("a workspace version bump moved the committed spec: %q -> %q", before, after)
	}
}

func TestVisitUsesOptionOverrides(t *testing.T) {
	g := generationWith(t, `package main

import "go.putnami.dev/http"

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	server.GET("/x", nil)
}
`)
	g.Project.Options = map[string]json.RawMessage{
		"openapi": json.RawMessage(`{"title":"My API","version":"2.0.0","description":"hand-set"}`),
	}
	v := &visitor{}
	res, _ := v.Visit(g)
	var doc map[string]any
	if err := json.Unmarshal(res.SchemaFiles[0].Content, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	info := doc["info"].(map[string]any)
	if info["title"] != "My API" {
		t.Errorf("title override not applied: %v", info["title"])
	}
	if info["version"] != "2.0.0" {
		t.Errorf("version override not applied: %v", info["version"])
	}
	if info["description"] != "hand-set" {
		t.Errorf("description override not applied: %v", info["description"])
	}
}

func TestVisitOutputIsStable(t *testing.T) {
	src := `package main

import "go.putnami.dev/http"

func main() {
	server := http.NewServerPlugin(http.ServerConfig{})
	server.POST("/zebras", nil)
	server.GET("/aardvarks", nil)
	server.PUT("/aardvarks/{id}", nil)
}
`
	v := &visitor{}
	res1, _ := v.Visit(generationWith(t, src))
	res2, _ := v.Visit(generationWith(t, src))

	if string(res1.SchemaFiles[0].Content) != string(res2.SchemaFiles[0].Content) {
		t.Errorf("visitor output is not stable across runs:\nrun1: %s\nrun2: %s",
			res1.SchemaFiles[0].Content, res2.SchemaFiles[0].Content)
	}
}
