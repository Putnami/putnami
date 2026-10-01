// Package configextract scans Go source files for config.Config[T] calls
// and extracts config schemas to schema/config.json (committed) by default.
//
// Project config can opt into the gitignored .gen/config-schema.json fallback
// with options.generate.schema=false.
//
// Resolution covers nested object/array/map shapes by building a project-wide
// type registry (no go/packages dependency: same-project cross-package refs
// are expanded by indexing struct decls under their module-relative import
// path; third-party refs remain opaque and serialize as `{type:"object"}` with
// no children). Map keys are restricted to primitive types — non-primitive
// keys produce an extraction error so manifests stay portable across the Go
// and TS extractors.
package configextract

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	protocfg "go.putnami.dev/protocol/config"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// DefaultOutputPath is the project-relative path the config schema is emitted to.
const DefaultOutputPath = "schema/config.json"

// FallbackOutputPath is the gitignored location used when project config sets
// options.generate.schema=false.
const FallbackOutputPath = ".gen/config-schema.json"

// ArtifactResult describes the files written by config schema extraction.
type ArtifactResult struct {
	SchemaPath     string
	JSONSchemaPath string
	Blocks         int
}

// resolveOutputPath returns the project-relative path where the schema should
// be written.
func resolveOutputPath(ctx *pctx.Context) string {
	if commit, ok := pctx.GenerateSchemaCommit(ctx); ok {
		if commit {
			return DefaultOutputPath
		}
		return FallbackOutputPath
	}
	return DefaultOutputPath
}

// ProjectWantsCommittedSchemas reads project.options.generate.schema. When
// the user sets it to false we still write under .gen/ but skip the project
// tree copy. Default is to commit, matching TypeScript.
func ProjectWantsCommittedSchemas(ctx *pctx.Context) bool {
	if commit, ok := pctx.GenerateSchemaCommit(ctx); ok {
		return commit
	}
	return true
}

// Run executes the config schema extraction job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	emit.PhaseStart("config-extract")

	projectPath := ctx.Project.FullPath
	appName := ctx.Project.Name
	version := ""
	if ctx.Version != nil {
		version = ctx.Version.Full
	}

	result, ok, err := WriteArtifacts(projectPath, appName, version, resolveOutputPath(ctx))
	if err != nil {
		emit.PhaseEnd("config-extract", "failed")
		return "FAILED", nil, err
	}
	if !ok {
		emit.PhaseEnd("config-extract", "skipped")
		return "SKIP", map[string]any{"reason": "no config definitions found"}, nil
	}

	emit.PhaseEnd("config-extract", "success")
	return "OK", map[string]any{
		"schema":     result.SchemaPath,
		"jsonSchema": result.JSONSchemaPath,
		"blocks":     result.Blocks,
	}, nil
}

// WriteArtifacts extracts config blocks, refreshes the per-project infra
// requirements sidecar, and writes the schema manifest plus JSON Schema
// companion. outputPath is project-relative and should be either
// schema/config.json or .gen/config-schema.json for the built-in flows.
//
// The boolean return value is false when the project has no framework config
// definitions. In that case stale generated fallback artifacts are removed;
// the committed schema/config.json is left alone because it may be
// developer-owned until a cleanup migration can make ownership explicit.
func WriteArtifacts(projectPath, appName, version, outputPath string) (*ArtifactResult, bool, error) {
	configs, err := extractFromProject(projectPath)
	if err != nil {
		_ = removeInfraRequirements(projectPath)
		return nil, false, err
	}
	return writeSchemaFromBlocks(projectPath, appName, version, outputPath, configs)
}

// MergeDependencyBlocks re-extracts the workload's own config blocks, unions
// them with depBlocks — the library-owned blocks aggregated during the describe
// phase (see app.ConfigContributor) — and rewrites the schema manifest, JSON
// Schema companion, and infra secrets sidecar so a workload publishes its
// dependencies' config (and secrets) transitively. A path declared by both the
// workload and a dependency is a build error: the dependency-owned block must
// use a path the workload does not define.
//
// It is the describe-time counterpart to WriteArtifacts. WriteArtifacts emits
// the workload's own blocks during build-generate (from source); this folds the
// contributed dependency blocks in once the describe binary has surfaced them in
// the .gen config-deps fragment.
func MergeDependencyBlocks(projectPath, appName, version, outputPath string, depBlocks []protocfg.Block) (*ArtifactResult, bool, error) {
	own, err := extractFromProject(projectPath)
	if err != nil {
		_ = removeInfraRequirements(projectPath)
		return nil, false, err
	}
	merged, err := unionBlocks(own, depBlocks)
	if err != nil {
		return nil, false, err
	}
	return writeSchemaFromBlocks(projectPath, appName, version, outputPath, merged)
}

