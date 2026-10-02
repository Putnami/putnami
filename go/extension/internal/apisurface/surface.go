// Package apisurface reads the exported API of the Go packages in a set of
// source files and compares two such surfaces.
//
// It parses source with go/parser and reads declarations only: no
// type-checking, no build, no module download. The answer is therefore a
// function of the file bytes alone, so the same files always give the same
// surface, whichever machine reads them and whether they come from a git tag
// or from the working tree.
package apisurface

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// File is one source file of a project: its slash-separated path relative to
// the project root, and its bytes.
type File struct {
	Path string
	Data []byte
}

// Surface is the exported API of every importable package of one module.
type Surface struct {
	// Module is the module path the import paths are built from.
	Module string
	// Packages maps each import path to its package.
	Packages map[string]*Package
}

// Package is the exported API of one importable package.
type Package struct {
	ImportPath string
	// Objects maps each exported top-level name to its declaration.
	Objects map[string]*Object
}

// Kind is the kind of a top-level declaration.
type Kind string

// The top-level declaration kinds.
const (
	KindFunc  Kind = "func"
	KindType  Kind = "type"
	KindVar   Kind = "var"
	KindConst Kind = "const"
)

// embeddedPrefix opens the key of an element an interface embeds.
const embeddedPrefix = "embedded "

// Type forms: what a type declaration declares.
const (
	formStruct    = "struct"
	formInterface = "interface"
	formAlias     = "alias"
	formDefined   = "defined"
)

// Text is a rendered signature or type. Key is what a comparison reads: the
// rendering with every type parameter replaced by its position, so renaming a
// type parameter is not a change. Display is the rendering a message shows.
// Both drop parameter and result names, so renaming them is not a change
// either.
type Text struct {
	Key     string
	Display string
}

// Position is where a declaration sits: a project-relative file and a line.
type Position struct {
	File string
	Line int
}

// Object is one exported top-level declaration.
type Object struct {
	Kind Kind
	Name string
	Pos  Position
	// Type is the signature of a func, the explicit type of a var or a const
	// (empty when the type is inferred), and the right-hand side of an alias
	// or of a defined type that is neither a struct nor an interface.
	Type Text
	// Form is what a type declaration declares: struct, interface, alias or
	// defined. Empty for any other kind.
	Form string
	// TypeParams is the type parameter list of a generic func or type.
	TypeParams Text
	// Fields are the exported fields of a struct type, embedded ones under the
	// name of the embedded type.
	Fields map[string]*Member
	// Methods are the exported methods declared on a type.
	Methods map[string]*Member
	// Interface holds the exported methods of an interface type under their
	// name, and its embedded elements under "embedded <element>".
	Interface map[string]*Member
	// Sealed is true for an interface no other package can implement: one with
	// an unexported method, or one that embeds a sealed interface of its own
	// package. Adding a method to it breaks no one.
	Sealed bool
}

// Member is a field, a method, or an interface element.
type Member struct {
	Type Text
	// Pointer is true for a method declared on a pointer receiver.
	Pointer bool
	Pos     Position
}

// Read returns the exported API of the packages files make up. files are the
// project's Go files and go.mod files; module is the module path to use when
// the project root has no go.mod.
//
// A package is a directory with at least one Go file the go command would
// build on some platform, other than a test file and other than a main
// package. Directories that no other module can import are left out: any
// path segment named internal, testdata or vendor, any segment starting with
// "." or "_", and any directory below a nested go.mod, which is another
// module.
func Read(files []File, module string) (*Surface, error) {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.Path
		if file.Path == "go.mod" {
			if declared := modfile.ModulePath(file.Data); declared != "" {
				module = declared
			}
		}
	}
	nested := nestedModules(paths)

	byDir := map[string][]File{}
	for _, file := range files {
		if !importableGoFile(file.Path, nested) {
			continue
		}
		dir := path.Dir(file.Path)
		byDir[dir] = append(byDir[dir], file)
	}

	surface := &Surface{Module: module, Packages: map[string]*Package{}}
	for _, dir := range sortedKeys(byDir) {
		pkg, err := readPackage(importPath(module, dir), byDir[dir])
		if err != nil {
			return nil, err
		}
		if pkg != nil {
			surface.Packages[pkg.ImportPath] = pkg
		}
	}
	return surface, nil
}

// Reads returns, in their order, the paths among paths that Read reads: every
// go.mod, which gives the module path or marks where a nested module starts,
// and every Go file of an importable package. A caller that pays to load file
// bytes can load only these; Read applies the same selection to the files it
// is given.
func Reads(paths []string) []string {
	nested := nestedModules(paths)
	var kept []string
	for _, file := range paths {
		if path.Base(file) == "go.mod" || importableGoFile(file, nested) {
			kept = append(kept, file)
		}
	}
	return kept
}

