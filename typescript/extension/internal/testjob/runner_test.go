package testjob

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/parse"
)

const toolchainFeature = "typescript/typescript-project-toolchain"

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRunnerIsTheParameterElseTheTestScriptElseBun(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "test-runner-choice", "the-runner-is-the-option-else-the-test-script-else-bun")
	cases := []struct {
		name, param, packageJSON string
		want                     Runner
	}{
		{"nothing", "", "", RunnerBun},
		{"the parameter wins over the script", "jest", `{"scripts":{"test":"vitest run"}}`, RunnerJest},
		{"bun wins over the script", "bun", `{"scripts":{"test":"vitest"}}`, RunnerBun},
		{"vitest script", "", `{"scripts":{"test":"vitest run --coverage"}}`, RunnerVitest},
		{"jest script", "", `{"scripts":{"test":"jest"}}`, RunnerJest},
		{"npx jest after an assignment", "", `{"scripts":{"test":"NODE_OPTIONS=--experimental-vm-modules npx jest --ci"}}`, RunnerJest},
		{"pnpm exec vitest", "", `{"scripts":{"test":"pnpm exec vitest run && echo done"}}`, RunnerVitest},
		{"vitest after another command", "", `{"scripts":{"test":"tsc && vitest run"}}`, RunnerVitest},
		{"node runs the jest entry", "", `{"scripts":{"test":"node --experimental-vm-modules node_modules/jest/bin/jest.js"}}`, RunnerJest},
		{"npm run follows the script", "", `{"scripts":{"test":"npm run test:unit","test:unit":"vitest run"}}`, RunnerVitest},
		{"bun run a binary", "", `{"scripts":{"test":"bun run vitest"}}`, RunnerVitest},
		{"dotenv with options", "", `{"scripts":{"test":"dotenv -e .env -- jest"}}`, RunnerJest},
		{"bun test script", "", `{"scripts":{"test":"bun test"}}`, RunnerBun},
		{"a script loop ends", "", `{"scripts":{"test":"npm run test","a":"b"}}`, RunnerBun},
		{"another runner falls back to bun", "", `{"scripts":{"test":"mocha"}}`, RunnerBun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.packageJSON != "" {
				writeFile(t, filepath.Join(dir, "package.json"), tc.packageJSON)
			}
			got, err := ResolveRunner(dir, tc.param)
			if err != nil || got != tc.want {
				t.Fatalf("ResolveRunner = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestResolveRunnerRefusesAnUnknownRunner(t *testing.T) {
	_, err := ResolveRunner(t.TempDir(), "mocha")
	if err == nil || !strings.Contains(err.Error(), "choose bun, vitest or jest") {
		t.Fatalf("ResolveRunner error = %v, want the runners named", err)
	}
}

func stubLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = fn
}

func TestResolveCommandRunsTheBinOfTheProjectOrWorkspaceWithNode(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "test-runner-choice", "vitest-and-jest-run-from-node-modules-bin-with-node")
	stubLookPath(t, func(name string) (string, error) {
		if name != "node" {
			t.Fatalf("looked up %q, want node", name)
		}
		return "/usr/bin/node", nil
	})
	root := t.TempDir()
	project := filepath.Join(root, "packages", "app")
	writeFile(t, filepath.Join(root, "node_modules", "jest", "package.json"), `{"bin":"./bin/jest.js"}`)
	writeFile(t, filepath.Join(project, "node_modules", "vitest", "package.json"), `{"bin":{"vitest":"./vitest.mjs"}}`)
	writeFile(t, filepath.Join(root, "node_modules", "@vitest", "coverage-v8", "package.json"), "{}")
	noBun := func() (string, error) { return "", errors.New("bun must not be resolved") }

	vitest, err := ResolveCommand(RunnerVitest, noBun, project, root)
	if err != nil {
		t.Fatal(err)
	}
	if vitest.Program != "/usr/bin/node" || vitest.Script != filepath.Join(project, "node_modules", "vitest", "vitest.mjs") || vitest.CoverageUnavailable != "" {
		t.Fatalf("vitest command = %+v", vitest)
	}
	jest, err := ResolveCommand(RunnerJest, noBun, project, root)
	if err != nil {
		t.Fatal(err)
	}
	if jest.Program != "/usr/bin/node" || jest.Script != filepath.Join(root, "node_modules", "jest", "bin", "jest.js") {
		t.Fatalf("jest command = %+v", jest)
	}

	// A runner installed above the workspace root is not the workspace's.
	outside := filepath.Join(root, "ws")
	if _, err := ResolveCommand(RunnerJest, noBun, filepath.Join(outside, "pkg"), outside); err == nil || !strings.Contains(err.Error(), "no jest package is installed") {
		t.Fatalf("ResolveCommand above the workspace = %v, want the missing package named", err)
	}
	writeFile(t, filepath.Join(outside, "node_modules", "jest", "package.json"), `{"bin":{"other":"x.js"}}`)
	if _, err := ResolveCommand(RunnerJest, noBun, outside, outside); err == nil || !strings.Contains(err.Error(), "declares no jest bin entry") {
		t.Fatalf("ResolveCommand without a bin entry = %v", err)
	}

	bun, err := ResolveCommand(RunnerBun, func() (string, error) { return "/bun", nil }, project, root)
	if err != nil || bun.Program != "/bun" || bun.Script != "" {
		t.Fatalf("bun command = %+v, %v", bun, err)
	}
}

func TestResolveCommandWithoutNodeFailsAndNamesNode(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "test-runner-choice", "a-missing-node-fails-the-task-and-names-node")
	stubLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "node_modules", "vitest", "package.json"), `{"bin":"vitest.mjs"}`)
	_, err := ResolveCommand(RunnerVitest, nil, root, root)
	if err == nil || !strings.Contains(err.Error(), "no node is on PATH") {
		t.Fatalf("ResolveCommand error = %v, want node named", err)
	}
}

