package nx

import (
	"reflect"
	"testing"
)

// TestSetCompilerLeavesAnUntouchedDocumentAsItIs pins that setCompiler
// changes a putnami.json only when it writes or removes the compiler: an
// empty options member the project wrote stays, so a sync with nothing to
// change rewrites no file.
func TestSetCompilerLeavesAnUntouchedDocumentAsItIs(t *testing.T) {
	for name, doc := range map[string]map[string]any{
		"empty options":              {"options": map[string]any{}},
		"empty typescript options":   {"options": map[string]any{TypeScriptExtension: map[string]any{}}},
		"empty nx options":           {"options": map[string]any{ExtensionName: map[string]any{}}},
		"a compiler the project set": {"options": map[string]any{TypeScriptExtension: map[string]any{"compiler": "bun"}}},
	} {
		for _, want := range []bool{false, true} {
			if want && name != "a compiler the project set" {
				continue
			}
			before := clone(doc)
			setCompiler(doc, want)
			if !reflect.DeepEqual(clone(doc), before) {
				t.Errorf("%s, want=%t: setCompiler changed %v into %v", name, want, before, doc)
			}
		}
	}

	// Writing then removing the compiler leaves no empty member behind.
	doc := map[string]any{}
	setCompiler(doc, true)
	if got, want := doc["options"], map[string]any{
		TypeScriptExtension: map[string]any{"compiler": "tsc"},
		ExtensionName:       map[string]any{compilerRecord: "tsc"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %v, want %v", got, want)
	}
	setCompiler(doc, false)
	if _, ok := doc["options"]; ok {
		t.Errorf("options = %v after the sync removed what it wrote, want none", doc["options"])
	}
}
