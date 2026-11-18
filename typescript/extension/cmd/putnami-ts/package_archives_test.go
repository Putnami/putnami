package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/sdk/extension/jsonl"
)

// archiveCommandWorkspace writes a workspace with one extension project at ext/
// whose manifest names a template directory under {extensionRoot}.
func archiveCommandWorkspace(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "ext")
	if err := os.MkdirAll(filepath.Join(project, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(project, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "templates", "page.tmpl"), []byte("page\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const archiveCommandManifest = `{
	"name": "@putnami/testext",
	"commands": {"sync": {"run": [{"id": "s", "task": "sync"}]}},
	"tasks": {"sync": {"kind": "command", "command": "echo", "args": ["{extensionRoot}/templates"]}}
}`

func TestRunPackageArchives_PackagesUnderTheComputedVersion(t *testing.T) {
	root := archiveCommandWorkspace(t, archiveCommandManifest)
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "@putnami/testext", Path: "ext"},
		Params:        pctx.Params{},
		Version:       &pctx.Version{Base: "1.2.3", Full: "1.2.3-abc"},
	}
	status, data, err := runPackageArchives(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("package-archives = %s, %v; want OK", status, err)
	}
	if data["name"] != "@putnami/testext" || data["version"] != "1.2.3-abc" {
		t.Errorf("data = %v", data)
	}
	archives, _ := data["archives"].([]string)
	if len(archives) == 0 {
		t.Fatalf("no archives in %v", data)
	}
	for _, archive := range archives {
		if _, err := os.Stat(archive); err != nil {
			t.Errorf("archive %s: %v", archive, err)
		}
	}

	ctx.Params = pctx.Params{"dry-run": json.RawMessage("true")}
	ctx.Version = &pctx.Version{Base: "1.2.3"}
	status, data, err = runPackageArchives(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" || data["dryRun"] != true || data["version"] != "1.2.3" {
		t.Fatalf("dry run = %s, %v, %v", status, data, err)
	}
}

func TestRunPackageArchives_FailsWithoutANamedManifest(t *testing.T) {
	for name, manifest := range map[string]string{
		"no manifest": "",
		"no name":     `{"tasks": {}}`,
		"malformed":   `{`,
	} {
		t.Run(name, func(t *testing.T) {
			root := archiveCommandWorkspace(t, manifest)
			var out strings.Builder
			emit := jsonl.New()
			emit.Observe(func(message string) { out.WriteString(message + "\n") })
			ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Path: "ext"}, Params: pctx.Params{}, Version: &pctx.Version{Full: "1.0.0"}}
			status, _, err := runPackageArchives(ctx, emit, nil)
			if err != nil || status != "FAILED" {
				t.Fatalf("package-archives = %s, %v; want FAILED", status, err)
			}
			if !strings.Contains(out.String(), "putnami.extension.json") {
				t.Errorf("the failure names no manifest:\n%s", out.String())
			}
		})
	}
}

func TestRunPackageArchives_ReportsAPackagingFailure(t *testing.T) {
	manifest := strings.Replace(archiveCommandManifest, "{extensionRoot}/templates", "{extensionRoot}/missing", 1)
	root := archiveCommandWorkspace(t, manifest)
	var out strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { out.WriteString(message + "\n") })
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Path: "ext"}, Params: pctx.Params{}, Version: &pctx.Version{Full: "1.0.0"}}
	status, _, err := runPackageArchives(ctx, emit, nil)
	if err != nil || status != "FAILED" {
		t.Fatalf("package-archives = %s, %v; want FAILED", status, err)
	}
	if !strings.Contains(out.String(), "{extensionRoot}/missing") {
		t.Errorf("the failure names no missing path:\n%s", out.String())
	}
}

func TestArchiveVersion_FallsBackToTheWorkspaceBaseAndTheGitSuffix(t *testing.T) {
	root := t.TempDir()
	gittest.Run(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "a.txt")
	gittest.Run(t, root, "commit", "-q", "-m", "init")
	sha := strings.TrimSpace(gittest.Run(t, root, "rev-parse", "--short", "HEAD"))

	ctx := &pctx.Context{WorkspaceRoot: root, Workspace: pctx.Workspace{Version: "2.0.0-dev"}}
	version, err := archiveVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(version, "2.0.0-") || !strings.Contains(version, sha) {
		t.Errorf("version = %q, want the base 2.0.0 with the suffix of %s", version, sha)
	}
}
