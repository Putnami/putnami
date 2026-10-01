package contracts

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// The emitter goldens live under testdata/ (Go tooling ignores that directory,
// so contracts.golden.go is never compiled and never reformatted by the build)
// and are regenerated with PUTNAMI_UPDATE_GOLDEN=1. The shared IR input is the
// equivalence corpus fixtures/equivalence/contracts.golden.json — the same file
// the TypeScript emitter reads by relative path.
const (
	equivInputPath   = "fixtures/equivalence/contracts.golden.json"
	schemaGoldenPath = "testdata/contracts.schema.json"
	goGoldenPath     = "testdata/contracts.golden.go"
)

func loadEquivManifest(t *testing.T) *Manifest {
	t.Helper()
	data, err := os.ReadFile(equivInputPath)
	if err != nil {
		t.Fatalf("read equivalence input: %v", err)
	}
	m, diags := ParseAndValidateManifest(data)
	if m == nil {
		t.Fatalf("equivalence input did not parse/validate clean: %v", diags)
	}
	return m
}

// TestEmitJSONSchema_Golden pins the canonical JSON Schema bytes. The golden it
// writes (testdata/contracts.schema.json) is the cross-language byte-parity
// anchor: the TypeScript emitter's parity test compares its own serialization
// against these exact bytes.
func TestEmitJSONSchema_Golden(t *testing.T) {
	got, err := MarshalJSONSchema(loadEquivManifest(t))
	if err != nil {
		t.Fatalf("marshal JSON schema: %v", err)
	}
	compareGolden(t, schemaGoldenPath, got)
}

// TestEmitGo_Golden pins the generated Go type source. EmitGo runs its output
// through go/format.Source, so a mismatch means either an intentional change
// (regenerate with PUTNAMI_UPDATE_GOLDEN=1) or a regression.
func TestEmitGo_Golden(t *testing.T) {
	got, err := EmitGo(loadEquivManifest(t))
	if err != nil {
		t.Fatalf("emit Go: %v", err)
	}
	compareGolden(t, goGoldenPath, []byte(got))
}

// TestEmitGo_Parses guards that the generated Go is syntactically valid beyond
// gofmt formatting — a defense against an emitter change that produces
// well-formatted but unparseable source.
func TestEmitGo_Parses(t *testing.T) {
	src, err := EmitGo(loadEquivManifest(t))
	if err != nil {
		t.Fatalf("emit Go: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "contracts_gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated Go does not parse: %v\n%s", err, src)
	}
}

// TestEmit_Deterministic asserts both emitters are byte-stable across repeated
// runs — the guard against map-iteration order or other nondeterminism leaking
// into generated output.
func TestEmit_Deterministic(t *testing.T) {
	m := loadEquivManifest(t)
	schemaFirst, err := MarshalJSONSchema(m)
	if err != nil {
		t.Fatalf("marshal JSON schema: %v", err)
	}
	goFirst, err := EmitGo(m)
	if err != nil {
		t.Fatalf("emit Go: %v", err)
	}
	for i := range 20 {
		schema, err := MarshalJSONSchema(m)
		if err != nil {
			t.Fatalf("iteration %d: marshal JSON schema: %v", i, err)
		}
		if string(schema) != string(schemaFirst) {
			t.Fatalf("iteration %d: JSON schema not deterministic", i)
		}
		goSrc, err := EmitGo(m)
		if err != nil {
			t.Fatalf("iteration %d: emit Go: %v", i, err)
		}
		if goSrc != goFirst {
			t.Fatalf("iteration %d: Go emission not deterministic", i)
		}
	}
}

func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("PUTNAMI_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with PUTNAMI_UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch for %s\n--- got:\n%s\n--- want:\n%s", path, got, want)
	}
}