// writeSchemaFromBlocks refreshes the infra secrets sidecar from blocks and
// writes the schema manifest plus its JSON Schema companion. outputPath is
// project-relative and should be either schema/config.json or
// .gen/config-schema.json for the built-in flows.
//
// The boolean return value is false when blocks is empty. In that case stale
// generated fallback artifacts are removed; the committed schema/config.json is
// left alone because it may be developer-owned until a cleanup migration can
// make ownership explicit.
func writeSchemaFromBlocks(projectPath, appName, version, outputPath string, blocks []protocfg.Block) (*ArtifactResult, bool, error) {
	// Emit the per-project infra requirements scratch fragment from the schema's
	// sensitive fields. Runs even when no config blocks are found so a stale
	// fragment from a prior build is cleared.
	if err := writeInfraRequirements(projectPath, blocks); err != nil {
		return nil, false, err
	}

	if len(blocks) == 0 {
		_ = removeGeneratedSchemaArtifacts(projectPath)
		return nil, false, nil
	}

	schema := protocfg.SchemaManifest{
		AppName:    appName,
		Version:    version,
		SchemaHash: protocfg.ComputeSchemaHash(blocks),
		Configs:    blocks,
	}

	outPath := filepath.Join(projectPath, outputPath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, false, err
	}

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, false, err
	}

	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return nil, false, err
	}

	// Emit a companion JSON Schema document so editor validation (VS Code
	// YAML extension, ajv-based publish-time checks, …) can consume the
	// schema without putnami-specific knowledge. The companion sits next
	// to the manifest under the same output directory.
	jsonSchemaPath := jsonSchemaCompanionPath(outPath)
	jsonSchemaData, err := json.MarshalIndent(protocfg.RenderJSONSchema(&schema), "", "  ")
	if err != nil {
		return nil, false, err
	}
	if err := os.WriteFile(jsonSchemaPath, jsonSchemaData, 0o644); err != nil {
		return nil, false, err
	}

	return &ArtifactResult{
		SchemaPath:     outPath,
		JSONSchemaPath: jsonSchemaPath,
		Blocks:         len(blocks),
	}, true, nil
}

// unionBlocks concatenates the workload's own blocks with dependency-contributed
// blocks, rejecting any path declared on both sides (or duplicated within
// depBlocks). Own blocks come first in extraction order, then dependency blocks
// in contribution order; the canonicalizer sorts by path for hashing, so the
// order does not affect the schema hash but keeps the emitted manifest
// diff-friendly.
func unionBlocks(own, dep []protocfg.Block) ([]protocfg.Block, error) {
	ownPaths := make(map[string]bool, len(own))
	for _, b := range own {
		ownPaths[b.Path] = true
	}
	conflicts := map[string]bool{}
	depSeen := map[string]bool{}
	for _, b := range dep {
		if ownPaths[b.Path] || depSeen[b.Path] {
			conflicts[b.Path] = true
		}
		depSeen[b.Path] = true
	}
	if len(conflicts) > 0 {
		quoted := make([]string, 0, len(conflicts))
		for p := range conflicts {
			quoted = append(quoted, fmt.Sprintf("%q", p))
		}
		sort.Strings(quoted)
		return nil, fmt.Errorf("config path(s) %s declared by both the workload and a dependency; a dependency-owned config block must use a path the workload does not define", strings.Join(quoted, ", "))
	}
	merged := make([]protocfg.Block, 0, len(own)+len(dep))
	merged = append(merged, own...)
	merged = append(merged, dep...)
	return merged, nil
}

