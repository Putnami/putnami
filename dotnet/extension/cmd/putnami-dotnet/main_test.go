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
	if code := run([]string{"__putnami", "runtime-info"}, strings.NewReader(""), &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), "@putnami/dotnet") {
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
	want := []string{"build", "deps-upgrade", "lint", "lint-docs", "package-docker", "publish-docker", "run", "serve", "test", "workspace-fetch", "workspace-install"}
	if code != 0 || !slices.Equal(got, want) {
		t.Fatalf("exit %d, jobs %v, want %v", code, got, want)
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
	spectest.Proves(t, "dotnet/stack-extension", "checks-documentation-links", "lint-checks-the-documentation-links")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := proto.ParseManifest(data)
	if manifest == nil {
		t.Fatal("the manifest does not parse")
	}
	if flag := manifest.Commands["lint"].Flags["docs-links"]; flag.Default != true {
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
	ctx.Params = protojob.Params{"docs-links": json.RawMessage(`false`)}
	if status, _, _ := job(ctx, jsonl.New(), nil); status != "SKIP" {
		t.Errorf("docs-links off: status %s", status)
	}
}

func TestPublishTakesTheDockerRegistryFlag(t *testing.T) {
	spectest.Proves(t, "dotnet/stack-extension", "publishes-an-application-image", "publish-pushes-the-application-image")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := proto.ParseManifest(data)
	if manifest == nil {
		t.Fatal("the manifest does not parse")
	}
	// The CLI hands a declared flag to the shared publisher as a parameter.
	if flag, ok := manifest.Commands["publish"].Flags["docker-registry"]; !ok || flag.Type != "string" {
		t.Errorf("the publish command declares no docker-registry flag: %+v", manifest.Commands["publish"].Flags)
	}
}