// nestedModules returns the directories below the project root that hold a
// go.mod: each is another module.
func nestedModules(paths []string) map[string]bool {
	nested := map[string]bool{}
	for _, file := range paths {
		if path.Base(file) == "go.mod" {
			if dir := path.Dir(file); dir != "." {
				nested[dir] = true
			}
		}
	}
	return nested
}

// importPath joins a module path and a project-relative directory.
func importPath(module, dir string) string {
	if dir == "." {
		return module
	}
	return module + "/" + dir
}

// importableGoFile reports whether a project-relative path is a non-test Go
// file of a package another module can import.
func importableGoFile(file string, nested map[string]bool) bool {
	base := path.Base(file)
	if !strings.HasSuffix(base, ".go") || strings.HasSuffix(base, "_test.go") {
		return false
	}
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
		return false
	}
	dir := path.Dir(file)
	if dir == "." {
		return true
	}
	segments := strings.Split(dir, "/")
	for i, segment := range segments {
		switch {
		case segment == "internal", segment == "testdata", segment == "vendor":
			return false
		case strings.HasPrefix(segment, "."), strings.HasPrefix(segment, "_"):
			return false
		case nested[strings.Join(segments[:i+1], "/")]:
			return false
		}
	}
	return true
}

// pendingMethod is a method read before every type of its package is known.
type pendingMethod struct {
	receiver string
	name     string
	member   *Member
}

// readPackage reads one directory's files in path order. The first
// declaration of a name wins, so two platform variants of one function always
// resolve the same way. It returns nil when no file belongs to an importable
// package.
func readPackage(importPath string, files []File) (*Package, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	fset := token.NewFileSet()
	pkg := &Package{ImportPath: importPath, Objects: map[string]*Object{}}
	interfaces := map[string]*localInterface{}
	var methods []pendingMethod
	found := false
	for _, file := range files {
		// The header decides whether the file is built at all, as it does for
		// the go command: a file no platform builds may hold anything below it.
		header, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Data, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file.Path, err)
		}
		if header.Name.Name == "main" || excludedOnEveryPlatform(header) {
			continue
		}
		parsed, err := parser.ParseFile(fset, file.Path, file.Data, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file.Path, err)
		}
		found = true
		reader := declReader{fset: fset, pkg: pkg, interfaces: interfaces, render: newRenderer(parsed)}
		for _, decl := range parsed.Decls {
			methods = append(methods, reader.read(decl)...)
		}
	}
	if !found {
		return nil, nil
	}
	sealed := sealedInterfaces(interfaces)
	for name, object := range pkg.Objects {
		if object.Form == formInterface && sealed[name] {
			object.Sealed = true
		}
	}
	for _, method := range methods {
		owner, ok := pkg.Objects[method.receiver]
		if !ok || owner.Kind != KindType {
			continue
		}
		if owner.Methods == nil {
			owner.Methods = map[string]*Member{}
		}
		if _, seen := owner.Methods[method.name]; !seen {
			owner.Methods[method.name] = method.member
		}
	}
	return pkg, nil
}

// excludedOnEveryPlatform reports whether a file's //go:build constraint
// cannot hold, whatever the platform and build tags: the go command never
// builds such a file, so it declares no API. The "ignore" tag is never set.
// A file without a constraint, or with one that does not parse, is kept.
func excludedOnEveryPlatform(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err != nil {
				return false
			}
			return !satisfiable(expr)
		}
	}
	return false
}

// maxConstraintTags bounds the tags satisfiable enumerates; a constraint with
// more is kept, which can only add API, never hide it.
const maxConstraintTags = 16

// satisfiable reports whether some assignment of the tags expr names makes it
// true, with "ignore" always false.
func satisfiable(expr constraint.Expr) bool {
	seen := map[string]bool{}
	var tags []string
	expr.Eval(func(tag string) bool {
		if tag != "ignore" && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
		return false
	})
	if len(tags) > maxConstraintTags {
		return true
	}
	for assignment := 0; assignment < 1<<len(tags); assignment++ {
		set := map[string]bool{}
		for i, tag := range tags {
			set[tag] = assignment&(1<<i) != 0
		}
		if expr.Eval(func(tag string) bool { return set[tag] }) {
			return true
		}
	}
	return false
}

