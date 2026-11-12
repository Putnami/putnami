package stacks

import (
	"os"
	"path/filepath"
	"testing"
)

func writeNamingFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNamedByTypeScriptNeedsTheExtensionAndANamedPackage(t *testing.T) {
	root := t.TempDir()
	writeNamingFile(t, root, "named/package.json", `{"name": "@acme/named"}`)
	writeNamingFile(t, root, "unnamed/package.json", `{"private": true}`)
	writeNamingFile(t, root, "bad/package.json", `{`)
	writeNamingFile(t, root, "local/ts/putnami.extension.json", `{"name": "@putnami/typescript"}`)
	writeNamingFile(t, root, "local/other/putnami.extension.json", `{"name": "@acme/other"}`)

	for _, tc := range []struct {
		workspace string
		dir       string
		want      bool
	}{
		{`{"extensions": ["@putnami/typescript"]}`, "named", true},
		{`{"extensions": {"@putnami/typescript": "^1.0.0"}}`, "named", true},
		{`{"extensions": ["/local/ts"]}`, "named", true},
		{`{"extensions": ["/local/other", "/missing", "@acme/go"]}`, "named", false},
		{`{"extensions": ["@putnami/typescript"]}`, "unnamed", false},
		{`{"extensions": ["@putnami/typescript"]}`, "bad", false},
		{`{"extensions": ["@putnami/typescript"]}`, "absent", false},
		{`{"extensions": 3}`, "named", false},
		{`{`, "named", false},
	} {
		writeNamingFile(t, root, "putnami.workspace.json", tc.workspace)
		if got := NamedByTypeScript(root, tc.dir); got != tc.want {
			t.Errorf("NamedByTypeScript(%s) with %s = %v, want %v", tc.dir, tc.workspace, got, tc.want)
		}
	}
	if DeclaresExtension(filepath.Join(root, "absent"), TypeScriptExtension) {
		t.Error("a root with no workspace file declares an extension")
	}
}
