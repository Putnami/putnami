package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	protocolruntime "go.putnami.dev/protocol/runtime"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// caseLine encodes one plugin record the way putnami_testcases.py writes it.
func caseLine(t *testing.T, record map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func writeCases(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func casesByName(cases []protocolcli.TestCase) map[string]protocolcli.TestCase {
	byName := make(map[string]protocolcli.TestCase, len(cases))
	for _, c := range cases {
		byName[c.Name] = c
	}
	return byName
}

func TestReadTestCasesConvertsPluginRecords(t *testing.T) {
	wsRoot := t.TempDir()
	testFile := filepath.Join(wsRoot, "pkg", "tests", "test_a.py")
	failure := "def test_fails():\n>       assert 1 == 2\nE       assert 1 == 2\n\npkg/tests/test_a.py:4: AssertionError"
	captured := "----- Captured stdout call -----\nprinted before failing"
	casesFile := filepath.Join(t.TempDir(), testCasesFileName)
	writeCases(t, casesFile,
		caseLine(t, map[string]any{"name": "test_fails", "status": "failed", "duration": 0.0125,
			"output": failure, "captured": captured, "file": testFile, "line": 3}),
		caseLine(t, map[string]any{"name": "test_fails_loudly", "status": "failed", "duration": 0.001,
			"output": "E       assert False", "captured": strings.Repeat("x", protocolcli.TestCaseMaxOutputBytes), "file": testFile, "line": 7}),
		caseLine(t, map[string]any{"name": "test_skipped", "status": "skipped", "duration": 0,
			"output": "Skipped: deliberate", "file": testFile, "line": 10}),
		caseLine(t, map[string]any{"name": "test_passes", "status": "passed", "duration": 0.2,
			"output": "a passed case carries no output", "file": testFile, "line": 14}),
		caseLine(t, map[string]any{"name": "TestGroup::test_param[a-1]", "status": "passed", "duration": -1,
			"output": "", "file": testFile}),
		caseLine(t, map[string]any{"name": "test_elsewhere", "status": "passed", "duration": 0,
			"output": "", "file": filepath.Join(filepath.Dir(wsRoot), "elsewhere", "test_b.py"), "line": 1}),
		"{not json\n",
		`{"name": "test_cut", "status": "passed", "dura`,
	)

	cases, dropped := readTestCases(casesFile, wsRoot, protocolcli.TestCaseMaxBytesPerTask)

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1 (the case outside the workspace)", dropped)
	}
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
	}
	want := []string{"test_fails", "test_fails_loudly", "test_skipped", "test_passes", "TestGroup::test_param[a-1]"}
	if !slices.Equal(names, want) {
		t.Fatalf("case names = %v, want %v in runner order", names, want)
	}
	byName := casesByName(cases)

	fails := byName["test_fails"]
	if fails.Status != protocolcli.TestCaseStatusFailed || fails.DurationMs != 13 {
		t.Errorf("test_fails = %+v, want failed in 13 ms", fails)
	}
	if fails.Suite != "pkg/tests/test_a.py" || fails.File != "pkg/tests/test_a.py" || fails.Line != 3 {
		t.Errorf("test_fails location = (%q, %q, %d), want the workspace-relative file at line 3", fails.Suite, fails.File, fails.Line)
	}
	if fails.Output != failure+"\n\n"+captured || fails.OutputTruncated {
		t.Errorf("test_fails output = %q, want the failure then what it printed", fails.Output)
	}

	loud := byName["test_fails_loudly"]
	if loud.Output != "E       assert False" || loud.OutputTruncated {
		t.Errorf("test_fails_loudly output = %q; printed output that does not fit must not displace the failure", loud.Output)
	}

	skipped := byName["test_skipped"]
	if skipped.Status != protocolcli.TestCaseStatusSkipped || skipped.Output != "Skipped: deliberate" {
		t.Errorf("test_skipped = %+v, want skipped with its reason", skipped)
	}

	passes := byName["test_passes"]
	if passes.Output != "" || passes.DurationMs != 200 {
		t.Errorf("test_passes = %+v, want no output and 200 ms", passes)
	}

	param := byName["TestGroup::test_param[a-1]"]
	if param.Line != 0 || param.DurationMs != 0 || param.Suite != "pkg/tests/test_a.py" {
		t.Errorf("parametrized case = %+v, want no line, a zero duration and the file as suite", param)
	}
}