func TestResolveCommandWithoutVitestCoverageProviderRunsWithoutCoverage(t *testing.T) {
	stubLookPath(t, func(string) (string, error) { return "/usr/bin/node", nil })
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "node_modules", "vitest", "package.json"), `{"bin":"vitest.mjs"}`)
	command, err := ResolveCommand(RunnerVitest, nil, root, root)
	if err != nil || !strings.Contains(command.CoverageUnavailable, "@vitest/coverage-v8") {
		t.Fatalf("command = %+v, %v; want the missing provider named", command, err)
	}
}

func stubExec(t *testing.T, fn func(name string, args []string) (*exec.Result, error)) {
	t.Helper()
	orig := execRunFunc
	t.Cleanup(func() { execRunFunc = orig })
	execRunFunc = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) { return fn(name, args) }
}

func flagValue(args []string, prefix string) string {
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, prefix); ok {
			return value
		}
	}
	return ""
}

func TestRunSuiteVitestWritesJUnitAndLCOVIntoTheOutput(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "test-runner-reports", "vitest-writes-junit-and-lcov-into-the-task-output")
	project, output := t.TempDir(), t.TempDir()
	var got []string
	stubExec(t, func(name string, args []string) (*exec.Result, error) {
		got = append([]string{name}, args...)
		writeFile(t, flagValue(args, "--outputFile.junit="), `<?xml version="1.0"?>
<testsuites name="vitest tests" tests="2" failures="1" errors="0" time="0.01">
  <testsuite name="src/math.test.ts" tests="2" failures="1" errors="0" skipped="0" time="0.01">
    <testcase classname="src/math.test.ts" name="math &gt; adds" time="0.001"></testcase>
    <testcase classname="src/math.test.ts" name="math &gt; subtracts" time="0.001"><failure message="expected 1 to be 2" type="AssertionError">AssertionError: expected 1 to be 2</failure></testcase>
  </testsuite>
</testsuites>
`)
		writeFile(t, filepath.Join(flagValue(args, "--coverage.reportsDirectory="), "lcov.info"), "TN:\nSF:src/math.ts\nLF:4\nLH:3\nend_of_record\n")
		return &exec.Result{Success: false, ExitCode: 1, Stdout: "FAIL src/math.test.ts\n"}, nil
	})
	command := Command{Runner: RunnerVitest, Program: "/usr/bin/node", Script: "/ws/node_modules/.bin/vitest"}
	success, logs, err := RunSuite(command, project, output, TestParams{Coverage: true, Test: "adds", Bail: 1, PassWithNoTests: true})
	if err != nil || success || !strings.Contains(logs, "FAIL") {
		t.Fatalf("RunSuite = %v, %q, %v", success, logs, err)
	}
	if got[0] != "/usr/bin/node" || got[1] != command.Script || got[2] != "run" {
		t.Fatalf("command line = %v, want node <vitest> run", got)
	}
	for _, want := range []string{"--reporter=junit", "--coverage.enabled=true", "--coverage.reporter=lcovonly", "--testNamePattern=adds", "--bail=1", "--passWithNoTests"} {
		if !slices.Contains(got, want) {
			t.Errorf("command line %v lacks %s", got, want)
		}
	}
	summary, coverage, _, err := ParseResults(output, project, true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 2 || summary.Failed != 1 || summary.Cases[0].File != "src/math.test.ts" || summary.Cases[0].Name != "math > adds" {
		t.Fatalf("summary = %+v", summary)
	}
	if coverage == nil || coverage.TotalLines != 4 || coverage.CoveredLines != 3 {
		t.Fatalf("coverage = %+v", coverage)
	}
	entries, _ := os.ReadDir(output)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{LCOVFile, JUnitFile}) {
		t.Fatalf("output holds %v, want only the declared reports", names)
	}
}

