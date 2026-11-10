package dotnet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "dotnet/stack-extension"

const library = `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup>
</Project>
`

// fixture writes a .NET repository the way dotnet new writes one: a
// solution, the Pricing library, the Api that references it, and the
// Pricing.Tests xunit project, with a global.json at the root.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	stackstest.WriteFile(t, root, "Demo.sln", "\xef\xbb\xbf\r\nMicrosoft Visual Studio Solution File, Format Version 12.00\r\n"+
		`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Pricing", "src\Pricing\Pricing.csproj", "{B2F797C7-2016-4B69-917D-114309AF047B}"`+"\r\nEndProject\r\n"+
		`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Api", "src\Api\Api.csproj", "{07D2D3BD-60A9-4733-899B-9054A2707FC4}"`+"\r\nEndProject\r\n"+
		`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Pricing.Tests", "tests\Pricing.Tests\Pricing.Tests.csproj", "{33A27E1E-B1F2-4215-9EEB-EBA57D2B14F6}"`+"\r\nEndProject\r\nGlobal\r\nEndGlobal\r\n")
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "8.0.100", "rollForward": "latestFeature"}}`)
	stackstest.WriteFile(t, root, "Directory.Build.props", `<Project><PropertyGroup><Nullable>enable</Nullable></PropertyGroup></Project>`)
	stackstest.WriteFile(t, root, "src/Pricing/Pricing.csproj", library)
	stackstest.WriteFile(t, root, "src/Pricing/Calculator.cs", "namespace Pricing;\npublic static class Calculator { public static int Add(int a, int b) => a + b; }\n")
	stackstest.WriteFile(t, root, "src/Api/Api.csproj", `<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup><ProjectReference Include="..\Pricing\Pricing.csproj" /></ItemGroup>
</Project>
`)
	stackstest.WriteFile(t, root, "src/Api/Quote.cs", "namespace Api;\npublic static class Quote { public static int Of(int a) => Pricing.Calculator.Add(a, 1); }\n")
	stackstest.WriteFile(t, root, "tests/Pricing.Tests/Pricing.Tests.csproj", `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net8.0</TargetFramework><IsTestProject>true</IsTestProject></PropertyGroup>
  <ItemGroup>
    <PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.6.0" />
    <PackageReference Include="coverlet.collector" Version="6.0.0" />
  </ItemGroup>
  <ItemGroup><ProjectReference Include="..\..\src\Pricing\Pricing.csproj" /></ItemGroup>
