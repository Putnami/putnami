package capabilities

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// capabilitiesSchemaPath is the JSON schema editors validate authored
// manifests against.
const capabilitiesSchemaPath = "schemas/capabilities.json"
const capabilitiesV2SchemaPath = "schemas/capabilities-v2.json"

// TestProtocolVersion_SchemasMatch asserts the JSON schema accepts exactly the
// current protocol version — no more, no less. The schema and the parser must
// agree in both directions: a schema that accepts fewer versions rejects a
// manifest the parser intentionally reads, and a schema that accepts more
// blesses a manifest the parser drops. Either gap is drift.
func TestProtocolVersion_SchemasMatch(t *testing.T) {
	for _, test := range []struct {
		path string
		want []int
	}{
		{capabilitiesSchemaPath, []int{ProtocolVersion}},
		{capabilitiesV2SchemaPath, []int{ProtocolVersionV2}},
	} {
		t.Run(filepath.Base(test.path), func(t *testing.T) {
			accepted := schemaProtocolVersions(t, test.path)
			want := test.want
			if !equalIntSets(accepted, want) {
				t.Errorf("%s: protocolVersion accepts %v, want exactly %v",
					test.path, sortInts(accepted), want)
			}
		})
	}
}

// tsProducers lists every TypeScript framework producer and the supported wire
// version it intentionally publishes. TypeScript does not import the Go
// constant, so each carries its own. Paths are relative to the workspace root.
var tsProducers = []struct {
	path    string
	version int
}{
	{"typescript/framework/application/src/capabilities/capabilities.producer.ts", ProtocolVersionV2},
}

// tsProtocolVersionPattern captures the integer a TypeScript producer assigns
// to its (CAPABILITIES_)PROTOCOL_VERSION constant, e.g.
// `const PROTOCOL_VERSION = 1 as const;` or
// `export const CAPABILITIES_PROTOCOL_VERSION = 1;`.
var tsProtocolVersionPattern = regexp.MustCompile(`(?m)\bconst\s+(?:CAPABILITIES_)?PROTOCOL_VERSION\s*=\s*(\d+)`)

// TestProtocolVersion_TSProducersSupported is the cross-language guard: it
// scans every TypeScript producer and asserts the protocolVersion it stamps is
// the version the parser accepts. It fails when a producer stamps a version the
// parser would reject — the silent contribution-drop failure mode. The scan
// skips itself if the producer list is ever emptied, so the guard survives a
// producer being moved rather than turning into a false pass.
func TestProtocolVersion_TSProducersSupported(t *testing.T) {
	if len(tsProducers) == 0 {
		t.Skip("no TypeScript capabilities producers are listed; add one when an emitter stamps this wire")
	}
	root := repoRoot(t)
	if root == "" {
		t.Skip("workspace root (putnami.workspace.json) not found; skipping cross-language producer scan")
	}
	for _, producer := range tsProducers {
		t.Run(producer.path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, producer.path))
			if err != nil {
				t.Fatalf("read TS producer %s: %v", producer.path, err)
			}
			matches := tsProtocolVersionPattern.FindAllStringSubmatch(string(data), -1)
			if len(matches) != 1 {
				t.Fatalf("%s: found %d protocolVersion constant declarations, want exactly 1 (pattern %q)",
					producer.path, len(matches), tsProtocolVersionPattern.String())
			}
			v, err := strconv.Atoi(matches[0][1])
			if err != nil {
				t.Fatalf("%s: parse stamped version %q: %v", producer.path, matches[0][1], err)
			}
			if v != producer.version {
				t.Errorf("%s stamps protocolVersion %d, want %d; bump the producer",
					producer.path, v, producer.version)
			}
		})
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

// repoRoot walks up from the working directory to the workspace root,
// identified by putnami.workspace.json. Returns "" when not found (e.g. when
// the module is tested in isolation outside the monorepo).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
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
