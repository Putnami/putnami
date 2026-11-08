package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// variantProfile is a stack with two builds: "gradle" and "maven".
const variantProfile = `{
  "id": "java",
  "markers": ["build.gradle.kts", "pom.xml"],
  "toolchain": {"command": ["java", "-version"], "pattern": "version \"(?<version>[^\"]+)\"", "files": [{"path": ".java-version"}]},
  "inputs": {"sources": ["src/main/**"], "tests": ["src/test/**"]},
  "variants": {
    "gradle": {
      "inputs": {"build": ["build.gradle.kts", "{root}/settings.gradle.kts"]},
      "tasks": {
        "lint": {"steps": [["{root}/gradlew", "{component}check"]], "inputs": ["sources", "build"]},
        "test": {"steps": [["{root}/gradlew", "{component}test"]], "inputs": ["sources", "tests", "build"],
          "reports": {"when": ["src/test/**"], "junit": ["build/test-results/test/*.xml"]}}
      }
    },
    "maven": {
      "inputs": {"build": ["pom.xml", "{root}/pom.xml"]},
      "tasks": {
        "test": {"steps": [["mvn", "--projects", "{component}", "verify"]], "inputs": ["sources", "tests", "build"],
          "reports": {"junit": ["target/surefire-reports/*.xml"], "coverage": [{"format": "jacoco", "paths": ["target/site/jacoco/jacoco.xml"], "optional": true}]}},
        "build": {"steps": [["mvn", "--projects", "{component}", "package"]], "inputs": ["sources", "build"]}
      }
    }
  }
}`

func variantStack(t *testing.T) Stack {
	t.Helper()
	p, err := Decode([]byte(variantProfile))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	detect := func(root string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		all := map[string]Component{
			".":       {Path: ".", Ref: ":", Variant: "gradle", Root: "."},
			"api":     {Path: "api", Name: "api", Ref: ":api:", Variant: "gradle", Root: ".", Dependencies: []string{"."}},
			"pricing": {Path: "pricing", Name: "pricing", Ref: "com.acme:pricing", Variant: "maven", Root: "."},
			"bad":     {Path: "bad", Name: "bad", Variant: "sbt", Root: "."},
		}
		var out []Component
		for _, p := range request.Paths {
			if c, ok := all[p]; ok {
				out = append(out, c)
			}
		}
		return out, nil
	}
	return Stack{Extension: "@putnami/java", Profile: p, Detect: detect}
}

func TestDecodeValidatesEachVariant(t *testing.T) {
	p := variantStack(t).Profile
	if got := p.VariantNames(); !reflect.DeepEqual(got, []string{"gradle", "maven"}) {
		t.Fatalf("variants = %v", got)
	}
	if !p.HasTask(TaskLint) || !p.HasTask(TaskBuild) || len(p.Applied()) != 2 {
		t.Errorf("the tasks are every variant's: %+v", p.Applied())
	}
	if got := p.ShippedRootPatterns(TaskTest); !reflect.DeepEqual(got, []string{".java-version", "pom.xml", "settings.gradle.kts"}) {
		t.Errorf("shipped root patterns = %v", got)
	}
	if _, err := p.Variant(""); err == nil {
		t.Error("a profile with variants needs a variant name")
	}
	if _, err := p.Variant("sbt"); err == nil {
		t.Error("an unknown variant is refused")
	}
	for name, doc := range map[string]string{
		"a variant that breaks the profile":   strings.Replace(variantProfile, `"inputs": ["sources", "build"]}`, `"inputs": ["nope"]}`, 1),
		"a variant that changes the id":       strings.Replace(variantProfile, `"gradle": {`, `"gradle": {"id": "kotlin",`, 1),
		"a variant name that is no id":        strings.Replace(variantProfile, `"gradle": {`, `"Gradle": {`, 1),
		"a when pattern that is not clean":    strings.Replace(variantProfile, `"when": ["src/test/**"]`, `"when": ["../src"]`, 1),
		"a profile without tasks or variants": `{"id": "java", "markers": ["pom.xml"], "toolchain": {"command": ["java"], "pattern": "(?<version>.*)"}}`,
	} {
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("%s: Decode accepted it", name)
		}
	}
	if _, err := p.Resolve(json.RawMessage(`{"variants": {}}`)); err == nil {
		t.Error("an override cannot change the variants")
	}
}

