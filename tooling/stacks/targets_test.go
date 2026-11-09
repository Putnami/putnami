package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	job "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// targetsProfile is a two-variant stack: the "base" variant runs the
// profile's "tool", the "alt" variant runs "alttool", checks another version
// file and reads one more stack-root file.
const targetsProfile = `{
  "id": "demo",
  "markers": ["demo.yaml"],
  "toolchain": {
    "command": ["tool", "--version"],
    "pattern": "tool (?<version>\\S+)",
    "files": [{"path": "demo.yaml", "key": "environment.tool"}]
  },
  "inputs": {
    "code": ["lib/**", "bin/**"],
    "tests": ["test/**"],
    "manifests": ["demo.yaml", "{root}/demo.yaml"],
    "alt": ["{root}/alt.yaml"],
    "app": ["main.src", "web/**", "native/**", "!web/generated/**"]
  },
  "tasks": {
    "lint": {
      "steps": [["tool", "check", "{target}"]],
      "inputs": ["code", "manifests"],
      "targets": [{"name": "lib", "when": ["lib"]}, {"name": "bin", "when": ["bin"]}]
    },
    "build": {
      "steps": [["tool", "prepare"], ["tool", "compile", "bin/{target}.src", "-o", "{output}/{target}"]],
      "inputs": ["code", "manifests"],
      "targets": [{"each": "bin/*.src"}]
    }
  },
  "variants": {
    "base": {},
    "alt": {
      "toolchain": {
        "command": ["alttool", "--version"],
        "files": [{"path": "demo.yaml", "key": "environment.alttool"}]
      },
      "tasks": {
        "build": {
          "steps": [["alttool", "build", "{target}"]],
          "inputs": ["code", "manifests", "alt", "app"],
          "targets": [
            {"name": "web", "when": ["main.src", "web"]},
            {"name": "native", "when": ["main.src", "native"], "hosts": ["` + runtime.GOOS + `"]},
            {"name": "elsewhere", "when": ["main.src"], "hosts": ["plan9-never"]}
          ]
        }
      }
    }
  }
}`

func TestAVariantOverlaysTheProfile(t *testing.T) {
	_, err := Decode([]byte(targetsProfile))
	if err == nil || !strings.Contains(err.Error(), "hosts[0]") {
		t.Fatalf("an unknown host fails the profile: %v", err)
	}
	p := mustTargetsProfile(t)
	alt, err := p.Variant("alt")
	if err != nil {
		t.Fatal(err)
	}
	if alt.Variants != nil || alt.Toolchain.Command[0] != "alttool" || alt.Toolchain.Pattern != p.Toolchain.Pattern || alt.Tasks[TaskLint].Steps[0][0] != "tool" {
		t.Errorf("the overlay merges into the profile: %+v", alt)
	}
	if _, err := p.Variant("nope"); err == nil {
		t.Error("an unknown variant is an error")
	}
	if got := p.ShippedRootPatterns(TaskBuild); !reflect.DeepEqual(got, []string{"alt.yaml", "demo.yaml"}) {
		t.Errorf("the declared stack-root files hold every variant's: %v", got)
	}
	for _, bad := range []string{
		`{"variants": {"Alt": {}}}`,
		`{"variants": {"alt": {"markers": ["x"]}}}`,
		`{"variants": {"alt": {"variants": {}}}}`,
		`{"variants": {"alt": {"tasks": {"build": {"stepz": []}}}}}`,
	} {
		var over map[string]any
		_ = json.Unmarshal([]byte(bad), &over)
		var base map[string]any
		_ = json.Unmarshal([]byte(strings.Replace(targetsProfile, "plan9-never", "linux", 1)), &base)
		mergeInto(base, over)
		data, _ := json.Marshal(base)
		if _, err := Decode(data); err == nil {
			t.Errorf("%s decodes", bad)
		}
	}
	if _, err := p.Resolve(json.RawMessage(`{"tasks": {"lint": {"targets": []}}}`)); err == nil || !strings.Contains(err.Error(), "declares no targets") {
		t.Errorf("a step that names {target} needs targets: %v", err)
	}
	if _, err := p.Resolve(json.RawMessage(`{"tasks": {"lint": {"targets": [{"name": "x", "when": ["test"]}]}}}`)); err == nil || !strings.Contains(err.Error(), "in no input") {
		t.Errorf("a target's paths are task inputs: %v", err)
	}
	if _, err := p.Resolve(json.RawMessage(`{"variants": {}}`)); err == nil {
		t.Error("an override cannot change the variants")
	}
}

