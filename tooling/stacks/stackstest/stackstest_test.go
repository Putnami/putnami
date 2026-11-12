package stackstest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/stacks"
)

const profile = `{
  "id": "rust",
  "markers": ["Cargo.toml"],
  "toolchain": {"command": ["rustc", "--version"], "pattern": "^rustc (?<version>\\S+)",
    "files": [{"path": "rust-toolchain.toml", "key": "toolchain.channel"}, {"path": "rust-toolchain"}]},
  "inputs": {"sources": ["src/**"], "manifests": ["Cargo.toml", "{root}/Cargo.toml"], "lockfiles": ["{root}/Cargo.lock"]},
  "tasks": {
    "lint": {"steps": [["cargo", "clippy"]], "inputs": ["sources", "manifests"]},
    "test": {"steps": [["cargo", "test"]], "inputs": ["sources", "manifests", "lockfiles"]}
  }
}`

// recorder is a testing.TB that records instead of failing.
type recorder struct {
	testing.TB
	errors []string
	fatal  bool
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	r.fatal = true
	panic(r)
}

func check(t *testing.T, path string) *recorder {
	t.Helper()
	return checkProfile(t, path, profile)
}

func checkProfile(t *testing.T, path, profile string) *recorder {
	t.Helper()
	p, err := stacks.Decode([]byte(profile))
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{TB: t}
	func() {
		defer func() {
			if v := recover(); v != nil && v != r {
				panic(v)
			}
		}()
		CheckManifest(r, path, stacks.Stack{Extension: "@putnami/sample", Profile: p})
	}()
	return r
}

func TestCheckManifestAcceptsAConformingManifest(t *testing.T) {
	if r := check(t, "testdata/manifest.json"); len(r.errors) > 0 {
		t.Fatalf("errors: %v", r.errors)
	}
}

func mutate(t *testing.T, edit func(m map[string]any)) string {
	t.Helper()
	data, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "putnami.extension.json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func obj(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

func TestCheckManifestReportsEachDrift(t *testing.T) {
	cases := map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"name": {func(m map[string]any) { m["name"] = "@putnami/other" }, "name ="},
		"markers": {func(m map[string]any) {
			obj(m, "workspace")["markers"] = []any{"Cargo.toml", "rust-toolchain"}
		}, "workspace.markers"},
		"adapter inputs": {func(m map[string]any) {
			obj(m, "workspace")["inputs"] = []any{"Cargo.toml"}
		}, "toolchain file rust-toolchain"},
		"selectors": {func(m map[string]any) { obj(m, "runtime")["inputSelectors"] = []any{"rust:lint"} }, "inputSelectors"},
		"upstream": {func(m map[string]any) {
			obj(m, "commands", "lint")["run"].([]any)[0].(map[string]any)["dependsOn"] = []any{}
		}, "depends on no ^lint"},
		"args": {func(m map[string]any) { obj(m, "tasks", "lint-run")["args"] = []any{"lint"} }, "runs {extensionRuntime}"},
		"root files": {func(m map[string]any) {
			obj(m, "tasks", "lint-run", "inputs", "stackRoot")["files"] = []any{"Cargo.toml"}
		}, "stack-root files"},
		"selector input": {func(m map[string]any) {
			obj(m, "tasks", "test-run", "inputs", "sources")["files"] = []any{"src/**"}
		}, "names rust:test"},
		"reports": {func(m map[string]any) {
			obj(m, "tasks", "test-run", "declares", "outputs", "reports")["path"] = "out"
		}, "reports"},
		"cache": {func(m map[string]any) {
			obj(m, "tasks", "test-run", "cache")["enabled"] = false
		}, "cache.enabled = false, want true"},
		"undeclared command": {func(m map[string]any) {
			obj(m, "commands")["build"] = map[string]any{"description": "Build.", "run": []any{map[string]any{"id": "build", "task": "lint-run"}}}
		}, "command build runs a task the profile does not declare"},
	}
	for name, c := range cases {
		r := check(t, mutate(t, c.edit))
		if !strings.Contains(strings.Join(r.errors, "\n"), c.want) {
			t.Errorf("%s: errors %v, want one with %q", name, r.errors, c.want)
		}
	}
}

