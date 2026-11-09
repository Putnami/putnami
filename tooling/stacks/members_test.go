package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// memberProfile tests with rspec unless the project holds only Minitest
// files, then with rails test; its test task runs at the stack root and
// scopes its Cobertura report to the component.
const memberProfile = `{
  "id": "ruby",
  "markers": ["Gemfile", "package.yml", "*.gemspec"],
  "toolchain": {"command": ["rustc", "--version"], "pattern": "^rustc (?<version>\\S+)", "version": "1.82.0"},
  "inputs": {
    "sources": ["lib/**/*.rb", "spec/**/*.rb", "test/**/*.rb"],
    "bundle": ["{root}/Gemfile.lock"]
  },
  "tasks": {
    "lint": {"steps": [["rubocop"]], "inputs": ["sources"]},
    "test": {
      "dir": "root",
      "steps": [["rspec", "{project}/spec"]],
      "inputs": ["sources", "bundle"],
      "reports": {"coverage": [{"format": "cobertura", "paths": ["{root}/coverage/coverage.xml"]}]}
    }
  }
}`

// memberDetector names a pack of the root bundle and a gem with its own.
func memberDetector(_ string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
	all := map[string]Component{
		"packs/pricing":             {Path: "packs/pricing", Name: "packs/pricing", Root: "."},
		"packs/pricing/engines/tax": {Path: "packs/pricing/engines/tax", Name: "tax", Root: "."},
		"gems/audit":                {Path: "gems/audit", Name: "audit", Root: "gems/audit"},
	}
	var out []Component
	for _, p := range request.Paths {
		if c, ok := all[p]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func memberStack(t *testing.T) Stack {
	t.Helper()
	p, err := Decode([]byte(memberProfile))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return Stack{Extension: "@putnami/ruby", Profile: p, Detect: memberDetector}
}

func TestDecodeRefusesAnInvalidExclusion(t *testing.T) {
	cases := map[string][2]string{
		"escaping !":    {`"lib/**/*.rb",`, `"lib/**/*.rb", "!../vendor/**",`},
		"rooted escape": {`"lib/**/*.rb",`, `"lib/**/*.rb", "!{root}/../vendor/**",`},
		"no input set":  {`"inputs": ["sources"]}`, `"inputs": []}`},
	}
	for name, edit := range cases {
		doc := strings.Replace(memberProfile, edit[0], edit[1], 1)
		if doc == memberProfile {
			t.Fatalf("%s: the edit matches nothing", name)
		}
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("%s: Decode accepted %s", name, doc)
		}
	}
}

const coberturaReport = `<?xml version="1.0"?>
<coverage line-rate="0.5" lines-covered="4" lines-valid="8" version="0" timestamp="1">
  <sources><source>%ROOT%</source></sources>
  <packages><package name="demo"><classes>
    <class name="a" filename="packs/pricing/lib/pricing.rb"><lines><line number="1" hits="1"/><line number="2" hits="0"/></lines></class>
    <class name="b" filename="packs/pricing/engines/tax/lib/tax.rb"><lines><line number="1" hits="1"/><line number="2" hits="1"/></lines></class>
    <class name="c" filename="app/models/order.rb"><lines><line number="1" hits="1"/><line number="2" hits="0"/></lines></class>
    <class name="d" filename="lib/gone.rb"><lines><line number="1" hits="1"/><line number="2" hits="0"/></lines></class>
  </classes></package></packages>
</coverage>`

func TestATaskRunsAtTheRootAndCountsItsOwnCoverage(t *testing.T) {
	// The steps' $PWD is the real path: on macOS the temporary directory is
	// a symbolic link.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "packs/pricing/package.yml", "")
	writeFile(t, root, "packs/pricing/lib/pricing.rb", "")
	writeFile(t, root, "packs/pricing/spec/pricing_spec.rb", "")
	writeFile(t, root, "packs/pricing/engines/tax/tax.gemspec", "")
	writeFile(t, root, "packs/pricing/engines/tax/lib/tax.rb", "")
	writeFile(t, root, "app/models/order.rb", "")
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	writeFile(t, bin, "rustc", "#!/bin/sh\necho 'rustc 1.82.0'\n")
	writeFile(t, bin, "rspec", "#!/bin/sh\necho \"$PWD: rspec $*\" >> "+log+"\nmkdir -p coverage\nsed \"s#%ROOT%#$PWD#\" > coverage/coverage.xml <<'XML'\n"+coberturaReport+"\nXML\n")
	for _, f := range []string{"rustc", "rspec"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	meta, _ := json.Marshal(Metadata{Component: "packs/pricing", Root: "."})
	rec := &recorder{}
	status, data, err := memberStack(t).run(context.Background(), runInput{
		task: TaskTest, workspaceRoot: root, projectAbs: filepath.Join(root, "packs/pricing"),
		metadata: meta, outputRoot: filepath.Join(root, ".putnami/out/pricing/test"), stdout: &bytes.Buffer{},
	}, rec)
	if status != statusOK || err != nil {
		t.Fatalf("test: %s %v %v", status, err, rec.diags)
	}
	ran, _ := os.ReadFile(log)
	if want := root + ": rspec " + filepath.Join(root, "packs/pricing") + "/spec"; strings.TrimSpace(string(ran)) != want {
		t.Errorf("the steps ran %q, want %q", ran, want)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 1 || got["total"] != 2 {
		t.Errorf("only the component's own file counts, not a nested gem's or the application's: %v", got)
	}
	if rec.codes() != "warning:"+CodeCoverageUnmapped || !strings.Contains(rec.diags[0], "2 coverage files") {
		t.Errorf("a file of no component, or found nowhere, is a warning: %v", rec.diags)
	}
}

func TestAGlobMarkerIsNoWatchedFile(t *testing.T) {
	root := t.TempDir()
	s := memberStack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"packs/pricing"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %+v %v", result, err)
	}
	if diags := wsproto.ValidateProbeResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	if w := result.Projects[0].WatchedFiles; !slices.Contains(w, "packs/pricing/Gemfile") || slices.ContainsFunc(w, func(f string) bool { return strings.Contains(f, "*") }) {
		t.Errorf("watched = %v", w)
	}
}

func TestAnExcludedPatternLeavesItsFilesOut(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "packs/pricing/lib/pricing.rb", "")
	writeFile(t, root, "packs/pricing/vendor/bundle/gems/rake/lib/rake.rb", "")
	writeFile(t, root, "packs/pricing/tmp/cache.rb", "")
	writeFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {"inputs": {"sources": ["**/*.rb", "!vendor/**", "!tmp/cache.rb"]}}}}`)
	got := resolveOne(t, memberStack(t), root, "packs/pricing", "ruby:lint")
	if want := []string{"lib/pricing.rb", "putnami.json"}; got.Error != "" || !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("lint = %+v, want %v", got, want)
	}
	if basis := got.Basis.Content[0]; !slices.Contains(basis, "!vendor/**") {
		t.Errorf("the basis carries the exclusion: %v", basis)
	}
}

func TestNullRemovesATaskThatThenRunsNothing(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "packs/pricing")
	writeFile(t, root, "packs/pricing/lib/pricing.rb", "")
	writeFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {"tasks": {"test": null}}}}`)
	s := memberStack(t)
	resolved, err := s.Resolved(pack, "")
	if _, ok := resolved.Tasks[TaskTest]; err != nil || ok {
		t.Fatalf("null removes the test task: %v %v", resolved.Tasks, err)
	}
	meta, _ := json.Marshal(Metadata{Component: "packs/pricing", Root: "."})
	rec := &recorder{}
	if status, _, err := s.run(context.Background(), runInput{task: TaskTest, workspaceRoot: root, projectAbs: pack, metadata: meta, outputRoot: t.TempDir(), stdout: &bytes.Buffer{}}, rec); status != statusOK || err != nil {
		t.Errorf("the removed task passes with nothing to run: %s %v %v", status, err, rec.diags)
	}
}