</Project>
`)
	stackstest.WriteFile(t, root, "tests/Pricing.Tests/CalculatorTest.cs", "namespace Pricing.Tests;\npublic class CalculatorTest {}\n")
	return root
}

// isolateSDKs keeps a test off the network and off the machine's Putnami home:
// a mismatched stub SDK reaches the check alone, never an installed SDK.
func isolateSDKs(t *testing.T) {
	t.Helper()
	t.Setenv(putnamihome.Env, t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
}

func stack(t *testing.T) stacks.Stack {
	t.Helper()
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func probe(t *testing.T, root string) wsproto.ProbeResult {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
		Paths: []string{".", "docs", "src/Api", "src/Pricing", "tests/Pricing.Tests"}})
	if err != nil {
		t.Fatal(err)
	}
	if diags := wsproto.ValidateProbeResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	return result
}

func TestTheProjectsOfADotnetRepositoryBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "projects-become-projects", "the-projects-of-a-dotnet-repository-become-projects-with-their-references")
	root := fixture(t)
	result := probe(t, root)
	want := map[string]struct {
		name, manifest string
		deps, tasks    []string
	}{
		"src/Api":             {"Api", "src/Api/Api.csproj", []string{"src/Pricing"}, []string{"lint", "build"}},
		"src/Pricing":         {"Pricing", "src/Pricing/Pricing.csproj", nil, []string{"lint", "build"}},
		"tests/Pricing.Tests": {"Pricing.Tests", "tests/Pricing.Tests/Pricing.Tests.csproj", []string{"src/Pricing"}, []string{"lint", "test", "build"}},
	}
	if len(result.Projects) != len(want) {
		t.Fatalf("projects = %+v, diagnostics = %v", result.Projects, result.Diagnostics)
	}
	for _, p := range result.Projects {
		w, ok := want[p.Path]
		if !ok {
			t.Errorf("unexpected project %s", p.Path)
			continue
		}
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if p.SourceName != w.name || !reflect.DeepEqual(p.Dependencies, w.deps) || meta.Component != w.name || meta.Root != "." ||
			meta.Manifest != w.manifest || !reflect.DeepEqual(meta.Tasks, w.tasks) {
			t.Errorf("%s = %+v, metadata %+v", p.Path, p, meta)
		}
		if meta.Toolchain != (stacks.Expected{Version: "8.0.100", Source: "global.json"}) {
			t.Errorf("%s toolchain = %+v", p.Path, meta.Toolchain)
		}
		if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"dotnet"}) {
			t.Errorf("%s extensions %v tags %v", p.Path, p.Extensions, p.Tags)
		}
		for _, f := range []string{w.manifest, "Directory.Build.props", under(p.Path, "Directory.Build.props"), "global.json"} {
			if !slices.Contains(p.WatchedFiles, f) {
				t.Errorf("%s watches no %s: %v", p.Path, f, p.WatchedFiles)
			}
		}
	}
}

func under(dir, name string) string { return dir + "/" + name }

// stubDotnet puts first on PATH a dotnet whose --version prints the first of
// the space-separated versions, the SDK it selects, or, when that first word
// is "none", exits 145 as dotnet does when no SDK satisfies global.json; its
// --list-sdks lists the others. It logs every other call, and for dotnet
// test writes into the results directory the TRX report and the Coverlet
// Cobertura report the real tools write, with trx as the report body, and
// exits 1 when a case failed, as dotnet test does. Every call, a --version
// included, also records its arguments, its environment and whether it
// inherited descriptor credentialFD (started). The real SDK is the stack's own
// build: the stub stands at that boundary.
func stubDotnet(t *testing.T, versions, trx string) string {
	t.Helper()
	isolateSDKs(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "dotnet.log")
	stackstest.WriteFile(t, bin, "trx.xml", trx)
	stackstest.WriteFile(t, bin, "dotnet", `#!/bin/sh
tool=dotnet
`+startedRecord(log)+`set -- "$@"
selected="$(echo "`+versions+`" | cut -d' ' -f1)"
if [ "$1" = "--version" ]; then
  if [ "$selected" = "none" ]; then echo "A compatible .NET SDK was not found." >&2; exit 145; fi
  echo "$selected"; exit 0
fi
if [ "$1" = "--list-sdks" ]; then for v in `+versions+`; do [ "$v" = "none" ] || echo "$v [/usr/share/dotnet/sdk]"; done; exit 0; fi
echo "$PWD: dotnet $*" >> "`+log+`"
if [ "$1" = "test" ]; then
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--results-directory" ]; then
      mkdir -p "$a/3f6c0e9a-run" "$a/_vm_2026-11-08/In/vm"
      cp "`+filepath.Join(bin, "trx.xml")+`" "$a/_vm_2026-11-08.trx"
      covered="$(cd "$PWD/../../src/Pricing" && pwd)/Calculator.cs"
      if [ -z "$STUB_NO_COVERAGE" ]; then
        printf '<coverage line-rate="0.5"><sources><source>/</source></sources><packages><package name="Pricing"><classes><class name="Pricing.Calculator" filename="%s"><lines><line number="1" hits="1"/><line number="2" hits="0"/></lines></class></classes></package></packages></coverage>' "${covered#/}" > "$a/3f6c0e9a-run/coverage.cobertura.xml"
        cp "$a/3f6c0e9a-run/coverage.cobertura.xml" "$a/_vm_2026-11-08/In/vm/coverage.cobertura.xml"
      fi
    fi
    prev="$a"
  done
  if grep -q 'outcome="Failed"' "`+filepath.Join(bin, "trx.xml")+`"; then exit 1; fi
fi
`)
	if err := os.Chmod(filepath.Join(bin, "dotnet"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const trxPass = `<?xml version="1.0" encoding="utf-8"?>
