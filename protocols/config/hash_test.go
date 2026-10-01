package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestComputeSchemaHash_Prefix(t *testing.T) {
	blocks := []Block{
		{Path: "server", Fields: []FieldSchema{
			{Name: "port", Type: "int"},
		}},
	}
	hash := ComputeSchemaHash(blocks)
	if !strings.HasPrefix(hash, "sha256:") {
		t.Errorf("hash should start with sha256:, got %q", hash)
	}
	// sha256: (7 chars) + 16 hex chars = 23 total
	if len(hash) != 7+HashLength {
		t.Errorf("hash length should be %d, got %d: %q", 7+HashLength, len(hash), hash)
	}
}

func TestComputeSchemaHash_Deterministic(t *testing.T) {
	blocks := []Block{
		{Path: "server", Fields: []FieldSchema{
			{Name: "port", Type: "int"},
			{Name: "host", Type: "string"},
		}},
		{Path: "database", Fields: []FieldSchema{
			{Name: "url", Type: "string"},
		}},
	}

	first := ComputeSchemaHash(blocks)
	for i := range 100 {
		if got := ComputeSchemaHash(blocks); got != first {
			t.Fatalf("hash changed on iteration %d: %q != %q", i, got, first)
		}
	}
}

func TestComputeSchemaHash_OrderIndependent(t *testing.T) {
	// Blocks in different order should produce the same hash.
	blocks1 := []Block{
		{Path: "server", Fields: []FieldSchema{
			{Name: "port", Type: "int"},
			{Name: "host", Type: "string"},
		}},
		{Path: "database", Fields: []FieldSchema{
			{Name: "url", Type: "string"},
		}},
	}
	blocks2 := []Block{
		{Path: "database", Fields: []FieldSchema{
			{Name: "url", Type: "string"},
		}},
		{Path: "server", Fields: []FieldSchema{
			{Name: "host", Type: "string"},
			{Name: "port", Type: "int"},
		}},
	}

	h1 := ComputeSchemaHash(blocks1)
	h2 := ComputeSchemaHash(blocks2)
	if h1 != h2 {
		t.Errorf("order-independent hash failed: %q != %q", h1, h2)
	}
}

// TestComputeSchemaHash_NestedFixture is the Go side of the cross-language
// hash conformance anchor: the fixture's schemaHash field is computed by
// recursively canonicalizing its blocks. The TS extractor's test must
// produce the same hash for byte-identical input.
func TestComputeSchemaHash_NestedFixture(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/nested-hash.json")
	if err != nil {
		t.Fatal(err)
	}
	var m SchemaManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	got := ComputeSchemaHash(m.Configs)
	if got != m.SchemaHash {
		t.Fatalf("hash mismatch for nested-hash fixture:\n  got:  %s\n  want: %s", got, m.SchemaHash)
	}
}

// TestComputeSchemaHash_FlatAnchorStable locks a known canonical hash
// for a leaf-only schema. This is a forward-determinism anchor — it
// pins the hash going forward so any accidental change to the
// canonicalizer or FieldSchema serialization (new fields without
// omitempty, key ordering changes, …) trips the test. It is NOT a
// backward-compat assertion against the pre-nesting implementation;
// the value was computed by the recursive canonicalizer. The same
// value is asserted independently by the TS extractor.
func TestComputeSchemaHash_FlatAnchorStable(t *testing.T) {
	blocks := []Block{
		{Path: "server", Fields: []FieldSchema{
			{Name: "host", Type: "string"},
			{Name: "port", Type: "int", Required: true},
		}},
	}
	const expected = "sha256:884119c1a3fd2c7b"
	got := ComputeSchemaHash(blocks)
	if got != expected {
		t.Fatalf("flat-schema hash changed: got %s want %s", got, expected)
	}
}

