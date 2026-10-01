package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// An --impacted run whose code spans two unrelated projects records the
// change shape with its mixed-intent flag and still exits with the run's own
// code: size and spread are information, never a verdict.
func TestImpactedRunRecordsTheChangeShapeWithoutChangingTheExitCode(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "change-shape-report", "the-change-is-sized-by-category-and-changes-no-exit-code")
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", ".putnami/\n")
	write("putnami.workspace.json", `{"name":"change-shape","includes":["app","tool","extension"]}`)
	write("app/putnami.json", `{"name":"app","extensions":["/extension"]}`)
	write("tool/putnami.json", `{"name":"tool","extensions":["/extension"]}`)
	write("extension/putnami.json", `{"name":"@putnami/change-shape"}`)
	write("extension/putnami.extension.json", `{
		"name":"@putnami/change-shape","version":"1.0.0","cliContract":4,
		"commands":{"build":{"run":[{"id":"build","task":"mark"}]}},
		"tasks":{"mark":{"kind":"command","command":`+taskCommand(t, fixtureproc.Program{})+`,"cache":false}}
	}`)
	initCLISelectionGitRepo(t, root)
	runCLISelectionGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, root, "checkout", "-b", "feature")
	write("app/main.go", "package main\n")
	write("tool/tool.go", "package tool\n\nfunc Tool() {}\n")
	write("tool/tool_test.go", "package tool\n")
	workspace.InvalidateLoadCache(root)

	var live bytes.Buffer
	renderer := output.NewRenderer(output.Config{Output: "jsonl", Command: "build", Out: &live, Err: &live})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := New().Run(ctx, Request{
		WorkspaceRoot: root, Config: wsproto.Load(root), Commands: []string{"build"},
		Global: GlobalFlags{Output: "jsonl", Impacted: true, Projects: impactedProjectsSentinel, NoCache: true, MaxParallel: 1},
	}, renderer)
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("a mixed-intent change must not fail the run: exit=%d err=%v\n%s", result.ExitCode, err, live.String())
	}

	var shape *workspace.ChangeShapeRecord
	for _, line := range strings.Split(strings.TrimSpace(live.String()), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode live record %q: %v", line, err)
		}
		if record.Event["type"] != workspace.ChangeShapeRecordType {
			continue
		}
		encoded, err := json.Marshal(record.Event)
		if err != nil {
			t.Fatal(err)
		}
		shape = &workspace.ChangeShapeRecord{}
		if err := json.Unmarshal(encoded, shape); err != nil {
			t.Fatalf("decode the change shape %s: %v", encoded, err)
		}
	}
	if shape == nil {
		t.Fatalf("no %s record on the live stream:\n%s", workspace.ChangeShapeRecordType, live.String())
	}
	if want := (workspace.ChangeSize{Files: 3, Added: 5}); shape.Total != want {
		t.Errorf("total = %+v, want %+v", shape.Total, want)
	}
	if want := (workspace.ChangeSize{Files: 1, Added: 1}); shape.Categories["tests"] != want {
		t.Errorf("tests = %+v, want %+v", shape.Categories["tests"], want)
	}
	if want := [][]string{{"app"}, {"tool"}}; !reflect.DeepEqual(shape.Areas, want) || !shape.MixedIntent {
		t.Errorf("areas = %v, mixedIntent = %v; want %v and true", shape.Areas, shape.MixedIntent, want)
	}
}