func TestAnOverrideMayAddATaskTheProfileOmits(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "packs/pricing")
	writeFile(t, root, "packs/pricing/lib/pricing.rb", "")
	writeFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {"tasks": {"build": {"steps": [["gem", "build"]], "inputs": ["sources"]}}}}}`)
	s := memberStack(t)
	p, err := s.Resolved(pack, "")
	if err != nil || !reflect.DeepEqual(p.Tasks[TaskBuild].Steps, [][]string{{"gem", "build"}}) {
		t.Fatalf("build = %+v, %v", p.Tasks[TaskBuild], err)
	}
	if got := resolveOne(t, s, root, "packs/pricing", "ruby:build"); got.Error != "" || !slices.Contains(got.Files, "lib/pricing.rb") {
		t.Errorf("the added task's selector answers its inputs: %+v", got)
	}
	if _, rootFiles := s.Profile.TaskPatterns(TaskBuild); len(rootFiles) != 0 {
		t.Errorf("a profile with no version file declares no stack-root input for build: %v", rootFiles)
	}
}

func TestAnExcludedReportIsNoSourceAndIsRemoved(t *testing.T) {
	root := t.TempDir()
	gem := filepath.Join(root, "gems/audit")
	writeFile(t, root, "gems/audit/lib/audit.rb", "")
	writeFile(t, root, "gems/audit/coverage/coverage.xml", "<coverage/>")
	writeFile(t, root, "gems/audit/putnami.json", `{"options": {"@putnami/ruby": {
	  "inputs": {"sources": ["**", "!coverage/**"]},
	  "tasks": {"test": {"steps": [["true"]]}}
	}}}`)
	bin := t.TempDir()
	writeFile(t, bin, "rustc", "#!/bin/sh\necho 'rustc 1.82.0'\n")
	if err := os.Chmod(filepath.Join(bin, "rustc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	meta, _ := json.Marshal(Metadata{Component: "audit", Root: "gems/audit"})
	rec := &recorder{}
	status, _, _ := memberStack(t).run(context.Background(), runInput{task: TaskTest, workspaceRoot: root, projectAbs: gem, metadata: meta, outputRoot: t.TempDir(), stdout: &bytes.Buffer{}}, rec)
	if status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeReportMissing) {
		t.Errorf("a report an exclusion leaves out of the inputs is removed, not read: %s %v", status, rec.diags)
	}
}

func TestTheAnswerKeepsLinksNestedFilesAndTheFilesEveryTaskReads(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "shared/shared.rb", "")
	writeFile(t, root, "packs/pricing/lib/pricing.rb", "")
	writeFile(t, root, "packs/pricing/spec/fixtures/top.rb", "")
	writeFile(t, root, "packs/pricing/spec/fixtures/deep/nested.rb", "")
	if err := os.Symlink(filepath.Join(root, "shared/shared.rb"), filepath.Join(root, "packs/pricing/lib/shared.rb")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {"inputs": {"sources": ["lib/**/*.rb", "spec/**/*.rb", "!spec/fixtures/*", "!**/*.json"]}}}}`)
	got := resolveOne(t, memberStack(t), root, "packs/pricing", "ruby:lint")
	want := []string{"lib/pricing.rb", "lib/shared.rb", "putnami.json", "spec/fixtures/deep/nested.rb"}
	if got.Error != "" || !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("lint = %+v, want %v", got, want)
	}
	if len(got.Basis.Content) != 2 || !slices.Contains(got.Basis.Content[1], ProjectConfigFile) {
		t.Errorf("the files every task reads have a basis set of their own: %v", got.Basis.Content)
	}
}

