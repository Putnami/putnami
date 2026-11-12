package moon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// inferringWorkspace writes a moon workspace in delegate mode that infers
// the tasks of packages/shared-ui from its package.json scripts: its moon.yml
// states none.
func inferringWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	stackstest.WriteFile(t, root, ".moon/workspace.yml", "projects:\n  - 'packages/*'\n")
	stackstest.WriteFile(t, root, ".moon/toolchains.yml", "javascript:\n  packageManager: 'pnpm'\n  inferTasksFromScripts: true\n")
	stackstest.WriteFile(t, root, "packages/shared-ui/moon.yml", "language: 'typescript'\n")
	stackstest.WriteFile(t, root, "packages/shared-ui/package.json", `{"name": "shared-ui", "scripts": {"build": "vite build", "lint": "eslint ."}}`)
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"name": "demo", "extensions": ["@putnami/moon"], "options": {"@putnami/moon": {"mode": "delegate"}}}`+"\n")
	return root
}

// inferredExport is moon project-graph --json for inferringWorkspace: moon
// lists the build and lint tasks it infers from the scripts.
const inferredExport = `{"graph": {"nodes": [
  {"id": "shared-ui", "source": "packages/shared-ui", "language": "typescript",
   "tasks": {"build": {"command": "pnpm", "args": ["run", "build"]}, "lint": {"command": "pnpm", "args": ["run", "lint"]}}}
], "edges": []}}`

// installMoon writes the repository's own moon, node_modules/.bin/moon, as
// a stub that prints export for moon project-graph --json, and logs each
// call to $STUB_LOG.
func installMoon(t *testing.T, root, export string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(data, []byte(export), 0o644); err != nil {
		t.Fatal(err)
	}
	stackstest.WriteFile(t, root, "node_modules/.bin/moon", `#!/bin/sh
