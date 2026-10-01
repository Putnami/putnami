// Package doccov is a deterministic guard over the documentation coverage of
// the wire contracts in the sibling protocols/* packages.
//
// A protocol package's exported struct fields ARE the contract: automation is a
// first-class consumer, so a field that carries a JSON value must be explainable
// from the types and schemas alone. This package walks each sibling package's Go
// source as text (go/parser, never importing the package) and — for every
// exported field of a JSON-marshaled struct — checks that the field is
// documented in EITHER of the two places a reader looks:
//
//   - a Go doc comment on the field itself (`// FieldName ...`), or
//   - a `description` on the field's JSON name in the package's schemas/*.json.
//
// A field that has neither is undocumented. The guard (see doccov_test.go)
// computes the undocumented ratio per package and fails a package above a
// threshold, tolerating a pinned allowlist of packages whose backfill is still
// pending. It scans source as text so it never has to import — and therefore
// never has to be in a module that depends on — every protocol package.
package doccov

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FieldReport is the coverage outcome for one exported wire-struct field.
type FieldReport struct {
	// Struct is the name of the enclosing (top-level, named) struct type.
	Struct string
	// Field is the Go field name.
	Field string
	// JSONName is the field's JSON object key (the json tag name, or the Go
	// field name when the tag omits a name).
	JSONName string
	// Documented reports whether the field carries a field-level Go doc comment
	// or a schema description for its JSON name.
	Documented bool
}

// PackageReport is the aggregate coverage outcome for one protocol package.
type PackageReport struct {
	// Package is the base directory name of the scanned package.
	Package string
	// Fields lists every scanned exported wire-struct field in source order,
	// grouped by struct.
	Fields []FieldReport
}

// Total returns the number of exported wire-struct fields scanned.
func (r PackageReport) Total() int { return len(r.Fields) }

// Undocumented returns the fields that carry neither a field-level Go doc
// comment nor a schema description, in scan order.
func (r PackageReport) Undocumented() []FieldReport {
	var out []FieldReport
	for _, f := range r.Fields {
		if !f.Documented {
			out = append(out, f)
		}
	}
	return out
}

// UndocumentedRatio returns the fraction of scanned fields that are
// undocumented, in [0,1]. An empty package (no wire fields) reports 0.
func (r PackageReport) UndocumentedRatio() float64 {
	if len(r.Fields) == 0 {
		return 0
	}
	return float64(len(r.Undocumented())) / float64(len(r.Fields))
}

// contractTwinSubdir is the conventional subdirectory into which
// `putnami contracts generate` emits a package's on-disk twin: the generated Go
// wire structs (contracts.gen.go) and JSON Schema (contracts.schema.json). A
// package authored from a contract manifest (e.g. protocols/identity) keeps its
// wire surface here rather than at the package root, so ScanPackage reaches one
// level into it. Arbitrary nested directories (conformance helpers, testdata)
// are deliberately NOT scanned — they are not the package's wire surface.
const contractTwinSubdir = "schema"

// ScanPackage walks the non-test .go source of a protocol package and reports
// the documentation coverage of every exported field of every JSON-marshaled
// struct. It scans the package root plus the contract-twin `schema/` subdir
// (see contractTwinSubdir), one level only. Schema descriptions under
// dir/schemas and dir/schema are consulted as the second, automation-facing
// documentation source. It returns an error only for I/O or parse failures; a
// package with no wire structs yields an empty report.
func ScanPackage(dir string) (PackageReport, error) {
	report := PackageReport{Package: filepath.Base(dir)}

	// Schema descriptions count as documentation. Merge the conventional
	// `schemas/` directory with the contract-twin `schema/` subdir; a merge only
	// ever marks MORE JSON names as documented, so it cannot regress coverage.
	schemaDesc, err := schemaDescribedNames(filepath.Join(dir, "schemas"))
	if err != nil {
		return report, err
	}
	twinDesc, err := schemaDescribedNames(filepath.Join(dir, contractTwinSubdir))
	if err != nil {
		return report, err
	}
	for name := range twinDesc {
		schemaDesc[name] = true
	}

	// Scan the package root plus its contract-twin subdir (one level only).
	goFiles, err := wireGoFiles(dir)
	if err != nil {
		return report, err
	}
	twinFiles, err := wireGoFiles(filepath.Join(dir, contractTwinSubdir))
	if err != nil {
		return report, err
	}
	goFiles = append(goFiles, twinFiles...)
	sort.Strings(goFiles)

	fset := token.NewFileSet()
	for _, path := range goFiles {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return report, err
		}
		collectFile(file, schemaDesc, &report)
	}
	return report, nil
}

