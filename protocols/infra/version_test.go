package infra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// TestProtocolVersion_SchemasMatch asserts each JSON schema accepts exactly
// the current protocol version — no more, no less. Editors validate authored
// manifests against these schemas, so the schema set and the parser set must
// agree in both directions: a schema that accepts fewer versions rejects a
// manifest the parser intentionally reads, and a schema that accepts more
// blesses a manifest the parser drops. Either gap is drift.
func TestProtocolVersion_SchemasMatch(t *testing.T) {
	want := []int{ProtocolVersion}
	for _, path := range []string{perProjectSchemaPath, aggregatedSchemaPath} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			accepted := schemaProtocolVersions(t, path)
			if !equalIntSets(accepted, want) {
				t.Errorf("%s: protocolVersion accepts %v, want exactly %v",
					path, sortInts(accepted), want)
			}
		})
	}
}

// tsProducers lists every TypeScript framework producer that stamps an infra
// protocolVersion as a hardcoded literal — TypeScript does not import the Go
// constant, so each carries its own. Paths are relative to the workspace root.
// Keep this list in sync with the producers that emit infra scratch fragments: a
// producer missing here is a hole in the guard, so the scan fails loudly when
// a listed file cannot be read or parsed.
var tsProducers = []string{
	"typescript/framework/database/src/infra.ts",
	"typescript/framework/storage/src/infra.ts",
	"typescript/framework/events/src/infra/requirements.ts",
	"typescript/framework/document/src/infra.ts",
	"typescript/framework/migration/src/infra.ts",
	"typescript/framework/runtime/src/config/infra-requirements.ts",
}

// tsProtocolVersionPattern captures the integer a TypeScript producer assigns
// to its (INFRA_)PROTOCOL_VERSION constant, e.g.
// `const PROTOCOL_VERSION = 1 as const;` or
// `export const INFRA_PROTOCOL_VERSION = 1;`.
var tsProtocolVersionPattern = regexp.MustCompile(`(?m)\bconst\s+(?:INFRA_)?PROTOCOL_VERSION\s*=\s*(\d+)`)

// TestProtocolVersion_TSProducersSupported is the cross-language guard the
// audit asked for: it scans every TypeScript producer and asserts the
// protocolVersion it stamps is the version the parser accepts. It fails when
// a producer stamps a version the parser would reject, which is exactly the
// silent contribution drop the audit's S1 finding hit.
//
// Go producers are not scanned: they reference infra.ProtocolVersion directly
// and so cannot drift from it (their own infra_test.go files assert this).
func TestProtocolVersion_TSProducersSupported(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("workspace root (putnami.workspace.json) not found; skipping cross-language producer scan")
	}
	for _, rel := range tsProducers {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("read TS producer %s: %v", rel, err)
			}
			matches := tsProtocolVersionPattern.FindAllStringSubmatch(string(data), -1)
			if len(matches) != 1 {
				t.Fatalf("%s: found %d protocolVersion constant declarations, want exactly 1 (pattern %q)",
					rel, len(matches), tsProtocolVersionPattern.String())
			}
			v, err := strconv.Atoi(matches[0][1])
			if err != nil {
				t.Fatalf("%s: parse stamped version %q: %v", rel, matches[0][1], err)
			}
			if v != ProtocolVersion {
				t.Errorf("%s stamps protocolVersion %d, want %d; bump the producer",
					rel, v, ProtocolVersion)
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
