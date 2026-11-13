package moonadoption

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// A project whose tasks all move to @putnami/typescript no longer names
// @putnami/moon, yet the sync writes the edges moon reads, one to a
// devDependency no source imports among them. moon's probe attributes those
// edges to its graph without claiming the project, so deps prune leaves them
// alone and the next sync finds nothing to write back.
func TestAnEdgeTheMoonSyncWritesIsNoPhantom(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "moon-import-keeps-the-edges-the-sync-writes", "an-edge-the-moon-sync-writes-is-no-phantom")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	// api lints with ESLint and tests with Vitest, which @putnami/typescript
	// runs, and depends on tooling, a shared configuration package no source
	// of api imports.
	root := moonWorkspace(t, map[string]map[string]string{"base": {
		".moon/workspace.yml":       "projects:\n  globs: ['apps/*']\n",
		".moon/tasks/node.yml":      "tasks:\n  lint:\n    command: 'eslint .'\n",
		"package.json":              `{"name": "shop", "private": true, "workspaces": ["apps/*"]}` + "\n",
		"apps/api/moon.yml":         "language: 'typescript'\ndependsOn: ['tooling']\ntasks:\n  test:\n    command: 'vitest run'\n",
		"apps/api/package.json":     `{"name": "@demo/api", "devDependencies": {"@demo/tooling": "workspace:*"}}` + "\n",
		"apps/api/src/index.ts":     "export const api = 1;\n",
		"apps/tooling/moon.yml":     "language: 'typescript'\n",
		"apps/tooling/package.json": `{"name": "@demo/tooling"}` + "\n",
		"apps/tooling/src/index.ts": "export const tooling = 1;\n",
	}}, buildRuntime(t), "import")
	stubMoon(t)
	for name, rel := range map[string]string{"typescript": "typescript/extension", "dev": "tooling/dev"} {
		target, err := filepath.Abs(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".ext"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, ".ext", name)); err != nil {
			t.Fatal(err)
		}
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/moon-extension", "/.ext/typescript", "/.ext/dev"], "includes": [], "options": {"@putnami/moon": {"mode": "import"}}}`)

	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	api := filepath.Join(root, "apps/api/putnami.json")
	synced, err := os.ReadFile(api)
	if p := readProject(t, root, "apps/api"); err != nil || !slices.Equal(p.Dependencies, []string{"/apps/tooling"}) || !slices.Equal(p.Extensions, []string{"@putnami/typescript"}) {
		t.Fatalf("api putnami.json = %s (%v), want the edge to tooling and no @putnami/moon", synced, err)
	}

	// prune reads the provider view, which validate records after the
	// sync's writes.
	if code, out := clitest.RunGateArgs(t, root, "validate", "--all"); code != 0 {
		t.Fatalf("validate exit %d:\n%s", code, out)
	}
	code, out := clitest.RunGateArgs(t, root, "deps", "prune", "--dry-run")
	if code != 0 || !strings.Contains(out, "No declared dependency edge without an import.") {
		t.Errorf("deps prune --dry-run exit %d lists an edge the sync writes:\n%s", code, out)
	}
	if code, out := clitest.RunGateArgs(t, root, "deps", "prune"); code != 0 {
		t.Fatalf("deps prune exit %d:\n%s", code, out)
	}
	if data, _ := os.ReadFile(api); string(data) != string(synced) {
		t.Errorf("deps prune rewrote api putnami.json:\n%s", data)
	}
	if code, out := clitest.RunGateArgs(t, root, "projects", "sync"); code != 0 {
		t.Fatalf("projects sync exit %d:\n%s", code, out)
	}
	if data, _ := os.ReadFile(api); string(data) != string(synced) {
		t.Errorf("the next sync rewrote api putnami.json:\n%s", data)
	}
}
