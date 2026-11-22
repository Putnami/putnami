package readers

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// surfaceRecord is the reviewed exported surface of the package. A consumer
// outside this repository compiles against it, so a change to it is an edit of
// this record, made in the same change and read by its reviewer.
const surfaceRecord = "testdata/api/surface.txt"

// fixturelessDirs are the testdata directories that hold no reader fixture:
// the malformed reports, pinned by their expectations instead of a golden
// output, the sources the fixtures were produced from, and the surface record.
var fixturelessDirs = []string{"api", "malformed", "sources"}

func TestExportedSurfaceMatchesItsRecord(t *testing.T) {
	got := exportedSurface(t, ".")
	if *update {
		if err := os.MkdirAll(filepath.Dir(surfaceRecord), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(surfaceRecord, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := read(t, surfaceRecord)
	if !bytes.Equal(got, want) {
		t.Errorf("the exported surface differs from %s; a change to the public API updates that record in the same change: run the tests with -update and review the diff\ngot:\n%s", surfaceRecord, got)
	}
	spectest.Proves(t, feature, "exported-surface-is-pinned", "the-exported-surface-matches-its-record")
}

func TestEveryFixtureHasItsGoldenOutput(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() || slices.Contains(fixturelessDirs, entry.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join("testdata", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		names := make(map[string]bool, len(files))
		for _, file := range files {
			names[file.Name()] = true
		}
		for name := range names {
			path := filepath.Join("testdata", entry.Name(), name)
			if report, ok := strings.CutSuffix(name, ".golden.json"); ok {
				if !names[report] {
					t.Errorf("%s has no report beside it", path)
				}
				continue
			}
			checked++
			if !names[name+".golden.json"] {
				t.Errorf("%s has no golden output; run the tests with -update and review it", path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("testdata holds no reader fixture, so this check proved nothing")
	}
	spectest.Proves(t, feature, "exported-surface-is-pinned", "every-fixture-has-its-golden-output")
}

func TestExportedSurfaceKeepsOnlyExportedNames(t *testing.T) {
	dir := t.TempDir()
	source := `package sample

// Doc comments are not part of the surface.
const (
	Exported = "a"
	hidden   = "b"
)

var Limit, internal = 1, 2

type Shape struct {
	Name   string ` + "`json:\"name\"`" + `
	secret int
	Embedded
	embedded
}

type Embedded interface {
	Area() float64
	scale()
}

type embedded struct{}

type hiddenType int

func (Shape) Method() {}

func (hiddenType) Method() {}

func (s *Shape) pointer() {}

func Read[T any](data []byte) T { var zero T; return zero }

func helper() {}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample_test.go"), []byte("package sample\n\nfunc TestOnly() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := `type Embedded interface {
	Area() float64
}

const Exported = "a"

var Limit = 1

func Read[T any](data []byte) T

type Shape struct {
	Name string ` + "`json:\"name\"`" + `
	Embedded
}

func (Shape) Method()
`
	if got := string(exportedSurface(t, dir)); got != want {
		t.Errorf("surface:\n%s\nwant:\n%s", got, want)
	}
}

// exportedSurface prints every exported declaration of the package in dir,
// test files left out: a function or a method of an exported type with its
// signature and no body, a constant or a variable with its type and value, a
// type with its exported fields and methods and their tags. Comments are left
// out, so rewording a doc comment does not touch the record. Declarations are
// sorted by name, a method after its type.
func exportedSurface(t *testing.T, dir string) []byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	type entry struct {
		key  string
		node any
	}
	var entries []entry
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				key, ok := funcKey(decl)
				if !ok {
					continue
				}
				entries = append(entries, entry{key, &ast.FuncDecl{Recv: decl.Recv, Name: decl.Name, Type: decl.Type}})
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					for _, single := range exportedSpecs(spec) {
						entries = append(entries, entry{specName(single), &ast.GenDecl{Tok: decl.Tok, Specs: []ast.Spec{single}}})
					}
				}
			}
		}
	}
	slices.SortStableFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
	// The declarations keep the positions of their source, where doc comments
	// left blank lines between fields. Printing them, dropping those lines and
	// formatting the result again lays the record out from its own text alone.
	var text bytes.Buffer
	text.WriteString("package surface\n")
	for _, entry := range entries {
		var declaration bytes.Buffer
		if err := printer.Fprint(&declaration, fset, entry.node); err != nil {
			t.Fatal(err)
		}
		text.WriteByte('\n')
		for line := range strings.SplitSeq(declaration.String(), "\n") {
			if strings.TrimSpace(line) != "" {
				text.WriteString(line + "\n")
			}
		}
	}
	formatted, err := format.Source(text.Bytes())
	if err != nil {
		t.Fatalf("format the exported surface: %v\n%s", err, text.Bytes())
	}
	_, surface, _ := bytes.Cut(formatted, []byte("package surface\n\n"))
	return surface
}

// funcKey names an exported function by its name, and a method of an
// exported type by its type, a dot and its name, so it sorts after the type.
func funcKey(decl *ast.FuncDecl) (string, bool) {
	if !decl.Name.IsExported() {
		return "", false
	}
	if decl.Recv == nil {
		return decl.Name.Name, true
	}
	receiver := decl.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	switch generic := receiver.(type) {
	case *ast.IndexExpr:
		receiver = generic.X
	case *ast.IndexListExpr:
		receiver = generic.X
	}
	name, ok := receiver.(*ast.Ident)
	if !ok || !name.IsExported() {
		return "", false
	}
	return name.Name + "." + decl.Name.Name, true
}

// exportedSpecs returns one spec per exported name of spec, its comments
// dropped, a struct or an interface reduced to its exported members.
func exportedSpecs(spec ast.Spec) []ast.Spec {
	switch spec := spec.(type) {
	case *ast.TypeSpec:
		if !spec.Name.IsExported() {
			return nil
		}
		return []ast.Spec{&ast.TypeSpec{Name: spec.Name, TypeParams: spec.TypeParams, Assign: spec.Assign, Type: exportedType(spec.Type)}}
	case *ast.ValueSpec:
		var specs []ast.Spec
		for index, name := range spec.Names {
			if !name.IsExported() {
				continue
			}
			single := &ast.ValueSpec{Names: []*ast.Ident{name}, Type: spec.Type}
			if index < len(spec.Values) {
				single.Values = []ast.Expr{spec.Values[index]}
			}
			specs = append(specs, single)
		}
		return specs
	}
	return nil
}

func specName(spec ast.Spec) string {
	switch spec := spec.(type) {
	case *ast.TypeSpec:
		return spec.Name.Name
	case *ast.ValueSpec:
		return spec.Names[0].Name
	}
	return ""
}

// exportedType keeps the exported fields of a struct and the exported methods
// of an interface, an embedded member counting by its type's name.
func exportedType(expr ast.Expr) ast.Expr {
	switch typed := expr.(type) {
	case *ast.StructType:
		return &ast.StructType{Fields: exportedFields(typed.Fields)}
	case *ast.InterfaceType:
		return &ast.InterfaceType{Methods: exportedFields(typed.Methods)}
	}
	return expr
}

func exportedFields(list *ast.FieldList) *ast.FieldList {
	kept := &ast.FieldList{}
	if list == nil {
		return kept
	}
	for _, field := range list.List {
		if len(field.Names) == 0 {
			if embeddedName(field.Type).IsExported() {
				kept.List = append(kept.List, &ast.Field{Type: field.Type, Tag: field.Tag})
			}
			continue
		}
		var names []*ast.Ident
		for _, name := range field.Names {
			if name.IsExported() {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			kept.List = append(kept.List, &ast.Field{Names: names, Type: exportedType(field.Type), Tag: field.Tag})
		}
	}
	return kept
}

// embeddedName returns the type name of an embedded field: T, *T, pkg.T or a
// generic instance of one of them.
func embeddedName(expr ast.Expr) *ast.Ident {
	for {
		switch typed := expr.(type) {
		case *ast.Ident:
			return typed
		case *ast.StarExpr:
			expr = typed.X
		case *ast.SelectorExpr:
			return typed.Sel
		case *ast.IndexExpr:
			expr = typed.X
		case *ast.IndexListExpr:
			expr = typed.X
		default:
			return ast.NewIdent("_")
		}
	}
}