func TestAnOverrideMustNameTheInputsOfATaskItAddsAndMayRemoveEveryTask(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "packs/pricing")
	writeFile(t, root, "packs/pricing/lib/pricing.rb", "")
	s := memberStack(t)
	writeFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {"tasks": {"test": null, "build": {"steps": [["rake", "build"]]}}}}}`)
	if _, err := s.Resolved(pack, ""); err == nil || !strings.Contains(err.Error(), "input set") {
		t.Errorf("an added task with no input set is refused: %v", err)
	}
	writeFile(t, root, "packs/pricing/putnami.json", `{"options": {"@putnami/ruby": {"tasks": {"lint": null, "test": null}}}}`)
	if p, err := s.Resolved(pack, ""); err != nil || len(p.Tasks) != 0 {
		t.Errorf("an override may remove the last task: %v %v", p.Tasks, err)
	}
}

func TestAStackRootExclusionReachesTheRootPatternsAndAnOwnRootAnswer(t *testing.T) {
	doc := strings.Replace(memberProfile, `"bundle": ["{root}/Gemfile.lock"]`, `"bundle": ["{root}/Gemfile.lock", "{root}/spec/**", "!{root}/spec/examples.txt"]`, 1)
	p, err := Decode([]byte(doc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, root := p.TaskPatterns(TaskTest); !slices.Contains(root, "spec/**") || !slices.Contains(root, "!spec/examples.txt") {
		t.Errorf("the root patterns carry the exclusion: %v", root)
	}
	root := t.TempDir()
	writeFile(t, root, "gems/audit/spec/audit_spec.rb", "")
	writeFile(t, root, "gems/audit/spec/examples.txt", "")
	s := Stack{Extension: "@putnami/ruby", Profile: p, Detect: memberDetector}
	got := resolveOne(t, s, root, "gems/audit", "ruby:test")
	if !slices.Contains(got.Files, "spec/audit_spec.rb") || slices.Contains(got.Files, "spec/examples.txt") {
		t.Errorf("a component that is its own root reads its root files less the exclusion: %v", got.Files)
	}
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"gems/audit"}})
	if err != nil || slices.ContainsFunc(result.Projects[0].WatchedFiles, func(f string) bool { return strings.Contains(f, "!") }) {
		t.Errorf("an exclusion is no watched file: %v %v", result.Projects, err)
	}
}

func TestTheAttributionAsksIsComponentInsteadOfTheDetector(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "packs/pricing/package.yml", "")
	writeFile(t, root, "packs/pricing/lib/pricing.rb", "")
	writeFile(t, root, "packs/checkout/package.yml", "")
	writeFile(t, root, "packs/checkout/lib/cart.rb", "")
	s := memberStack(t)
	detected := 0
	s.Detect = func(string, wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) { detected++; return nil, nil }
	var asked []string
	s.IsComponent = func(_, dir string) bool { asked = append(asked, dir); return true }
	a := s.attribution(root, filepath.Join(root, "packs/pricing"), filepath.Join(root, "packs/pricing"), "packs/pricing")
	if got := a.File(filepath.Join(root, "packs/pricing/lib/pricing.rb")); got != OwnerTask {
		t.Errorf("pricing.rb owner = %v", got)
	}
	if got := a.File(filepath.Join(root, "packs/checkout/lib/cart.rb")); got != OwnerOther {
		t.Errorf("cart.rb owner = %v", got)
	}
	_ = a.File(filepath.Join(root, "packs/checkout/lib/cart.rb"))
	if detected != 0 || !reflect.DeepEqual(asked, []string{"packs/pricing", "packs/checkout"}) {
		t.Errorf("the predicate answers each marker directory once, the detector never: detected %d, asked %v", detected, asked)
	}
}

func TestAFileATaskReadsStaysThoughAnotherTaskExcludesIt(t *testing.T) {
	doc := strings.Replace(memberProfile, `"sources": ["lib/**/*.rb", "spec/**/*.rb", "test/**/*.rb"],`, `"sources": ["lib/**/*.rb", "spec/**/*.rb", "test/**/*.rb", "lib/**/*.xml"],
    "lean": ["!lib/fixtures/junit.xml"],`, 1)
	doc = strings.Replace(doc, `"inputs": ["sources", "bundle"],`, `"inputs": ["sources", "bundle", "lean"],`, 1)
	doc = strings.Replace(doc, `"reports": {"coverage": [{"format": "cobertura", "paths": ["{root}/coverage/coverage.xml"]}]}`, `"reports": {"junit": ["lib/fixtures/junit.xml"]}`, 1)
	p, err := Decode([]byte(doc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	root := t.TempDir()
	pack := filepath.Join(root, "packs/pricing")
	writeFile(t, root, "packs/pricing/spec/pricing_spec.rb", "")
	writeFile(t, root, "packs/pricing/lib/fixtures/junit.xml", junitPass)
	if err := removeReports(p, TaskTest, pack, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pack, "lib/fixtures/junit.xml")); err != nil {
		t.Errorf("lint reads the file, so the test task's exclusion does not make it a stale report: %v", err)
	}
}