echo "$(pwd) moon $*" >> "$STUB_LOG"
[ "$1 $2" = "project-graph --json" ] || exit 2
cat '`+data+`'
`)
	if err := os.Chmod(filepath.Join(root, "node_modules/.bin/moon"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
}

// withoutMoonOnPath makes the sync find no moon on PATH.
func withoutMoonOnPath(t *testing.T) {
	t.Helper()
	previous := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPath = previous })
}

func TestTheSyncReadsTheTasksMoonInfers(t *testing.T) {
	spectest.Proves(t, feature, "inferred-tasks-are-read", "the-sync-reads-the-tasks-moon-infers")
	withoutMoonOnPath(t)
	root := inferringWorkspace(t)
	installMoon(t, root, inferredExport)
	result, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "packages/shared-ui")["disable"]; !reflect.DeepEqual(got, map[string]any{"jobs": []any{"test"}}) {
		t.Errorf("shared-ui disable = %v, want test alone: moon infers build and lint", got)
	}
	if result.Export != "moon project-graph --json" || result.Unresolved != "" {
		t.Errorf("export %q, unresolved %q", result.Export, result.Unresolved)
	}
	if log := stubLog(t); len(log) != 1 || log[0] != root+" moon project-graph --json" {
		t.Errorf("moon calls = %v, want one moon project-graph --json at the root", log)
	}
	// moon infers a test task from a new script: validate fails until the
	// sync writes it.
	installMoon(t, root, strings.Replace(inferredExport, `"lint": {`, `"test": {"command": "pnpm", "args": ["run", "test"]}, "lint": {`, 1))
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	if status, _, _ := RunCheck(&pctx.Context{WorkspaceRoot: root}, emit, nil); status != "FAILED" || !strings.Contains(events.String(), "packages/shared-ui/putnami.json no longer matches the moon project graph of moon project-graph --json") {
		t.Errorf("validate on a new inferred task: %s\n%s", status, events.String())
	}
}

func TestWithoutMoonTheSyncSaysItCannotReadTheInferredTasks(t *testing.T) {
	spectest.Proves(t, feature, "inferred-tasks-are-read", "without-moon-the-sync-says-it-cannot-read-the-inferred-tasks")
	withoutMoonOnPath(t)
	root := inferringWorkspace(t)
	result, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Unresolved, "inferTasksFromScripts") || !strings.Contains(result.Unresolved, "moon project-graph --json > moon-graph.json") {
		t.Errorf("unresolved = %q", result.Unresolved)
	}
	for name, job := range map[string]func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error){"sync": RunSync, "validate": RunCheck} {
		var events strings.Builder
		emit := jsonl.New()
		emit.Observe(func(message string) { events.WriteString(message + "\n") })
		status, _, _ := job(&pctx.Context{WorkspaceRoot: root}, emit, nil)
		if status != "OK" || !strings.Contains(events.String(), result.Unresolved) {
			t.Errorf("%s: %s, want the unresolved warning\n%s", name, status, events.String())
		}
	}
	// moon 1.x states the setting under node in .moon/toolchain.yml.
	stackstest.WriteFile(t, root, ".moon/toolchains.yml", "javascript:\n  packageManager: 'pnpm'\n")
	stackstest.WriteFile(t, root, ".moon/toolchain.yml", "node:\n  inferTasksFromScripts: true\n")
	if result, err := Sync(context.Background(), root); err != nil || result.Unresolved == "" {
		t.Errorf("moon 1.x setting: %q %v", result.Unresolved, err)
	}
	// A workspace that infers no task reads its configuration alone.
	stackstest.WriteFile(t, root, ".moon/toolchain.yml", "node:\n  inferTasksFromScripts: false\n")
	if result, err := Sync(context.Background(), root); err != nil || result.Unresolved != "" || result.Export != ".moon/workspace.yml" {
		t.Errorf("no inference: %+v %v", result, err)
	}
}

func TestAFailingMoonProjectGraphFailsTheSync(t *testing.T) {
	spectest.Proves(t, feature, "inferred-tasks-are-read", "a-failing-moon-project-graph-fails-the-sync")
	withoutMoonOnPath(t)
	root := inferringWorkspace(t)
	installMoon(t, root, "not json")
	if _, err := Sync(context.Background(), root); err == nil || !strings.Contains(err.Error(), "moon project-graph --json") {
		t.Errorf("an unreadable export: %v", err)
	}
	stackstest.WriteFile(t, root, "node_modules/.bin/moon", "#!/bin/sh\necho 'moon: invalid workspace' >&2\nexit 1\n")
	if _, err := Sync(context.Background(), root); err == nil || !strings.Contains(err.Error(), "moon project-graph --json") {
		t.Errorf("a failing moon: %v", err)
	}
}

func TestTheSyncNamesTheRootProjectItDoesNotAdopt(t *testing.T) {
	spectest.Proves(t, feature, "the-root-project-is-named", "the-sync-names-the-root-project-it-does-not-adopt")
	root := t.TempDir()
	stackstest.WriteFile(t, root, ".moon/workspace.yml", "projects:\n  root: '.'\n  ui: 'packages/ui'\n")
	stackstest.WriteFile(t, root, "moon.yml", "tasks:\n  lint:\n    command: 'vp lint --fix'\n  format:\n    command: 'vp fmt'\n")
	stackstest.WriteFile(t, root, "packages/ui/moon.yml", "tasks:\n  build:\n    command: 'vite build'\n")
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"name": "demo", "extensions": ["@putnami/moon"], "options": {"@putnami/moon": {"mode": "delegate"}}}`+"\n")
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	status, _, err := RunSync(&pctx.Context{WorkspaceRoot: root}, emit, nil)
	want := "moon project root at the workspace root is no Putnami project: a project there would own every file no deeper project claims; its tasks format, lint keep running through moon run root:<task>"
	if status != "OK" || err != nil || !strings.Contains(events.String(), want) {
		t.Errorf("the sync job: %s %v, want %q\n%s", status, err, want, events.String())
	}
	if _, err := os.Stat(filepath.Join(root, "putnami.json")); !os.IsNotExist(err) {
		t.Errorf("the sync wrote a root putnami.json: %v", err)
	}
}
