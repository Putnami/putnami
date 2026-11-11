package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const eslintFeature = "typescript/typescript-project-toolchain"

// The nearest node_modules/.bin/eslint wins, from the project up to the
// workspace, and a JavaScript launcher starts with the node ResolveNode found,
// never with Bun.
func TestResolveESLint_NearestBinStartsWithTheNodeOnPath(t *testing.T) {
	spectest.Proves(t, eslintFeature, "lint-runs-eslint", "eslint-runs-from-node-modules-bin-with-the-node-on-path")
	root := t.TempDir()
	project := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	workspaceESLint := writeShim(t, root, "eslint")
	if err := os.WriteFile(workspaceESLint, []byte("#!/usr/bin/env node\nrequire('eslint');\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unix := platform{goos: "linux", goarch: "amd64"}

	got, err := resolveESLintOn(unix, project, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != workspaceESLint {
		t.Errorf("resolveESLintOn() = %q, want the workspace's %q", got, workspaceESLint)
	}

	projectESLint := writeShim(t, project, "eslint")
	if got, _ := resolveESLintOn(unix, project, root); got != projectESLint {
		t.Errorf("resolveESLintOn() = %q, want the project's own %q", got, projectESLint)
	}

	program, args, err := ESLintCommand(workspaceESLint, "/opt/node/bin/node", []string{"--format", "json", "."})
	if err != nil {
		t.Fatal(err)
	}
	if program != "/opt/node/bin/node" || !slices.Equal(args, []string{workspaceESLint, "--format", "json", "."}) {
		t.Errorf("ESLintCommand() = %q %q, want node starting the launcher", program, args)
	}

	// pnpm writes a shell script that finds node on PATH itself.
	shell := filepath.Join(t.TempDir(), "eslint")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec node \"$basedir/../eslint/bin/eslint.js\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	program, args, err = ESLintCommand(shell, "/opt/node/bin/node", []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if program != shell || !slices.Equal(args, []string{"."}) {
		t.Errorf("ESLintCommand() = %q %q, want the shell script started as itself", program, args)
	}
}

// Without eslint in node_modules/.bin, or without node on PATH, the error
// names what is missing. An eslint on PATH is never used.
func TestResolveESLint_MissingESLintOrNodeIsNamed(t *testing.T) {
	spectest.Proves(t, eslintFeature, "lint-runs-eslint", "a-missing-eslint-or-node-is-named")
	root := t.TempDir()
	pathDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathDir, "eslint"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	if _, err := ResolveESLint(root, root); !errors.Is(err, ErrESLintNotFound) {
		t.Errorf("ResolveESLint() error = %v, want ErrESLintNotFound", err)
	}

	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := ResolveNode(); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("ResolveNode() error = %v, want ErrNodeNotFound", err)
	}
	if _, _, err := ESLintCommand("eslint", "", nil); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("ESLintCommand() without node error = %v, want ErrNodeNotFound", err)
	}

	lookPath = func(string) (string, error) { return "/usr/bin/node", nil }
	if got, err := ResolveNode(); err != nil || got != "/usr/bin/node" {
		t.Errorf("ResolveNode() = %q, %v, want the node on PATH", got, err)
	}
}