func TestProbeRecordsTheRefAndTheVariant(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".java-version", "21\n")
	s := variantStack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{".", "api", "bad", "pricing"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 3 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != CodeProjectConfigInvalid {
		t.Fatalf("projects %+v diagnostics %v", result.Projects, result.Diagnostics)
	}
	want := map[string]Metadata{
		".":       {Component: ":", Variant: "gradle", Root: ".", Toolchain: Expected{Version: "21", Source: ".java-version"}},
		"api":     {Component: ":api:", Variant: "gradle", Root: ".", Toolchain: Expected{Version: "21", Source: ".java-version"}},
		"pricing": {Component: "com.acme:pricing", Variant: "maven", Root: ".", Toolchain: Expected{Version: "21", Source: ".java-version"}},
	}
	for _, p := range result.Projects {
		var meta Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil || meta != want[p.Path] {
			t.Errorf("%s metadata = %s, want %+v", p.Path, p.Metadata, want[p.Path])
		}
		if p.Path == "." && p.SourceName != "" {
			t.Errorf("a component without a name keeps its directory's: %q", p.SourceName)
		}
		if p.Path == "pricing" && (p.SourceName != "pricing" || !contains(p.WatchedFiles, "pom.xml") || contains(p.WatchedFiles, "settings.gradle.kts")) {
			t.Errorf("pricing = %+v", p)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestResolveInputsFollowsTheVariant(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api/build.gradle.kts", "")
	writeFile(t, root, "api/pom.xml", "")
	writeFile(t, root, "api/src/main/A.kt", "")
	writeFile(t, root, "api/src/test/ATest.kt", "")
	writeFile(t, root, "pricing/pom.xml", "")
	writeFile(t, root, "pricing/src/main/P.java", "")
	s := variantStack(t)
	if got := resolveOne(t, s, root, "api", "java:lint"); !reflect.DeepEqual(got.Files, []string{"build.gradle.kts", "src/main/A.kt"}) {
		t.Errorf("gradle lint = %+v", got)
	}
	if got := resolveOne(t, s, root, "pricing", "java:build"); !reflect.DeepEqual(got.Files, []string{"pom.xml", "src/main/P.java"}) {
		t.Errorf("maven build = %+v", got)
	}
	if got := resolveOne(t, s, root, "pricing", "java:lint"); len(got.Files) != 0 || got.Error != "" {
		t.Errorf("a task the variant does not declare reads only putnami.json: %+v", got)
	}
	if got := resolveOne(t, s, root, "unknown", "java:lint"); got.Error == "" {
		t.Errorf("a project the detector does not know has no variant: %+v", got)
	}
	writeFile(t, root, "pricing/putnami.json", `{"options": {"@putnami/java": {"inputs": {"tests": ["src/test/**", "{root}/settings.gradle.kts"]}}}}`)
	if got := resolveOne(t, s, root, "pricing", "java:test"); got.Error != "" {
		t.Errorf("an override may name a stack-root file another variant ships for the task: %+v", got)
	}
	writeFile(t, root, "pricing/putnami.json", `{"options": {"@putnami/java": {"inputs": {"build": ["{root}/gradle.properties"]}}}}`)
	if got := resolveOne(t, s, root, "pricing", "java:build"); got.Error == "" {
		t.Errorf("an override cannot add a stack-root file no variant ships: %+v", got)
	}
}

// runVariant runs a task of the component at rel with its metadata, under a
// stub mvn and java first on PATH.
func runVariant(t *testing.T, s Stack, root, rel, ref, variant, task string) (string, map[string]any, *recorder) {
	t.Helper()
	meta, _ := json.Marshal(Metadata{Component: ref, Variant: variant, Root: "."})
	rec := &recorder{}
	var out bytes.Buffer
	status, data, _ := s.run(context.Background(), runInput{
		task: task, workspaceRoot: root, projectAbs: filepath.Join(root, rel),
		metadata: meta, outputRoot: filepath.Join(root, ".putnami/out", rel, task), stdout: &out,
	}, rec)
	return status, data, rec
}

func stubJava(t *testing.T, root string) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "java", "#!/bin/sh\necho 'openjdk version \"21.0.4\" 2024-07-16' >&2\n")
	writeFile(t, bin, "mvn", "#!/bin/sh\necho \"$PWD: mvn $*\" >> \""+root+"/log\"\n"+
		"mkdir -p target/surefire-reports\nprintf '<testsuite name=\"P\"><testcase classname=\"P\" name=\"a\"/></testsuite>' > target/surefire-reports/TEST-P.xml\n")
	writeFile(t, bin, "gradlew", "#!/bin/sh\necho \"$PWD: gradlew $*\" >> \""+root+"/log\"\n")
	for _, f := range []string{"java", "mvn", "gradlew"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeFile(t, root, "gradlew", "#!/bin/sh\nexec gradlew \"$@\"\n")
	if err := os.Chmod(filepath.Join(root, "gradlew"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunFollowsTheVariantAndItsReportRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".java-version", "21\n")
	writeFile(t, root, "pricing/pom.xml", "")
	writeFile(t, root, "api/build.gradle.kts", "")
	stubJava(t, root)
	s := variantStack(t)

	// A Maven module without a JaCoCo report: the optional coverage is not
	// read, and fails nothing.
	status, data, rec := runVariant(t, s, root, "pricing", "com.acme:pricing", "maven", TaskTest)
	if status != statusOK || data["testSummary"].(map[string]any)["total"] != 1 || data["coverageSummary"] != nil {
		t.Fatalf("maven test: %s %v %v", status, data, rec.diags)
	}
	// A Gradle project without src/test expects no report.
	status, data, rec = runVariant(t, s, root, "api", ":api:", "gradle", TaskTest)
	if status != statusOK || data["testSummary"] != nil {
		t.Fatalf("gradle test without tests: %s %v %v", status, data, rec.diags)
	}
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	for _, want := range []string{filepath.Join(root, "pricing") + ": mvn --projects com.acme:pricing verify", filepath.Join(root, "api") + ": gradlew :api:test"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("ran\n%s\nwant %q", log, want)
		}
	}
	// With tests, a missing report fails the task.
	writeFile(t, root, "api/src/test/ATest.kt", "")
	if status, _, rec := runVariant(t, s, root, "api", ":api:", "gradle", TaskTest); status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeReportMissing) {
		t.Errorf("a project with tests and no report fails: %s %v", status, rec.diags)
	}
	if status, _, rec := runVariant(t, s, root, "api", ":api:", "sbt", TaskTest); status != statusFailed || rec.codes() != "error:"+CodeProjectConfigInvalid {
		t.Errorf("an unknown variant fails the task: %s %v", status, rec.diags)
	}
}

func TestAMajorVersionNamesItsReleaseLine(t *testing.T) {
	tc := variantStack(t).Profile.Toolchain
	if !IsExact("21") || IsExact("stable") {
		t.Error("a major is a version, a channel is not")
	}
	if v := tc.Compare(Expected{Version: "21", Source: ".java-version"}, "21.0.4"); v.Code != "" {
		t.Errorf("21 matches 21.0.4: %+v", v)
	}
	if v := tc.Compare(Expected{Version: "21", Source: ".java-version"}, "17.0.9"); v.Code != CodeToolchainMismatch {
		t.Errorf("21 does not match 17.0.9: %+v", v)
	}
}
