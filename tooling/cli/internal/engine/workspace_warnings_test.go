package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// warnedWorkspace writes, at root, a workspace whose one project declares the
// ignored `jobs` key, so its load reports one warning, and whose extension
// declares build.
func warnedWorkspace(t *testing.T, root string) *workspacepb.Config {
	t.Helper()
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/build", Version: "1.0.0", CLIContract: protocolcli.CurrentContract,
		Commands: map[string]extensionproto.CommandDefinition{"build": {
			Run: []extensionproto.PipelineStep{{ID: "build", Task: "noop"}},
		}},
		Tasks: map[string]extensionproto.TaskDefinition{
			"noop": {Kind: "command", Command: "true", Cwd: "{projectRoot}", TimeoutMs: -1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		"putnami.workspace.json":           `{"name":"warned","includes":["app","extension"]}`,
		"app/putnami.json":                 `{"name":"app","extensions":["@test/build"],"jobs":{}}`,
		"extension/putnami.json":           `{"name":"@test/build"}`,
		"extension/putnami.extension.json": string(manifest),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace.InvalidateLoadCache(root)
	return workspacepb.Load(root)
}

// TestRun_ReturnsTheWarningsOfItsWorkspaceLoad holds the run to returning what
// its workspace load reported, quiet or not, so a command that writes its own
// result envelope carries it. A run that ended before the load returns none.
func TestRun_ReturnsTheWarningsOfItsWorkspaceLoad(t *testing.T) {
	root := rootWithoutRepository(t)
	cfg := warnedWorkspace(t, root)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	for _, quiet := range []bool{false, true} {
		name := "default"
		if quiet {
			name = "quiet"
		}
		t.Run(name, func(t *testing.T) {
			result, err := New().Run(context.Background(), Request{
				WorkspaceRoot: root,
				Config:        cfg,
				Commands:      []string{"build"},
				Global:        GlobalFlags{Projects: "app", Plan: true, Quiet: quiet},
				Stdout:        io.Discard,
				ErrorOut:      io.Discard,
			}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(result.WorkspaceWarnings) != 1 || result.WorkspaceWarnings[0].Code != workspace.WarningCodeJobsIgnored {
				t.Fatalf("WorkspaceWarnings = %+v, want the one ignored-jobs warning", result.WorkspaceWarnings)
			}
		})
	}

	// A run whose request is refused before the workspace loads has none.
	t.Setenv("PUTNAMI_SOURCE_REVISION", "not-a-commit")
	result, err := New().Run(context.Background(), Request{
		WorkspaceRoot: root, Config: cfg, Commands: []string{"build"},
		Global: GlobalFlags{Plan: true}, Stdout: io.Discard, ErrorOut: io.Discard,
	}, nil)
	if err == nil || result.WorkspaceWarnings != nil {
		t.Fatalf("a run refused before the load returned %+v, %v; want an error and no warnings", result.WorkspaceWarnings, err)
	}
}

// TestBuildPlan_ReportsTheWarningsItsPlanRecords holds planning to reporting
// the warnings on the workspace once the plan is made, into the report of the
// load: a warning the planner records, such as shared executions it dropped,
// reaches the run's stderr and its envelope like a load warning does. The
// warning is recorded before planning here, so the test does not depend on a
// plan that makes the planner warn.
func TestBuildPlan_ReportsTheWarningsItsPlanRecords(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ext := &extension.ExtensionDescription{Name: "@test/build", Jobs: map[string]*extension.JobDefinition{
		"build": {Name: "build", ExtensionName: "@test/build", Command: "/bin/true"},
	}}
	proj := &workspace.Project{ID: "/app", Name: "app", Path: "app", Extensions: []string{"@test/build"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{proj})
	var out bytes.Buffer
	req := &Request{WorkspaceRoot: root, Commands: []string{"build"}, workspaceWarnings: workspace.NewWarningReport(&out)}
	ws.AddWarning(workspace.WarningCodeSharedExecutionsDropped, "the plan dropped its shared executions")

	exts := []*extension.ExtensionDescription{ext}
	if _, code := buildPlan(req, ws, ws.Projects, exts, &extension.DiscoveryResult{Extensions: exts}); code != ExitSuccess {
		t.Fatalf("buildPlan exit code = %d", code)
	}
	if got := req.workspaceWarnings.Warnings(); len(got) != 1 || got[0].Code != workspace.WarningCodeSharedExecutionsDropped {
		t.Fatalf("the run holds %+v, want the one shared-executions-dropped warning", got)
	}
	if want := "putnami: warning: the plan dropped its shared executions\n"; out.String() != want {
		t.Fatalf("the report wrote %q, want %q", out.String(), want)
	}
}

// TestLoadWorkspaceAndExtensions_PublishesTheLoadToAWatchSession holds each
// load of a watch session to publishing its report to the session's cell, so
// the renderer built once from the outer run reads the latest load.
func TestLoadWorkspaceAndExtensions_PublishesTheLoadToAWatchSession(t *testing.T) {
	root := rootWithoutRepository(t)
	cfg := warnedWorkspace(t, root)
	req := &Request{
		WorkspaceRoot: root, Config: cfg, Commands: []string{"build"},
		Global: GlobalFlags{Quiet: true}, latestWarnings: &atomic.Pointer[workspace.WarningReport]{},
	}
	if _, _, code := loadWorkspaceAndExtensions(req); code != ExitSuccess {
		t.Fatalf("load exit code = %d", code)
	}
	if got := req.latestWarnings.Load(); got == nil || got != req.workspaceWarnings || len(got.Warnings()) != 1 {
		t.Fatalf("latest load = %+v, want the request's report with the ignored-jobs warning", got)
	}
}

// TestLoadWorkspaceAndExtensions_ReportsIntoTheCallersReport holds a load to
// reporting into Request.WarningReport when the caller passes one: a warning
// the caller already reported is neither written again nor kept twice.
func TestLoadWorkspaceAndExtensions_ReportsIntoTheCallersReport(t *testing.T) {
	root := rootWithoutRepository(t)
	cfg := warnedWorkspace(t, root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report := workspace.NewWarningReport(&out)
	report.Report(ws)
	written := out.String()

	req := &Request{WorkspaceRoot: root, Config: cfg, Commands: []string{"build"}, WarningReport: report}
	if _, _, code := loadWorkspaceAndExtensions(req); code != ExitSuccess {
		t.Fatalf("load exit code = %d", code)
	}
	if req.workspaceWarnings != report {
		t.Fatal("the load made its own report instead of the caller's")
	}
	if got := report.Warnings(); len(got) != 1 || got[0].Code != workspace.WarningCodeJobsIgnored {
		t.Fatalf("the caller's report holds %+v, want the one ignored-jobs warning", got)
	}
	if out.String() != written {
		t.Fatalf("the load wrote the warning again: %q", out.String())
	}
}

// TestLoadWorkspaceAndExtensions_ASupervisedServeRunWritesNoWarningOnItsServeLog
// holds a supervised serve run to keeping the warnings of its load off its
// stderr, which is the serve log its supervisor passes through: the
// supervisor wrote the warnings of its own load of the workspace. The run
// still holds them, and a jsonl run without --serve-env writes them as every
// run does.
func TestLoadWorkspaceAndExtensions_ASupervisedServeRunWritesNoWarningOnItsServeLog(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "a-supervised-serve-run-splits-its-output", "the-serve-log-carries-no-load-warning")
	root := rootWithoutRepository(t)
	cfg := warnedWorkspace(t, root)
	for _, tc := range []struct {
		name     string
		serveEnv string
		written  int
	}{
		{name: "jsonl run", written: 1},
		{name: "supervised serve run", serveEnv: "-", written: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{
				WorkspaceRoot: root, Config: cfg, Commands: []string{"build"},
				Global: GlobalFlags{Output: "jsonl", ServeEnv: tc.serveEnv},
			}
			var code int
			stderr := captureStderr(t, func() { _, _, code = loadWorkspaceAndExtensions(req) })
			if code != ExitSuccess {
				t.Fatalf("load exit code = %d", code)
			}
			if got := strings.Count(stderr, "putnami: warning: "); got != tc.written {
				t.Errorf("stderr carries %d warnings, want %d:\n%s", got, tc.written, stderr)
			}
			if got := req.workspaceWarnings.Warnings(); len(got) != 1 || got[0].Code != workspace.WarningCodeJobsIgnored {
				t.Errorf("the run holds %+v, want the one ignored-jobs warning", got)
			}
		})
	}
}
