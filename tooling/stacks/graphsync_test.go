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

func TestTheGraphSyncWritesAnotherExtensionTheTagAndTheOwnedJobs(t *testing.T) {
	root := t.TempDir()
	g := testGraphSync()
	g.Extensions = []string{"@acme/lang"}
	g.Tag = "tool"
	g.Marked = true
	g.Jobs = []string{"build", "lint", "test"}
	g.Retract = func(doc map[string]any) { delete(doc, "owned") }
	writeTestFile(t, filepath.Join(root, "web", "BUILD"), "")
	writeTestFile(t, filepath.Join(root, "api", "putnami.json"), `{"extensions": ["@acme/tool", "@acme/other"], "disable": {"jobs": ["serve", "lint"]}}`)
	packages := []GraphPackage{
		{Dir: "api", Extension: "@acme/lang", Disabled: []string{"build"}, Edit: func(doc map[string]any) { doc["owned"] = true }},
		{Dir: "web", Disabled: []string{"lint", "test"}},
	}
	if _, err := g.Apply(root, packages, false); err != nil {
		t.Fatal(err)
	}
	api := readTestProject(t, root, "api")
	if got := api["extensions"]; !reflect.DeepEqual(got, []any{"@acme/other", "@acme/lang"}) {
		t.Fatalf("api extensions %v", got)
	}
	if got := api["disable"]; !reflect.DeepEqual(got, map[string]any{"jobs": []any{"serve", "build"}}) {
		t.Fatalf("api disable %v", got)
	}
	if api["owned"] != true || !reflect.DeepEqual(api["tags"], []any{"tool"}) || !reflect.DeepEqual(api["options"], map[string]any{testExtension: map[string]any{MarkMember: true}}) {
		t.Fatalf("api %v", api)
	}
	web := readTestProject(t, root, "web")
	if got := web["disable"]; !reflect.DeepEqual(got, map[string]any{"jobs": []any{"lint", "test"}}) {
		t.Fatalf("web disable %v", got)
	}

	// api is no package any more: the mark names it the sync's, so the sync
	// retracts what it wrote, and keeps the language extension it runs.
	result, err := g.Apply(root, packages[1:], false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Retracted, []string{"api/putnami.json"}) {
		t.Fatalf("retracted %v", result.Retracted)
	}
	api = readTestProject(t, root, "api")
	want := map[string]any{"extensions": []any{"@acme/other", "@acme/lang"}, "disable": map[string]any{"jobs": []any{"serve"}}, "$schema": ProjectSchema}
	delete(api, "$schema")
	delete(want, "$schema")
	if !reflect.DeepEqual(api, want) {
		t.Fatalf("retracted api %v", api)
	}
}

func TestADryGraphSyncAnswersEachChangeWithItsContent(t *testing.T) {
	root := t.TempDir()
	g := testGraphSync()
	for _, dir := range []string{"api", "pricing"} {
		writeTestFile(t, filepath.Join(root, dir, "BUILD"), "")
	}
	writeTestFile(t, filepath.Join(root, "old", "putnami.json"), `{"extensions": ["@acme/tool"]}`)
	existing := "{\n  \"name\": \"api\"\n}\n"
	writeTestFile(t, filepath.Join(root, "api", "putnami.json"), existing)
	result, err := g.Apply(root, []GraphPackage{{Dir: "api", DependsOn: []string{"pricing"}, Tested: true}, {Dir: "pricing", Tested: true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, c := range result.Changes {
		files = append(files, c.File)
	}
	if !reflect.DeepEqual(files, []string{"api/putnami.json", "pricing/putnami.json", "old/putnami.json"}) {
		t.Fatalf("changes %v", files)
	}
	api := result.Changes[0]
	if string(api.Before) != existing || !strings.Contains(string(api.After), `"/pricing"`) || result.Changes[1].Before != nil {
		t.Errorf("api change %q → %q", api.Before, api.After)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "api", "putnami.json")); string(data) != existing {
		t.Errorf("a dry run wrote %s", data)
	}
	if _, err := os.Stat(filepath.Join(root, "pricing", "putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a dry run created a file: %v", err)
	}
	// The preview prints each change with its diff.
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	EmitPreview(emit, "tool", result)
	if !strings.Contains(events.String(), "--- /dev/null") || !strings.Contains(events.String(), "+    \"/pricing\"") {
		t.Errorf("preview:\n%s", events.String())
	}
}

func TestDiffAlignsTheLinesOfAFile(t *testing.T) {
	got := Diff("f", []byte("a\nb\nc\n"), []byte("a\nx\nc\nd\n"))
	if want := "--- a/f\n+++ b/f\n a\n-b\n+x\n c\n+d\n"; got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := Diff("f", nil, []byte("a\n")); got != "--- /dev/null\n+++ b/f\n+a\n" {
		t.Errorf("a new file:\n%s", got)
	}
}

func TestATagAloneMarksNoPackage(t *testing.T) {
	root := t.TempDir()
	g := testGraphSync()
	g.Tag, g.Marked = "tool", true
	writeTestFile(t, filepath.Join(root, "api", "BUILD"), "")
	own := `{"name": "scripts", "tags": ["tool"], "extensions": ["@acme/lang"], "dependencies": ["/api"]}`
	writeTestFile(t, filepath.Join(root, "scripts", "putnami.json"), own)
	result, err := g.Apply(root, []GraphPackage{{Dir: "api", Tested: true}}, false)
	if err != nil || len(result.Retracted) != 0 {
		t.Fatalf("retracted %v (%v)", result.Retracted, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "scripts", "putnami.json")); string(data) != own {
		t.Errorf("a project the team tagged was rewritten: %s", data)
	}
}

func TestTheSyncJobReportsTheWarningsOfTheToolsSync(t *testing.T) {
	root := t.TempDir()
	jobs := GraphJobs{
		Tool: "tool",
		Sync: func(context.Context, string, io.Writer, bool) (GraphResult, error) {
			return GraphResult{Export: "tool-graph.json", Warnings: []string{"not mapped: deploy"}}, nil
		},
	}
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	if status, _, err := jobs.RunSync(&pctx.Context{WorkspaceRoot: root}, emit, nil); status != statusOK || err != nil {
		t.Fatalf("sync = %s, %v", status, err)
	}
	if !strings.Contains(events.String(), "not mapped: deploy") {
		t.Errorf("the sync job reports no warning:\n%s", events.String())
	}
}
