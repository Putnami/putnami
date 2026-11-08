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
	if r := checkProfile(t, "testdata/manifest.json", delegated); !strings.Contains(strings.Join(r.errors, "\n"), "cache.enabled = true, want false") {
		t.Errorf("a cached task of a delegated stack is a drift: %v", r.errors)
	}
	uncached := mutate(t, func(m map[string]any) {
		for _, task := range []string{"lint-run", "test-run"} {
			obj(m, "tasks", task, "cache")["enabled"] = false
		}
	})
	if r := checkProfile(t, uncached, delegated); len(r.errors) > 0 {
		t.Errorf("a delegated stack with its cache off conforms: %v", r.errors)
	}
}