func TestReadTestCasesTruncatesLongOutput(t *testing.T) {
	wsRoot := t.TempDir()
	casesFile := filepath.Join(t.TempDir(), testCasesFileName)
	writeCases(t, casesFile, caseLine(t, map[string]any{
		"name": "test_huge", "status": "failed", "duration": 0,
		"output": strings.Repeat("y", 3*protocolcli.TestCaseMaxOutputBytes),
		"file":   filepath.Join(wsRoot, "tests", "test_a.py"),
	}))

	cases, dropped := readTestCases(casesFile, wsRoot, protocolcli.TestCaseMaxBytesPerTask)
	if dropped != 0 || len(cases) != 1 {
		t.Fatalf("cases = %+v dropped = %d, want one case", cases, dropped)
	}
	if !cases[0].OutputTruncated || len(cases[0].Output) > protocolcli.TestCaseMaxOutputBytes {
		t.Errorf("output of %d bytes, truncated = %t; want it bounded and marked", len(cases[0].Output), cases[0].OutputTruncated)
	}
}

func TestReadTestCasesMissingFileReportsNothing(t *testing.T) {
	cases, dropped := readTestCases(filepath.Join(t.TempDir(), "absent.jsonl"), t.TempDir(), protocolcli.TestCaseMaxBytesPerTask)
	if cases != nil || dropped != 0 {
		t.Fatalf("missing file = (%v, %d), want nothing", cases, dropped)
	}
}

