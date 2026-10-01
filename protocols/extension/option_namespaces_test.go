package extension

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestOptionNamespaces_ParseAndRoundTrip pins the declaration a cache key reads
// to decide who owns a bare `options.<name>` block.
//
// The field is load-bearing for someone else's key: an extension that declares
// `sdd` makes that block droppable from every OTHER extension's task key, so
// the manifest must carry it through parsing and re-encoding unchanged, and a
// manifest that declares none must encode none.
func TestOptionNamespaces_ParseAndRoundTrip(t *testing.T) {
	t.Parallel()
	manifest, diags := ParseManifest([]byte(
		`{"name":"@putnami/sdd","cliContract":4,"optionNamespaces":["sdd","publish"]}`))
	if manifest == nil {
		t.Fatalf("parse manifest: %v", diags)
	}
	if got := strings.Join(manifest.OptionNamespaces, ","); got != "sdd,publish" {
		t.Fatalf("optionNamespaces = %q, want sdd,publish", got)
	}

	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("re-encode manifest: %v", err)
	}
	if !strings.Contains(string(encoded), `"optionNamespaces":["sdd","publish"]`) {
		t.Errorf("re-encoded manifest lost the declaration: %s", encoded)
	}

	bare, diags := ParseManifest([]byte(`{"name":"@putnami/go","cliContract":4}`))
	if bare == nil {
		t.Fatalf("parse bare manifest: %v", diags)
	}
	if len(bare.OptionNamespaces) != 0 {
		t.Errorf("a manifest that declares nothing reported %v", bare.OptionNamespaces)
	}
	encoded, err = json.Marshal(bare)
	if err != nil {
		t.Fatalf("re-encode bare manifest: %v", err)
	}
	if strings.Contains(string(encoded), "optionNamespaces") {
		t.Errorf("an absent declaration was encoded as present: %s", encoded)
	}
}

// TestOptionNamespaces_SchemaDeclaresTheField keeps the published schema and the
// Go type describing the same manifest. A field only one of them knows is a
// field a workspace cannot author: the editor refuses it, or the CLI ignores it.
func TestOptionNamespaces_SchemaDeclaresTheField(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("schemas/extension.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Type  string `json:"type"`
			Items struct {
				Type    string `json:"type"`
				Pattern string `json:"pattern"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	property, ok := schema.Properties["optionNamespaces"]
	if !ok {
		t.Fatal("the schema declares no optionNamespaces property")
	}
	if property.Type != "array" || property.Items.Type != "string" {
		t.Errorf("optionNamespaces = %+v, want an array of strings", property)
	}
	if property.Items.Pattern == "" {
		t.Error("optionNamespaces items carry no pattern; a namespace spelled like an extension layer " +
			"would claim a block the layer rules already own")
	}
}