// declReader reads the exported declarations of one file into a package.
type declReader struct {
	fset *token.FileSet
	pkg  *Package
	// interfaces collects every interface type the package declares, exported
	// or not, for sealedInterfaces.
	interfaces map[string]*localInterface
	render     renderer
}

// localInterface is what sealing reads of an interface type a package
// declares.
type localInterface struct {
	// unexported is true when the interface declares an unexported method.
	unexported bool
	// embeds names the interfaces of the same package it embeds.
	embeds []string
}

// describeInterface reads the sealing facts of an interface type.
func describeInterface(typ *ast.InterfaceType) *localInterface {
	described := &localInterface{}
	for _, field := range typ.Methods.List {
		if len(field.Names) == 0 {
			if name := localTypeName(field.Type); name != "" {
				described.embeds = append(described.embeds, name)
			}
			continue
		}
		for _, name := range field.Names {
			if !name.IsExported() {
				described.unexported = true
			}
		}
	}
	return described
}

// localTypeName returns the name of a type of the same package expr names,
// with or without type arguments; empty for a qualified name or any other
// expression.
func localTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return localTypeName(e.X)
	case *ast.IndexListExpr:
		return localTypeName(e.X)
	}
	return ""
}

// sealedInterfaces returns the names of the interfaces no other package can
// implement: those with an unexported method, and those that embed one,
// through any number of embeddings within the package.
func sealedInterfaces(interfaces map[string]*localInterface) map[string]bool {
	sealed := map[string]bool{}
	visiting := map[string]bool{}
	done := map[string]bool{}
	var visit func(name string) bool
	visit = func(name string) bool {
		described, ok := interfaces[name]
		if !ok || visiting[name] {
			return false
		}
		if done[name] {
			return sealed[name]
		}
		visiting[name] = true
		result := described.unexported
		for _, embedded := range described.embeds {
			if visit(embedded) {
				result = true
			}
		}
		visiting[name] = false
		done[name] = true
		sealed[name] = result
		return result
	}
	for name := range interfaces {
		visit(name)
	}
	return sealed
}

func (r declReader) position(pos token.Pos) Position {
	at := r.fset.Position(pos)
	return Position{File: at.Filename, Line: at.Line}
}

// add records an object unless an earlier file already declared its name.
func (r declReader) add(object *Object) {
	if _, seen := r.pkg.Objects[object.Name]; !seen {
		r.pkg.Objects[object.Name] = object
	}
}

// read records the exported objects decl declares and returns its exported
// method, which waits until every type of the package is known.
func (r declReader) read(decl ast.Decl) []pendingMethod {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if !d.Name.IsExported() {
			return nil
		}
		if d.Recv == nil || len(d.Recv.List) == 0 {
			params := typeParamNames(d.Type.TypeParams)
			r.add(&Object{
				Kind:       KindFunc,
				Name:       d.Name.Name,
				Pos:        r.position(d.Name.Pos()),
				TypeParams: r.render.renderTypeParams(d.Type.TypeParams, params),
				Type:       r.render.renderSignature(d.Type, params),
			})
			return nil
		}
		receiver, pointer, params := receiverOf(d.Recv.List[0].Type)
		if receiver == "" || !ast.IsExported(receiver) {
			return nil
		}
		return []pendingMethod{{receiver: receiver, name: d.Name.Name, member: &Member{
			Type:    r.render.renderSignature(d.Type, params),
			Pointer: pointer,
			Pos:     r.position(d.Name.Pos()),
		}}}
	case *ast.GenDecl:
		switch d.Tok {
		case token.TYPE:
			for _, spec := range d.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				r.recordInterface(typeSpec)
				if typeSpec.Name.IsExported() {
					r.add(r.readType(typeSpec))
				}
			}
		case token.VAR, token.CONST:
			r.readValues(d)
		}
	}
	return nil
}

// recordInterface records the sealing facts of an interface type declaration,
// exported or not, unless an earlier file already declared its name.
func (r declReader) recordInterface(spec *ast.TypeSpec) {
	typ, ok := spec.Type.(*ast.InterfaceType)
	if !ok || spec.Assign.IsValid() {
		return
	}
	if _, seen := r.interfaces[spec.Name.Name]; !seen {
		r.interfaces[spec.Name.Name] = describeInterface(typ)
	}
}

