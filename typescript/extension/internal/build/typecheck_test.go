package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
)

func writeProjectFiles(t *testing.T, dir string, files ...string) {
	t.Helper()
	for _, name := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("export {};\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunTypeCheck_SkipsAProjectWithNothingToCheck(t *testing.T) {
	dir := t.TempDir()
	writeProjectFiles(t, dir, "tsconfig.json", "src/types.d.ts", "dist/index.ts", "node_modules/pkg/index.ts", "clients/ts/putnami.json", "clients/ts/src/index.ts")

	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		t.Error("tsc ran on a project with no file of its own to check")
		return &exec.Result{Success: true}, nil
	})

	result, err := RunTypeCheck("bun", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("a project with nothing to check failed")
	}
}

func TestRunTypeCheck_ChecksEveryProjectFileWithoutEmitting(t *testing.T) {
	dir := t.TempDir()
	writeProjectFiles(t, dir, "tsconfig.json", "src/index.ts", "test/index.test.ts", "clients/ts/putnami.json", "clients/ts/src/index.ts")
	scratch := t.TempDir()

	var args []string
	var config map[string]any
	withMockExec(t, func(_ string, a []string, _ ...exec.Option) (*exec.Result, error) {
		args = a
		data, err := os.ReadFile(a[len(a)-1])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		return &exec.Result{Success: false, Stdout: "test/index.test.ts(1,1): error TS2578: Unused '@ts-expect-error' directive.\n"}, nil
	})

	result, err := RunTypeCheck("bun", dir, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "TS2578" {
		t.Errorf("result %+v, want the failure and its one diagnostic", result)
	}
	for _, want := range []string{"--noEmit", "--strict", "--incremental"} {
		if !slices.Contains(args, want) {
			t.Errorf("tsc args %v lack %s", args, want)
		}
	}
	for _, unwanted := range []string{"--outDir", "--declaration", "--emitDeclarationOnly"} {
		if slices.Contains(args, unwanted) {
			t.Errorf("tsc args %v hold %s", args, unwanted)
		}
	}
	if got := args[slices.Index(args, "--tsBuildInfoFile")+1]; filepath.Dir(got) != scratch {
		t.Errorf("build info %s, want it under %s", got, scratch)
	}

	anchored := func(pattern string) string { return filepath.ToSlash(filepath.Join(dir, pattern)) }
	include, _ := config["include"].([]any)
	if !slices.Contains(include, any(anchored("**/*.ts"))) || !slices.Contains(include, any(anchored("**/*.tsx"))) {
		t.Errorf("include %v, want every TypeScript file of the project", include)
	}
	exclude, _ := config["exclude"].([]any)
	for _, want := range []string{"clients/ts", "node_modules", "**/node_modules", "dist", "**/dist"} {
		if !slices.Contains(exclude, any(anchored(want))) {
			t.Errorf("exclude %v lacks %s", exclude, anchored(want))
		}
	}
	if extends, _ := config["extends"].(string); extends != filepath.ToSlash(filepath.Join(dir, "tsconfig.json")) {
		t.Errorf("extends %q, want the project tsconfig", extends)
	}
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Errorf("the scratch tsconfig outlived the run: %v", entries)
	}
}

func TestRunTypeCheck_WithoutATsconfigExtendsNothing(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "project")
	writeProjectFiles(t, dir, "test/index.test.ts")

	var config map[string]any
	withMockExec(t, func(_ string, a []string, _ ...exec.Option) (*exec.Result, error) {
		data, err := os.ReadFile(a[len(a)-1])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		return &exec.Result{Success: true}, nil
	})

	result, err := RunTypeCheck("bun", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("the check failed")
	}
	if _, ok := config["extends"]; ok {
		t.Errorf("config %v extends a tsconfig that does not exist", config)
	}
}

func TestIsTypeCheckFile(t *testing.T) {
	for name, want := range map[string]bool{
		"a.ts": true, "a.tsx": true, "a.mts": true, "a.cts": true, "a.test.ts": true,
		"a.d.ts": false, "a.d.mts": false, "a.d.cts": false, "a.js": false, "a.json": false,
	} {
		if got := isTypeCheckFile(name); got != want {
			t.Errorf("isTypeCheckFile(%q) = %v, want %v", name, got, want)
		}
	}
}
