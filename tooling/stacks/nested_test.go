package stacks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	readers "go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// nestedDetector names a virtual nested root svc with two components, a root
// component app that depends on its member app/plugin, and a component whose
// root is no component.
func nestedDetector(_ string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
	all := map[string]Component{
		"svc":        {Path: "svc", SourceFile: "svc/Cargo.toml", Root: "svc", InputOnly: true},
		"svc/core":   {Path: "svc/core", Name: "core", Root: "svc"},
		"svc/http":   {Path: "svc/http", Name: "http", Root: "svc", Dependencies: []string{"svc/core"}},
		"app":        {Path: "app", Name: "app", Root: "app", Dependencies: []string{"app/plugin"}},
		"app/plugin": {Path: "app/plugin", Name: "plugin", Root: "app"},
		"lost":       {Path: "lost", Name: "lost", Root: "nowhere"},
	}
	var out []Component
	for _, p := range request.Paths {
		if c, ok := all[p]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestANestedStackRootIsAnInputOnlyProjectItsComponentsDependOn(t *testing.T) {
	root := t.TempDir()
	s := testStack(t)
	s.Detect = nestedDetector
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
		Paths: []string{"app", "app/plugin", "lost", "svc", "svc/core", "svc/http"}})
	if err != nil {
		t.Fatal(err)
	}
	deps := map[string][]string{}
	metas := map[string]Metadata{}
	for _, p := range result.Projects {
		deps[p.Path] = p.Dependencies
		var m Metadata
		if err := json.Unmarshal(p.Metadata, &m); err != nil {
			t.Fatal(err)
		}
		metas[p.Path] = m
	}
	want := map[string][]string{"app": {"app/plugin"}, "svc": nil, "svc/core": {"svc"}, "svc/http": {"svc", "svc/core"}}
	if !reflect.DeepEqual(deps, want) {
		t.Errorf("projects and edges = %v, want %v", deps, want)
	}
	if m := metas["svc"]; m.Component != "" || m.Root != "svc" {
		t.Errorf("the virtual root's metadata = %+v", m)
	}
	var left []string
	for _, d := range result.Diagnostics {
		if d.Code == CodeStackRootUnsupported {
			left = append(left, d.Field)
		}
	}
	if slices.Sort(left); !reflect.DeepEqual(left, []string{"app/plugin", "lost"}) {
		t.Errorf("left out = %v (%v)", left, result.Diagnostics)
	}

	writeFile(t, root, "svc/Cargo.toml", "[workspace]\n")
	writeFile(t, root, "svc/Cargo.lock", "")
	writeFile(t, root, "svc/src/lib.rs", "")
	writeFile(t, root, "svc/core/Cargo.toml", "")
	if got := resolveOne(t, s, root, "svc", "rust:lint"); !reflect.DeepEqual(got.Files, []string{"Cargo.toml"}) {
		t.Errorf("the root's lint reads its root files alone: %v", got.Files)
	}
	if got := resolveOne(t, s, root, "svc", "rust:test"); !reflect.DeepEqual(got.Files, []string{"Cargo.lock", "Cargo.toml"}) {
		t.Errorf("the root's test reads its lockfile: %v", got.Files)
	}
	if got := resolveOne(t, s, root, "svc/core", "rust:test"); !reflect.DeepEqual(got.Files, []string{"Cargo.toml"}) {
		t.Errorf("a member reads its own files; the edge carries the root's: %v", got.Files)
	}

	meta, _ := json.Marshal(Metadata{Root: "svc"})
	rec := &recorder{}
	status, data, err := s.run(context.Background(), runInput{task: TaskTest, workspaceRoot: root, projectAbs: filepath.Join(root, "svc"),
		metadata: meta, outputRoot: t.TempDir()}, rec)
	if status != statusOK || err != nil || data != nil || len(rec.diags) != 0 {
		t.Errorf("a root with no component runs no step: %s %v %v %v", status, err, data, rec.diags)
	}
}