func removeGeneratedSchemaArtifacts(projectPath string) error {
	for _, rel := range []string{FallbackOutputPath, jsonSchemaCompanionPath(FallbackOutputPath)} {
		if err := os.Remove(filepath.Join(projectPath, rel)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// jsonSchemaCompanionPath turns "schema/config.json" into
// "schema/config.jsonschema.json" (and the legacy ".gen" path into the
// matching companion). Keeping both files in lockstep means consumers
// can derive one path from the other without inspecting both env vars.
func jsonSchemaCompanionPath(manifestPath string) string {
	ext := filepath.Ext(manifestPath)
	base := manifestPath[:len(manifestPath)-len(ext)]
	return base + ".jsonschema" + ext
}

// extractFromProject walks the project, builds a type registry, and extracts
// every config.Config[T]("path") block found across all Go files. Returns an
// aggregated error when any resolver invariant has been violated (e.g.
// non-primitive map keys), so the manifest is never partially emitted.
func extractFromProject(projectPath string) ([]protocfg.Block, error) {
	files, err := collectGoFiles(projectPath)
	if err != nil {
		return nil, err
	}
	modulePath := readModulePath(projectPath)

	reg := newTypeRegistry(projectPath, modulePath)
	var parsed []*parsedFile
	for _, file := range files {
		pf := parseFile(file)
		if pf == nil {
			continue
		}
		reg.indexFile(pf)
		parsed = append(parsed, pf)
	}

	var blocks []protocfg.Block
	var allErrs []error
	// pathSources records every Config[T]("path") call site so the
	// duplicate-path error message can point at the offending file:line
	// pairs. Without these positions, a user who accidentally registers
	// the same path twice gets only the bare path back — actionable
	// telemetry is a lot more useful when it tells you where to look.
	pathSources := make(map[string][]blockSource)
	for _, pf := range parsed {
		b, sources, errs := extractBlocks(pf, reg)
		blocks = append(blocks, b...)
		allErrs = append(allErrs, errs...)
		for i, blk := range b {
			pathSources[blk.Path] = append(pathSources[blk.Path], sources[i])
		}
	}
	// Iterate sorted by path for deterministic error messages.
	dupPaths := make([]string, 0, len(pathSources))
	for path, sources := range pathSources {
		if len(sources) > 1 {
			dupPaths = append(dupPaths, path)
		}
	}
	sort.Strings(dupPaths)
	for _, path := range dupPaths {
		seen := map[string]bool{}
		uniq := make([]string, 0, len(pathSources[path]))
		for _, src := range pathSources[path] {
			key := src.String()
			if !seen[key] {
				seen[key] = true
				uniq = append(uniq, key)
			}
		}
		sort.Strings(uniq)
		allErrs = append(allErrs, fmt.Errorf("duplicate config path %q registered in %s",
			path, strings.Join(uniq, ", ")))
	}
	if len(allErrs) > 0 {
		msgs := make([]string, len(allErrs))
		for i, e := range allErrs {
			msgs[i] = e.Error()
		}
		return nil, fmt.Errorf("config schema extraction failed:\n  - %s", strings.Join(msgs, "\n  - "))
	}
	return blocks, nil
}

// skippedDirs are project-relative directory basenames that never contain
// extractable Go source. Dotfile-prefixed directories are skipped
// generically so we don't waste I/O walking into .git, .idea, .cache,
// etc.
var skippedDirs = map[string]bool{
	"vendor":   true,
	".gen":     true,
	"testdata": true,
	".putnami": true,
	// `node_modules` doesn't normally host Go sources, but multi-language
	// monorepos pull it in next to Go packages — skip it preemptively.
	"node_modules": true,
}

func collectGoFiles(projectPath string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(projectPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			// Skip dotfile directories generically, plus known no-source
			// dirs. The project root itself can be `.putnami/foo` — only
			// skip dotdirs we encounter *below* projectPath.
			if path != projectPath && (skippedDirs[base] || strings.HasPrefix(base, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

var modulePathRe = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// readModulePath returns the module path declared in <projectPath>/go.mod, or
// "" when no go.mod is present. The module path is used to compute the
// import path of each file's containing directory.
func readModulePath(projectPath string) string {
	data, err := os.ReadFile(filepath.Join(projectPath, "go.mod"))
	if err != nil {
		return ""
	}
	m := modulePathRe.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// parsedFile bundles a file's AST with the metadata the resolver needs.
type parsedFile struct {
	path        string
	pkgPath     string            // import path for the file's directory
	configAlias string            // local name of the go.putnami.dev/config import (empty if not imported)
	imports     map[string]string // local alias → import path
	file        *ast.File
	fset        *token.FileSet // retained so resolver errors can include positions
}

func parseFile(filename string) *parsedFile {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
	if err != nil {
		return nil
	}
	imports := make(map[string]string, len(f.Imports))
	configAlias := ""
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		} else {
			alias = aliasFromImportPath(path)
		}
		imports[alias] = path
		if path == "go.putnami.dev/config" {
			configAlias = alias
		}
	}
	return &parsedFile{
		path:        filename,
		configAlias: configAlias,
		imports:     imports,
		file:        f,
		fset:        fset,
	}
}

// gopkgVersionRe matches the gopkg.in SIV suffix on a basename, e.g.
// "yaml.v3" → strips ".v3" so the alias becomes "yaml".
var gopkgVersionRe = regexp.MustCompile(`\.v\d+$`)

// majorVersionRe matches the go-modules SIV suffix used in Go module
// import paths, e.g. "github.com/foo/bar/v2" where the basename is "v2"
// but the package name is "bar".
var majorVersionRe = regexp.MustCompile(`^v\d+$`)

// aliasFromImportPath approximates the package name Go assigns to an
// import when no explicit alias is given. The exact value comes from the
// `package` clause of the imported file, which we can't read for
// cross-module imports. We handle the two conventional Semantic Import
// Versioning shapes:
//
//   - gopkg.in style: "gopkg.in/yaml.v3" → "yaml"
//   - go-modules style: "github.com/foo/bar/v2" → "bar"
//
// Falls back to the plain basename otherwise, matching `go list`'s
// default for unversioned modules.
func aliasFromImportPath(path string) string {
	base := filepath.Base(path)
	if loc := gopkgVersionRe.FindStringIndex(base); loc != nil {
		return base[:loc[0]]
	}
	if majorVersionRe.MatchString(base) {
		parent := filepath.Base(filepath.Dir(path))
		if parent != "" && parent != "." && parent != "/" {
			return parent
		}
	}
	return base
}

// typeRegistry indexes every named type declaration in the project by
// (pkgPath, typeName). The package path is the module-relative import path
// of the file's directory; when no go.mod is found we fall back to the
// file's package name, which still works for single-module projects.
//
// Indexing every TypeSpec (not just struct types) lets the resolver
// follow named-type indirections like `type Database = shared.Database`
// (alias, Assign != 0) and `type DB shared.Database` (named type) until
// it lands on a concrete shape.
type typeRegistry struct {
	projectPath string
	modulePath  string
	types       map[typeRef]ast.Expr
}

type typeRef struct {
	pkgPath string
	name    string
}

func newTypeRegistry(projectPath, modulePath string) *typeRegistry {
	return &typeRegistry{
		projectPath: projectPath,
		modulePath:  modulePath,
		types:       make(map[typeRef]ast.Expr),
	}
}

func (r *typeRegistry) indexFile(pf *parsedFile) {
	pf.pkgPath = r.filePkgPath(pf)
	for _, decl := range pf.file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			r.types[typeRef{pkgPath: pf.pkgPath, name: ts.Name.Name}] = ts.Type
		}
	}
}

// filePkgPath computes the import path for a file's directory. When a
// module path is known, it is "<module>/<rel>"; otherwise the AST package
// name is used as a synthetic key.
func (r *typeRegistry) filePkgPath(pf *parsedFile) string {
	if r.modulePath == "" {
		return pf.file.Name.Name
	}
	rel, err := filepath.Rel(r.projectPath, filepath.Dir(pf.path))
	if err != nil || rel == "." {
		return r.modulePath
	}
	return r.modulePath + "/" + filepath.ToSlash(rel)
}

func (r *typeRegistry) lookup(ref typeRef) (ast.Expr, bool) {
	expr, ok := r.types[ref]
	return expr, ok
}

// blockSource pairs a path with the source position of its registration
// call, so duplicate-path errors can point at the offending line(s)
// instead of just the file name.
type blockSource struct {
	file string
	pos  token.Position
}

func (s blockSource) String() string {
	if s.pos.Line > 0 {
		return fmt.Sprintf("%s:%d", s.file, s.pos.Line)
	}
	return s.file
}

// extractBlocks finds every config.Config[T]("path") call in a parsed file
// and emits a Block for each. Returns parallel source positions so the
// caller can aggregate duplicate-path errors with line numbers, plus any
// resolver errors so the build fails atomically.
func extractBlocks(pf *parsedFile, reg *typeRegistry) ([]protocfg.Block, []blockSource, []error) {
	if pf.configAlias == "" {
		return nil, nil, nil
	}

	var blocks []protocfg.Block
	var sources []blockSource
	var errs []error
	ast.Inspect(pf.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		idx, ok := call.Fun.(*ast.IndexExpr)
		if !ok {
			return true
		}
		sel, ok := idx.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != pf.configAlias || sel.Sel.Name != "Config" {
			return true
		}
		if len(call.Args) < 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		configPath := strings.Trim(lit.Value, `"`)

		resolver := newFieldResolver(pf, reg)
		field := resolver.resolve(idx.Index)
		errs = append(errs, resolver.errs...)
		// The block's fields are taken directly from the resolved struct
		// (Type=="object" with Fields). When resolution fails the block is
		// emitted with no fields, matching pre-existing behavior.
		var fields []protocfg.FieldSchema
		if field.Type == protocfg.FieldTypeObject {
			fields = field.Fields
		}
		blocks = append(blocks, protocfg.Block{Path: configPath, Fields: fields})
		var pos token.Position
		if pf.fset != nil {
			pos = pf.fset.Position(call.Pos())
		}
		sources = append(sources, blockSource{file: pf.path, pos: pos})
		return true
	})

	return blocks, sources, errs
}

// fieldResolver walks AST type expressions and produces FieldSchemas. It
// owns the cycle-guard stack and the import alias map needed to translate
// SelectorExpr references into registry lookups. Resolution errors (e.g.
// non-primitive map keys) accumulate on the resolver and are surfaced by
// the caller — extraction never partially succeeds when a protocol
// invariant has been violated.
type fieldResolver struct {
	pf  *parsedFile
	reg *typeRegistry
	// stack contains the struct types currently being expanded. When a type
	// reappears on the stack, the resolver emits an opaque object so the
	// recursion terminates deterministically (both languages apply the same
	// rule).
	stack map[*ast.StructType]bool
	// nameStack guards against alias cycles like `type A = B; type B = A`,
	// where the indirection runs through named types instead of struct
	// pointers. Same shape as stack: a revisited name yields an opaque
	// object.
	nameStack map[typeRef]bool
	errs      []error
}

func newFieldResolver(pf *parsedFile, reg *typeRegistry) *fieldResolver {
	return &fieldResolver{
		pf:        pf,
		reg:       reg,
		stack:     map[*ast.StructType]bool{},
		nameStack: map[typeRef]bool{},
	}
}

func (r *fieldResolver) recordError(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf("%s: "+format, append([]any{r.pf.path}, args...)...))
}

// resolve produces a FieldSchema describing the shape of a Go type
// expression. Name is left empty: callers (e.g. struct field walkers) fill
// it in.
func (r *fieldResolver) resolve(expr ast.Expr) protocfg.FieldSchema {
	switch t := expr.(type) {
	case *ast.Ident:
		if prim, ok := primitiveType(t.Name); ok {
			return protocfg.FieldSchema{Type: prim}
		}
		// Same-package named type — could be a struct, alias, or named type
		// pointing at another type. resolveNamed follows the indirection.
		ref := typeRef{pkgPath: r.pf.pkgPath, name: t.Name}
		if next, ok := r.reg.lookup(ref); ok {
			return r.resolveNamed(ref, next)
		}
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	case *ast.StarExpr:
		return r.resolve(t.X)
	case *ast.StructType:
		return r.resolveStruct(t)
	case *ast.SelectorExpr:
		// Qualified type: pkg.TypeName.
		if ident, ok := t.X.(*ast.Ident); ok {
			if ident.Name == "time" && t.Sel.Name == "Duration" {
				return protocfg.FieldSchema{Type: protocfg.FieldTypeDuration}
			}
			if importPath, ok := r.pf.imports[ident.Name]; ok {
				ref := typeRef{pkgPath: importPath, name: t.Sel.Name}
				if next, ok := r.reg.lookup(ref); ok {
					return r.resolveNamed(ref, next)
				}
			}
		}
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	case *ast.ArrayType:
		items := r.resolve(t.Elt)
		return protocfg.FieldSchema{Type: protocfg.FieldTypeArray, Items: &items}
	case *ast.MapType:
		key := r.resolve(t.Key)
		val := r.resolve(t.Value)
		if !protocfg.ValidMapKeyTypes[key.Type] {
			r.recordError("map key type %q is not a primitive; map keys must be string, int, or bool", key.Type)
			return protocfg.FieldSchema{Type: protocfg.FieldTypeMap, Keys: protocfg.FieldTypeString, Values: &val}
		}
		return protocfg.FieldSchema{Type: protocfg.FieldTypeMap, Keys: key.Type, Values: &val}
	case *ast.InterfaceType:
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	default:
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	}
}

// resolveNamed follows a named-type indirection. The ref identifies the
// declaration site (so alias cycles can be detected); expr is the
// underlying type expression the registry returned.
func (r *fieldResolver) resolveNamed(ref typeRef, expr ast.Expr) protocfg.FieldSchema {
	if r.nameStack[ref] {
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	}
	r.nameStack[ref] = true
	defer delete(r.nameStack, ref)
	return r.resolve(expr)
}

func (r *fieldResolver) resolveStruct(st *ast.StructType) protocfg.FieldSchema {
	if r.stack[st] {
		// Cycle: emit an opaque object so the recursion terminates with the
		// same shape that an unresolvable cross-module type would produce.
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	}
	r.stack[st] = true
	defer delete(r.stack, st)

	fields := r.extractStructFields(st)
	return protocfg.FieldSchema{Type: protocfg.FieldTypeObject, Fields: fields}
}

// extractStructFields walks a struct's fields. Outer named fields are
// emitted first; embedded fields are then inlined unless a name collides
// (Go's JSON marshaling resolves shadowing by preferring the outer field).
func (r *fieldResolver) extractStructFields(st *ast.StructType) []protocfg.FieldSchema {
	var fields []protocfg.FieldSchema
	seen := map[string]bool{}

	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue // embedded field — handled in the second pass
		}
		for _, name := range field.Names {
			if !name.IsExported() {
				continue
			}
			f := r.buildField(name.Name, field.Type, field.Tag)
			if f == nil {
				continue
			}
			if seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			fields = append(fields, *f)
		}
	}

	for _, field := range st.Fields.List {
		if len(field.Names) != 0 {
			continue
		}
		inlined := r.resolve(field.Type)
		if inlined.Type != protocfg.FieldTypeObject {
			continue
		}
		for _, nested := range inlined.Fields {
			if seen[nested.Name] {
				continue
			}
			seen[nested.Name] = true
			fields = append(fields, nested)
		}
	}

	return fields
}

