package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/typescript/extension/internal/emit"
)

// build-emit keys on what decides its runner, as test-run does: the runner
// parameter, and the project's package.json, which holds the test script.
func TestBuildEmitKeysOnTheRunnerOfTheTests(t *testing.T) {
	manifest := readTscManifest(t)
	for _, task := range []string{"build-emit", "test-run"} {
		if input, ok := manifest.Tasks[task].Inputs["runner"]; !ok || input.From != "params" {
			t.Errorf("%s does not key on the runner parameter: %+v", task, input)
		}
	}
	if sources := manifest.Tasks["build-emit"].Inputs["sources"]; sources.From != "project" || !slices.Contains(sources.Files, "git:**/*") {
		t.Errorf("build-emit does not key on the project's package.json: %+v", sources)
	}
}

// The emit step resolves the runner as test-run does, and only Bun narrows: a
// parameter that names no runner is not Bun.
func TestTheEmitRunnerIsTheParameterElseTheTestScriptElseBun(t *testing.T) {
	project := func(packageJSON string) string {
		dir := t.TempDir()
		if packageJSON != "" {
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for _, tc := range []struct {
		name, packageJSON, param, want string
	}{
		{"no script", "", "", emit.RunnerBun},
		{"a jest script", `{"scripts":{"test":"jest --ci"}}`, "", "jest"},
		{"a vitest script", `{"scripts":{"test":"vitest run"}}`, "", "vitest"},
		{"the parameter over the script", `{"scripts":{"test":"jest"}}`, "bun", emit.RunnerBun},
		{"a parameter over no script", "", "vitest", "vitest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := emitRunner(project(tc.packageJSON), tc.param); got != tc.want {
				t.Fatalf("emitRunner = %q, want %q", got, tc.want)
			}
		})
	}
	if got := emitRunner(project(""), "mocha"); got == emit.RunnerBun {
		t.Fatalf("an unknown runner parameter resolved to %q", got)
	}
}
