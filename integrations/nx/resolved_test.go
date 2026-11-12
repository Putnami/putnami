package nx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// inferredWorkspace writes an Nx workspace in delegate mode where apps/app
// imports @demo/lib through a tsconfig path, and nothing else states the
// edge: only Nx's import analysis finds it.
func inferredWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "nx.json", "{}\n")
	writeFile(t, root, "package.json", `{"name": "demo", "private": true, "devDependencies": {"nx": "21.0.0"}}`)
	writeFile(t, root, "tsconfig.base.json", `{"compilerOptions": {"paths": {"@demo/lib": ["libs/lib/src/index.ts"]}}}`)
	writeFile(t, root, "apps/app/project.json", `{"name": "app", "targets": {"build": {"executor": "nx:run-commands", "options": {"command": "echo app"}}}}`)
	writeFile(t, root, "apps/app/src/main.ts", "import { lib } from '@demo/lib';\nexport const app = lib;\n")
	writeFile(t, root, "libs/lib/project.json", `{"name": "lib", "targets": {"build": {"executor": "nx:run-commands", "options": {"command": "echo lib"}}}}`)
	writeFile(t, root, "libs/lib/src/index.ts", "export const lib = 1;\n")
	writeFile(t, root, "putnami.workspace.json", `{"name": "demo", "extensions": ["@putnami/nx"], "options": {"@putnami/nx": {"mode": "delegate"}}}`)
	return root
}

// installNx writes the repository's own nx, node_modules/.bin/nx, as a stub
// that answers nx graph --file=<file> with graph: Nx's resolved graph, where
// the import edge app -> lib is static and lib infers a test target from a
// plugin. It logs each call to $STUB_LOG.
func installNx(t *testing.T, root, graph string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(data, []byte(graph), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "node_modules/.bin/nx", `#!/bin/sh
echo "$(pwd) nx $*" >> "$STUB_LOG"
[ "$1" = graph ] || exit 2
for arg do
  case "$arg" in --file=*) file="${arg#--file=}";; esac
done
[ -n "$file" ] || exit 2
cp '`+data+`' "$file"
`)
	if err := os.Chmod(filepath.Join(root, "node_modules/.bin/nx"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
}

const nxResolvedGraph = `{"graph": {
  "nodes": {
    "app": {"name": "app", "type": "app", "data": {"root": "apps/app", "targets": {"build": {"executor": "nx:run-commands", "options": {"command": "echo app"}}}}},
    "lib": {"name": "lib", "type": "lib", "data": {"root": "libs/lib", "targets": {"build": {"executor": "nx:run-commands", "options": {"command": "echo lib"}}, "test": {"executor": "@nx/vite:test"}}}}
  },
  "dependencies": {"app": [{"source": "app", "target": "lib", "type": "static"}], "lib": []}
}}`

func dependenciesOf(t *testing.T, root, dir string) []any {
	t.Helper()
	deps, _ := readProjectFile(t, root, dir)["dependencies"].([]any)
	return deps
}

func TestTheSyncReadsTheGraphNxResolves(t *testing.T) {
	spectest.Proves(t, feature, "sync-reads-the-graph-nx-resolves", "the-sync-reads-the-graph-nx-resolves")
	root := inferredWorkspace(t)
	installNx(t, root, nxResolvedGraph)
	result := sync(t, root, SyncOptions{})
	if want := []any{"/libs/lib"}; !reflect.DeepEqual(dependenciesOf(t, root, "apps/app"), want) {
		t.Errorf("apps/app dependencies = %v, want %v: the edge Nx infers from the import", dependenciesOf(t, root, "apps/app"), want)
	}
	// The test target a plugin infers is no disabled job.
	if jobs := readProjectFile(t, root, "libs/lib")["disable"]; !reflect.DeepEqual(jobs, map[string]any{"jobs": []any{"lint"}}) {
		t.Errorf("libs/lib disable = %v, want lint alone: Nx infers its test target", jobs)
	}
	if result.Source != "nx graph" || len(result.Warnings) != 0 {
		t.Errorf("source %q, warnings %v", result.Source, result.Warnings)
	}
	log := stubLog(t)
	if len(log) != 1 || !strings.HasPrefix(log[0], root+" nx graph --file=") {
		t.Errorf("nx calls = %v, want one nx graph at the root", log)
	}

	// A new import adds an edge to Nx's graph: validate fails until the sync
	// writes it.
	installNx(t, root, strings.Replace(nxResolvedGraph, `"lib": []`, `"lib": [{"source": "lib", "target": "app", "type": "static"}]`, 1))
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	if status, _, _ := RunCheck(&pctx.Context{WorkspaceRoot: root}, emit, nil); status != "FAILED" || !strings.Contains(events.String(), "libs/lib/putnami.json no longer matches the Nx project graph of nx graph") {
		t.Errorf("validate on a new import: %s\n%s", status, events.String())
	}
}

func TestWithoutNxTheSyncSaysWhatTheStaticReadMisses(t *testing.T) {
	spectest.Proves(t, feature, "sync-reads-the-graph-nx-resolves", "without-nx-the-sync-says-what-the-static-read-misses")
	root := inferredWorkspace(t)
	result := sync(t, root, SyncOptions{})
	if deps := dependenciesOf(t, root, "apps/app"); deps != nil {
		t.Errorf("apps/app dependencies = %v from nx.json alone", deps)
	}
	if result.Source != "nx.json" || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "the edges Nx infers from imports") || !strings.Contains(result.Warnings[0], "nx graph --file=graph.json") {
		t.Errorf("source %q, warnings %v", result.Source, result.Warnings)
	}
	for name, job := range map[string]func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error){"sync": RunSync, "validate": RunCheck} {
		var events strings.Builder
		emit := jsonl.New()
		emit.Observe(func(message string) { events.WriteString(message + "\n") })
		status, _, _ := job(&pctx.Context{WorkspaceRoot: root}, emit, nil)
		if status != "OK" || !strings.Contains(events.String(), staticWarning) {
			t.Errorf("%s: %s, want the static read warning\n%s", name, status, events.String())
		}
	}
	// A graph export at the root answers without nx.
	writeFile(t, root, "graph.json", nxResolvedGraph)
	if result := sync(t, root, SyncOptions{}); result.Source != "graph.json" || len(result.Warnings) != 0 || !reflect.DeepEqual(dependenciesOf(t, root, "apps/app"), []any{"/libs/lib"}) {
		t.Errorf("with graph.json: source %q, warnings %v", result.Source, result.Warnings)
	}
}

