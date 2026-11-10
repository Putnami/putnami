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
	if code := run([]string{"__putnami", "runtime-info"}, strings.NewReader(""), &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), "@putnami/java") {
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
	want := []string{"build", "lint", "lint-docs", "package-docker", "publish-docker", "run", "serve", "test", "workspace-install"}
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
	spectest.Proves(t, "java/stack-extension", "checks-documentation-links", "lint-fails-on-a-broken-documentation-link-unless-docs-links-is-off")
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
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "[gone](missing.md)\n")
	write("settings.gradle.kts", "include(\":api\")\n")
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

	// A nested Gradle project, Maven module or build answers for its own
	// documents.
	write("README.md", "# Root\n")
	ctx.Params = nil
	for _, nested := range []struct{ dir, marker string }{
		{"api", "build.gradle.kts"},
		{"legacy", "build.gradle"},
		{"services/pricing", "pom.xml"},
		{"tools/plugin", "settings.gradle"},
		{"tools/catalog", "settings.gradle.kts"},
	} {
		write(nested.dir+"/README.md", "[gone](missing.md)\n")
		write(nested.dir+"/"+nested.marker, "")
		ctx.Project.FullPath = root
		if status, _, _ := job(ctx, jsonl.New(), nil); status != "OK" {
			t.Errorf("the root read the document of %s, which holds %s: status %s", nested.dir, nested.marker, status)
		}
		ctx.Project.FullPath = filepath.Join(root, filepath.FromSlash(nested.dir))
		if status, _, _ := job(ctx, jsonl.New(), nil); status != "FAILED" {
			t.Errorf("the broken link of %s: status %s", nested.dir, status)
		}
	}
}