func mustTargetsProfile(t *testing.T) Profile {
	t.Helper()
	p, err := Decode([]byte(strings.Replace(targetsProfile, "plan9-never", otherHost(), 1)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// otherHost is a host system the test does not run on.
func otherHost() string {
	if runtime.GOOS == "windows" {
		return "linux"
	}
	return "windows"
}

func targetsDetector(root string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
	all := map[string]Component{
		"pkgs/core": {Path: "pkgs/core", Name: "core", Root: ".", Variant: "base"},
		"apps/web":  {Path: "apps/web", Name: "web", Root: ".", Variant: "alt", Dependencies: []string{"pkgs/core"}},
		"bad":       {Path: "bad", Name: "bad", Root: ".", Variant: "nope"},
	}
	var out []Component
	for _, p := range request.Paths {
		if c, ok := all[p]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func targetsFixture(t *testing.T) (string, Stack) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "demo.yaml", "name: workspace\nenvironment:\n  tool: ^2.1.0\n  alttool: '>=5.0.0 <6.0.0'\n")
	writeFile(t, root, "alt.yaml", "")
	writeFile(t, root, "pkgs/core/demo.yaml", "name: core\n")
	writeFile(t, root, "pkgs/core/lib/core.src", "")
	writeFile(t, root, "pkgs/core/bin/serve.src", "")
	writeFile(t, root, "pkgs/core/bin/migrate.src", "")
	writeFile(t, root, "pkgs/core/test/core_test.src", "")
	writeFile(t, root, "apps/web/demo.yaml", "name: web\n")
	writeFile(t, root, "apps/web/main.src", "")
	writeFile(t, root, "apps/web/lib/app.src", "")
	writeFile(t, root, "apps/web/web/index.html", "")
	writeFile(t, root, "apps/web/native/runner.c", "")
	return root, Stack{Extension: "@putnami/demo", Profile: mustTargetsProfile(t), Detect: targetsDetector}
}

func TestTheProbeRecordsEachComponentsVariant(t *testing.T) {
	root, s := targetsFixture(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"apps/web", "bad", "pkgs/core"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 2 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != CodeProjectConfigInvalid {
		t.Fatalf("projects %+v, diagnostics %v", result.Projects, result.Diagnostics)
	}
	want := map[string]Metadata{
		"apps/web":  {Component: "web", Root: ".", Variant: "alt", Toolchain: Expected{Version: ">=5.0.0 <6.0.0", Source: "demo.yaml"}},
		"pkgs/core": {Component: "core", Root: ".", Variant: "base", Toolchain: Expected{Version: "^2.1.0", Source: "demo.yaml"}},
	}
	for _, p := range result.Projects {
		var meta Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil || !reflect.DeepEqual(meta, want[p.Path]) {
			t.Errorf("%s metadata = %s, want %+v", p.Path, p.Metadata, want[p.Path])
		}
	}
	if web := result.Projects[0]; !reflect.DeepEqual(web.WatchedFiles, []string{"alt.yaml", "apps/web/demo.yaml", "demo.yaml"}) {
		t.Errorf("the variant's stack-root files are watched: %v", web.WatchedFiles)
	}
}

func TestTheSelectorsFollowTheVariant(t *testing.T) {
	root, s := targetsFixture(t)
	if got := resolveOne(t, s, root, "pkgs/core", "demo:lint"); !reflect.DeepEqual(got.Files, []string{"bin/migrate.src", "bin/serve.src", "demo.yaml", "lib/core.src"}) {
		t.Errorf("lint reads no test file: %+v", got)
	}
	writeFile(t, root, "apps/web/web/generated/registrant.js", "")
	if got := resolveOne(t, s, root, "apps/web", "demo:build"); !reflect.DeepEqual(got.Files, []string{"demo.yaml", "lib/app.src", "main.src", "native/runner.c", "web/index.html"}) {
		t.Errorf("an excluded file is no input: %+v", got)
	}
	writeFile(t, root, "pkgs/core/putnami.json", `{"options": {"@putnami/demo": {"inputs": {"manifests": ["demo.yaml", "!demo.yaml", "!putnami.json", "{root}/demo.yaml"]}}}}`)
	if got := resolveOne(t, s, root, "pkgs/core", "demo:lint"); !slices.Contains(got.Files, "demo.yaml") || !slices.Contains(got.Files, "putnami.json") {
		t.Errorf("an exclusion never leaves out putnami.json or a toolchain file: %+v", got)
	}
	writeFile(t, root, "pkgs/core/putnami.json", `{"options": {"@putnami/demo": {"tasks": {"lint": {"targets": [{"name": "lib", "when": ["lib"], "hosts": ["linux"]}]}}}}}`)
	if got := resolveOne(t, s, root, "pkgs/core", "demo:lint"); !strings.Contains(got.Error, "hostPlatform") {
		t.Errorf("an override cannot make a task host-dependent: %+v", got)
	}
	writeFile(t, root, "apps/web/putnami.json", `{"options": {"@putnami/demo": {"inputs": {"alt": ["{root}/other.yaml"]}}}}`)
	if got := resolveOne(t, s, root, "apps/web", "demo:build"); !strings.Contains(got.Error, "other.yaml") {
		t.Errorf("a stack-root file no variant declares is refused: %+v", got)
	}
	writeFile(t, root, "pkgs/core/putnami.json", `{"options": {"@putnami/demo": {"tasks": {"build": {"inputs": ["code", "manifests", "alt"]}}}}}`)
	if got := resolveOne(t, s, root, "pkgs/core", "demo:build"); got.Error != "" {
		t.Errorf("an override may add a stack-root file another variant declares: %+v", got)
	}
}

func runTargetsTask(t *testing.T, s Stack, root, project, component, variant, task string) (string, *recorder, string) {
	t.Helper()
	meta, _ := json.Marshal(Metadata{Component: component, Root: ".", Variant: variant})
	rec := &recorder{}
	var out bytes.Buffer
	status, _, _ := s.run(context.Background(), runInput{
		task: task, workspaceRoot: root, projectAbs: filepath.Join(root, project),
		metadata: meta, outputRoot: filepath.Join(root, ".putnami/out", component, task), stdout: &out,
	}, rec)
	return status, rec, out.String()
}

// stubTargetsTools puts a tool and an alttool first on PATH that print their
// version and their arguments.
func stubTargetsTools(t *testing.T, toolVersion, altVersion string) {
	t.Helper()
	bin := t.TempDir()
	for name, version := range map[string]string{"tool": toolVersion, "alttool": altVersion} {
		writeFile(t, bin, name, "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '"+name+" "+version+"'; exit 0; fi\necho \"ran "+name+" $*\"\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAStepThatNamesATargetRunsOncePerTarget(t *testing.T) {
	root, s := targetsFixture(t)
	stubTargetsTools(t, "2.4.1", "5.2.0")

	status, rec, out := runTargetsTask(t, s, root, "pkgs/core", "core", "base", TaskBuild)
	if status != statusOK {
		t.Fatalf("build: %s %v", status, rec.diags)
	}
	output := filepath.Join(root, ".putnami/out/core/build/build")
	for _, want := range []string{"ran tool prepare\n", "ran tool compile bin/migrate.src -o " + output + "/migrate\nran tool compile bin/serve.src -o " + output + "/serve\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("build ran\n%s\nwant %q", out, want)
		}
	}

	status, rec, out = runTargetsTask(t, s, root, "apps/web", "web", "alt", TaskBuild)
	if status != statusOK || out != "ran alttool build web\nran alttool build native\n" {
		t.Errorf("the variant builds the targets its paths and host allow: %s %v\n%s", status, rec.diags, out)
	}
	status, rec, out = runTargetsTask(t, s, root, "apps/web", "web", "alt", TaskLint)
	if status != statusOK || out != "ran tool check lib\n" {
		t.Errorf("lint runs the target whose path exists: %s %v\n%s", status, rec.diags, out)
	}

	if err := os.RemoveAll(filepath.Join(root, "apps/web/main.src")); err != nil {
		t.Fatal(err)
	}
	status, rec, out = runTargetsTask(t, s, root, "apps/web", "web", "alt", TaskBuild)
	if status != statusOK || out != "" || !strings.Contains(strings.Join(rec.infos, "\n"), "no build target in web") {
		t.Errorf("a step with no target does not run: %s %v %q %v", status, rec.diags, out, rec.infos)
	}
}

func TestAVersionRangeIsChecked(t *testing.T) {
	root, s := targetsFixture(t)
	stubTargetsTools(t, "3.0.0", "5.2.0")
	if status, rec, _ := runTargetsTask(t, s, root, "pkgs/core", "core", "base", TaskLint); status != statusFailed || rec.codes() != "error:"+CodeToolchainMismatch {
		t.Errorf("a version outside ^2.1.0 fails: %s %v", status, rec.diags)
	}
	if status, rec, _ := runTargetsTask(t, s, root, "apps/web", "web", "alt", TaskLint); status != statusOK || rec.codes() != "" {
		t.Errorf("the variant checks its own toolchain: %s %v", status, rec.diags)
	}
}

func TestReportStepsRunAfterAFailedStep(t *testing.T) {
	root, s := targetsFixture(t)
	stubTargetsTools(t, "2.4.1", "5.2.0")
	task := s.Profile.Tasks[TaskBuild]
	task.Steps = [][]string{{"sh", "-c", "exit 1"}}
	task.Targets = nil
	task.ReportSteps = [][]string{{"sh", "-c", "mkdir -p {output} && printf '%s' \"$JUNIT\" > {output}/junit.xml"}}
	task.Reports = &Reports{JUnit: []string{"{output}/junit.xml"}}
	s.Profile.Tasks[TaskBuild] = task
	t.Setenv("JUNIT", junitFail)
	status, rec, _ := runTargetsTask(t, s, root, "pkgs/core", "core", "base", TaskBuild)
	if status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeStepFailed) || !strings.Contains(rec.codes(), "error:"+CodeTestFailed) {
		t.Errorf("the report step writes the report a failed runner leaves, and its failed case names the failure: %s %v", status, rec.diags)
	}
	// A report step may run the extension's runtime, which collects the
	// reports a tool wrote outside the output directory.
	collect := filepath.Join(t.TempDir(), "collect")
	writeFile(t, filepath.Dir(collect), "collect", "#!/bin/sh\nmkdir -p \"$2\" && printf '%s' \"$JUNIT\" > \"$2/junit.xml\"\n")
	if err := os.Chmod(collect, 0o755); err != nil {
		t.Fatal(err)
	}
	s.Runtime = collect
	task.ReportSteps = [][]string{{"{runtime}", "collect-reports", "{output}"}}
	s.Profile.Tasks[TaskBuild] = task
	status, rec, _ = runTargetsTask(t, s, root, "pkgs/core", "core", "base", TaskBuild)
	if status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeTestFailed) {
		t.Errorf("a report step that names {runtime} runs the runtime: %s %v", status, rec.diags)
	}
	if _, err := Decode([]byte(strings.Replace(targetsProfile, `"inputs": ["code", "manifests"],
      "targets": [{"each": "bin/*.src"}]`, `"inputs": ["code", "manifests"],
      "targets": [{"each": "bin/*.src"}], "reportSteps": [["tool", "{target}"]]`, 1))); err == nil || !strings.Contains(err.Error(), "reportSteps") {
		t.Errorf("a report step names no target: %v", err)
	}
}

func TestAFailedFurtherCheckInstallsTheToolchain(t *testing.T) {
	root, s := targetsFixture(t)
	bin, installed := t.TempDir(), t.TempDir()
	writeFile(t, bin, "tool", "#!/bin/sh\nprintf 'tool 2.4.1\\nbundled 1.0.0\\n'\n")
	writeFile(t, installed, "tool", "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'tool 2.4.1\\nbundled 2.1.0\\n'; exit 0; fi\necho \"installed $*\"\n")
	for _, f := range []string{filepath.Join(bin, "tool"), filepath.Join(installed, "tool")} {
		if err := os.Chmod(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeFile(t, root, "pkgs/core/demo.yaml", "name: core\nenvironment:\n  bundled: ^2.0.0\n")
	s.Profile.Toolchain.Provisioned = "STUB_TOOL"
	s.Profile.Toolchain.Also = []Toolchain{{Command: []string{"tool", "--version"}, Pattern: `bundled (?<version>\S+)`, Files: []VersionFile{{Path: "demo.yaml", Key: "environment.bundled"}}}}
	calls := 0
	s.Provide = func(_ context.Context, _ Toolchain, expected []Expected, _ func(string)) (Provided, error) {
		calls++
		if len(expected) != 2 || expected[1].Version != "^2.0.0" {
			t.Errorf("expected = %+v", expected)
		}
		return Provided{Executable: filepath.Join(installed, "tool")}, nil
	}
	status, rec, out := runTargetsTask(t, s, root, "pkgs/core", "core", "base", TaskLint)
	if status != statusOK || calls != 1 || !strings.Contains(out, "installed check lib") {
		t.Errorf("a further check that fails installs and runs the toolchain: %s %d %v %q", status, calls, rec.diags, out)
	}
}

func TestARootJobRunsAtAStackRootWithNoComponent(t *testing.T) {
	root, s := targetsFixture(t)
	stubTargetsTools(t, "2.4.1", "5.2.0")
	meta, _ := json.Marshal(Metadata{Root: "pkgs", Variant: "alt"})
	ran := ""
	job := s.RootJob(func(_ context.Context, step RootStep, _ *jsonl.Emitter) error {
		ran = step.Program("alttool")
		return nil
	})
	var status string
	var err error
	out := captureStdout(t, func() {
		status, _, err = job(&pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
			Path: "pkgs", FullPath: filepath.Join(root, "pkgs"), Metadata: map[string]json.RawMessage{s.Extension: meta},
		}}, jsonl.New(), nil)
	})
	if status != statusOK || err != nil || ran != "alttool" {
		t.Errorf("a root job at an input-only stack root selects its variant's toolchain: %s %v %q %s", status, err, ran, out)
	}
}

func TestAWorkspaceJobRunsAtARootWithItsComponentsToolchain(t *testing.T) {
	root, s := targetsFixture(t)
	stubTargetsTools(t, "2.4.1", "5.2.0")
	writeFile(t, root, "nest/demo.yaml", "name: nest\n")
	writeFile(t, root, "nest/member/demo.yaml", "name: member\n")
	s.Detect = func(_ string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		all := map[string]Component{
			"nest":        {Path: "nest", Root: "nest", InputOnly: true},
			"nest/member": {Path: "nest/member", Name: "member", Root: "nest", Variant: "alt"},
		}
		var out []Component
		for _, p := range request.Paths {
			if c, ok := all[p]; ok {
				out = append(out, c)
			}
		}
		return out, nil
	}
	var projects []string
	rootJob := s.WorkspaceRootJob(FailOnMissingToolchain, func(_ context.Context, step RootStep, _ *jsonl.Emitter) error {
		projects = append(projects, step.Project)
		return nil
	})
	refs := []job.ProjectRef{
		{Name: "nest", Path: "nest", FullPath: filepath.Join(root, "nest"), Extensions: []string{s.Extension}},
		{Name: "member", Path: "nest/member", FullPath: filepath.Join(root, "nest/member"), Extensions: []string{s.Extension}},
	}
	var status string
	var err error
	captureStdout(t, func() {
		status, _, err = rootJob(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs}, jsonl.New(), nil)
	})
	if status != statusOK || err != nil || !reflect.DeepEqual(projects, []string{filepath.Join(root, "nest/member")}) {
		t.Errorf("the member stands for its stack root: %s %v %v", status, err, projects)
	}
}
