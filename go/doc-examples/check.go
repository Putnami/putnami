package docexamples

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Example is one Go block of a markdown document.
type Example struct {
	// Doc is the document's path, as reported in findings.
	Doc   string
	Block Block
}

// Finding is an error an example has, at its markdown line.
type Finding struct {
	Doc     string
	Line    int
	Message string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.Doc, f.Line, f.Message)
}

// workspaceModule is the import path prefix of this workspace's Go modules.
const workspaceModule = "go.putnami.dev/"

// tolerated are the type errors a documentation fragment may have: it may use
// a name it does not declare, leave a variable or an import unused, or stop
// before a return. Every other error fails the example.
var tolerated = []*regexp.Regexp{
	regexp.MustCompile(`^undefined: [\pL_][\pL\pN_]*$`),
	regexp.MustCompile(`^declared and not used: `),
	regexp.MustCompile(` imported and not used$`),
	regexp.MustCompile(` imported as \S+ and not used$`),
	regexp.MustCompile(`^missing return$`),
	regexp.MustCompile(`^label \S+ declared and not used$`),
	regexp.MustCompile(`^generic function is missing function body$`),
}

// toleratedInStatements are the errors that wrapping a block of statements
// in a function causes: the block may show an expression on its own, or
// declare a name again in a later step. Its returns are judged by
// acceptableReturn.
var toleratedInStatements = []*regexp.Regexp{
	regexp.MustCompile(` is not used$`),
	regexp.MustCompile(`^no new variables on left side of :=$`),
}

// Checker type-checks Go examples against the packages a directory of this
// workspace resolves through go.work.
type Checker struct {
	// Dir is where `go list` runs; it resolves the workspace's modules.
	Dir string

	fset    *token.FileSet
	exports map[string]string // import path -> export data file
	missing map[string]string // import path -> why it does not resolve
	pkgName map[string]string // import path -> package name
	dirs    map[string]string // workspace import path -> directory
	// packages maps a package name to the standard and workspace packages
	// of that name.
	packages map[string][]string
	// dependencies maps a package name to the workspace's third-party
	// dependencies of that name.
	dependencies map[string][]string
	// importer reads export data and keeps the packages it read.
	importer types.Importer
}

// NewChecker returns a checker that runs `go list` in dir.
func NewChecker(dir string) (*Checker, error) {
	c := &Checker{
		Dir:     dir,
		fset:    token.NewFileSet(),
		exports: map[string]string{},
		missing: map[string]string{},
		pkgName: map[string]string{},
		dirs:    map[string]string{},
	}
	c.importer = importer.ForCompiler(c.fset, "gc", c.lookup)
	// The standard library and the workspace give import paths by name; the
	// workspace's third-party dependencies, such as google.golang.org/grpc,
	// only stop a guess for a framework module of the same name.
	packages, err := c.goList("-e", "-deps", "-json=ImportPath,Name,Standard", "std", workspaceModule+"...")
	if err != nil {
		return nil, err
	}
	c.packages = map[string][]string{}
	c.dependencies = map[string][]string{}
	for _, pkg := range packages {
		path := pkg.ImportPath
		if pkg.Name == "" || pkg.Name == "main" || strings.Contains(path, "internal") ||
			strings.HasPrefix(path, "vendor/") || strings.HasPrefix(path, "cmd/") || strings.Contains(path, "/testdata") ||
			strings.Contains(path, "/examples/") || strings.Contains(path, "/samples/") {
			continue
		}
		if pkg.Standard || strings.HasPrefix(path, workspaceModule) {
			c.packages[pkg.Name] = append(c.packages[pkg.Name], path)
		} else {
			c.dependencies[pkg.Name] = append(c.dependencies[pkg.Name], path)
		}
	}
	return c, nil
}

// source is an example turned into a Go file.
type source struct {
	example Example
	file    *ast.File
	// statements is set when the block is statements wrapped in a function.
	statements bool
}

