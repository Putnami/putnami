package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// workloadWorkspace writes, at root, a workspace whose one extension declares
// build with no trait, daemon and compose-up with the serve workload, migrate
// and qualify-run with the once workload and tidy with rewritesSources:
// compose-up and qualify-run are declared as @putnami/dev declares them.
func workloadWorkspace(t *testing.T, root string) *workspacepb.Config {
	t.Helper()
	step := func(id string) []extensionproto.PipelineStep {
		return []extensionproto.PipelineStep{{ID: id, Task: "noop"}}
	}
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/workload", Version: "1.0.0", CLIContract: protocolcli.CommandKindContract,
		Commands: map[string]extensionproto.CommandDefinition{
			"build":       {Run: step("build")},
			"daemon":      {Run: step("daemon"), Traits: &extensionproto.CommandTraits{Workload: extensionproto.WorkloadServe}},
			"migrate":     {Run: step("migrate"), Traits: &extensionproto.CommandTraits{Workload: extensionproto.WorkloadOnce}},
			"tidy":        {Run: step("tidy"), Traits: &extensionproto.CommandTraits{RewritesSources: true}},
			"compose-up":  {Run: step("compose-up"), Traits: &extensionproto.CommandTraits{Workload: extensionproto.WorkloadServe}},
			"qualify-run": {Run: step("qualify-run"), Traits: &extensionproto.CommandTraits{Workload: extensionproto.WorkloadOnce}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"noop": {Kind: "command", Command: "true", Cwd: "{projectRoot}", TimeoutMs: -1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"putnami.workspace.json":           []byte(`{"name":"workload","includes":["app","extension"],"extensions":["/extension"]}`),
		"app/putnami.json":                 []byte(`{"name":"app","extensions":["/extension"]}`),
		"extension/putnami.json":           []byte(`{"name":"@test/workload"}`),
		"extension/putnami.extension.json": manifest,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace.InvalidateLoadCache(root)
	return workspacepb.Load(root)
}

// The executing engine of a bound request of either version refuses a
// command whose traits, read from the manifests it loaded, declare a workload
// or rewritesSources: right after discovery, as a usage error, before any
// selection, plan or session. The protocol does not refuse these commands by
// name, and their plan is the one the submitter expects, so this refusal is
// the only one a request that skipped the submitter's checks meets.
//
// It is not parallel: captureStderr swaps the process's os.Stderr, which every
// parallel test that builds a renderer reads.
func TestPortableExecutionRefusesAWorkloadOrRewritingCommand(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "commit-request", "the-executing-engine-refuses-a-workload-or-rewriting-command")
	all := runner.RequestedSelection{Mode: runner.SelectionModeAll}
	for command, refusal := range map[string]string{
		"daemon":      `"daemon" streams a live workload and is not portable`,
		"migrate":     `"migrate" streams a live workload and is not portable`,
		"tidy":        `"tidy" rewrites source and is not portable`,
		"compose-up":  `"compose-up" streams a live workload and is not portable`,
		"qualify-run": `"qualify-run" streams a live workload and is not portable`,
	} {
		for version, portable := range map[string]*PortableExecution{
			"version 1": {Request: runner.ExecutionRequest{Invocation: runner.InvocationBlock{Commands: []string{"build", command}}}},
			"version 2": commitExecution(all, "", nil),
		} {
			t.Run(command+" "+version, func(t *testing.T) {
				root := t.TempDir()
				cfg := workloadWorkspace(t, root)
				req := &Request{WorkspaceRoot: root, Config: cfg, Commands: []string{"build", command}, Portable: portable}
				var result SessionResult
				var err error
				stderr := captureStderr(t, func() { result, err = New().run(context.Background(), req, nil) })
				if result.ExitCode != ExitUsage || err != nil {
					t.Fatalf("run = exit %d, %v, want exit %d", result.ExitCode, err, ExitUsage)
				}
				if want := "putnami: portable execution refused: " + refusal; !strings.Contains(stderr, want) {
					t.Fatalf("stderr = %q, want %q", stderr, want)
				}
				if result.Plan != nil || result.Session != nil {
					t.Fatalf("a refused request planned %d jobs or recorded a session", len(result.Plan))
				}
				if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
					t.Fatalf("a refused request recorded a session: %v", err)
				}
			})
		}
	}

	// The refusal reads the traits, never a name: a finite command passes it.
	root := t.TempDir()
	cfg := workloadWorkspace(t, root)
	ws, discovered, code, err := loadRunWorkspace(&Request{WorkspaceRoot: root, Config: cfg, Commands: []string{"build"}})
	if code != ExitSuccess || err != nil || ws == nil {
		t.Fatalf("load the fixture: exit %d, %v", code, err)
	}
	if err := refuseUnportableCommands(discovered.Extensions, []string{"build", "serve", "format"}); err != nil {
		t.Fatalf("a command that declares neither trait was refused: %v", err)
	}
}
