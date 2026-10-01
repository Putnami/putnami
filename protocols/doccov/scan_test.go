package doccov

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanPackage_Invariants pins the scanner's field-classification rules on a
// synthetic package written to a temp dir: which fields count as wire fields and
// which of the two documentation sources (field doc comment, schema description)
// satisfy each. A regression in any of these rules would silently change the
// guard's verdict, so they are asserted here rather than only through the live
// protocols scan.
func TestScanPackage_Invariants(t *testing.T) {
	dir := t.TempDir()

	src := `package sample

import "encoding/json"

// Wire is a sample wire struct.
type Wire struct {
	// Documented has a field doc comment.
	Documented string ` + "`json:\"documented\"`" + `

	SchemaDescribed string ` + "`json:\"schemaDescribed\"`" + ` // (documented via schema)

	Bare int ` + "`json:\"bare\"`" + `

	Ignored string ` + "`json:\"-\"`" + `

	unexported string ` + "`json:\"unexported\"`" + `

	// NoTag defaults its JSON name to the Go field name.
	NoTag string

	// Raw is still a wire field.
	Raw json.RawMessage ` + "`json:\"raw\"`" + `
}

// Embedder embeds Wire; the embedded field itself is not a wire field.
type Embedder struct {
	Wire
	// Own is documented.
	Own string ` + "`json:\"own\"`" + `
}

// unexportedType is skipped entirely.
type unexportedType struct {
	Field string ` + "`json:\"field\"`" + `
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// A test file must be ignored by the scanner.
	if err := os.WriteFile(filepath.Join(dir, "sample_test.go"), []byte("package sample\n\ntype ExcludedFromScan struct {\n\tX string `json:\"x\"`\n}\n"), 0o600); err != nil {
		t.Fatalf("write test source: %v", err)
	}

	// A schema whose `description` on "schemaDescribed" documents that field.
	schemaDir := filepath.Join(dir, "schemas")
	if err := os.MkdirAll(schemaDir, 0o750); err != nil {
		t.Fatalf("mkdir schemas: %v", err)
	}
	schema := `{
	  "properties": {
	    "schemaDescribed": { "type": "string", "description": "documented in the schema" },
	    "bare": { "type": "integer" }
	  }
	}`
	if err := os.WriteFile(filepath.Join(schemaDir, "sample.json"), []byte(schema), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	report, err := ScanPackage(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	got := map[string]bool{}      // "Struct.Field" -> documented
	seenJSON := map[string]bool{} // "Struct.Field" -> jsonName present
	for _, f := range report.Fields {
		key := f.Struct + "." + f.Field
		got[key] = f.Documented
		seenJSON[f.JSONName] = true
	}

	// Wire fields that must be scanned, with their expected documented verdict.
	wantDocumented := map[string]bool{
		"Wire.Documented":      true,  // field doc comment
		"Wire.SchemaDescribed": true,  // schema description
		"Wire.Bare":            false, // neither (schema has no description for it)
		"Wire.NoTag":           true,  // field doc comment; defaults json name to Go name
		"Wire.Raw":             true,  // field doc comment
		"Embedder.Own":         true,  // field doc comment
	}
	for key, want := range wantDocumented {
		doc, scanned := got[key]
		if !scanned {
			t.Errorf("%s: expected to be scanned as a wire field, but it was not", key)
			continue
		}
		if doc != want {
			t.Errorf("%s: documented=%v, want %v", key, doc, want)
		}
	}

	// Fields that must NOT be scanned.
	mustSkip := []string{
		"Wire.Ignored",         // json:"-"
		"Wire.unexported",      // unexported field
		"Embedder.Wire",        // embedded field (no field name)
		"unexportedType.Field", // field of an unexported type
		"ExcludedFromScan.X",   // in a _test.go file
	}
	for _, key := range mustSkip {
		if _, scanned := got[key]; scanned {
			t.Errorf("%s: must not be scanned as a wire field, but it was", key)
		}
	}

	// json:"-" must never surface a JSON name.
	if seenJSON["-"] {
		t.Errorf("a field tagged json:\"-\" leaked a JSON name into the report")
	}

	// NoTag must default its JSON name to the Go field name.
	if !seenJSON["NoTag"] {
		t.Errorf("NoTag field: expected JSON name to default to the Go field name")
	}

	// Aggregate sanity: 6 scanned fields, 1 undocumented (Wire.Bare).
	if report.Total() != len(wantDocumented) {
		t.Errorf("Total()=%d, want %d", report.Total(), len(wantDocumented))
	}
	if n := len(report.Undocumented()); n != 1 {
		t.Errorf("Undocumented() count=%d, want 1 (Wire.Bare)", n)
	}
}

// TestScanPackage_ContractTwinSubdir pins that ScanPackage reaches into the
// `schema/` contract-twin subdir (where `putnami contracts generate` emits the
// Go wire structs and JSON Schema, e.g. protocols/identity) — a struct nested
// there is scanned, and a description in schema/*.json documents its fields.
// Without this, a manifest-authored package scans as zero wire fields and the
// coverage guard silently passes.
func TestScanPackage_ContractTwinSubdir(t *testing.T) {
	dir := t.TempDir()

	// Package root carries only a doc file (no wire structs), mirroring a
	// manifest-authored package whose surface lives under schema/.
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("// Package sample is a doc-only root.\npackage sample\n"), 0o600); err != nil {
		t.Fatalf("write root doc: %v", err)
	}

	twinDir := filepath.Join(dir, "schema")
	if err := os.MkdirAll(twinDir, 0o750); err != nil {
		t.Fatalf("mkdir schema: %v", err)
	}
	// Sub is documented by a Go comment; Roles is documented ONLY via the
	// schema/ description below — so this exercises both twin sources.
	twinSrc := "package sample\n\n" +
		"// Claims is the generated wire twin.\n" +
		"type Claims struct {\n" +
		"\t// Sub has a field doc comment.\n" +
		"\tSub string `json:\"sub\"`\n" +
		"\tRoles []string `json:\"roles\"`\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(twinDir, "contracts.gen.go"), []byte(twinSrc), 0o600); err != nil {
		t.Fatalf("write twin source: %v", err)
	}
	schema := `{ "properties": { "roles": { "type": "array", "description": "role names" } } }`
	if err := os.WriteFile(filepath.Join(twinDir, "contracts.schema.json"), []byte(schema), 0o600); err != nil {
		t.Fatalf("write twin schema: %v", err)
	}

	report, err := ScanPackage(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	got := map[string]bool{}
	for _, f := range report.Fields {
		got[f.Struct+"."+f.Field] = f.Documented
	}
	if report.Total() != 2 {
		t.Fatalf("Total()=%d, want 2 (Claims.Sub, Claims.Roles from schema/)", report.Total())
	}
	if doc, ok := got["Claims.Sub"]; !ok || !doc {
		t.Errorf("Claims.Sub: scanned=%v documented=%v, want scanned+documented (Go comment)", ok, doc)
	}
	if doc, ok := got["Claims.Roles"]; !ok || !doc {
		t.Errorf("Claims.Roles: scanned=%v documented=%v, want scanned+documented (schema/ description)", ok, doc)
	}
}

// TestScanPackage_MissingSchemasDir asserts a package with no schemas/ dir is
// scanned cleanly (godoc becomes the only documentation source).
func TestScanPackage_MissingSchemasDir(t *testing.T) {
	dir := t.TempDir()
	src := "package sample\n\n" +
		"// Wire is a sample.\n" +
		"type Wire struct {\n" +
		"\t// A is documented.\n" +
		"\tA string `json:\"a\"`\n" +
		"\tB string `json:\"b\"`\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	report, err := ScanPackage(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Total() != 2 {
		t.Fatalf("Total()=%d, want 2", report.Total())
	}
	if n := len(report.Undocumented()); n != 1 {
		t.Errorf("Undocumented()=%d, want 1 (Wire.B)", n)
	}
}
