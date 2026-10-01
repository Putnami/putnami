package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
)

func TestRunLintBatchOneInvocationSplitsStatusesAndDiagnostics(t *testing.T) {
	mockBiomeResolution(t)
	ctx, root := makeLintBatchContext(t)

	var calls int
	var gotArgs []string
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		gotArgs = append([]string(nil), args...)
		return &exec.Result{
			Success:  false,
			ExitCode: 1,
			Stdout: `{
				"command":"check",
				"diagnostics":[
					{"category":"lint/a","severity":"error","message":"a failed","location":{"path":"packages/a/src/a.ts","start":{"line":2,"column":3}}},
					{"category":"lint/b","severity":"warning","message":"b warned","location":{"path":"packages/b/src/b.ts","start":{"line":4,"column":5}}}
				],
				"summary":{"errors":1,"warnings":1,"infos":0,"changed":0,"unchanged":0,"skipped":0}
			}`,
		}, nil
	})

	status, data, err := runLintBatch(ctx, lintBatchCombined)
	if err != nil {
		t.Fatalf("runLintBatch: %v", err)
	}
	if status != "OK" {
		t.Fatalf("aggregate status = %q, want protocol success", status)
	}
	if calls != 1 {
		t.Fatalf("Biome calls = %d, want one", calls)
	}
	for _, want := range []string{
		"packages/a",
		"packages/b",
		"--assist-enabled=true",
		"--enforce-assist=false",
		"--max-diagnostics=none",
	} {
		if !slices.Contains(gotArgs, want) {
			t.Fatalf("batch args %v missing %q", gotArgs, want)
		}
	}

	results, ok := data["batchResults"].([]lintBatchProjectResult)
	if !ok || len(results) != 2 {
		t.Fatalf("batchResults = %#v", data["batchResults"])
	}
	if results[0].ProjectID != "/packages/a" || results[0].Status != "FAILED" ||
		len(results[0].Diagnostics) != 1 || results[0].Diagnostics[0].File != "packages/a/src/a.ts" {
		t.Fatalf("project a result = %+v", results[0])
	}
	if results[1].ProjectID != "/packages/b" || results[1].Status != "OK" ||
		len(results[1].Diagnostics) != 1 || results[1].Summary.Warnings != 1 {
		t.Fatalf("project b result = %+v", results[1])
	}
	if root == "" {
		t.Fatal("fixture root unexpectedly empty")
	}
}

func TestRunLintFormatBatchWritesEverySelectedProject(t *testing.T) {
	mockBiomeResolution(t)
	ctx, root := makeLintBatchContext(t)
	aFile := filepath.Join(root, "packages", "a", "src", "a.ts")
	bFile := filepath.Join(root, "packages", "b", "src", "b.ts")
	for _, file := range []string{aFile, bFile} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var calls int
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		if !slices.Contains(args, "--write") || !slices.Contains(args, "packages/a") || !slices.Contains(args, "packages/b") {
			t.Fatalf("format batch args = %v", args)
		}
		for _, file := range []string{aFile, bFile} {
			if err := os.WriteFile(file, []byte("after"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &exec.Result{
			Success:  true,
			ExitCode: 0,
			Stdout:   `{"diagnostics":[],"summary":{"errors":0,"warnings":0,"infos":0,"changed":2,"unchanged":0,"skipped":0}}`,
		}, nil
	})

	status, data, err := runLintBatch(ctx, lintBatchFormat)
	if err != nil || status != "OK" {
		t.Fatalf("format batch status=%q err=%v", status, err)
	}
	if calls != 1 {
		t.Fatalf("format calls = %d, want one", calls)
	}
	results := data["batchResults"].([]lintBatchProjectResult)
	if len(results) != 2 || results[0].Status != "OK" || results[1].Status != "OK" {
		t.Fatalf("format split results = %+v", results)
	}
	for _, file := range []string{aFile, bFile} {
		if content, err := os.ReadFile(file); err != nil || string(content) != "after" {
			t.Fatalf("fix write %s = %q, %v", file, content, err)
		}
	}
}

func makeLintBatchContext(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	ctx, root := makeTestCtx(t)
	for _, path := range []string{"packages/a", "packages/b"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx.SelectedProjects = []pctx.ProjectRef{
		{ID: "/packages/a", Name: "a", Path: "packages/a", FullPath: filepath.Join(root, "packages/a")},
		{ID: "/packages/b", Name: "b", Path: "packages/b", FullPath: filepath.Join(root, "packages/b")},
	}
	ctx.Params = pctx.Params{
		"fix":             []byte("true"),
		"max-diagnostics": []byte("10"),
	}
	return ctx, root
}
