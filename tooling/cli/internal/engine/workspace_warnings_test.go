package engine

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	workspacepb "go.putnami.dev/protocol/workspace"
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
