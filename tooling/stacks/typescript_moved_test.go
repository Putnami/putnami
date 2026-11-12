package stacks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAMovedTestWritesItsOptions(t *testing.T) {
	doc := map[string]any{}
	SetTypeScriptTestTypesOff(doc, true)
	want := map[string]any{"options": map[string]any{
		TypeScriptRunnerLayer: map[string]any{"types": false},
	}}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("doc = %v", doc)
	}
	SetTypeScriptTestTypesOff(doc, false)
	if len(doc) != 0 {
		t.Errorf("a cleared doc = %v", doc)
	}
	// A value the project set to true is its own choice and stays.
	doc = map[string]any{"options": map[string]any{TypeScriptRunnerLayer: map[string]any{"types": true}}}
	SetTypeScriptTestTypesOff(doc, false)
	if doc["options"].(map[string]any)[TypeScriptRunnerLayer].(map[string]any)["types"] != true {
		t.Errorf("the project's types changed: %v", doc)
	}
}

func TestABunBuildMovesOnlyWhenItWritesTheEntriesThePackageNames(t *testing.T) {
	const keeps = "it keeps running through the tool"
	for _, c := range []struct {
		name, manifest, published string
		source                    bool
		want                      string
	}{
		{name: "no entry", manifest: `{"name": "a"}`},
		{name: "a source entry", manifest: `{"name": "a", "main": "./src/index.ts"}`},
		{name: "a source export", manifest: `{"name": "a", "exports": {".": "./src/index.ts", "./package.json": "./package.json"}}`},
		{name: "a source export beside a built main", manifest: `{"name": "a", "main": "dist/index.js", "exports": {".": "./src/index.ts"}}`, published: "npm"},
		{name: "an entry the import rewrites", manifest: "{\n  \"name\": \"a\",\n  \"main\": \"dist/index.js\"\n}\n", source: true},
		{
			name: "a published entry", manifest: "{\n  \"name\": \"a\",\n  \"main\": \"dist/index.js\"\n}\n", source: true,
			published: "its package.json, which is not private,",
			want:      `its package.json names "dist/index.js", which the Bun build of @putnami/typescript does not write, and its package.json, which is not private, publishes it, so that entry stays: ` + keeps,
		},
		{
			name: "built exports", manifest: `{"name": "a", "types": "./cjs/index.d.cts", "exports": {".": {"types": "./cjs/index.d.cts", "default": "./cjs/index.cjs"}}}`, source: true,
			want: `its package.json names "./cjs/index.cjs", which the Bun build of @putnami/typescript does not write, and it declares exports, which the import does not rewrite: ` + keeps,
		},
		{
			name: "no source entry", manifest: "{\n  \"name\": \"a\",\n  \"main\": \"lib/index.js\"\n}\n",
			want: `its package.json names "lib/index.js", which the Bun build of @putnami/typescript does not write, and it holds no src/index.ts to name instead: ` + keeps,
		},
		{
			name: "an entry it cannot edit", manifest: `{"name": "a", "main": "dist/index.js"}`, source: true,
			want: `its package.json names "dist/index.js", which the Bun build of @putnami/typescript does not write, and the import cannot add its source entry: its entry is on its last line: ` + keeps,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "pkg")
			if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(c.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			if c.source {
				if err := os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := BunBuildKeptReason(root, "pkg", c.published, keeps); got != c.want {
				t.Errorf("reason = %q\nwant     %q", got, c.want)
			}
		})
	}
	if got := BunBuildKeptReason(t.TempDir(), "missing", "", keeps); got != "" {
		t.Errorf("a project with no package.json: %q", got)
	}
}

func TestTheLintDependentsNoteNamesTheExtension(t *testing.T) {
	if !strings.Contains(LintDependentsNote, TypeScriptExtension) {
		t.Errorf("note = %q", LintDependentsNote)
	}
}