func TestAttributionOwnsCasesAndFilesByComponent(t *testing.T) {
	// The temporary directory of macOS is itself reached through a symbolic link.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "crates/api/Cargo.toml", "")
	writeFile(t, root, "crates/api/src/lib.rs", "")
	writeFile(t, root, "crates/pricing/Cargo.toml", "")
	s := testStack(t)
	s.CaseComponent = func(classname, suite string) (string, bool) {
		name, _, _ := strings.Cut(classname, "::")
		return name, name != ""
	}
	s.ComponentKey = func(name string) string { return strings.ReplaceAll(name, "-", "_") }
	a := s.attribution(root, filepath.Join(root, "crates/api"), "api")
	for classname, want := range map[string]Owner{"api::http": OwnerTask, "pricing": OwnerOther, "": OwnerNone} {
		if got := a.Case(readers.TestCase{Classname: classname}); got != want {
			t.Errorf("case %q owner = %v, want %v", classname, got, want)
		}
	}
	for file, want := range map[string]Owner{
		filepath.Join(root, "crates/api/src/lib.rs"): OwnerTask,
		"src/lib.rs": OwnerTask,
		filepath.Join(root, "crates/pricing/src/lib.rs"): OwnerOther,
		filepath.Join(root, "docs/gen.rs"):               OwnerNone,
		"/opt/registry/itoa/lib.rs":                      OwnerNone,
	} {
		if got := a.File(filepath.ToSlash(file)); got != want {
			t.Errorf("file %s owner = %v, want %v", file, got, want)
		}
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip(err)
	}
	linked := s.attribution(link, filepath.Join(link, "crates/api"), "api")
	if got := linked.File(filepath.ToSlash(filepath.Join(root, "crates/api/src/lib.rs"))); got != OwnerTask {
		t.Errorf("a path under the resolved workspace root is attributed: %v", got)
	}
	if got := (Stack{}).attribution(root, root, "api").Case; got != nil {
		t.Errorf("a stack with no case resolver reads every case as the task's")
	}
}

func TestReadReportsLeavesOutOtherComponentsEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "junit.xml", `<testsuites><testsuite name="s"><testcase classname="api" name="a"/><testcase classname="pricing" name="p"/><testcase classname="" name="x"/></testsuite></testsuites>`)
	writeFile(t, dir, "lcov.info", "SF:mine.rs\nDA:1,1\nend_of_record\nSF:theirs.rs\nDA:1,1\nend_of_record\nSF:nowhere.rs\nDA:1,0\nend_of_record\n")
	attr := Attribution{
		Case: func(c readers.TestCase) Owner {
			return map[string]Owner{"api": OwnerTask, "pricing": OwnerOther, "": OwnerNone}[c.Classname]
		},
		File: func(p string) Owner {
			return map[string]Owner{"mine.rs": OwnerTask, "theirs.rs": OwnerOther, "nowhere.rs": OwnerNone}[p]
		},
	}
	s, d := ReadReports(&Reports{JUnit: []string{"junit.xml"}, Coverage: []Coverage{{Format: "lcov", Paths: []string{"lcov.info"}}}}, globUnder(dir), dir, attr)
	if s.Tests.Total != 2 || s.OtherCases != 1 || s.OtherFiles != 1 || s.Coverage == nil || s.Coverage.Total != 1 || s.Coverage.Covered != 1 {
		t.Errorf("summary = %+v coverage %+v", s, s.Coverage)
	}
	if got := codes(d); got != CodeTestUnattributed+","+CodeCoverageUnmapped {
		t.Errorf("diagnostics = %s", got)
	}
	names := make([]string, 12)
	for i := range names {
		names[i] = "n"
	}
	if got := named(names); !strings.HasSuffix(got, "and 2 more") {
		t.Errorf("named = %s", got)
	}
}

