package doctor

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// doctorSchemaPath is the JSON schema editors validate authored reports and
// waiver files against.
const doctorSchemaPath = "schemas/doctor.json"

// TestProtocolVersion_SchemaMatches asserts the JSON schema accepts exactly the
// current protocol version — no more, no less — for BOTH wire shapes. The
// schema and the parser must agree in both directions: a schema that accepts
// fewer versions rejects a document the parser reads, and a schema that accepts
// more blesses one the parser drops. Either gap is drift.
func TestProtocolVersion_SchemaMatches(t *testing.T) {
	want := []int{ProtocolVersion}
	for _, def := range []string{"report", "waiverFile"} {
		t.Run(def, func(t *testing.T) {
			accepted := schemaDefProtocolVersions(t, doctorSchemaPath, def)
			if !equalIntSets(accepted, want) {
				t.Errorf("%s $defs.%s: protocolVersion accepts %v, want exactly %v",
					doctorSchemaPath, def, sortInts(accepted), want)
			}
		})
	}
}

// schemaDefProtocolVersions returns the protocolVersion values a schema $def
// accepts: the single value of a `const`, or the list in an `enum`.
func schemaDefProtocolVersions(t *testing.T, path, def string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties struct {
				ProtocolVersion struct {
					Const *int  `json:"const"`
					Enum  []int `json:"enum"`
				} `json:"protocolVersion"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", path, err)
	}
	d, ok := schema.Defs[def]
	if !ok {
		t.Fatalf("%s: $defs.%s not found", path, def)
	}
	pv := d.Properties.ProtocolVersion
	switch {
	case pv.Const != nil:
		return []int{*pv.Const}
	case len(pv.Enum) > 0:
		return pv.Enum
	default:
		t.Fatalf("%s: $defs.%s.properties.protocolVersion declares neither const nor enum", path, def)
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