func TestRunSuiteWithoutCoverageAsksForNone(t *testing.T) {
	output := t.TempDir()
	writeFile(t, filepath.Join(output, LCOVFile), "stale")
	var got []string
	stubExec(t, func(_ string, args []string) (*exec.Result, error) {
		got = args
		return &exec.Result{Success: true}, nil
	})
	command := Command{Runner: RunnerVitest, Program: "node", Script: "vitest", CoverageUnavailable: "none"}
	if _, _, err := RunSuite(command, t.TempDir(), output, TestParams{Coverage: true}); err != nil {
		t.Fatal(err)
	}
	if flagValue(got, "--coverage.enabled=") != "" {
		t.Fatalf("args %v ask for coverage without a provider", got)
	}
	if _, err := os.Stat(filepath.Join(output, LCOVFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stale lcov.info survived the run: %v", err)
	}
}

const jestResultFixture = `{
  "numTotalTests": 4,
  "testResults": [
    {
      "name": "%[1]s/src/sum.test.ts",
      "status": "failed",
      "message": "",
      "startTime": 1000,
      "endTime": 1250,
      "assertionResults": [
        {"ancestorTitles": ["sum"], "title": "adds", "status": "passed", "duration": 3, "failureMessages": [], "location": {"line": 4, "column": 3}},
        {"ancestorTitles": ["sum"], "title": "carries", "status": "failed", "duration": 2, "failureMessages": ["\u001b[31mError: expect(received).toBe(expected)\u001b[39m\n\nExpected: 3\nReceived: 2"], "location": null},
        {"ancestorTitles": [], "title": "later", "status": "todo", "duration": null, "failureMessages": []}
      ]
    },
    {
      "name": "%[1]s/src/broken.test.ts",
      "status": "failed",
      "message": "SyntaxError: Unexpected token",
      "startTime": 1000,
      "endTime": 1010,
      "assertionResults": []
    }
  ]
}`

func TestConvertJestResultWritesJUnit(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "test-runner-reports", "jest-json-results-convert-to-junit")
	project, scratch := t.TempDir(), t.TempDir()
	resultPath := filepath.Join(scratch, "jest.json")
	writeFile(t, resultPath, strings.ReplaceAll(jestResultFixture, "%[1]s", filepath.ToSlash(project)))
	junitPath := filepath.Join(scratch, JUnitFile)
	if err := ConvertJestResult(resultPath, junitPath, project); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(junitPath)
	if err := xml.Unmarshal(data, new(junitDocument)); err != nil {
		t.Fatalf("not XML: %v\n%s", err, data)
	}
	summary, err := parse.ParseJUnitFile(junitPath)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 4 || summary.Passed != 1 || summary.Failed != 2 || summary.Skipped != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	byName := map[string]parse.TestCase{}
	for _, c := range summary.Cases {
		byName[c.Name] = c
	}
	adds := byName["sum > adds"]
	if adds.File != "src/sum.test.ts" || adds.SuiteFile != "src/sum.test.ts" || adds.Line != 4 || adds.Status != parse.TestCasePassed {
		t.Fatalf("adds = %+v", adds)
	}
	carries := byName["sum > carries"]
	if carries.Status != parse.TestCaseFailed || strings.Contains(carries.Output, "\x1b") || !strings.Contains(carries.Output, "Expected: 3") {
		t.Fatalf("carries = %+v", carries)
	}
	if broken := byName["Test suite failed to run"]; broken.File != "src/broken.test.ts" || !strings.Contains(broken.Output, "SyntaxError") {
		t.Fatalf("broken = %+v", broken)
	}
}