func TestAFailingNxGraphFailsTheSync(t *testing.T) {
	spectest.Proves(t, feature, "sync-reads-the-graph-nx-resolves", "a-failing-nx-graph-fails-the-sync")
	root := inferredWorkspace(t)
	installNx(t, root, "not json")
	if _, err := Sync(t.Context(), root, SyncOptions{}); err == nil || !strings.Contains(err.Error(), "nx graph") {
		t.Errorf("an unreadable nx graph: %v", err)
	}
	writeFile(t, root, "node_modules/.bin/nx", "#!/bin/sh\necho 'NX failed to read nx.json' >&2\nexit 1\n")
	if _, err := Sync(t.Context(), root, SyncOptions{}); err == nil || !strings.Contains(err.Error(), "nx graph") {
		t.Errorf("a failing nx graph: %v", err)
	}
}

func TestAnUnmappedTargetNameIsOneLine(t *testing.T) {
	spectest.Proves(t, feature, "unmapped-targets-are-listed", "an-unmapped-target-name-is-one-line")
	root := t.TempDir()
	writeFile(t, root, "nx.json", "{}\n")
	writeFile(t, root, "putnami.workspace.json", `{"name": "demo", "extensions": ["@putnami/nx"], "options": {"@putnami/nx": {"mode": "delegate"}}}`)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeFile(t, root, "packages/"+name+"/project.json", `{"name": "@demo/`+name+`", "targets": {"build": {"executor": "nx:run-commands"}, "test:lib": {"executor": "nx:run-commands"}, "test:eslint": {"executor": "nx:run-commands"}}}`)
	}
	writeFile(t, root, "packages/f/project.json", `{"name": "@demo/f", "targets": {"serve": {"executor": "nx:run-commands"}}}`)
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	status, data, _ := RunSync(&pctx.Context{WorkspaceRoot: root}, emit, nil)
	out := events.String()
	lines := func(target string) (n int) {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, target) {
				n++
			}
		}
		return n
	}
	if status != "OK" || lines("test:lib") != 1 || lines("test:eslint") != 1 {
		t.Fatalf("want one line per target name: %s\n%s", status, out)
	}
	for _, want := range []string{
		"target test:lib of 5 projects (@demo/a, @demo/b, @demo/c and 2 more): no Putnami task runs it: run nx run <project>:test:lib",
		"@demo/f:serve no Putnami task runs it: run nx run @demo/f:serve",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the sync output lacks %q:\n%s", want, out)
		}
	}
	// The job's data still lists each project and target.
	if unmapped, _ := data["unmapped"].([]Unmapped); len(unmapped) != 11 {
		t.Errorf("unmapped data = %v, want 11 entries", data["unmapped"])
	}
}