<TestRun id="1" name="run" xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010">
  <Results>
    <UnitTestResult testId="a" testName="Adds" duration="00:00:00.0010000" outcome="Passed" />
    <UnitTestResult testId="b" testName="Rounds" duration="00:00:00.0010000" outcome="Passed" />
  </Results>
  <TestDefinitions>
    <UnitTest id="a"><TestMethod codeBase="/w/tests/Pricing.Tests/bin/Debug/net8.0/Pricing.Tests.dll" className="Pricing.Tests.CalculatorTest" /></UnitTest>
    <UnitTest id="b"><TestMethod codeBase="/w/tests/Pricing.Tests/bin/Debug/net8.0/Pricing.Tests.dll" className="Pricing.Tests.CalculatorTest" /></UnitTest>
  </TestDefinitions>
  <ResultSummary outcome="Completed"><Counters total="2" executed="2" passed="2" failed="0" /></ResultSummary>
</TestRun>`

var trxFail = strings.Replace(strings.Replace(trxPass, `testName="Rounds" duration="00:00:00.0010000" outcome="Passed"`, `testName="Rounds" duration="00:00:00.0010000" outcome="Failed"`, 1),
	`passed="2" failed="0"`, `passed="1" failed="1"`, 1)

// runJob runs a task of the project at rel through the extension's job, the
// way the CLI calls it.
func runJob(t *testing.T, root, rel, task string) (string, map[string]any, string, error) {
	t.Helper()
	s := stack(t)
	result := probe(t, root)
	var meta json.RawMessage
	for _, p := range result.Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"), task)
	job := s.Jobs()[task]
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	status, data, runErr := job(ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	return status, data, out, runErr
}

func TestATaskRunsTheDotnetStepsOnTheProjectFileAfterTheSDKCheck(t *testing.T) {
	spectest.Proves(t, feature, "tasks-run-the-profile", "a-task-runs-the-dotnet-steps-on-the-project-file-after-the-sdk-check")
	root := fixture(t)
	// A folder that holds a solution beside its project needs the file named.
	stackstest.WriteFile(t, root, "src/Api/Api.sln", "")
	log := stubDotnet(t, "8.0.404", trxPass)
	for _, task := range []string{stacks.TaskLint, stacks.TaskBuild} {
		if status, _, _, err := runJob(t, root, "src/Api", task); status != "OK" || err != nil {
			t.Fatalf("%s: %s %v", task, status, err)
		}
	}
	ran, _ := os.ReadFile(log)
	api := filepath.Join(root, "src/Api")
	for _, want := range []string{
		api + ": dotnet format " + filepath.Join(api, "Api.csproj") + " --verify-no-changes",
		api + ": dotnet build " + filepath.Join(api, "Api.csproj"),
	} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("ran\n%s\nwant %q", ran, want)
		}
	}

	// Offline, no SDK is installed: the check decides alone.
	stubDotnet(t, "9.0.100", trxPass)
	if status, _, _, err := runJob(t, root, "src/Api", stacks.TaskBuild); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("an SDK outside the roll-forward policy fails the task: %s %v", status, err)
	}
	// The SDK dotnet selects is checked, not any installed one.
	stubDotnet(t, "9.0.100 8.0.404", trxPass)
	if status, _, _, err := runJob(t, root, "src/Api", stacks.TaskBuild); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("the selected SDK outside the policy fails: %s %v", status, err)
	}
	// dotnet refuses to start when no SDK satisfies global.json: the
	// installed SDKs make it a mismatch, which warn relaxes.
	stubDotnet(t, "none 7.0.410 9.0.100", trxPass)
	if status, _, _, err := runJob(t, root, "src/Api", stacks.TaskBuild); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("no SDK satisfies global.json: %s %v", status, err)
	}
	stackstest.WriteFile(t, root, "src/Api/putnami.json", `{"options": {"@putnami/dotnet": {"toolchain": {"check": "warn"}}}}`)
	if status, _, _, err := runJob(t, root, "src/Api", stacks.TaskBuild); status != "OK" || err != nil {
		t.Errorf("warn relaxes it: %s %v", status, err)
	}
}

func TestAProjectThatIsNoTestProjectRunsNoTest(t *testing.T) {
	spectest.Proves(t, feature, "only-test-projects-test", "a-project-that-is-no-test-project-runs-no-test")
	root := fixture(t)
	// An SDK the check would refuse: a skipped task never reaches it.
	log := stubDotnet(t, "6.0.100", trxPass)
	status, data, _, err := runJob(t, root, "src/Pricing", stacks.TaskTest)
	if status != "OK" || err != nil || data != nil {
		t.Errorf("a library's test task passes with nothing run: %s %v %v", status, err, data)
	}
	if _, err := os.Stat(log); err == nil {
		t.Errorf("a library ran dotnet")
	}
	if status, _, _, err := runJob(t, root, "tests/Pricing.Tests", stacks.TaskTest); status != "FAILED" || err == nil {
		t.Errorf("a test project's test task is checked and run: %s %v", status, err)
	}

	// A project file denies what the nearest Directory.Build.props states.
	stackstest.WriteFile(t, root, "tests/Directory.Build.props", `<Project><ItemGroup><PackageReference Include="Microsoft.NET.Test.Sdk" /></ItemGroup></Project>`)
	stackstest.WriteFile(t, root, "tests/Helpers/Helpers.csproj", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><IsTestProject>false</IsTestProject></PropertyGroup></Project>`)
	stackstest.WriteFile(t, root, "tests/Api.Tests/Api.Tests.csproj", library)
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"tests/Api.Tests", "tests/Helpers"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, p := range result.Projects {
		var meta stacks.Metadata
		_ = json.Unmarshal(p.Metadata, &meta)
		got[p.Path] = meta.Tasks
	}
	if want := map[string][]string{"tests/Api.Tests": {"lint", "test", "build"}, "tests/Helpers": {"lint", "build"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("tasks = %v, want %v", got, want)
	}
}

