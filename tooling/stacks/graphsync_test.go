package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

const testExtension = "@acme/tool"

func testGraphSync() GraphSync {
	return GraphSync{
		Extension: testExtension,
		HoldsBuildFile: func(root, dir string) bool {
			_, err := os.Stat(filepath.Join(root, dir, "BUILD"))
			return err == nil
		},
		SkippedDirs: []string{"out"},
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestProject(t *testing.T, root, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, dir, "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestTheGraphSyncWritesEachPackageAndKeepsForeignMembers(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"api", "pricing", "web"} {
		writeTestFile(t, filepath.Join(root, dir, "BUILD"), "")
	}
	writeTestFile(t, filepath.Join(root, "design", "package.json"), "{}")
	writeTestFile(t, filepath.Join(root, "api", "putnami.json"), `{"name": "api", "dependencies": ["/design", "/gone"], "disable": {"jobs": ["test"]}}`)
	packages := []GraphPackage{
		{Dir: "api", DependsOn: []string{"pricing", "."}, Inputs: []string{"../MODULE.bazel"}, Tested: true},
		{Dir: "pricing", Tested: true},
		{Dir: "web"},
		{Dir: "."},
	}
	g := testGraphSync()

	dry, err := g.Apply(root, packages, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api/putnami.json", "pricing/putnami.json", "web/putnami.json"}; !reflect.DeepEqual(dry.Written, want) {
		t.Errorf("dry Written = %v, want %v", dry.Written, want)
	}
	if _, err := os.Stat(filepath.Join(root, "web", "putnami.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry sync wrote web/putnami.json: %v", err)
	}

	result, err := g.Apply(root, packages, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Written, dry.Written) {
		t.Errorf("Written = %v, want %v", result.Written, dry.Written)
	}
	api := readTestProject(t, root, "api")
	want := map[string]any{
		"name":         "api",
		"extensions":   []any{testExtension},
		"dependencies": []any{"/design", "/pricing"},
		"options":      map[string]any{testExtension: map[string]any{"filePatterns": []any{"../MODULE.bazel"}}},
	}
	if !reflect.DeepEqual(api, want) {
		t.Errorf("api putnami.json = %v, want %v", api, want)
	}
	web := readTestProject(t, root, "web")
	if !reflect.DeepEqual(web["disable"], map[string]any{"jobs": []any{"test"}}) || web["$schema"] != ProjectSchema {
		t.Errorf("web putnami.json = %v", web)
	}

	again, err := g.Apply(root, packages, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Written) != 0 || again.Unchanged != 3 {
		t.Errorf("a second sync = %+v, want nothing written and 3 unchanged", again)
	}
}

func TestTheGraphSyncRetractsAFormerPackage(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "api", "BUILD"), "")
	writeTestFile(t, filepath.Join(root, "legacy", "putnami.json"), `{"name": "legacy", "extensions": ["@acme/tool", "@acme/lint"], "dependencies": ["/api", "@acme/design"], "options": {"@acme/tool": {"filePatterns": ["x"]}}}`)
	writeTestFile(t, filepath.Join(root, "out", "stale", "putnami.json"), `{"extensions": ["@acme/tool"]}`)
	g := testGraphSync()
	packages := []GraphPackage{{Dir: "api", Tested: true}}

	stale, err := g.Apply(root, packages, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stale.Retracted, []string{"legacy/putnami.json"}) {
		t.Errorf("dry Retracted = %v", stale.Retracted)
	}
	result, err := g.Apply(root, packages, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Retracted, []string{"legacy/putnami.json"}) {
		t.Errorf("Retracted = %v", result.Retracted)
	}
	legacy := readTestProject(t, root, "legacy")
	if !reflect.DeepEqual(legacy, map[string]any{"name": "legacy", "extensions": []any{"@acme/lint"}, "dependencies": []any{"@acme/design"}}) {
		t.Errorf("legacy putnami.json = %v", legacy)
	}

	// An export that lists no package retracts none of the marked ones.
	writeTestFile(t, filepath.Join(root, "legacy", "putnami.json"), `{"extensions": ["@acme/tool"]}`)
	empty, err := g.Apply(root, nil, false)
	if err != nil || !empty.EmptyExport || len(empty.Retracted) != 0 {
		t.Errorf("an empty export = %+v, %v", empty, err)
	}
}

func TestTheGraphSyncFailsOnAProjectFileThatIsNoObject(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "api", "putnami.json"), `[]`)
	if _, err := testGraphSync().Apply(root, []GraphPackage{{Dir: "api"}}, false); err == nil || !strings.Contains(err.Error(), "api/putnami.json") {
		t.Errorf("err = %v, want one that names api/putnami.json", err)
	}
}

func TestTheGraphJobsReportTheSyncAndAStaleGraph(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "api", "BUILD"), "")
	g := testGraphSync()
	var exported bool
	var failure error
	jobs := GraphJobs{
		Tool:          "tool",
		GraphFile:     "tool-graph.json",
		ExportCommand: "tool export > tool-graph.json",
		Sync: func(_ context.Context, root string, _ io.Writer, dry bool) (GraphResult, error) {
			if failure != nil {
				return GraphResult{}, failure
			}
			if !exported {
				return GraphResult{}, nil
			}
			r, err := g.Apply(root, []GraphPackage{{Dir: "api", Tested: true}}, dry)
			r.Export = "tool-graph.json"
			return r, err
		},
	}
	ctx := &pctx.Context{WorkspaceRoot: root}

	// Without the export, both jobs warn and pass.
	if status, _, err := jobs.RunSync(ctx, jsonl.New(), nil); status != statusOK || err != nil {
		t.Errorf("sync without export = %s, %v", status, err)
	}
	if status, _, err := jobs.RunCheck(ctx, jsonl.New(), nil); status != statusOK || err != nil {
		t.Errorf("check without export = %s, %v", status, err)
	}
	if s := jobs.Summary(GraphResult{}); !strings.Contains(s, "tool export > tool-graph.json") {
		t.Errorf("summary = %s", s)
	}
	if s := jobs.Summary(GraphResult{Export: "tool-graph.json", EmptyExport: true}); !strings.Contains(s, "lists no package") {
		t.Errorf("summary = %s", s)
	}

	exported = true
	if status, _, err := jobs.RunCheck(ctx, jsonl.New(), nil); status != statusFailed || err == nil || err.Error() != "tool"+SyncStaleSuffix {
		t.Errorf("check on a stale graph = %s, %v", status, err)
	}
	status, data, err := jobs.RunSync(ctx, jsonl.New(), nil)
	if status != statusOK || err != nil || !reflect.DeepEqual(data["written"], []string{"api/putnami.json"}) {
		t.Errorf("sync = %s, %v, %v", status, data, err)
	}
	if status, _, err := jobs.RunCheck(ctx, jsonl.New(), nil); status != statusOK || err != nil {
		t.Errorf("check after the sync = %s, %v", status, err)
	}

	failure = errors.New("the export cannot be read")
	if _, _, err := jobs.RunSync(ctx, jsonl.New(), nil); err == nil || err.Error() != "tool"+SyncFailedSuffix {
		t.Errorf("a failed sync = %v", err)
	}
	if _, _, err := jobs.RunCheck(ctx, jsonl.New(), nil); err == nil || err.Error() != "tool"+SyncFailedSuffix {
		t.Errorf("a failed check = %v", err)
	}
}