func TestReadTestCasesResolvesSymlinkedWorkspace(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symbolic links unavailable here: %v", err)
	}
	testFile := filepath.Join(realRoot, "pkg", "tests", "test_a.py")
	if err := os.MkdirAll(filepath.Dir(testFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testFile, []byte("def test_a():\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	casesFile := filepath.Join(t.TempDir(), testCasesFileName)
	writeCases(t, casesFile, caseLine(t, map[string]any{
		"name": "test_a", "status": "passed", "duration": 0, "output": "", "file": testFile, "line": 1,
	}))

	cases, dropped := readTestCases(casesFile, linkRoot, protocolcli.TestCaseMaxBytesPerTask)
	if dropped != 0 || len(cases) != 1 || cases[0].Suite != "pkg/tests/test_a.py" {
		t.Fatalf("cases = %+v dropped = %d, want the file relative to the linked workspace", cases, dropped)
	}
}

func TestAttachTestCases(t *testing.T) {
	if got := attachTestCases(nil, nil, 0); got != nil {
		t.Errorf("no cases = %v, want the payload untouched", got)
	}
	summary := map[string]any{"testSummary": map[string]any{"total": 1}}
	if got := attachTestCases(summary, nil, 0); len(got) != 1 {
		t.Errorf("no cases = %v, want only testSummary", got)
	}

	cases := []protocolcli.TestCase{{Name: "test_a", Suite: "tests/test_a.py", Status: protocolcli.TestCaseStatusPassed}}
	got := attachTestCases(summary, cases, 0)
	if _, ok := got[protocolruntime.TestCasesDroppedResultDataKey]; ok {
		t.Errorf("payload = %v; testCasesDropped must be absent when nothing was dropped", got)
	}
	if kept, ok := got[protocolruntime.TestCasesResultDataKey].([]protocolcli.TestCase); !ok || len(kept) != 1 {
		t.Errorf("testCases = %#v, want the kept cases", got[protocolruntime.TestCasesResultDataKey])
	}
	if _, ok := got["testSummary"]; !ok {
		t.Error("attaching cases lost testSummary")
	}

	onlyDropped := attachTestCases(nil, nil, 3)
	if onlyDropped[protocolruntime.TestCasesDroppedResultDataKey] != 3 {
		t.Errorf("payload = %v, want testCasesDropped = 3", onlyDropped)
	}
	if _, ok := onlyDropped[protocolruntime.TestCasesResultDataKey]; ok {
		t.Errorf("payload = %v, want no testCases when none was kept", onlyDropped)
	}
}

func TestTestCaseSinkNilReportsNothing(t *testing.T) {
	var sink *testCaseSink
	if args := sink.pytestArgs(); args != nil {
		t.Errorf("nil sink args = %v, want none", args)
	}
	extra := map[string]string{"PYTHONPATH": "/project"}
	sink.decorateEnv(extra)
	if len(extra) != 1 || extra["PYTHONPATH"] != "/project" {
		t.Errorf("nil sink changed the environment: %v", extra)
	}
	sink.reset()
	if cases, dropped := sink.read("/workspace", protocolcli.TestCaseMaxBytesPerTask); cases != nil || dropped != 0 {
		t.Errorf("nil sink read = (%v, %d)", cases, dropped)
	}
	sink.remove()
}

func TestProvisionTestCasesMaterializesPlugin(t *testing.T) {
	sink, err := provisionTestCases()
	if err != nil {
		t.Fatal(err)
	}
	directory := sink.dir.Path()
	module, err := os.ReadFile(filepath.Join(directory, testCasePluginModule+".py"))
	if err != nil || !bytes.Equal(module, testCasePluginSource) {
		t.Fatalf("plugin module = (%d bytes, %v), want the embedded source", len(module), err)
	}
	if info, err := os.Stat(sink.file); err != nil || info.Size() != 0 {
		t.Fatalf("cases file = (%v, %v), want an empty file", info, err)
	}
	if args := sink.pytestArgs(); !slices.Equal(args, []string{"-p", "putnami_testcases"}) {
		t.Errorf("args = %v", args)
	}
	extra := map[string]string{"PYTHONPATH": "/project"}
	sink.decorateEnv(extra)
	if extra[testCasesFileEnv] != sink.file {
		t.Errorf("%s = %q, want %q", testCasesFileEnv, extra[testCasesFileEnv], sink.file)
	}
	if extra["PYTHONPATH"] != directory+string(os.PathListSeparator)+"/project" {
		t.Errorf("PYTHONPATH = %q, want the plugin directory before the project", extra["PYTHONPATH"])
	}

	writeCases(t, sink.file, "stale\n")
	sink.reset()
	if info, err := os.Stat(sink.file); err != nil || info.Size() != 0 || sink.stale {
		t.Fatalf("reset left (%v, %v, stale %t), want an empty file", info, err, sink.stale)
	}

	sink.remove()
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("remove left the scratch directory (stat err = %v)", err)
	}
}

// fakePytestCases stands in for the plugin: it appends records to the file the
// adapter named in the pytest environment and checks the plugin is loaded.
func fakePytestCases(t *testing.T, args, env []string, records ...map[string]any) {
	t.Helper()
	path := specEnvValue(env, testCasesFileEnv)
	if path == "" {
		t.Error("the pytest process received no test case file")
		return
	}
	loaded := false
	for index, arg := range args {
		if arg == "-p" && index+1 < len(args) && args[index+1] == testCasePluginModule {
			loaded = true
		}
	}
	if !loaded {
		t.Errorf("pytest args %v do not load the test case plugin", args)
	}
	if !strings.Contains(specEnvValue(env, "PYTHONPATH"), filepath.Dir(path)) {
		t.Errorf("PYTHONPATH %q does not carry the plugin directory", specEnvValue(env, "PYTHONPATH"))
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer file.Close()
	for _, record := range records {
		if _, err := file.WriteString(caseLine(t, record)); err != nil {
			t.Error(err)
		}
	}
}

func TestTestBatchReportsTestCasesPerMember(t *testing.T) {
	stubSync(t)
	ctx := makeTestBatchWorkspace(t)
	orig := pytestCommandRunner
	t.Cleanup(func() { pytestCommandRunner = orig })
	pytestCommandRunner = func(_ context.Context, _ string, args []string, dir string, env []string) (string, error) {
		member := filepath.Base(dir)
		fakePytestCases(t, args, env, map[string]any{
			"name": "test_" + member, "status": "passed", "duration": 0.004, "output": "",
			"file": filepath.Join(dir, "tests", "test_x.py"), "line": 1,
		})
		return "1 passed in 0.01s", nil
	}

	status, data, err := TestBatch(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("TestBatch = (%q, %v)", status, err)
	}
	results := data["batchResults"].([]pyTestBatchProjectResult)
	for index, member := range []string{"a", "b"} {
		memberData := results[index].Data
		cases, ok := memberData[protocolruntime.TestCasesResultDataKey].([]protocolcli.TestCase)
		if !ok || len(cases) != 1 {
			t.Fatalf("%s testCases = %#v, want exactly its own case", member, memberData[protocolruntime.TestCasesResultDataKey])
		}
		want := protocolcli.TestCase{
			Name: "test_" + member, Suite: member + "/tests/test_x.py", Status: protocolcli.TestCaseStatusPassed,
			DurationMs: 4, File: member + "/tests/test_x.py", Line: 1,
		}
		if cases[0] != want {
			t.Errorf("%s case = %+v, want %+v", member, cases[0], want)
		}
		if _, ok := memberData[protocolruntime.TestCasesDroppedResultDataKey]; ok {
			t.Errorf("%s carries testCasesDropped with nothing dropped", member)
		}
		if _, ok := memberData["testSummary"]; !ok {
			t.Errorf("%s lost its testSummary", member)
		}
	}
}

func TestTestBatchRespawnReportsOnlyItsOwnCases(t *testing.T) {
	stubSync(t)
	ctx := makeTestBatchWorkspace(t)
	attempts := 0
	orig := pytestCommandRunner
	t.Cleanup(func() { pytestCommandRunner = orig })
	pytestCommandRunner = func(_ context.Context, _ string, args []string, dir string, env []string) (string, error) {
		if filepath.Base(dir) != "a" {
			return "1 passed in 0.01s", nil
		}
		attempts++
		record := map[string]any{
			"name": "test_attempt_" + strconv.Itoa(attempts), "status": "passed", "duration": 0, "output": "",
			"file": filepath.Join(dir, "tests", "test_x.py"),
		}
		fakePytestCases(t, args, env, record)
		if attempts == 1 {
			return "Traceback: internal error", errors.New("exit status 2")
		}
		return "1 passed in 0.01s", nil
	}

	_, data, err := TestBatch(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want one respawn", attempts)
	}
	results := data["batchResults"].([]pyTestBatchProjectResult)
	cases, _ := results[0].Data[protocolruntime.TestCasesResultDataKey].([]protocolcli.TestCase)
	if len(cases) != 1 || cases[0].Name != "test_attempt_2" {
		t.Fatalf("respawned member cases = %+v, want only the second attempt's", cases)
	}
	if _, ok := results[1].Data[protocolruntime.TestCasesResultDataKey]; ok {
		t.Errorf("member b reports cases its pytest never recorded: %v", results[1].Data)
	}
}

func TestTestReportsTestCasesInResultData(t *testing.T) {
	stubSync(t)
	wsRoot := t.TempDir()
	projectRoot := filepath.Join(wsRoot, "pkg")
	testFile := filepath.Join(projectRoot, "tests", "test_x.py")
	if err := os.MkdirAll(filepath.Dir(testFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testFile, []byte("def test_ok():\n    pass\n\n\ndef test_ko():\n    assert False\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := pytestCommandRunner
	t.Cleanup(func() { pytestCommandRunner = orig })
	pytestCommandRunner = func(_ context.Context, _ string, args []string, _ string, env []string) (string, error) {
		fakePytestCases(t, args, env,
			map[string]any{"name": "test_ok", "status": "passed", "duration": 0.001, "output": "", "file": testFile, "line": 1},
			map[string]any{"name": "test_ko", "status": "failed", "duration": 0.002, "output": "E       assert False", "file": testFile, "line": 5},
		)
		return "1 failed, 1 passed in 0.01s", errors.New("exit status 1")
	}
	ctx := &pctx.Context{
		WorkspaceRoot: wsRoot,
		Project:       pctx.Project{Name: "pkg", Path: "pkg", FullPath: projectRoot},
		Params:        pctx.Params{},
	}

	var status string
	var data map[string]any
	captureEvents(t, func(emit *jsonl.Emitter) {
		var err error
		status, data, err = Test(ctx, emit, nil)
		if err != nil {
			t.Fatal(err)
		}
	})

	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if _, ok := data["testSummary"]; !ok {
		t.Fatalf("result data %v lost testSummary", data)
	}
	cases, ok := data[protocolruntime.TestCasesResultDataKey].([]protocolcli.TestCase)
	if !ok || len(cases) != 2 {
		t.Fatalf("testCases = %#v, want both cases", data[protocolruntime.TestCasesResultDataKey])
	}
	// The bound never reorders fewer cases than it keeps: runner order holds.
	if cases[0].Name != "test_ok" || cases[1].Name != "test_ko" {
		t.Fatalf("cases = %+v, want runner order", cases)
	}
	if cases[1].Status != protocolcli.TestCaseStatusFailed || cases[1].Output != "E       assert False" ||
		cases[1].Suite != "pkg/tests/test_x.py" || cases[1].Line != 5 {
		t.Errorf("failed case = %+v", cases[1])
	}
	if _, ok := data[protocolruntime.TestCasesDroppedResultDataKey]; ok {
		t.Errorf("result data carries testCasesDropped with nothing dropped: %v", data)
	}
}

func TestReadTestCasesTakesTheSuiteFromTheCollectingModule(t *testing.T) {
	wsRoot := t.TempDir()
	casesFile := filepath.Join(t.TempDir(), testCasesFileName)
	writeCases(t, casesFile,
		caseLine(t, map[string]any{"name": "TestInherited::test_shared", "status": "passed", "duration": 0,
			"module": filepath.Join(wsRoot, "pkg", "tests", "test_inherits.py"),
			"file":   filepath.Join(wsRoot, "pkg", "tests", "base_cases.py"), "line": 2}),
		caseLine(t, map[string]any{"name": "test_outside", "status": "passed", "duration": 0,
			"module": filepath.Join(filepath.Dir(wsRoot), "elsewhere", "test_b.py"),
			"file":   filepath.Join(wsRoot, "pkg", "tests", "base_cases.py"), "line": 2}),
	)

	cases, dropped := readTestCases(casesFile, wsRoot, protocolcli.TestCaseMaxBytesPerTask)
	if len(cases) != 1 || dropped != 1 {
		t.Fatalf("cases = %+v, dropped = %d; want the inherited case and the outside one dropped", cases, dropped)
	}
	if got := cases[0]; got.Suite != "pkg/tests/test_inherits.py" || got.File != "pkg/tests/base_cases.py" || got.Line != 2 {
		t.Errorf("case = %+v, want suite pkg/tests/test_inherits.py declared at pkg/tests/base_cases.py:2", got)
	}
}