func TestCheckManifestStopsOnAnUnreadableManifest(t *testing.T) {
	if r := check(t, filepath.Join(t.TempDir(), "missing.json")); !r.fatal {
		t.Errorf("a missing manifest is fatal: %v", r.errors)
	}
	path := filepath.Join(t.TempDir(), "putnami.extension.json")
	if err := os.WriteFile(path, []byte(`{"name": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := check(t, path); !r.fatal {
		t.Errorf("an invalid manifest is fatal: %v", r.errors)
	}
}

func TestCheckManifestHoldsADelegatedStackUncached(t *testing.T) {
	delegated := strings.Replace(profile, `"id": "rust",`, `"id": "rust", "delegated": true,`, 1)
	delegate := func(edit func(m map[string]any)) string {
		return mutate(t, func(m map[string]any) {
			for _, task := range []string{"lint-run", "test-run"} {
				obj(m, "tasks", task, "cache")["enabled"] = false
			}
			obj(m, "tasks")["graph-run"] = map[string]any{"kind": "command", "command": "{extensionRuntime}", "args": []any{"graph"}, "cwd": "{projectRoot}", "cache": map[string]any{"enabled": false}}
			for _, command := range []string{"lint", "test"} {
				run := obj(m, "commands", command)["run"].([]any)
				run[0].(map[string]any)["dependsOn"] = []any{"graph"}
				obj(m, "commands", command)["run"] = append([]any{map[string]any{"id": "graph", "task": "graph-run", "dependsOn": []any{"^graph"}}}, run...)
			}
			if edit != nil {
				edit(m)
			}
		})
	}
	if r := checkProfile(t, delegate(nil), delegated); len(r.errors) > 0 {
		t.Errorf("a delegated stack with its graph steps and its cache off conforms: %v", r.errors)
	}
	for name, c := range map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"a tool step with an upstream need": {func(m map[string]any) {
			obj(m, "commands", "test")["run"].([]any)[1].(map[string]any)["dependsOn"] = []any{"graph", "^test"}
		}, "want its own graph step alone"},
		"a graph step with no upstream need": {func(m map[string]any) {
			obj(m, "commands", "lint")["run"].([]any)[0].(map[string]any)["dependsOn"] = []any{}
		}, "want graph depending on ^graph"},
		"a cached tool task": {func(m map[string]any) {
			obj(m, "tasks", "test-run", "cache")["enabled"] = true
		}, "cache.enabled = true, want false"},
		"a cached graph task": {func(m map[string]any) {
			obj(m, "tasks", "graph-run", "cache")["enabled"] = true
		}, "uncached"},
		"a single step": {func(m map[string]any) {
			obj(m, "commands", "lint")["run"] = obj(m, "commands", "lint")["run"].([]any)[1:]
		}, "want the graph step then the lint step"},
	} {
		if r := checkProfile(t, delegate(c.edit), delegated); !strings.Contains(strings.Join(r.errors, "\n"), c.want) {
			t.Errorf("%s: errors %v, want one with %q", name, r.errors, c.want)
		}
	}
}

func TestCheckManifestHoldsEveryVariant(t *testing.T) {
	variants := strings.Replace(profile, `"tasks": {`, `"variants": {"alt": {
    "toolchain": {"files": [{"path": "alt-toolchain"}]},
    "inputs": {"lockfiles": ["{root}/Cargo.lock", "{root}/alt.lock"]},
    "tasks": {"lint": {"steps": [["alt", "{target}"]], "targets": [{"name": "x", "hosts": ["linux"]}]}}
  }},
  "tasks": {`, 1)
	r := checkProfile(t, "testdata/manifest.json", variants)
	got := strings.Join(r.errors, "\n")
	for _, want := range []string{"toolchain file alt-toolchain", "alt.lock", "lint-run builds host-dependent targets"} {
		if !strings.Contains(got, want) {
			t.Errorf("errors %v, want one with %q", r.errors, want)
		}
	}
}

func TestWriteFileCreatesTheDirectories(t *testing.T) {
	root := t.TempDir()
	WriteFile(t, root, "a/b/c.txt", "hello")
	got, err := os.ReadFile(filepath.Join(root, "a", "b", "c.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("%q %v", got, err)
	}
}

func TestChangedListsEachFileAddedRemovedOrRewritten(t *testing.T) {
	root := t.TempDir()
	WriteFile(t, root, "kept.txt", "same")
	WriteFile(t, root, "rewritten.txt", "old")
	WriteFile(t, root, "removed.txt", "gone")
	before := Snapshot(t, root)
	if got := Changed(t, root, before); len(got) != 0 {
		t.Fatalf("changed = %v before any write", got)
	}
	WriteFile(t, root, "rewritten.txt", "new")
	WriteFile(t, root, "sub/added.txt", "added")
	if err := os.Remove(filepath.Join(root, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(Changed(t, root, before), ","), "removed.txt,rewritten.txt,sub/added.txt"; got != want {
		t.Errorf("changed = %s, want %s", got, want)
	}
}