func TestTRXAndCoberturaBecomeTheTestAndCoverageSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "trx-and-cobertura-become-the-test-and-coverage-summary")
	root := fixture(t)
	log := stubDotnet(t, "8.0.404", trxPass)
	status, data, out, err := runJob(t, root, "tests/Pricing.Tests", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v", status, err)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 2 || got["passed"] != 2 || got["failed"] != 0 {
		t.Errorf("testSummary = %v", got)
	}
	// The attachment copy under the TRX run directory is not read twice.
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 1 || got["total"] != 2 {
		t.Errorf("coverageSummary = %v", got)
	}
	ran, _ := os.ReadFile(log)
	tests := filepath.Join(root, "tests/Pricing.Tests")
	want := tests + ": dotnet test " + filepath.Join(tests, "Pricing.Tests.csproj") + " --logger trx --results-directory " + filepath.Join(out, "test") + " --collect XPlat Code Coverage"
	if !strings.Contains(string(ran), want) {
		t.Errorf("test ran\n%s\nwant %q", ran, want)
	}

	// A test project with no Coverlet collector writes no Cobertura report: it
	// passes, with no coverage.
	t.Setenv("STUB_NO_COVERAGE", "1")
	stubDotnet(t, "8.0.404", trxPass)
	if status, data, _, err := runJob(t, root, "tests/Pricing.Tests", stacks.TaskTest); status != "OK" || err != nil || data["coverageSummary"] != nil || data["testSummary"].(map[string]any)["passed"] != 2 {
		t.Errorf("no Cobertura report: %s %v %v", status, err, data)
	}
	t.Setenv("STUB_NO_COVERAGE", "")

	stubDotnet(t, "8.0.404", trxFail)
	if status, data, _, err := runJob(t, root, "tests/Pricing.Tests", stacks.TaskTest); status != "FAILED" || err == nil || data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("a failed case fails the task: %s %v %v", status, err, data)
	}

	// A test step that writes no report fails: it ran nothing.
	stackstest.WriteFile(t, root, "tests/Pricing.Tests/putnami.json", `{"options": {"@putnami/dotnet": {"tasks": {"test": {"steps": [["dotnet", "build", "{manifest}"]]}}}}}`)
	if status, _, _, err := runJob(t, root, "tests/Pricing.Tests", stacks.TaskTest); status != "FAILED" || err == nil {
		t.Errorf("a test that writes no report fails: %s %v", status, err)
	}
}

