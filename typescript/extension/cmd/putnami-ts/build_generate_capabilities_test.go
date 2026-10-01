package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
)

func generateOptions(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	return map[string]json.RawMessage{"generate": json.RawMessage(body)}
}

// The committed capability manifest is opt-in. `options.generate.schema`
// defaults to true and would start tracking a new file in every TypeScript
// application; a manifest that `architecture validate` reads as DARC evidence is
// a reviewed artifact, so the project asks for it.
func TestProjectCommitsCapabilityManifestIsOptIn(t *testing.T) {
	cases := []struct {
		name string
		ctx  *pctx.Context
		want bool
	}{
		{"nil context", nil, false},
		{"no options", &pctx.Context{}, false},
		{"no generate block", &pctx.Context{Project: pctx.Project{Options: map[string]json.RawMessage{"test": json.RawMessage(`{}`)}}}, false},
		{"generate without capabilities", &pctx.Context{Project: pctx.Project{Options: generateOptions(t, `{"schema": true}`)}}, false},
		{"capabilities false", &pctx.Context{Project: pctx.Project{Options: generateOptions(t, `{"capabilities": false}`)}}, false},
		{"capabilities true", &pctx.Context{Project: pctx.Project{Options: generateOptions(t, `{"capabilities": true}`)}}, true},
		{"malformed generate block", &pctx.Context{Project: pctx.Project{Options: generateOptions(t, `"nonsense"`)}}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := projectCommitsCapabilityManifest(testCase.ctx); got != testCase.want {
				t.Errorf("projectCommitsCapabilityManifest = %v, want %v", got, testCase.want)
			}
		})
	}
}

// Promotion copies; it never composes. The framework producer is the manifest's
// sole author, and this task only makes what it wrote readable without a build.
func TestPromoteCapabilityManifestCopiesTheEmittedBytes(t *testing.T) {
	projectPath := t.TempDir()
	emittedPath := filepath.Join(projectPath, ".gen", "schema", "capabilities.json")
	if err := os.MkdirAll(filepath.Dir(emittedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	emitted := []byte("{\n  \"protocolVersion\": 2\n}\n")
	if err := os.WriteFile(emittedPath, emitted, 0o644); err != nil {
		t.Fatal(err)
	}

	committed, err := promoteCapabilityManifest(projectPath, emittedPath)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if committed != filepath.Join(projectPath, "schema", "capabilities.json") {
		t.Errorf("committed path = %q", committed)
	}
	got, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("read the committed manifest: %v", err)
	}
	if string(got) != string(emitted) {
		t.Errorf("committed bytes = %q, want %q", got, emitted)
	}
}

// A warm build must not touch the file. The manifest is tracked, so a rewrite
// with identical bytes is still a worktree event for anything watching mtimes.
func TestPromoteCapabilityManifestLeavesAnIdenticalFileAlone(t *testing.T) {
	projectPath := t.TempDir()
	emittedPath := filepath.Join(projectPath, ".gen", "schema", "capabilities.json")
	if err := os.MkdirAll(filepath.Dir(emittedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emittedPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := promoteCapabilityManifest(projectPath, emittedPath)
	if err != nil {
		t.Fatalf("first promote: %v", err)
	}
	before, err := os.Stat(committed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteCapabilityManifest(projectPath, emittedPath); err != nil {
		t.Fatalf("second promote: %v", err)
	}
	after, err := os.Stat(committed)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged manifest was rewritten; a warm build must leave the tracked file alone")
	}
}

// A project that emits nothing gets nothing. Creating an empty tracked manifest
// would be an evidence artifact with no producer behind it.
func TestPromoteCapabilityManifestWritesNothingWithoutAnEmittedManifest(t *testing.T) {
	projectPath := t.TempDir()
	for _, emitted := range []string{"", filepath.Join(projectPath, ".gen", "schema", "capabilities.json")} {
		committed, err := promoteCapabilityManifest(projectPath, emitted)
		if err != nil {
			t.Fatalf("promote(%q): %v", emitted, err)
		}
		if committed != "" {
			t.Errorf("promote(%q) = %q, want no committed path", emitted, committed)
		}
	}
	if _, err := os.Stat(filepath.Join(projectPath, "schema", "capabilities.json")); !os.IsNotExist(err) {
		t.Errorf("stat committed manifest = %v, want it to be absent", err)
	}
}
