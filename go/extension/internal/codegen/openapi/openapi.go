// Package openapi extracts an OpenAPI 3.0.3 specification from a project's
// Go source by walking calls to go.putnami.dev/http's ServerPlugin route
// methods (GET/POST/PUT/DELETE/PATCH).
//
// The visitor is registered with the SDK codegen registry in init() and runs
// during the build-generate phase. It self-gates: projects that don't import
// go.putnami.dev/http produce no output, so the visitor is free to ship in
// the extension binary regardless of which frameworks a project uses.
package openapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"go/ast"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/go/extension/internal/codegen/openapiutil"
	"go.putnami.dev/sdk/extension/codegen"
)

// Register the visitor when the package is loaded. The extension binary
// blank-imports this package; user code never does.
func init() {
	codegen.Register(&visitor{})
}

// SchemaRelPath is the project-relative output path for the generated spec.
// It matches the TypeScript convention so docs and AI guidance stay one rule.
const SchemaRelPath = "schema/openapi.json"

// httpImportPath is the framework module whose route methods we track.
// We require an exact import match so generic .GET("/path") calls on
// unrelated types don't get swept into the spec.
const httpImportPath = "go.putnami.dev/http"

// supportedMethods is the closed set of HTTP verbs the http.ServerPlugin
// exposes as same-named methods.
var supportedMethods = map[string]string{
	"GET":    "get",
	"POST":   "post",
	"PUT":    "put",
	"DELETE": "delete",
	"PATCH":  "patch",
}

type visitor struct{}

func (v *visitor) Name() string { return "openapi" }

// Visit scans every parsed file, collects route calls into routes, and
// renders a spec when at least one route is found. When the project has no
// matching imports the result is empty and the runner skips writing.
func (v *visitor) Visit(g *codegen.Generation) (*codegen.Result, error) {
	var routes []discoveredRoute
	for _, file := range filesByPath(g.Files) {
		if !importsHTTP(file) {
			continue
		}
		alias := httpAlias(file)
		routes = append(routes, scanFile(file, alias)...)
	}

	if len(routes) == 0 {
		return &codegen.Result{}, nil
	}

	sortRoutes(routes)
	doc := buildDocument(routes, g.Project)
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding openapi spec: %w", err)
	}
	body, err = openapiutil.Canonicalize(body)
	if err != nil {
		return nil, err
	}

	if g.Emit != nil {
		g.Emit.Log("info", fmt.Sprintf("openapi: %d route(s)", len(routes)))
	}

	// Exports is intentionally not set here: the runner populates
	// "<visitorName>-spec" with the .gen/ copy (always present), so the
	// entry never points at a missing file regardless of the
	// options.generate.schema opt-out.
	return &codegen.Result{
		SchemaFiles: []codegen.SchemaFile{{
			RelPath: SchemaRelPath,
			Content: body,
		}},
	}, nil
}

// discoveredRoute carries the per-route metadata the visitor extracts from
// a Go AST. Body, query, and return schemas would attach here once a typed
// builder (api.Endpoint) is recognized; for now path + method is enough.
type discoveredRoute struct {
	Method string
	Path   string
}

// importsHTTP reports whether the file imports go.putnami.dev/http.
func importsHTTP(f *ast.File) bool {
	for _, imp := range f.Imports {
		if importPath(imp) == httpImportPath {
			return true
		}
	}
	return false
}

// httpAlias returns the local name the file uses for go.putnami.dev/http.
// Defaults to "http" when no explicit alias is set.
func httpAlias(f *ast.File) string {
	for _, imp := range f.Imports {
		if importPath(imp) != httpImportPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "http"
	}
	return ""
}

// scanFile finds route registration calls in a file. To avoid sweeping in
// unrelated GET/POST methods (e.g. on an HTTP client), we walk per-function
// and only emit routes whose receiver is bound to a *http.ServerPlugin in
// the same scope.
//
// "Bound" means one of:
//   - Local var assigned from <httpAlias>.NewServerPlugin(...)
//   - Function parameter typed *<httpAlias>.ServerPlugin (or ServerPlugin)
//   - Direct chained call on <httpAlias>.NewServerPlugin(...)
//
// This is a heuristic — without go/types we can't follow assignments through
// helper functions or struct fields. False negatives (missed routes from
// indirect bindings) are acceptable; false positives (bogus routes from
// unrelated types) are not, since they end up committed to schema/openapi.json.
func scanFile(f *ast.File, alias string) []discoveredRoute {
	var routes []discoveredRoute

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		bindings := serverBindings(fn, alias)
		routes = append(routes, scanFunctionBody(fn.Body, alias, bindings)...)
	}

	return routes
}

