package stacks

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const sampleProfile = `{
  "id": "rust",
  "markers": ["Cargo.toml"],
  "toolchain": {
    "command": ["rustc", "--version"],
    "pattern": "^rustc (?<version>\\S+)",
    "files": [{"path": "rust-toolchain.toml", "key": "toolchain.channel"}, {"path": "rust-toolchain"}]
  },
  "inputs": {
    "sources": ["src/**"],
    "manifests": ["Cargo.toml", "{root}/Cargo.toml"],
    "lockfiles": ["{root}/Cargo.lock"]
  },
  "tasks": {
    "lint": {"steps": [["cargo", "clippy", "--package", "{component}"]], "inputs": ["sources", "manifests"]},
    "test": {
      "steps": [["cargo", "test", "--package", "{component}"]],
      "inputs": ["sources", "manifests", "lockfiles"],
      "reports": {"junit": ["{root}/target/junit.xml"], "coverage": [{"format": "lcov", "paths": ["{output}/lcov.info"]}]}
    }
  }
}`

func mustProfile(t *testing.T) Profile {
	t.Helper()
	p, err := Decode([]byte(sampleProfile))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return p
}

func TestDecodeReadsAValidProfile(t *testing.T) {
	p := mustProfile(t)
	if p.ID != "rust" || len(p.Tasks) != 2 || p.Tasks[TaskTest].Reports.Coverage[0].Format != "lcov" {
		t.Fatalf("unexpected profile: %+v", p)
	}
}

func TestDecodeRefusesWhatTheSchemaDoesNotName(t *testing.T) {
	cases := map[string]string{
		"unknown member":    strings.Replace(sampleProfile, `"id": "rust",`, `"id": "rust", "colour": 1,`, 1),
		"trailing data":     sampleProfile + `{}`,
		"bad id":            strings.Replace(sampleProfile, `"id": "rust"`, `"id": "Rust"`, 1),
		"no version group":  strings.Replace(sampleProfile, `(?<version>\\S+)`, `(\\S+)`, 1),
		"unknown task":      strings.Replace(sampleProfile, `"lint": {`, `"fmt": {`, 1),
		"unknown input set": strings.Replace(sampleProfile, `"inputs": ["sources", "manifests"]}`, `"inputs": ["docs"]}`, 1),
		"bad placeholder":   strings.Replace(sampleProfile, `"{component}"]], "inputs": ["sources", "manifests"]`, `"{crate}"]], "inputs": ["sources", "manifests"]`, 1),
		"escaping pattern":  strings.Replace(sampleProfile, `"src/**"`, `"../src/**"`, 1),
		"coverage format":   strings.Replace(sampleProfile, `"lcov"`, `"gcov"`, 1),
		"bad check":         strings.Replace(sampleProfile, `"command": ["rustc"`, `"check": "ignore", "command": ["rustc"`, 1),
	}
	for name, doc := range cases {
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("%s: Decode accepted %s", name, doc)
		}
	}
}

func TestResolveReplacesValuesWholeAndMergesObjects(t *testing.T) {
	p := mustProfile(t)
	got, err := p.Resolve(json.RawMessage(`{
	  "tasks": {"test": {"steps": [["cargo", "nextest", "run", "--package", "{component}"]]}},
	  "inputs": {"sources": ["src/**", "fixtures/**"]},
	  "toolchain": {"check": "warn"}
	}`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := [][]string{{"cargo", "nextest", "run", "--package", "{component}"}}; !reflect.DeepEqual(got.Tasks[TaskTest].Steps, want) {
		t.Errorf("steps = %v, want %v", got.Tasks[TaskTest].Steps, want)
	}
	if got.Tasks[TaskTest].Reports == nil || len(got.Tasks[TaskTest].Reports.JUnit) != 1 {
		t.Errorf("the reports a test override does not name are kept: %+v", got.Tasks[TaskTest].Reports)
	}
	if want := []string{"src/**", "fixtures/**"}; !reflect.DeepEqual(got.Inputs["sources"], want) {
		t.Errorf("sources = %v, want %v", got.Inputs["sources"], want)
	}
	if got.Toolchain.Check != CheckWarn || got.Toolchain.Command[0] != "rustc" {
		t.Errorf("toolchain = %+v", got.Toolchain)
	}
	if !reflect.DeepEqual(got.Tasks[TaskLint], p.Tasks[TaskLint]) {
		t.Errorf("lint changed: %+v", got.Tasks[TaskLint])
	}
	if p.Inputs["sources"][0] != "src/**" || len(p.Inputs["sources"]) != 1 {
		t.Errorf("Resolve changed the shipped profile: %v", p.Inputs["sources"])
	}
}

func TestResolveRefusesAnInvalidOverride(t *testing.T) {
	p := mustProfile(t)
	for _, over := range []string{
		`{"id": "go"}`,
		`{"markers": ["go.mod"]}`,
		`{"tasks": {"test": {"stepz": []}}}`,
		`{"tasks": {"test": {"inputs": ["docs"]}}}`,
		`[1]`,
	} {
		if _, err := p.Resolve(json.RawMessage(over)); err == nil {
			t.Errorf("Resolve accepted %s", over)
		}
	}
	for _, empty := range []string{``, `null`, ` `} {
		got, err := p.Resolve(json.RawMessage(empty))
		if err != nil || !reflect.DeepEqual(got, p) {
			t.Errorf("Resolve(%q) = %v, %v", empty, got, err)
		}
	}
}

func TestTaskPatternsAnchorTheSetsAndTheToolchainFiles(t *testing.T) {
	p := mustProfile(t)
	got := p.TaskPatterns(TaskLint, "crates/api", ".")
	want := []string{
		"crates/api/src/**", "crates/api/Cargo.toml", "Cargo.toml",
		"crates/api/rust-toolchain.toml", "crates/api/rust-toolchain",
		"crates/rust-toolchain.toml", "crates/rust-toolchain",
		"rust-toolchain.toml", "rust-toolchain",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TaskPatterns(lint) =\n%v\nwant\n%v", got, want)
	}
	if test := p.TaskPatterns(TaskTest, "crates/api", "."); !strings.Contains(strings.Join(test, " "), " Cargo.lock ") {
		t.Errorf("test reads the lockfile: %v", test)
	}
	if got := p.TaskPatterns(TaskBuild, "crates/api", "."); got != nil {
		t.Errorf("an undeclared task reads nothing: %v", got)
	}
}

func TestMatchPattern(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"src/**", "src/lib.rs", true},
		{"src/**", "src/a/b/c.rs", true},
		{"src/**", "tests/a.rs", false},
		{"**/*.xml", "target/nextest/ci/junit.xml", true},
		{"target/*.xml", "target/a/junit.xml", false},
		{"Cargo.toml", "Cargo.toml", true},
	} {
		if got := MatchPattern(c.pattern, c.name); got != c.want {
			t.Errorf("MatchPattern(%q, %q) = %v", c.pattern, c.name, got)
		}
	}
}
