package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// contractsSchemaPath is the JSON schema editors validate authored IR documents
// against.
const contractsSchemaPath = "schemas/contracts.json"

// TestCanonicalFilenames pins the emit path constants. The .gen/schema/ prefix
// is load-bearing: the codegen committer only promotes files under .gen/schema/
// into the tracked tree, so an emitter that writes anywhere under the .gen/ root
// instead produces an ephemeral IR that is never committed or shipped in a
// packaged workload. Mirrors protocols/capabilities' TestCanonicalFilenames.
func TestCanonicalFilenames(t *testing.T) {
	if ManifestFilename != "contracts.json" {
		t.Errorf("ManifestFilename = %q, want contracts.json", ManifestFilename)
	}
	if EmitDir != ".gen/schema" {
		t.Errorf("EmitDir = %q, want .gen/schema (committer promotes only .gen/schema/*)", EmitDir)
	}
	if CommittedPath != "schema/contracts.json" {
		t.Errorf("CommittedPath = %q, want schema/contracts.json", CommittedPath)
	}
}

// TestProtocolVersion_SchemasMatch asserts the JSON schema accepts exactly the
// current protocol version — no more, no less. The schema and the parser must
// agree in both directions: a schema that accepts fewer versions rejects an IR
// the parser intentionally reads, and a schema that accepts more blesses an IR
// the parser drops. Either gap is drift.
func TestProtocolVersion_SchemasMatch(t *testing.T) {
	want := []int{ProtocolVersion}
	for _, path := range []string{contractsSchemaPath} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			accepted := schemaProtocolVersions(t, path)
			if !equalIntSets(accepted, want) {
				t.Errorf("%s: protocolVersion accepts %v, want exactly %v",
					path, sortInts(accepted), want)
			}
		})
	}
}

// tsProducers lists every TypeScript producer that stamps a contracts
// protocolVersion as a hardcoded literal. TypeScript does not import the Go
// constant, so each carries its own. The scan is skipped while no producer
// exists (the TS emitter lands in a later slice); the scaffolding stays so the
// guard goes live the moment a producer is added.
var tsProducers = []string{}

// TestProtocolVersion_TSProducersSupported is the cross-language guard: it
// asserts the protocolVersion a TypeScript producer stamps is the version the
// parser accepts. It is skipped until a producer exists.
func TestProtocolVersion_TSProducersSupported(t *testing.T) {
	if len(tsProducers) == 0 {
		t.Skip("no TypeScript contracts producers yet; the TS emitter lands in a later slice")
	}
}

// schemaProtocolVersions returns the protocolVersion values a schema accepts:
// the single value of a `const`, or the list in an `enum`.
func schemaProtocolVersions(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	var schema struct {
		Properties struct {
			ProtocolVersion struct {
				Const *int  `json:"const"`
				Enum  []int `json:"enum"`
			} `json:"protocolVersion"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", path, err)
	}
	pv := schema.Properties.ProtocolVersion
	switch {
	case pv.Const != nil:
		return []int{*pv.Const}
	case len(pv.Enum) > 0:
		return pv.Enum
	default:
		t.Fatalf("%s: properties.protocolVersion declares neither const nor enum", path)
		return nil
	}
}

// equalIntSets reports whether a and b contain the same distinct integers,
// ignoring order and duplicates.
func equalIntSets(a, b []int) bool {
	sa := make(map[int]bool, len(a))
	for _, v := range a {
		sa[v] = true
	}
	sb := make(map[int]bool, len(b))
	for _, v := range b {
		sb[v] = true
	}
	if len(sa) != len(sb) {
		return false
	}
	for v := range sa {
		if !sb[v] {
			return false
		}
	}
	return true
}

// sortInts returns a sorted copy of xs for stable failure messages.
func sortInts(xs []int) []int {
	out := append([]int(nil), xs...)
	sort.Ints(out)
	return out
}