func TestTheSDKCheckAppliesTheGlobalJSONRollForwardPolicy(t *testing.T) {
	spectest.Proves(t, feature, "sdk-roll-forward-is-honored", "the-sdk-check-applies-the-global-json-roll-forward-policy")
	root := t.TempDir()
	expected := stacks.Expected{Version: "8.0.400", Source: "global.json"}
	cases := map[string]map[string]bool{
		// No policy is patch.
		"":              {"8.0.400": true, "8.0.404": true, "8.0.399": false, "8.0.500": false, "8.1.400": false, "9.0.100": false},
		"patch":         {"8.0.404": true, "8.0.500": false},
		"latestPatch":   {"8.0.499": true, "8.0.500": false},
		"disable":       {"8.0.400": true, "8.0.401": false},
		"feature":       {"8.0.500": true, "8.0.300": false, "8.1.100": false},
		"latestFeature": {"8.0.612": true, "8.1.100": false},
		"minor":         {"8.1.100": true, "9.0.100": false},
		"latestMinor":   {"8.9.100": true, "9.0.100": false, "7.0.100": false},
		"major":         {"9.0.100": true, "10.0.100-rc.1.25451.107": true, "8.0.399": false, "8.0.400-preview.7": false},
		"LatestMajor":   {"10.0.100": true},
	}
	for policy, machines := range cases {
		doc := `{"sdk": {"version": "8.0.400"}}`
		if policy != "" {
			doc = `{"sdk": {"version": "8.0.400", "rollForward": "` + policy + `"}}`
		}
		stackstest.WriteFile(t, root, "global.json", doc)
		for machine, want := range machines {
			if got := MatchSDK(root, expected, machine); got != want {
				t.Errorf("rollForward %q: MatchSDK(8.0.400, %s) = %v, want %v", policy, machine, got, want)
			}
		}
	}
	// An expected version the profile sets keeps the default policy.
	if !MatchSDK(root, stacks.Expected{Version: "8.0.400", Source: "profile"}, "8.0.401") || MatchSDK(root, stacks.Expected{Version: "8.0.400", Source: "profile"}, "8.0.500") {
		t.Errorf("a profile version keeps the patch policy")
	}
	if MatchSDK(root, stacks.Expected{Version: "latest"}, "8.0.400") || !MatchSDK(root, stacks.Expected{Version: "x"}, "x") {
		t.Errorf("a version that is no SDK version matches only itself")
	}
	// A prerelease orders below its release, and its identifiers in order.
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "9.0.100-rc.1.24452.12", "rollForward": "disable"}}`)
	rc := stacks.Expected{Version: "9.0.100-rc.1.24452.12", Source: "global.json"}
	if MatchSDK(root, rc, "9.0.100-rc.2.24474.11") || !MatchSDK(root, rc, "9.0.100-rc.1.24452.12") {
		t.Errorf("disable needs the same prerelease")
	}
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "9.0.100-rc.1.24452.12"}}`)
	for machine, want := range map[string]bool{"9.0.100-rc.2.24474.11": true, "9.0.100": true, "9.0.100-preview.7.24407.12": false, "9.0.100-rc.1.24452.9": false, "9.0.100-rc.1": false} {
		if got := MatchSDK(root, rc, machine); got != want {
			t.Errorf("MatchSDK(%s, %s) = %v, want %v", rc.Version, machine, got, want)
		}
	}
}

func resolve(t *testing.T, s stacks.Stack, root, project, selector string) []string {
	t.Helper()
	result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
		Inputs: []wsproto.InputResolveQuery{{Project: project, Selector: selector}}})
	if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	if result.Inputs[0].Error != "" {
		t.Fatalf("%s: %s", selector, result.Inputs[0].Error)
	}
	return result.Inputs[0].Files
}