// serverBindings collects the names of identifiers in fn that resolve to
// *http.ServerPlugin: function parameters and local variable assignments
// from the http.NewServerPlugin constructor.
func serverBindings(fn *ast.FuncDecl, alias string) map[string]bool {
	bindings := map[string]bool{}

	// Parameters typed *http.ServerPlugin or http.ServerPlugin.
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			if !isServerPluginType(field.Type, alias) {
				continue
			}
			for _, name := range field.Names {
				bindings[name.Name] = true
			}
		}
	}

	// Receiver typed *http.ServerPlugin (rare but valid).
	if fn.Recv != nil {
		for _, field := range fn.Recv.List {
			if !isServerPluginType(field.Type, alias) {
				continue
			}
			for _, name := range field.Names {
				bindings[name.Name] = true
			}
		}
	}

	// Local assignments: x := http.NewServerPlugin(...) or x = http.NewServerPlugin(...).
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			if !isNewServerPluginCall(rhs, alias) {
				continue
			}
			if i >= len(assign.Lhs) {
				continue
			}
			ident, ok := assign.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			bindings[ident.Name] = true
		}
		return true
	})

	return bindings
}

// scanFunctionBody walks fn looking for route registration calls whose
// receiver matches one of the known server bindings.
func scanFunctionBody(body *ast.BlockStmt, alias string, bindings map[string]bool) []discoveredRoute {
	var routes []discoveredRoute

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		method, ok := supportedMethods[sel.Sel.Name]
		if !ok {
			return true
		}
		if !receiverIsServer(sel.X, alias, bindings) {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}
		path, ok := stringLiteral(call.Args[0])
		if !ok {
			return true
		}
		routes = append(routes, discoveredRoute{
			Method: strings.ToUpper(method),
			Path:   path,
		})
		return true
	})

	return routes
}

// receiverIsServer reports whether expr resolves to a known *http.ServerPlugin.
// Handles:
//   - bound identifier: server.GET(...)
//   - direct constructor: http.NewServerPlugin(cfg).GET(...)
func receiverIsServer(expr ast.Expr, alias string, bindings map[string]bool) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return bindings[e.Name]
	case *ast.CallExpr:
		return isNewServerPluginCall(e, alias)
	}
	return false
}

// isNewServerPluginCall reports whether expr is a call to
// <httpAlias>.NewServerPlugin(...).
func isNewServerPluginCall(expr ast.Expr, alias string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "NewServerPlugin" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == alias
}

// isServerPluginType reports whether typeExpr is *<httpAlias>.ServerPlugin or
// <httpAlias>.ServerPlugin (rare but valid for value-receiver helpers).
func isServerPluginType(typeExpr ast.Expr, alias string) bool {
	if star, ok := typeExpr.(*ast.StarExpr); ok {
		typeExpr = star.X
	}
	sel, ok := typeExpr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "ServerPlugin" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == alias
}

// sortRoutes sorts by path then method so the generated spec is stable
// across runs. Without sorting, map iteration order would create churn
// in the committed schema/openapi.json on every build.
func sortRoutes(routes []discoveredRoute) {
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
}

// buildDocument renders the OpenAPI document. Schemas are intentionally
// minimal here: this v1 visitor only knows method + path. Body, query, and
// response schemas are populated by future visitor additions that recognize
// the api.Endpoint typed builder.
//
// Title and version come from project.options.openapi when set, so the
// committed spec doesn't drift on every project rename or workspace bump.
// Title falls back to the project name then "API".
//
// Version resolves through DECLARED inputs of this project only:
// options.openapi.version → the project's own putnami.json "version" →
// "0.0.0". The workspace version (project.Version) is deliberately NOT in that
// chain: schema/openapi.json is a COMMITTED artifact, and stamping workspace
// state into it made one workspace bump re-stamp every committed spec in the
// repo — surfacing on CI as an unrelated project's "worktree mutated" failure.
// A project that wants a meaningful API version declares one; the
// default is a constant, which is stable by construction.
func buildDocument(routes []discoveredRoute, project codegen.ProjectInfo) *document {
	overrides := readOptions(project.Options)

	title := cmp.Or(overrides.Title, project.Name, "API")
	version := cmp.Or(overrides.Version, project.DeclaredVersion, "0.0.0")

	doc := &document{
		OpenAPI: "3.0.3",
		Info: info{
			Title:       title,
			Version:     version,
			Description: overrides.Description,
		},
		Paths: map[string]pathItem{},
	}

	for _, r := range routes {
		path := normalizePath(r.Path)
		params := pathParameters(path)

		op := operation{
			OperationID: operationID(r.Method, path),
			Responses: map[string]response{
				"200": {Description: "Successful response"},
			},
		}
		if len(params) > 0 {
			op.Parameters = params
		}

		item, ok := doc.Paths[path]
		if !ok {
			item = pathItem{}
		}
		item[strings.ToLower(r.Method)] = op
		doc.Paths[path] = item
	}

	return doc
}

