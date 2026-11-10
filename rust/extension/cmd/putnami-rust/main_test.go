package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protojob "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/docslinks"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestRunAnswersTheRuntimeHandshake(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, strings.NewReader(""), &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), "@putnami/rust") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunHandsTheTasksToTheJobDispatcher(t *testing.T) {
	var got []string
	code := run([]string{"test"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, func(jobs map[string]cli.JobFunc) {
		for name := range jobs {
			got = append(got, name)
		}
	})
	slices.Sort(got)
	want := []string{
		"build", "deps-upgrade", "lint", "lint-docs", "package-archives", "package-compile", "package-crate", "package-docker",
		"publish-crates", "publish-docker", "run", "serve", "test", "workspace-fetch", "workspace-install",
	}
	if code != 0 || !slices.Equal(got, want) {
		t.Fatalf("exit %d, jobs %v", code, got)
	}
}

func TestRunReportsAFailedControlCallOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"__putnami", "workspace-probe"}, strings.NewReader("not json"), &stdout, &stderr, nil)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "workspace-probe") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestLintFailsOnABrokenDocumentationLinkUnlessDocsLinksIsOff(t *testing.T) {
	spectest.Proves(t, "rust/stack-extension", "checks-documentation-links", "lint-fails-on-a-broken-documentation-link-unless-docs-links-is-off")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := proto.ParseManifest(data)
	if manifest == nil {
		t.Fatal("the manifest does not parse")
	}
	if flag := manifest.Commands["lint"].Flags[docslinks.Param]; flag.Default != true {
		t.Errorf("the docs-links flag = %+v", flag)
	}
	step := manifest.Commands["lint"].Run[len(manifest.Commands["lint"].Run)-1]
	if step.Task != "lint-docs" || step.If != "params.docs-links" {
		t.Errorf("the lint pipeline's last step = %+v", step)
	}
	if got, want := manifest.Tasks["lint-docs"].Inputs, docslinks.Inputs(); !reflect.DeepEqual(got, want) {
		t.Errorf("lint-docs inputs = %+v, want %+v", got, want)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("[gone](missing.md)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var job cli.JobFunc
	run([]string{"test"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, func(jobs map[string]cli.JobFunc) { job = jobs["lint-docs"] })
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{FullPath: root}}
	if status, _, _ := job(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Errorf("a broken link: status %s", status)
	}
	ctx.Params = protojob.Params{docslinks.Param: json.RawMessage(`false`)}
	if status, _, _ := job(ctx, jsonl.New(), nil); status != "SKIP" {
		t.Errorf("docs-links off: status %s", status)
	}

	// A nested crate answers for its own documents.
	nested := filepath.Join(root, "crates", "pricing")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"README.md": "[gone](missing.md)\n", "Cargo.toml": "[package]\nname = \"pricing\"\n"} {
		if err := os.WriteFile(filepath.Join(nested, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx.Params = nil
	if status, _, _ := job(ctx, jsonl.New(), nil); status != "OK" {
		t.Errorf("the root read a nested crate's document: status %s", status)
	}
	ctx.Project.FullPath = nested
	if status, _, _ := job(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Errorf("the nested crate's broken link: status %s", status)
	}
}
