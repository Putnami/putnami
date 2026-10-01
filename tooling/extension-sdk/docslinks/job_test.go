package docslinks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// captureStdout runs fn with os.Stdout redirected to a file and returns what
// was written: the emitter binds to os.Stdout at emit time.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	defer func() {
		os.Stdout = original
		_ = file.Close()
	}()
	fn()
	_ = file.Sync()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func jobContext(root, project string, params pctx.Params) *pctx.Context {
	return &pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: project, Path: project, FullPath: filepath.Join(root, project)},
		Params:        params,
	}
}

func runJob(t *testing.T, ctx *pctx.Context) (string, map[string]any, string, error) {
	t.Helper()
	var status string
	var data map[string]any
	var err error
	out := captureStdout(t, func() {
		status, data, err = Job()(ctx, jsonl.NewForVersion(1), nil)
	})
	return status, data, out, err
}

func TestJobFailsOnABrokenLink(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/README.md", "# Lib\n\nSee [the guide](doc/guide.md) and [usage](#usage).\n")

	status, data, out, err := runJob(t, jobContext(root, "lib", nil))
	if err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	summary, _ := data["lintSummary"].(map[string]any)
	if summary["errors"] != 2 {
		t.Fatalf("lintSummary = %v, want 2 errors", data["lintSummary"])
	}
	for _, want := range []string{`"lib/README.md"`, `"docs-links"`, "lint-errors", "2 broken documentation links in 1 file"} {
		if !strings.Contains(out, want) {
			t.Errorf("event stream lacks %s:\n%s", want, out)
		}
	}
}

func TestJobPassesAProjectWithoutBrokenLinks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/README.md", "# Lib\n\nSee [the guide](doc/guide.md#set-up).\n")
	writeFile(t, root, "lib/doc/guide.md", "# Guide\n\n## Set up\n")

	status, _, out, err := runJob(t, jobContext(root, "lib", nil))
	if err != nil || status != "OK" {
		t.Fatalf("status = %q, err = %v:\n%s", status, err, out)
	}
	if strings.Contains(out, "lint-errors") {
		t.Errorf("a clean run reports lint errors:\n%s", out)
	}
}

func TestJobSkipsWhenTurnedOff(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/README.md", "[gone](gone.md)\n")

	status, _, _, err := runJob(t, jobContext(root, "lib", pctx.Params{Param: json.RawMessage("false")}))
	if err != nil || status != "SKIP" {
		t.Fatalf("status = %q, err = %v, want SKIP", status, err)
	}
}

func TestJobFailsWhenTheProjectCannotBeRead(t *testing.T) {
	status, _, _, err := runJob(t, jobContext(t.TempDir(), "absent", nil))
	if err == nil || status != "FAILED" {
		t.Fatalf("status = %q, err = %v, want FAILED with an error", status, err)
	}
}