func TestRunSuiteJestConvertsItsResultAndCoverage(t *testing.T) {
	project, output := t.TempDir(), t.TempDir()
	var got []string
	stubExec(t, func(_ string, args []string) (*exec.Result, error) {
		got = args
		writeFile(t, flagValue(args, "--outputFile="), strings.ReplaceAll(jestResultFixture, "%[1]s", filepath.ToSlash(project)))
		writeFile(t, filepath.Join(flagValue(args, "--coverageDirectory="), "lcov.info"), "SF:"+filepath.Join(project, "src", "sum.ts")+"\nLF:2\nLH:2\nend_of_record\n")
		return &exec.Result{Success: false, ExitCode: 1}, nil
	})
	command := Command{Runner: RunnerJest, Program: "node", Script: "jest"}
	if _, _, err := RunSuite(command, project, output, TestParams{Coverage: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--json", "--testLocationInResults", "--coverage", "--coverageReporters=lcovonly"} {
		if !slices.Contains(got, want) {
			t.Errorf("args %v lack %s", got, want)
		}
	}
	lcov, _ := os.ReadFile(filepath.Join(output, LCOVFile))
	if !strings.Contains(string(lcov), "SF:src/sum.ts\n") {
		t.Fatalf("lcov.info = %q, want a project-relative source", lcov)
	}
	if _, err := os.Stat(filepath.Join(output, JUnitFile)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "--ci") {
		t.Errorf("args %v force CI mode, which the project's configuration owns", got)
	}
	snapshot := jestArgs("r", "c", false, TestParams{UpdateSnapshots: true})
	if !slices.Contains(snapshot, "--updateSnapshot") {
		t.Fatalf("snapshot update args = %v", snapshot)
	}
}

func TestRunSuiteJestWithAnUnreadableResultFailsWithoutError(t *testing.T) {
	stubExec(t, func(_ string, args []string) (*exec.Result, error) {
		writeFile(t, flagValue(args, "--outputFile="), "not json")
		return &exec.Result{Success: true}, nil
	})
	success, logs, err := RunSuite(Command{Runner: RunnerJest, Program: "node", Script: "jest"}, t.TempDir(), t.TempDir(), TestParams{})
	if err != nil || success || !strings.Contains(logs, "jest --json result") {
		t.Fatalf("RunSuite = %v, %q, %v; want a failed run naming the result", success, logs, err)
	}
}

func TestRunnerConfigurationFilesAreCacheInputs(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "test-runner-reports", "runner-configuration-files-are-cache-inputs")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Commands map[string]struct {
			Flags map[string]any `json:"flags"`
		} `json:"commands"`
		Tasks map[string]struct {
			Inputs map[string]struct {
				From  string   `json:"from"`
				Files []string `json:"files"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, input := range manifest.Tasks["test-run"].Inputs {
		if input.From == "project" {
			declared = append(declared, input.Files...)
		}
	}
	for _, file := range append(RunnerConfigFiles, "**/__tests__/**/*.ts") {
		if !slices.Contains(declared, file) {
			t.Errorf("test-run inputs %v lack %s", declared, file)
		}
	}
	if _, ok := manifest.Commands["test"].Flags["runner"]; !ok {
		t.Error("the test command declares no runner flag")
	}
	for _, task := range []string{"test-run", "test-types"} {
		if input, ok := manifest.Tasks[task].Inputs["runner"]; !ok || input.From != "params" {
			t.Errorf("%s does not key on the runner parameter: %+v", task, input)
		}
	}
}