// TestProductionUnsafeDefault_OmitemptyByteStable pins the additive-marker
// invariant: an UNSET productionUnsafeDefault marker must serialize to nothing
// (omitempty), so the canonical JSON — and therefore the schema hash — of an
// existing field is byte-for-byte unchanged. A SET marker must serialize as
// `"productionUnsafeDefault":true` immediately after `sensitive`, and must
// participate in the hash so a marked field is distinguishable from an unmarked
// one across languages.
func TestProductionUnsafeDefault_OmitemptyByteStable(t *testing.T) {
	base := FieldSchema{Name: "driver", Type: FieldTypeString, Default: "memory"}

	baseBytes, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(baseBytes), "productionUnsafeDefault") {
		t.Fatalf("zero-value marker must be omitted, got %s", baseBytes)
	}

	// A field that only differs by an unset (false) marker must be byte-identical.
	zero := base
	zero.ProductionUnsafeDefault = false
	zeroBytes, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if string(zeroBytes) != string(baseBytes) {
		t.Fatalf("unset marker changed the canonical bytes:\n  base: %s\n  zero: %s", baseBytes, zeroBytes)
	}

	marked := base
	marked.ProductionUnsafeDefault = true
	markedBytes, err := json.Marshal(marked)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markedBytes), `"productionUnsafeDefault":true`) {
		t.Fatalf("set marker must serialize, got %s", markedBytes)
	}
	// Marker sits after sensitive (absent here) and before constraints — assert
	// it comes after the default key so struct-order parity with the TS twin holds.
	if strings.Index(string(markedBytes), "productionUnsafeDefault") < strings.Index(string(markedBytes), "default") {
		t.Fatalf("marker must serialize after default, got %s", markedBytes)
	}

	// The marker must participate in the canonical hash: an otherwise-identical
	// schema hashes differently once the marker is set.
	unmarkedHash := ComputeSchemaHash([]Block{{Path: "database", Fields: []FieldSchema{base}}})
	markedHash := ComputeSchemaHash([]Block{{Path: "database", Fields: []FieldSchema{marked}}})
	if unmarkedHash == markedHash {
		t.Fatalf("marker must participate in the hash; both hashed to %s", markedHash)
	}
}

// TestComputeSchemaHash_ProductionUnsafeFixture is the Go side of the
// cross-language anchor for the production-unsafe marker: the shared fixture's
// baked schemaHash must be reproduced by the recursive canonicalizer with the
// marker included. The TS extractor asserts the same fixture hash, proving the
// marker hashes byte-identically in both languages.
func TestComputeSchemaHash_ProductionUnsafeFixture(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/production-unsafe.json")
	if err != nil {
		t.Fatal(err)
	}
	var m SchemaManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	// Sanity: the fixture actually exercises the marker.
	if !m.Configs[0].Fields[0].ProductionUnsafeDefault {
		t.Fatal("fixture must set productionUnsafeDefault to anchor the marker hash")
	}
	if got := ComputeSchemaHash(m.Configs); got != m.SchemaHash {
		t.Fatalf("hash mismatch for production-unsafe fixture:\n  got:  %s\n  want: %s", got, m.SchemaHash)
	}
}

func TestCanonicalizeBlocks(t *testing.T) {
	blocks := []Block{
		{Path: "z-last", Fields: []FieldSchema{
			{Name: "b", Type: "string"},
			{Name: "a", Type: "int"},
		}},
		{Path: "a-first", Fields: []FieldSchema{
			{Name: "x", Type: "bool"},
		}},
	}

	canonical := CanonicalizeBlocks(blocks)
	if canonical[0].Path != "a-first" {
		t.Errorf("first block should be a-first, got %s", canonical[0].Path)
	}
	if canonical[1].Path != "z-last" {
		t.Errorf("second block should be z-last, got %s", canonical[1].Path)
	}
	if canonical[1].Fields[0].Name != "a" {
		t.Errorf("first field should be a, got %s", canonical[1].Fields[0].Name)
	}
}