// wireGoFiles lists the non-test .go files directly in dir (not recursively),
// in sorted order. A missing dir yields no files and no error, so callers can
// probe an optional subdir unconditionally.
func wireGoFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var goFiles []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		goFiles = append(goFiles, filepath.Join(dir, name))
	}
	return goFiles, nil
}

// collectFile appends the wire-field reports for every top-level struct type
// declared in file. Only exported struct types are scanned: an unexported type
// is not part of the package's public wire surface.
func collectFile(file *ast.File, schemaDesc map[string]bool, report *PackageReport) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			collectStruct(ts.Name.Name, st, schemaDesc, report)
		}
	}
}

// collectStruct appends a FieldReport for each exported field of st that is
// serialized to JSON. Embedded fields (no field name) are skipped: they promote
// the embedded type's own fields, which are documented on that type. A field
// tagged `json:"-"` is not on the wire and is skipped.
func collectStruct(structName string, st *ast.StructType, schemaDesc map[string]bool, report *PackageReport) {
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue // embedded field
		}
		jsonName, marshaled := jsonFieldName(field)
		if !marshaled {
			continue
		}
		for _, name := range field.Names {
			if !name.IsExported() {
				continue
			}
			documented := fieldHasDoc(field) || schemaDesc[jsonName]
			report.Fields = append(report.Fields, FieldReport{
				Struct:     structName,
				Field:      name.Name,
				JSONName:   jsonName,
				Documented: documented,
			})
		}
	}
}

// jsonFieldName returns the field's JSON object key and whether it is
// marshaled. A field with no struct tag defaults to its Go name (encoding/json
// marshals exported fields by default). `json:"-"` reports not-marshaled.
func jsonFieldName(field *ast.Field) (name string, marshaled bool) {
	goName := ""
	if len(field.Names) > 0 {
		goName = field.Names[0].Name
	}
	if field.Tag == nil {
		return goName, true
	}
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return goName, true
	}
	jsonTag, ok := structTag(tag, "json")
	if !ok {
		return goName, true
	}
	tagName, _, _ := strings.Cut(jsonTag, ",")
	if tagName == "-" {
		return "", false
	}
	if tagName == "" {
		return goName, true
	}
	return tagName, true
}

// fieldHasDoc reports whether the field carries a field-level comment: either a
// doc comment above it or a line comment beside it. Either satisfies a human or
// automation reading the Go source.
func fieldHasDoc(field *ast.Field) bool {
	if field.Doc != nil && strings.TrimSpace(field.Doc.Text()) != "" {
		return true
	}
	if field.Comment != nil && strings.TrimSpace(field.Comment.Text()) != "" {
		return true
	}
	return false
}

// structTag extracts the value of key from a raw struct tag string, mirroring
// reflect.StructTag.Lookup without a reflect dependency.
func structTag(tag, key string) (value string, ok bool) {
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]

		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		quoted := tag[:i+1]
		tag = tag[i+1:]

		if name == key {
			v, err := strconv.Unquote(quoted)
			if err != nil {
				return "", false
			}
			return v, true
		}
	}
	return "", false
}

// schemaDescribedNames returns the set of JSON property names that carry a
// non-empty `description` anywhere in the package's schemas. It walks every
// schemas/*.json file recursively so a description on a property — wherever the
// property is defined in the schema tree ($defs, nested properties) — counts as
// documentation for that JSON name. A missing schemas dir yields an empty set.
func schemaDescribedNames(schemaDir string) (map[string]bool, error) {
	described := map[string]bool{}
	entries, err := os.ReadDir(schemaDir)
	if os.IsNotExist(err) {
		return described, nil
	}
	if err != nil {
		return described, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(schemaDir, e.Name()))
		if err != nil {
			return described, err
		}
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			return described, err
		}
		collectSchemaDescriptions(doc, described)
	}
	return described, nil
}

// collectSchemaDescriptions walks a decoded JSON schema and records the name of
// every property whose subschema declares a non-empty string `description`.
// It descends through the standard object-schema shape: `properties` maps
// property name -> subschema, and it recurses through every other value so
// descriptions in `$defs`, `items`, `allOf`, etc. are all found.
func collectSchemaDescriptions(node any, described map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if props, ok := n["properties"].(map[string]any); ok {
			for propName, sub := range props {
				if subMap, ok := sub.(map[string]any); ok {
					if desc, ok := subMap["description"].(string); ok && strings.TrimSpace(desc) != "" {
						described[propName] = true
					}
				}
			}
		}
		for _, v := range n {
			collectSchemaDescriptions(v, described)
		}
	case []any:
		for _, v := range n {
			collectSchemaDescriptions(v, described)
		}
	}
}