// buildField resolves a single named field's type and overlays struct tags.
// Returns nil when the field should be skipped (json:"-").
func (r *fieldResolver) buildField(goName string, typeExpr ast.Expr, tag *ast.BasicLit) *protocfg.FieldSchema {
	resolved := r.resolve(typeExpr)
	resolved.Name = goName

	if tag != nil {
		st := reflect.StructTag(strings.Trim(tag.Value, "`"))
		if jsonTag := st.Get("json"); jsonTag != "" {
			name, _, _ := strings.Cut(jsonTag, ",")
			if name == "-" {
				return nil
			}
			if name != "" {
				resolved.Name = name
			}
		}
		if def := st.Get("default"); def != "" {
			resolved.Default = def
		}
		if env := st.Get("env"); env != "" {
			resolved.Env = env
		}
		if validate := st.Get("validate"); validate != "" {
			resolved.Constraints = strings.Split(validate, ",")
			for _, c := range resolved.Constraints {
				if c == "required" {
					resolved.Required = true
				}
			}
		}
		if st.Get("sensitive") == "true" {
			resolved.Sensitive = true
		}
		if st.Get("productionUnsafeDefault") == "true" {
			resolved.ProductionUnsafeDefault = true
		}
		if desc := st.Get("desc"); desc != "" {
			resolved.Description = desc
		}
	}

	return &resolved
}

// primitiveType maps Go identifier types to the canonical protocol vocabulary.
func primitiveType(name string) (string, bool) {
	switch name {
	case "string":
		return protocfg.FieldTypeString, true
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune":
		return protocfg.FieldTypeInt, true
	case "float32", "float64":
		return protocfg.FieldTypeFloat, true
	case "bool":
		return protocfg.FieldTypeBool, true
	}
	return "", false
}