func TestArtifactsAreCopiedIntoTheOutput(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "target/debug/api", "bin")
	if err := os.Chmod(filepath.Join(root, "target/debug/api"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := copyArtifacts([]string{"{root}/target/debug/{component}", "target/debug/missing"}, "api", filepath.Join(root, "crates/api"), root, out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(out, "artifacts", "api"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("the artifact is copied with its mode: %v %v", info, err)
	}
	p := mustProfile(t)
	task := p.Tasks[TaskTest]
	task.Artifacts = []string{"../outside"}
	p.Tasks[TaskTest] = task
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "artifacts[0]") {
		t.Errorf("an artifact outside the project is refused: %v", err)
	}
}

func TestRootEdgesLeaveOutAMemberWhoseRootReachesIt(t *testing.T) {
	// Two nested roots whose packages each depend on the other's member:
	// a/ma -> a -> b/mb -> b -> a/ma once the root edges are added.
	deps, leftOut := rootEdges([]Component{
		{Path: "a", Name: "a", Root: "a", Dependencies: []string{"b/mb"}},
		{Path: "a/ma", Name: "ma", Root: "a"},
		{Path: "b", Name: "b", Root: "b", Dependencies: []string{"a/ma"}},
		{Path: "b/mb", Name: "mb", Root: "b"},
		{Path: "c/mc", Name: "mc", Root: "c"},
	})
	if len(leftOut) != 3 || leftOut["a/ma"] == "" || leftOut["b/mb"] == "" || !strings.Contains(leftOut["c/mc"], "no project") {
		t.Errorf("left out = %v", leftOut)
	}
	if !reflect.DeepEqual(deps, map[string][]string{"a": {"b/mb"}, "b": {"a/ma"}}) {
		t.Errorf("deps = %v", deps)
	}
}

func TestAStaleArtifactIsRemovedBeforeTheSteps(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	writeFile(t, root, "target/debug/api", "old")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testStack(t)
	s.Profile.Tasks[TaskBuild] = Task{Steps: [][]string{{"cargo", "build", "--package", "{component}"}}, Inputs: []string{"sources"}, Artifacts: []string{"{root}/target/debug/{component}"}}
	status, _, rec := runTask(t, s, root, TaskBuild)
	if status != statusOK {
		t.Fatalf("build: %s %v", status, rec.diags)
	}
	if _, err := os.Stat(filepath.Join(root, "target/debug/api")); !os.IsNotExist(err) {
		t.Errorf("the stale binary is removed before the steps: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/api/build/build/artifacts/api")); !os.IsNotExist(err) {
		t.Errorf("a binary the steps did not write is not copied: %v", err)
	}
}

func TestArtifactRemovalStaysOnLiteralPathsInsideItsBase(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	writeFile(t, root, "crates/api/pkg/sample.wasm", "kept")
	writeFile(t, root, "Cargo.lock", "test reads it")
	writeFile(t, filepath.Dir(root), "victim", "keep")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testStack(t)
	s.Profile.Tasks[TaskBuild] = Task{Steps: [][]string{{"cargo", "build"}}, Inputs: []string{"sources"},
		Artifacts: []string{"**/*.wasm", "{root}/{component}", "{root}/Cargo.lock"}}

	status, _, rec := runTask(t, s, root, TaskBuild)
	if status != statusOK {
		t.Fatalf("build: %s %v", status, rec.diags)
	}
	if _, err := os.Stat(filepath.Join(root, "crates/api/pkg/sample.wasm")); err != nil {
		t.Errorf("a wildcard artifact pattern removes nothing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Cargo.lock")); err != nil {
		t.Errorf("a file another task reads is kept: %v", err)
	}

	meta, _ := json.Marshal(Metadata{Component: "../victim", Root: "."})
	rec = &recorder{}
	status, _, _ = s.run(context.Background(), runInput{task: TaskBuild, workspaceRoot: root, projectAbs: filepath.Join(root, "crates/api"),
		metadata: meta, outputRoot: t.TempDir(), stdout: &strings.Builder{}}, rec)
	if status != statusOK || rec.codes() != "warning:"+CodeProjectConfigInvalid {
		t.Errorf("a component that is no path segment keeps no artifact: %s %v", status, rec.diags)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "victim")); err != nil {
		t.Errorf("no file outside the workspace is removed: %v", err)
	}
	if err := removeOutputs([]string{"{root}/../victim"}, s.Profile, filepath.Join(root, "crates/api"), root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "victim")); err != nil {
		t.Errorf("a match outside its base is kept: %v", err)
	}
}