func TestBuildOutputsAndTestSettingsStayOutOfTheLintInputs(t *testing.T) {
	spectest.Proves(t, feature, "inputs-stay-narrow", "build-outputs-and-test-settings-stay-out-of-the-lint-inputs")
	root := fixture(t)
	for _, f := range []string{
		"bin/Debug/net8.0/Pricing.Tests.dll", "obj/Debug/net8.0/Pricing.Tests.AssemblyInfo.cs", "obj/project.assets.json",
		"TestResults/run/coverage.cobertura.xml", "Nested/obj/Gen.g.cs", "node_modules/pkg/index.cs",
		".gen/version.json", ".vs/Pricing/v17/.suo", ".idea/workspace.xml", ".vscode/settings.json", ".DS_Store", "Pricing.Tests.sln.DotSettings.user",
	} {
		stackstest.WriteFile(t, root, "tests/Pricing.Tests/"+f, "generated")
	}
	for f, content := range map[string]string{
		"coverlet.runsettings": "<RunSettings/>", "xunit.runner.json": "{}", "packages.lock.json": "{}",
		"appsettings.json": "{}", "Nested/Case.cs": "", "Protos/greet.proto": "syntax = \"proto3\";",
		"TestData/prices.json": "[]", "Snapshots/Quote.verified.txt": "", "Migrations/.editorconfig": "root = false",
		"BannedSymbols.txt": "", "Pricing.globalconfig": "is_global = true",
	} {
		stackstest.WriteFile(t, root, "tests/Pricing.Tests/"+f, content)
	}
	s := stack(t)

	lint := []string{"BannedSymbols.txt", "CalculatorTest.cs", "Migrations/.editorconfig", "Nested/Case.cs", "Pricing.Tests.csproj", "Pricing.globalconfig", "packages.lock.json"}
	build := []string{"BannedSymbols.txt", "CalculatorTest.cs", "Migrations/.editorconfig", "Nested/Case.cs", "Pricing.Tests.csproj", "Pricing.globalconfig",
		"Protos/greet.proto", "Snapshots/Quote.verified.txt", "TestData/prices.json", "appsettings.json", "packages.lock.json"}
	test := append(slices.Clone(build), "coverlet.runsettings", "xunit.runner.json")
	for selector, files := range map[string][]string{"dotnet:lint": lint, "dotnet:build": build, "dotnet:test": test} {
		slices.Sort(files)
		if got := resolve(t, s, root, "tests/Pricing.Tests", selector); !reflect.DeepEqual(got, files) {
			t.Errorf("%s = %v, want %v", selector, got, files)
		}
	}

	// MSBuild and the analyzers read these files in every directory up to
	// the root: each task's workspace input names them all.
	for _, task := range stacks.TaskNames {
		_, rootPatterns := s.Profile.TaskPatterns(task)
		for _, f := range []string{"tests/Directory.Build.props", "src/Directory.Build.targets", "tests/Directory.Build.rsp", "tests/.editorconfig", "Directory.Packages.props", "tests/NuGet.config", "tests/global.json"} {
			if !slices.ContainsFunc(rootPatterns, func(p string) bool { return stacks.MatchPattern(p, f) }) {
				t.Errorf("%s reads no %s: %v", task, f, rootPatterns)
			}
		}
		// A build copies a project's global.json into bin/: no copy reaches
		// the key.
		includes, excludes := wsproto.SplitFilePatterns(rootPatterns)
		for _, f := range []string{"src/Api/bin/Debug/net8.0/global.json", "src/Api/obj/Directory.Build.props", "tests/Pricing.Tests/TestResults/global.json"} {
			if wsproto.MatchesAnyFilePattern(f, includes) && !wsproto.MatchesAnyFilePattern(f, excludes) {
				t.Errorf("%s reads the build output %s", task, f)
			}
		}
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	stackstest.CheckManifest(t, "putnami.extension.json", stack(t))
	if got := stack(t).Selectors(); !slices.Equal(got, []string{"dotnet:build", "dotnet:lint", "dotnet:test"}) {
		t.Errorf("selectors = %v", got)
	}
}
