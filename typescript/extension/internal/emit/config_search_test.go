package emit

import (
	"path/filepath"
	"testing"
)

// kindOf returns the kind the manifest gives a member's file.
func kindOf(t *testing.T, m Manifest, project, path string) string {
	t.Helper()
	for _, e := range m.Files {
		if e.Project == project && e.Path == path {
			return e.Kind
		}
	}
	t.Fatalf("the manifest holds no %s/%s: %+v", project, path, m.Files)
	return ""
}

// The task's key reads the files of each closure member and the workspace
// root's tsconfig*.json, not the directories between them. A module whose
// search for its tsconfig leaves its member below the workspace root keys on
// its bytes, whether such a directory holds a tsconfig.json or not: adding
// one there would change what Bun emits without moving the key. A module that
// a config of its member governs, or whose member sits at the workspace root,
// keeps its emitted JavaScript.
func TestASearchThatLeavesItsMemberBelowTheRootKeysOnBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "no config between the member and the root",
			files: map[string]string{
				"tsconfig.json": `{}`,
			},
			want: KindBytes,
		},
		{
			name: "a config between the member and the root",
			files: map[string]string{
				"tsconfig.json":          `{}`,
				"packages/tsconfig.json": `{ "extends": "../tsconfig.json" }`,
			},
			want: KindBytes,
		},
		{
			name: "a config of the member",
			files: map[string]string{
				"tsconfig.json":                 `{}`,
				"packages/nested/tsconfig.json": `{ "extends": "../../tsconfig.json" }`,
			},
			want: KindJS,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			for rel, content := range tc.files {
				writeFile(t, filepath.Join(ws, filepath.FromSlash(rel)), content)
			}
			writeFile(t, filepath.Join(ws, "packages", "nested", "src", "index.ts"), "export const a: number = 1\n")
			writeFile(t, filepath.Join(ws, "app", "src", "index.ts"), "export const b: number = 2\n")
			members := []Member{
				{Path: "packages/nested", Root: filepath.Join(ws, "packages", "nested")},
				{Path: "app", Root: filepath.Join(ws, "app")},
			}
			m, err := Build(Options{WorkspaceRoot: ws, Members: members, Transpile: sameEmit, Runner: RunnerBun})
			if err != nil {
				t.Fatal(err)
			}
			if got := kindOf(t, m, "packages/nested", "src/index.ts"); got != tc.want {
				t.Errorf("packages/nested/src/index.ts keyed as %s, want %s", got, tc.want)
			}
			// app sits at the workspace root: the root's config governs it.
			if got := kindOf(t, m, "app", "src/index.ts"); got != KindJS {
				t.Errorf("app/src/index.ts keyed as %s, want %s", got, KindJS)
			}
		})
	}
}
