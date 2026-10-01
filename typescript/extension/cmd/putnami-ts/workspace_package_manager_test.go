package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// bunVersionExec answers `bun --version` with version and every other call
// with success, recording the argument lists it saw.
func bunVersionExec(version string, calls *[][]string) func(string, []string, ...exec.Option) (*exec.Result, error) {
	return func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if len(args) == 1 && args[0] == "--version" {
			return &exec.Result{Success: true, ExitCode: 0, Stdout: version}, nil
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	}
}

func sawBunVersion(calls [][]string) bool {
	for _, args := range calls {
		if len(args) == 1 && args[0] == "--version" {
			return true
		}
	}
	return false
}

func TestWorkspaceInstallDeclaresTheBunItInstallsWith(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-bun-pin", "a-fresh-workspace-declares-the-bun-it-installs-with")
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{\"name\":\"fresh\",\"workspaces\":[\"app\"],\"private\":true}\n"), 0644)
	writeExtensionBiomeDefault(t, dir)
	var calls [][]string
	mockAllExec(t, bunVersionExec("1.3.14\n", &calls))

	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"name\": \"fresh\",\n  \"workspaces\": [\n    \"app\"\n  ],\n  \"private\": true,\n  \"packageManager\": \"bun@1.3.14\"\n}\n"
	if string(got) != want {
		t.Fatalf("package.json =\n%s\nwant\n%s", got, want)
	}
	if last := calls[len(calls)-1]; len(last) == 0 || last[0] != "install" {
		t.Fatalf("the declaration must precede bun install; calls = %v", calls)
	}
}

func TestWorkspaceInstallKeepsAnExistingPackageManager(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-bun-pin", "an-existing-package-manager-is-kept")
	ctx, dir := makeTestCtx(t)
	original := []byte(`{"packageManager":"bun@1.2.0","private":true}`)
	os.WriteFile(filepath.Join(dir, "package.json"), original, 0644)
	var calls [][]string
	mockAllExec(t, bunVersionExec("1.3.14", &calls))

	if err := ensureWorkspacePackageManager(ctx, jsonl.New(), "/mock/bun"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if string(got) != string(original) {
		t.Fatalf("package.json changed to %s", got)
	}
	if sawBunVersion(calls) {
		t.Fatal("bun was probed although the workspace already declares its package manager")
	}
}

func TestWorkspaceInstallDeclaresNoPrereleaseBun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-bun-pin", "a-bun-that-is-not-a-release-is-not-declared")
	for _, version := range []string{"1.3.0-canary.12+abc", "1.3", "", "bun 1.3.14", "1..4"} {
		t.Run(version, func(t *testing.T) {
			ctx, dir := makeTestCtx(t)
			original := []byte(`{"private":true}`)
			os.WriteFile(filepath.Join(dir, "package.json"), original, 0644)
			var calls [][]string
			mockAllExec(t, bunVersionExec(version, &calls))

			if err := ensureWorkspacePackageManager(ctx, jsonl.New(), "/mock/bun"); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "package.json"))
			if string(got) != string(original) {
				t.Fatalf("package.json changed to %s", got)
			}
		})
	}
}

func TestWorkspaceInstallDeclaresNothingWithoutARootManifest(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	var calls [][]string
	mockAllExec(t, bunVersionExec("1.3.14", &calls))

	if err := ensureWorkspacePackageManager(ctx, jsonl.New(), "/mock/bun"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); !os.IsNotExist(err) {
		t.Fatalf("package.json was created: %v", err)
	}
	if sawBunVersion(calls) {
		t.Fatal("bun was probed for a workspace with no root manifest")
	}
}

func TestWorkspaceInstallReportsAFailedBunProbe(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	original := []byte(`{"private":true}`)
	os.WriteFile(filepath.Join(dir, "package.json"), original, 0644)

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 3, Stderr: "boom"}, nil
	})
	if err := ensureWorkspacePackageManager(ctx, jsonl.New(), "/mock/bun"); err == nil {
		t.Fatal("a failed bun probe must be reported")
	}
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return nil, errors.New("exec failed")
	})
	if err := ensureWorkspacePackageManager(ctx, jsonl.New(), "/mock/bun"); err == nil {
		t.Fatal("a bun that cannot start must be reported")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if string(got) != string(original) {
		t.Fatalf("package.json changed to %s", got)
	}
}
