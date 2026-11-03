package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
)

func writeProjectConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The committed capability manifest is opt-in. `options.generate.schema`
// defaults to true and would start tracking a new file in every TypeScript
// application; a manifest that `architecture validate` reads as DARC evidence is
// a reviewed artifact, so the project asks for it.
func TestProjectCommitsCapabilityManifestIsOptIn(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   bool
	}{
		{"no config file", "", false},
		{"no options", `{"name": "app"}`, false},
		{"no generate block", `{"options": {"test": {}}}`, false},
		{"generate without capabilities", `{"options": {"generate": {"schema": true}}}`, false},
		{"capabilities false", `{"options": {"generate": {"capabilities": false}}}`, false},
		{"capabilities true", `{"options": {"generate": {"capabilities": true}}}`, true},
		{"malformed generate block", `{"options": {"generate": "nonsense"}}`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			projectPath := t.TempDir()
			if testCase.config != "" {
				writeProjectConfig(t, projectPath, testCase.config)
			}
			got, err := projectCommitsCapabilityManifest(projectPath)
			if err != nil {
				t.Fatalf("projectCommitsCapabilityManifest: %v", err)
			}
			if got != testCase.want {
				t.Errorf("projectCommitsCapabilityManifest = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A config that cannot be parsed is an error, not a "no": the answer decides
// whether the build removes a tracked manifest.
func TestProjectCommitsCapabilityManifestRefusesAnUnreadableConfig(t *testing.T) {
	projectPath := t.TempDir()
	writeProjectConfig(t, projectPath, `{"options": `)
	if _, err := projectCommitsCapabilityManifest(projectPath); err == nil {
		t.Fatal("projectCommitsCapabilityManifest accepted a truncated putnami.json")
	}
}

// A batched generate hands every member a copy of the leader's context, with
// the leader's options (perProjectContext). The opt-in is read from each
// member's own config, so a follower that opts in keeps its manifest when the
// leader does not, and the reverse.
func TestCapabilityManifestOptInIsReadPerBatchMember(t *testing.T) {
	leader := t.TempDir()
	follower := t.TempDir()
	writeProjectConfig(t, leader, `{"name": "leader"}`)
	writeProjectConfig(t, follower, `{"name": "follower", "options": {"generate": {"capabilities": true}}}`)

	ctx := &pctx.Context{Project: pctx.Project{Name: "leader", Options: map[string]json.RawMessage{}}}
	member := perProjectContext(ctx, pctx.ProjectRef{Name: "follower", FullPath: follower})
	if _, ok := member.Project.Options["generate"]; ok {
		t.Fatal("the member context carries an options block; this test pins the leader-options case")
	}

	for _, testCase := range []struct {
		path string
		want bool
	}{{leader, false}, {follower, true}} {
		got, err := projectCommitsCapabilityManifest(testCase.path)
		if err != nil {
			t.Fatalf("projectCommitsCapabilityManifest(%s): %v", testCase.path, err)
		}
		if got != testCase.want {
			t.Errorf("projectCommitsCapabilityManifest(%s) = %v, want %v", testCase.path, got, testCase.want)
		}
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

// No cache key covers the bytes at schema/capabilities.json, so a file there
// that this run did not produce would be captured under the key of the clean
// sources and restored into every checkout at that key. The task removes
// it unless the project opted in and the application emitted a manifest.
func TestCommitCapabilityManifestRemovesAFileThisRunDidNotProduce(t *testing.T) {
	cases := []struct {
		name    string
		optedIn bool
		emitted bool
		want    string
	}{
		{"not opted in, manifest emitted", false, true, ""},
		{"not opted in, no manifest emitted", false, false, ""},
		{"opted in, no manifest emitted", true, false, ""},
		{"opted in, manifest emitted", true, true, "{\"emitted\":true}\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			projectPath := t.TempDir()
			committed := filepath.Join(projectPath, "schema", "capabilities.json")
			if err := os.MkdirAll(filepath.Dir(committed), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(committed, []byte("{\"leftover\":true}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			emittedPath := ""
			if testCase.emitted {
				emittedPath = filepath.Join(projectPath, ".gen", "schema", "capabilities.json")
				if err := os.MkdirAll(filepath.Dir(emittedPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(emittedPath, []byte("{\"emitted\":true}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := commitCapabilityManifest(projectPath, emittedPath, testCase.optedIn); err != nil {
				t.Fatalf("commit: %v", err)
			}
			got, err := os.ReadFile(committed)
			if testCase.want == "" {
				if !os.IsNotExist(err) {
					t.Errorf("read committed manifest = %q, %v; want it to be absent", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read committed manifest: %v", err)
			}
			if string(got) != testCase.want {
				t.Errorf("committed bytes = %q, want %q", got, testCase.want)
			}
		})
	}
}

// A project with no file at the path and nothing to commit stays as it is.
func TestCommitCapabilityManifestWithNothingToRemove(t *testing.T) {
	projectPath := t.TempDir()
	if err := commitCapabilityManifest(projectPath, "", false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "schema")); !os.IsNotExist(err) {
		t.Errorf("stat schema = %v, want no directory created", err)
	}
}