// Check type-checks each example and returns the findings, sorted by
// document and line.
func (c *Checker) Check(examples []Example) ([]Finding, error) {
	var findings []Finding
	var sources []*source
	for _, example := range examples {
		src, err := c.parse(example)
		if err != nil {
			line, message := example.Block.Line, err.Error()
			var list scanner.ErrorList
			if errors.As(err, &list) && len(list) > 0 {
				line, message = list[0].Pos.Line, list[0].Msg
			}
			findings = append(findings, Finding{
				Doc:  example.Doc,
				Line: line,
				Message: "the block does not parse as a Go file, declarations or statements: " + message +
					"; complete it, or mark the fence `" + NoCheck + "` when it is not meant to compile",
			})
			continue
		}
		sources = append(sources, src)
	}

	if err := c.resolve(declaredImports(sources)); err != nil {
		return nil, err
	}
	documents := c.documentScopes(sources)
	// A fragment uses a package or a type it does not declare: when another
	// block of its document imports that package or declares that type, carry
	// the import or the declaration over; otherwise import the standard or
	// workspace package of that name whose exports hold a selector the
	// fragment reads from it. Then check again. A carried type may itself use
	// names to carry, hence the rounds.
	pending := sources
	for round := 0; round < carryRounds && len(pending) > 0; round++ {
		undefined := map[*source][]undefinedName{}
		var candidates []string
		for _, src := range pending {
			names := c.undefinedNames(src)
			undefined[src] = names
			for _, name := range names {
				if _, ok := documents[src.example.Doc].imports[name.name]; !ok && name.qualifier {
					candidates = append(candidates, c.packages[name.name]...)
					candidates = append(candidates, c.dependencies[name.name]...)
				}
			}
		}
		if err := c.resolve(candidates); err != nil {
			return nil, err
		}
		var changed []*source
		var wanted []string
		for _, src := range pending {
			scope := documents[src.example.Doc]
			carried := false
			for _, name := range undefined[src] {
				path, ok := scope.imports[name.name]
				if !ok && name.qualifier {
					path, ok = c.packageHolding(name.name, selectors(src.file, name.name), scope.paths())
				}
				if ok && name.qualifier {
					addImport(src.file, name.name, path)
					wanted = append(wanted, path)
					carried = true
				} else if spec, ok := scope.types[name.name]; ok && scope.origin[spec] != src {
					src.file.Decls = append(src.file.Decls, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{spec}})
					carried = true
				}
			}
			if carried {
				changed = append(changed, src)
			}
		}
		if err := c.resolve(wanted); err != nil {
			return nil, err
		}
		pending = changed
	}
	seen := map[Finding]bool{}
	for _, src := range sources {
		for _, err := range c.typeErrors(src) {
			// A message that opens with a tab continues the previous one.
			if strings.HasPrefix(err.Msg, "\t") || acceptable(err.Msg, src.statements) || c.unresolvedOutside(err.Msg) {
				continue
			}
			if acceptableReturn(src, err) {
				continue
			}
			finding := Finding{
				Doc:     src.example.Doc,
				Line:    c.fset.Position(err.Pos).Line,
				Message: err.Msg,
			}
			// A carried type reports its own errors once per block that uses it.
			if !seen[finding] {
				seen[finding] = true
				findings = append(findings, finding)
			}
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Doc != findings[j].Doc {
			return findings[i].Doc < findings[j].Doc
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

func acceptable(message string, statements bool) bool {
	for _, pattern := range tolerated {
		if pattern.MatchString(message) {
			return true
		}
	}
	if statements {
		for _, pattern := range toleratedInStatements {
			if pattern.MatchString(message) {
				return true
			}
		}
	}
	return false
}

var couldNotImport = regexp.MustCompile(`^could not import (\S+) `)

// unresolvedOutside tolerates an import of a package outside this
// workspace, such as the reader's own module: the example uses it like a name
// it does not declare. A missing workspace package fails the example.
func (c *Checker) unresolvedOutside(message string) bool {
	match := couldNotImport.FindStringSubmatch(message)
	return match != nil && !strings.HasPrefix(strings.Trim(match[1], `"`), workspaceModule)
}

// parse turns a block into a Go file: a complete file, top-level
// declarations, or statements, wrapped in that order of preference. Line
// directives map the generated file's lines to the markdown document's.
func (c *Checker) parse(example Example) (*source, error) {
	code := example.Block.Code
	first := example.Block.Line + 1
	directive := func(line int) string { return fmt.Sprintf("//line %s:%d\n", example.Doc, line) }
	if file, err := parser.ParseFile(c.fset, example.Doc, directive(first)+code, parser.ParseComments); err == nil {
		return &source{example: example, file: file}, nil
	}
	declarations := "package example\n" + directive(first) + code
	if file, err := parser.ParseFile(c.fset, example.Doc, declarations, parser.ParseComments); err == nil {
		return &source{example: example, file: file}, nil
	}
	// Declarations, then statements: the longest prefix of top-level
	// declarations that parses stays at the top level, the rest runs in a
	// function.
	lines := strings.Split(code, "\n")
	// The error the plain statements form gives is the one to report.
	var lastErr error
	for k := len(lines); k >= 0; k-- {
		if k > 0 && k < len(lines) && (lines[k] == "" || lines[k][0] == ' ' || lines[k][0] == '\t' || lines[k][0] == '}' || lines[k][0] == ')') {
			continue
		}
		head, body := strings.Join(lines[:k], "\n"), strings.Join(lines[k:], "\n")
		statements := "package example\n" + directive(first) + head + "\nfunc _() {\n" + directive(first+k) + body + "\n}\n"
		file, err := parser.ParseFile(c.fset, example.Doc, statements, parser.ParseComments)
		if err == nil {
			return &source{example: example, file: file, statements: true}, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Checker) importName(spec *ast.ImportSpec, path string) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	if name := c.pkgName[path]; name != "" {
		return name
	}
	element := path[strings.LastIndex(path, "/")+1:]
	if isMajorVersion(element) && strings.Contains(path, "/") {
		trimmed := strings.TrimSuffix(path, "/"+element)
		element = trimmed[strings.LastIndex(trimmed, "/")+1:]
	}
	return strings.ReplaceAll(element, "-", "")
}

func isMajorVersion(element string) bool {
	if len(element) < 2 || element[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(element[1:])
	return err == nil
}

func declaredImports(sources []*source) []string {
	var paths []string
	for _, src := range sources {
		for _, spec := range src.file.Imports {
			if path, err := strconv.Unquote(spec.Path.Value); err == nil {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func addImport(file *ast.File, name, path string) {
	spec := &ast.ImportSpec{Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(path)}}
	if element := path[strings.LastIndex(path, "/")+1:]; element != name {
		spec.Name = ast.NewIdent(name)
	}
	file.Imports = append(file.Imports, spec)
	file.Decls = append([]ast.Decl{&ast.GenDecl{Tok: token.IMPORT, Specs: []ast.Spec{spec}}}, file.Decls...)
}

var returnCount = regexp.MustCompile(`^(too many|not enough) return values`)

// acceptableReturn tolerates a return of the wrong arity in a block of
// statements, where the wrapping function declares no results; a return
// inside a function the block itself writes is checked.
func acceptableReturn(src *source, err types.Error) bool {
	if !src.statements || !returnCount.MatchString(err.Msg) {
		return false
	}
	wrapped := true
	ast.Inspect(src.file, func(node ast.Node) bool {
		switch fn := node.(type) {
		case *ast.FuncLit:
			if fn.Pos() <= err.Pos && err.Pos < fn.End() {
				wrapped = false
			}
		case *ast.FuncDecl:
			if fn.Name.Name != "_" && fn.Pos() <= err.Pos && err.Pos < fn.End() {
				wrapped = false
			}
		}
		return wrapped
	})
	return wrapped
}

// selectors returns the names a file reads from a qualifier, as `Get` in
// `http.Get`.
func selectors(file *ast.File, qualifier string) []string {
	var names []string
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == qualifier {
				names = append(names, selector.Sel.Name)
			}
		}
		return true
	})
	return names
}

// packageHolding returns the one standard or workspace package named name
// whose exports hold a selector the fragment reads; the other selectors are
// then checked against it. When only one package bears the name and it is a
// framework module, it is imported even with no selector found, unless a
// selector names a field or a method of a type it or the document's workspace
// imports export: `client.Do` on a client variable stays a name the fragment
// does not declare, `inject.New` fails.
func (c *Checker) packageHolding(name string, selectors, documentPaths []string) (string, bool) {
	var found []string
	for _, path := range c.packages[name] {
		if _, ok := c.exports[path]; !ok {
			continue
		}
		pkg, err := c.importer.Import(path)
		if err != nil {
			continue
		}
		if exportsAny(pkg, selectors) {
			found = append(found, path)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	// One package of that name, none of whose exports the block reads: the
	// block either misspells a member, which must fail, or reads a variable
	// of one of the package's types, such as `client.Do` on a client.
	// Standard names such as log, trace or user are too often variables to
	// take that guess; a framework module is not.
	if paths := c.packages[name]; len(found) == 0 && len(paths) == 1 && frameworkModule(paths[0]) {
		if _, ok := c.exports[paths[0]]; !ok {
			return "", false
		}
		pkg, err := c.importer.Import(paths[0])
		if err != nil || readsMember(pkg, selectors) {
			return "", false
		}
		// A dependency of that name that exports what the block reads, such as
		// google.golang.org/grpc's NewServer, is the package the block means.
		for _, path := range c.dependencies[name] {
			if _, ok := c.exports[path]; !ok {
				continue
			}
			if other, err := c.importer.Import(path); err == nil && exportsAny(other, selectors) {
				return "", false
			}
		}
		for _, path := range documentPaths {
			// A standard type's member, such as os.LinkError's New field,
			// says nothing about a workspace variable.
			if !strings.HasPrefix(path, workspaceModule) {
				continue
			}
			if other, err := c.importer.Import(path); err == nil && readsMember(other, selectors) {
				return "", false
			}
		}
		return paths[0], true
	}
	return "", false
}

// exportsAny reports whether the package exports one of the selectors.
func exportsAny(pkg *types.Package, selectors []string) bool {
	for _, selector := range selectors {
		if object := pkg.Scope().Lookup(selector); object != nil && object.Exported() {
			return true
		}
	}
	return false
}

// frameworkModule reports whether a path is a module of the documented
// framework, such as go.putnami.dev/inject, rather than a deeper package whose
// name, such as runner or registry, a fragment's variable often bears.
func frameworkModule(path string) bool {
	rest, ok := strings.CutPrefix(path, workspaceModule)
	return ok && !strings.Contains(rest, "/")
}

// readsMember reports whether a selector is a field or a method of a type the
// package exports.
func readsMember(pkg *types.Package, selectors []string) bool {
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		typeName, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || !typeName.Exported() {
			continue
		}
		for _, selector := range selectors {
			for _, typ := range []types.Type{typeName.Type(), types.NewPointer(typeName.Type())} {
				if object, _, _ := types.LookupFieldOrMethod(typ, true, pkg, selector); object != nil {
					return true
				}
			}
		}
	}
	return false
}

// WorkspaceDirs returns the directories of the workspace packages the checked
// examples import, directly or through their dependencies.
func (c *Checker) WorkspaceDirs() []string {
	dirs := make([]string, 0, len(c.dirs))
	for _, dir := range c.dirs {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	return dirs
}

// carryRounds bounds how many times a fragment takes imports and types from
// its document.
const carryRounds = 3

// scope is what the blocks of one document declare, for its fragments.
type scope struct {
	// imports maps a package name to the path the document imports it from.
	imports map[string]string
	// types maps a type name to the declaration a block of the document holds.
	types map[string]*ast.TypeSpec
	// origin maps a type declaration to the block that holds it.
	origin map[*ast.TypeSpec]*source
}

// paths returns the import paths the document's blocks import, resolved.
func (sc *scope) paths() []string {
	paths := make([]string, 0, len(sc.imports))
	for _, path := range sc.imports {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// documentScopes collects, per document, the packages its blocks import and
// the types they declare. A name two blocks import or declare differently is
// left out.
func (c *Checker) documentScopes(sources []*source) map[string]*scope {
	scopes := map[string]*scope{}
	conflicts := map[string]map[string]bool{}
	for _, src := range sources {
		doc := src.example.Doc
		sc := scopes[doc]
		if sc == nil {
			sc = &scope{imports: map[string]string{}, types: map[string]*ast.TypeSpec{}, origin: map[*ast.TypeSpec]*source{}}
			scopes[doc] = sc
			conflicts[doc] = map[string]bool{}
		}
		for _, spec := range src.file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			name := c.importName(spec, path)
			if name == "" || name == "_" || name == "." || conflicts[doc]["import "+name] {
				continue
			}
			if known, ok := sc.imports[name]; ok && known != path {
				delete(sc.imports, name)
				conflicts[doc]["import "+name] = true
				continue
			}
			sc.imports[name] = path
		}
		for _, spec := range typeSpecs(src.file) {
			name := spec.Name.Name
			if conflicts[doc]["type "+name] {
				continue
			}
			if _, ok := sc.types[name]; ok {
				delete(sc.types, name)
				conflicts[doc]["type "+name] = true
				continue
			}
			sc.types[name] = spec
			sc.origin[spec] = src
		}
	}
	return scopes
}

// typeSpecs returns the types a file declares at the top level, or in the
// function that wraps a block of statements.
func typeSpecs(file *ast.File) []*ast.TypeSpec {
	var specs []*ast.TypeSpec
	collect := func(decl ast.Decl) {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					specs = append(specs, typeSpec)
				}
			}
		}
	}
	for _, decl := range file.Decls {
		collect(decl)
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "_" && fn.Recv == nil && fn.Body != nil {
			for _, stmt := range fn.Body.List {
				if declStmt, ok := stmt.(*ast.DeclStmt); ok {
					collect(declStmt.Decl)
				}
			}
		}
	}
	return specs
}

// undefinedName is a name an example uses without declaring it.
type undefinedName struct {
	name string
	// qualifier is set when the name qualifies a selector, as `http` in
	// `http.Get`.
	qualifier bool
}

// undefinedNames returns the names the example uses without declaring them.
func (c *Checker) undefinedNames(src *source) []undefinedName {
	undefined := map[string]bool{}
	var order []string
	for _, err := range c.typeErrors(src) {
		if name, ok := strings.CutPrefix(err.Msg, "undefined: "); ok && !strings.Contains(name, ".") && !undefined[name] {
			undefined[name] = true
			order = append(order, name)
		}
	}
	// A name used on its own, as `log` in `run(log)`, is a value, not a
	// package, even where it also qualifies a selector.
	qualifiers := map[string]bool{}
	values := map[string]bool{}
	ast.Inspect(src.file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if ident, ok := selector.X.(*ast.Ident); ok && undefined[ident.Name] {
				qualifiers[ident.Name] = true
				return false
			}
		}
		if ident, ok := node.(*ast.Ident); ok && undefined[ident.Name] {
			values[ident.Name] = true
		}
		return true
	})
	names := make([]undefinedName, 0, len(order))
	for _, name := range order {
		names = append(names, undefinedName{name: name, qualifier: qualifiers[name] && !values[name]})
	}
	return names
}

func (c *Checker) typeErrors(src *source) []types.Error {
	var errs []types.Error
	config := types.Config{
		Importer: c.importer,
		Error: func(err error) {
			var typeErr types.Error
			if errors.As(err, &typeErr) {
				errs = append(errs, typeErr)
			}
		},
	}
	_, _ = config.Check(src.file.Name.Name, c.fset, []*ast.File{src.file}, nil)
	return errs
}

func (c *Checker) lookup(path string) (io.ReadCloser, error) {
	file, ok := c.exports[path]
	if !ok {
		if why, missing := c.missing[path]; missing {
			return nil, errors.New(why)
		}
		return nil, fmt.Errorf("package %s is not resolved", path)
	}
	return os.Open(file)
}

// listed is the part of `go list -json` output the checker reads.
type listed struct {
	ImportPath string
	Name       string
	Dir        string
	Export     string
	Standard   bool
	Error      *struct{ Err string }
}

// resolve compiles the packages and their dependencies with `go list
// -export`, and records their export data.
func (c *Checker) resolve(paths []string) error {
	var pending []string
	for _, path := range paths {
		if _, ok := c.exports[path]; ok {
			continue
		}
		if _, ok := c.missing[path]; ok {
			continue
		}
		if path == "C" || path == "unsafe" {
			continue
		}
		pending = append(pending, path)
	}
	slices.Sort(pending)
	pending = slices.Compact(pending)
	if len(pending) == 0 {
		return nil
	}
	packages, err := c.goList(append([]string{"-e", "-export", "-deps", "-json=ImportPath,Name,Dir,Export,Standard,Error"}, pending...)...)
	if err != nil {
		return err
	}
	for _, pkg := range packages {
		if pkg.Name != "" {
			c.pkgName[pkg.ImportPath] = pkg.Name
		}
		if strings.HasPrefix(pkg.ImportPath, workspaceModule) && pkg.Dir != "" {
			c.dirs[pkg.ImportPath] = pkg.Dir
		}
		if pkg.Export != "" {
			c.exports[pkg.ImportPath] = pkg.Export
		} else if pkg.Error != nil {
			c.missing[pkg.ImportPath] = pkg.Error.Err
		}
	}
	for _, path := range pending {
		if _, ok := c.exports[path]; !ok {
			if _, ok := c.missing[path]; !ok {
				c.missing[path] = "package " + path + " has no export data"
			}
		}
	}
	return nil
}

func (c *Checker) goList(args ...string) ([]listed, error) {
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = c.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		return nil, fmt.Errorf("go list %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	var packages []listed
	decoder := json.NewDecoder(&stdout)
	for decoder.More() {
		var pkg listed
		if err := decoder.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("go list: %w", err)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}
