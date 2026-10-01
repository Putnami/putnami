package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/clientgen/extension/internal/workspaceclient"
)

func TestCommandHandlersExposeEveryDeclaredRuntimeAction(t *testing.T) {
	handlers := commandHandlers()
	got := make([]string, 0, len(handlers))
	for name := range handlers {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"generate-go", "generate-ts", "workspace-adopt", "workspace-check", "workspace-sync"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime handlers = %v, want %v", got, want)
	}
}

func TestGenerateTargetRequiresAProjectAndSkipsAnUnconfiguredLanguage(t *testing.T) {
	handler := generateTarget(clientcontract.GeneratedLanguageGo)
	status, _, err := handler(&pctx.Context{}, nil, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "selected provider project") {
		t.Fatalf("missing project result = status %q err %v", status, err)
	}

	root := t.TempDir()
	project := filepath.Join(root, "provider")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatal(err)
	}
	status, output, err := handler(&pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "provider", Path: "provider", FullPath: project},
	}, nil, nil)
	if err != nil || status != "OK" || len(output) != 0 {
		t.Fatalf("unconfigured target result = status %q output %v err %v", status, output, err)
	}
}

// A provider that commits a client but has no .gen/clientgen/config.json is a
// contract caught mid-rewrite, not a provider with nothing to generate. Reporting
// OK with no output port would let the engine empty the committed target while
// the task succeeds.
func TestGenerateTargetFailsWhenACommittedClientHasNoGenerationContract(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "provider")
	manifest := `{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"go",` +
		`"service":{"id":"items"},"contractSha256":"` + strings.Repeat("a", 64) + `"}`
	committed := map[string]string{
		"provider/clients/go/client.putnami.json": manifest,
		"provider/clients/go/client.gen.go":       "package client\n",
	}
	for rel, body := range committed {
		writeMainTestFile(t, root, rel, body)
	}
	jobContext := &pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "provider", Path: "provider", FullPath: project},
	}

	status, output, err := generateTarget(clientcontract.GeneratedLanguageGo)(jobContext, nil, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "clients/go") ||
		!strings.Contains(err.Error(), ".gen/clientgen/config.json") {
		t.Fatalf("missing contract with a committed Go client = status %q output %v err %v", status, output, err)
	}
	for rel, body := range committed {
		got, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if readErr != nil || string(got) != body {
			t.Fatalf("committed %s = %q (%v), want it untouched", rel, got, readErr)
		}
	}

	// The provider commits no TypeScript client, so that target has nothing to
	// generate and stays a successful no-op.
	status, output, err = generateTarget(clientcontract.GeneratedLanguageTypeScript)(jobContext, nil, nil)
	if err != nil || status != "OK" || len(output) != 0 {
		t.Fatalf("missing contract without a committed TS client = status %q output %v err %v", status, output, err)
	}
}

func TestGenerateTargetFailsWhenACommittedManifestCannotBeRead(t *testing.T) {
	root := t.TempDir()
	writeMainTestFile(t, root, "provider/clients/go/client.putnami.json", `{"generatedBy":"someone else"}`)
	status, _, err := generateTarget(clientcontract.GeneratedLanguageTypeScript)(&pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "provider", Path: "provider", FullPath: filepath.Join(root, "provider")},
	}, nil, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "client.putnami.json") {
		t.Fatalf("unreadable committed manifest = status %q err %v", status, err)
	}
}

func TestGenerateTargetRunsTheConfiguredPackageShimAndReportsItsOutputPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable script")
	}
	root := t.TempDir()
	project := filepath.Join(root, "provider")
	writeMainTestFile(t, root, "provider/.gen/clientgen/config.json", `{
  "targets":["ts"],
  "ts":{"output":"clients/ts"},
  "go":{}
}`)
	shim := filepath.Join(project, "clients", "ts", "node_modules", ".bin", "putnami-client-generate")
	writeMainTestFile(t, root, "provider/clients/ts/node_modules/.bin/putnami-client-generate", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(shim, 0o700); err != nil {
		t.Fatal(err)
	}
	status, output, err := generateTarget(clientcontract.GeneratedLanguageTypeScript)(&pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "provider", Path: "provider"},
	}, nil, nil)
	if err != nil || status != "OK" || output["typescriptClientOutput"] != "clients/ts" {
		t.Fatalf("configured TS target result = status %q output %v err %v", status, output, err)
	}
}

func TestWorkspaceHandlerRejectsMissingRootIndexAndSpawner(t *testing.T) {
	emitter := jsonl.NewForVersion(1)
	status, _, err := runWorkspace(workspaceclient.ModeCheck)(&pctx.Context{}, emitter, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("missing root result = status %q err %v", status, err)
	}

	// The check reads committed inputs only, so a missing project index is a
	// finding the run exits on, reported like every other verdict: through the
	// report and the diagnostics, never as a bare error that loses the count.
	root := t.TempDir()
	status, _, err = runWorkspace(workspaceclient.ModeCheck)(&pctx.Context{WorkspaceRoot: root}, emitter, nil)
	if status != "FAILED" || err == nil {
		t.Fatalf("missing index result = status %q err %v", status, err)
	}
	report := workspaceclient.InspectCommitted(root)
	indexReported := false
	for _, finding := range report.Findings {
		if finding.Code == "clientgen.discovery" && strings.Contains(finding.Message, "workspace project index") {
			indexReported = true
		}
	}
	if !indexReported {
		t.Fatalf("a missing project index was not reported as a discovery finding: %+v", report.Findings)
	}

	writeMainTestFile(t, root, ".putnami/workspace-index.json", `{"version":1,"projects":[]}`)
	t.Setenv(registry.CLIExecutableEnv, "")
	status, _, err = runWorkspace(workspaceclient.ModeSync)(&pctx.Context{WorkspaceRoot: root}, emitter, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), registry.CLIExecutableEnv) {
		t.Fatalf("missing spawner result = status %q err %v", status, err)
	}
}

func writeMainTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