// normalizePath converts framework path syntax to OpenAPI 3 syntax. The
// http router accepts both `{id}` (Go 1.22+ stdlib style) and `[id]`; the
// OpenAPI spec only supports `{id}`.
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "[", "{")
	p = strings.ReplaceAll(p, "]", "}")
	return p
}

// pathParameters extracts the variable segments from an OpenAPI path and
// declares them as required string params. Without typed schema info every
// param defaults to string — sufficient for documentation and clients.
func pathParameters(path string) []parameter {
	var out []parameter
	for seg := range strings.SplitSeq(path, "/") {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := seg[1 : len(seg)-1]
		out = append(out, parameter{
			Name:     name,
			In:       "path",
			Required: true,
			Schema:   &schemaObject{Type: "string"},
		})
	}
	return out
}

// operationID builds a stable, lowercase identifier matching the runtime
// generator (go/framework/openapi/spec.go) so committed specs and runtime
// specs collide minimally.
func operationID(method, path string) string {
	clean := strings.ReplaceAll(path, "{", "")
	clean = strings.ReplaceAll(clean, "}", "")
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	id := strings.ToLower(method) + strings.Join(parts, "_")
	if len(parts) == 1 && parts[0] == "" {
		id = strings.ToLower(method)
	}
	return id
}

// stringLiteral extracts a Go string literal value from an expression,
// returning the unquoted content. Returns false for non-literal args
// (e.g. a constant identifier or a runtime expression) so we don't sweep
// dynamic registrations into a static spec.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind.String() != "STRING" {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// importPath returns the unquoted import path for an import spec.
func importPath(imp *ast.ImportSpec) string {
	v, err := strconv.Unquote(imp.Path.Value)
	if err != nil {
		return ""
	}
	return v
}

// filesByPath returns the file list sorted by absolute path so visitor
// output is deterministic regardless of map iteration order.
func filesByPath(files map[string]*ast.File) []*ast.File {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]*ast.File, 0, len(paths))
	for _, p := range paths {
		out = append(out, files[p])
	}
	return out
}

// --- minimal OpenAPI 3.0.3 wire types ---
//
// We intentionally don't import go.putnami.dev/openapi here: that module
// pulls in api/app/http and would balloon the extension binary. The wire
// format is small and stable, so we keep just enough to encode it.

type document struct {
	OpenAPI string              `json:"openapi"`
	Info    info                `json:"info"`
	Paths   map[string]pathItem `json:"paths"`
}

type info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// optionOverrides mirrors the structure of project.options.openapi in
// putnami.json. Each field is optional; absent values fall back to project
// metadata or defaults so the visitor still emits a usable spec without
// any explicit configuration.
type optionOverrides struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// readOptions extracts the openapi section of project.options if present.
// Malformed values are silently ignored — a typo in putnami.json should
// degrade to defaults, not fail the build.
func readOptions(opts map[string]json.RawMessage) optionOverrides {
	raw, ok := opts["openapi"]
	if !ok {
		return optionOverrides{}
	}
	var out optionOverrides
	_ = json.Unmarshal(raw, &out)
	return out
}

type pathItem map[string]operation

type operation struct {
	OperationID string              `json:"operationId,omitempty"`
	Parameters  []parameter         `json:"parameters,omitempty"`
	Responses   map[string]response `json:"responses"`
}

type parameter struct {
	Name     string        `json:"name"`
	In       string        `json:"in"`
	Required bool          `json:"required"`
	Schema   *schemaObject `json:"schema,omitempty"`
}

type response struct {
	Description string `json:"description"`
}

type schemaObject struct {
	Type string `json:"type,omitempty"`
}