// readValues records the exported names of a var or const declaration with
// their explicit type. In a const group, a spec with neither a type nor values
// repeats the previous spec's, as iota groups do.
func (r declReader) readValues(decl *ast.GenDecl) {
	kind := KindVar
	if decl.Tok == token.CONST {
		kind = KindConst
	}
	var inherited ast.Expr
	for _, spec := range decl.Specs {
		value, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		typ := value.Type
		if kind == KindConst {
			if typ == nil && len(value.Values) == 0 {
				typ = inherited
			} else {
				inherited = typ
			}
		}
		rendered := Text{}
		if typ != nil {
			rendered = r.render.renderType(typ, nil)
		}
		for _, name := range value.Names {
			if name.IsExported() {
				r.add(&Object{Kind: kind, Name: name.Name, Pos: r.position(name.Pos()), Type: rendered})
			}
		}
	}
}

// readType reads one exported type declaration.
func (r declReader) readType(spec *ast.TypeSpec) *Object {
	params := typeParamNames(spec.TypeParams)
	object := &Object{
		Kind:       KindType,
		Name:       spec.Name.Name,
		Pos:        r.position(spec.Name.Pos()),
		TypeParams: r.render.renderTypeParams(spec.TypeParams, params),
	}
	if spec.Assign.IsValid() {
		object.Form = formAlias
		object.Type = r.render.renderType(spec.Type, params)
		return object
	}
	switch typ := spec.Type.(type) {
	case *ast.StructType:
		object.Form = formStruct
		object.Fields = map[string]*Member{}
		for _, field := range typ.Fields.List {
			names := fieldNames(field)
			if len(names) == 0 {
				continue
			}
			member := &Member{Type: r.render.renderType(field.Type, params), Pos: r.position(field.Type.Pos())}
			for _, name := range names {
				if _, seen := object.Fields[name]; !seen {
					object.Fields[name] = member
				}
			}
		}
	case *ast.InterfaceType:
		object.Form = formInterface
		object.Interface = map[string]*Member{}
		for _, field := range typ.Methods.List {
			if len(field.Names) == 0 {
				element := r.render.renderType(field.Type, params)
				object.Interface[embeddedPrefix+element.Key] = &Member{Type: element, Pos: r.position(field.Type.Pos())}
				continue
			}
			// An unexported method is no one else's to call or implement; it
			// seals the interface, which readPackage records.
			for _, name := range field.Names {
				if !name.IsExported() {
					continue
				}
				object.Interface[name.Name] = &Member{Type: r.render.renderType(field.Type, params), Pos: r.position(name.Pos())}
			}
		}
	default:
		object.Form = formDefined
		object.Type = r.render.renderType(spec.Type, params)
	}
	return object
}

// fieldNames returns the exported names a struct field declares: its own
// names, or the name of the type it embeds.
func fieldNames(field *ast.Field) []string {
	if len(field.Names) == 0 {
		if name := baseTypeName(field.Type); name != "" && ast.IsExported(name) {
			return []string{name}
		}
		return nil
	}
	var names []string
	for _, name := range field.Names {
		if name.IsExported() {
			names = append(names, name.Name)
		}
	}
	return names
}

// baseTypeName returns the name of the type expr names, without pointer,
// package qualifier or type arguments; empty when expr names no type.
func baseTypeName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.SelectorExpr:
			return e.Sel.Name
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		default:
			return ""
		}
	}
}

// receiverOf returns the base type name of a method receiver, whether the
// receiver is a pointer, and the receiver's type parameter names in order.
func receiverOf(expr ast.Expr) (name string, pointer bool, params map[string]string) {
	if paren, ok := expr.(*ast.ParenExpr); ok {
		expr = paren.X
	}
	if star, ok := expr.(*ast.StarExpr); ok {
		pointer = true
		expr = star.X
	}
	var indices []ast.Expr
	switch e := expr.(type) {
	case *ast.IndexExpr:
		indices = []ast.Expr{e.Index}
		expr = e.X
	case *ast.IndexListExpr:
		indices = e.Indices
		expr = e.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return "", false, nil
	}
	params = map[string]string{}
	for i, index := range indices {
		if param, ok := index.(*ast.Ident); ok && param.Name != "_" {
			params[param.Name] = placeholder(i)
		}
	}
	return ident.Name, pointer, params
}

// typeParamNames maps each type parameter name of list to its positional
// placeholder.
func typeParamNames(list *ast.FieldList) map[string]string {
	if list == nil {
		return nil
	}
	params := map[string]string{}
	position := 0
	for _, field := range list.List {
		for _, name := range field.Names {
			if name.Name != "_" {
				params[name.Name] = placeholder(position)
			}
			position++
		}
	}
	return params
}

// placeholder is the name a type parameter takes in a Key. "$" cannot start a
// Go identifier, so a placeholder never collides with a declared name.
func placeholder(position int) string {
	return fmt.Sprintf("$%d", position)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
