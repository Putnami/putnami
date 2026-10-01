package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMachineOutputBudgetsAreFixedByMode(t *testing.T) {
	normal, ok := MachineOutputBudgetFor(MachineOutputModeNormal)
	if !ok {
		t.Fatal("normal machine-output mode has no budget")
	}
	verbose, ok := MachineOutputBudgetFor(MachineOutputModeVerbose)
	if !ok {
		t.Fatal("verbose machine-output mode has no budget")
	}
	wantNormal := MachineOutputBudget{
		MaxBytes:              1 << 20,
		MaxRecords:            1024,
		FailureReserveBytes:   256 << 10,
		FailureReserveRecords: 256,
		FinalReserveBytes:     16 << 10,
		FinalReserveRecords:   1,
	}
	wantVerbose := MachineOutputBudget{
		MaxBytes:              8 << 20,
		MaxRecords:            8192,
		FailureReserveBytes:   2 << 20,
		FailureReserveRecords: 2048,
		FinalReserveBytes:     16 << 10,
		FinalReserveRecords:   1,
	}
	if normal != wantNormal || verbose != wantVerbose {
		t.Fatalf("machine-output budgets drifted\nnormal:  %#v\nverbose: %#v", normal, verbose)
	}
	if _, ok := MachineOutputBudgetFor("custom"); ok {
		t.Fatal("a producer-selected machine-output mode acquired a budget")
	}
}

func TestSelectMachineOutputKeepsHardPartitionsAndArrivalOrder(t *testing.T) {
	ordinary := func(rawBytes int) machineOutputLine {
		return machineOutputLine{
			raw:  bytes.Repeat([]byte{'o'}, rawBytes),
			root: map[string]any{"record": RecordTaskEvent, "event": map[string]any{"level": "info"}},
		}
	}
	failure := func(rawBytes int) machineOutputLine {
		return machineOutputLine{
			raw:  bytes.Repeat([]byte{'f'}, rawBytes),
			root: map[string]any{"record": RecordTaskEvent, "event": map[string]any{"level": "error"}},
		}
	}
	candidates := []machineOutputLine{
		ordinary(9),  // 10 bytes with LF: admitted ordinary.
		ordinary(19), // 20 bytes: cannot borrow the unused failure reserve.
		failure(9),   // 10 bytes: admitted from the protected failure reserve.
		ordinary(4),  // 5 bytes: later fitting ordinary detail remains admissible.
		failure(1),   // failure record quota is already exhausted.
	}
	budget := MachineOutputBudget{
		MaxBytes:              50,
		MaxRecords:            4,
		FailureReserveBytes:   15,
		FailureReserveRecords: 1,
		FinalReserveBytes:     10,
		FinalReserveRecords:   1,
	}
	selected, ordinaryElided, failureElided := selectMachineOutput(candidates, MachineOutputModeNormal, budget)
	if got, want := selected, []machineOutputLine{candidates[0], candidates[2], candidates[3]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected records = %#v, want arrival-ordered %#v", got, want)
	}
	if got, want := ordinaryElided, (MachineOutputElision{Records: 1, Bytes: 20}); got != want {
		t.Errorf("ordinary elision = %#v, want %#v", got, want)
	}
	if got, want := failureElided, (MachineOutputElision{Records: 1, Bytes: 2}); got != want {
		t.Errorf("failure elision = %#v, want %#v", got, want)
	}
}

func TestSelectMachineOutputNormalSuppressesDebugAndVerboseAdmitsIt(t *testing.T) {
	line := func(level string) machineOutputLine {
		return machineOutputLine{
			raw:  []byte(level),
			root: map[string]any{"record": RecordTaskEvent, "event": map[string]any{"level": level}},
		}
	}
	debug := line("debug")
	info := line("info")
	debugFailure := machineOutputLine{
		raw: []byte("debug-failure"),
		root: map[string]any{
			"record": RecordTaskEvent,
			"event":  map[string]any{"type": "diagnostic", "level": "debug", "severity": "error"},
		},
	}
	candidates := []machineOutputLine{debug, info, debugFailure}
	budget := MachineOutputBudget{
		MaxBytes:              100,
		MaxRecords:            10,
		FailureReserveBytes:   20,
		FailureReserveRecords: 2,
		FinalReserveBytes:     10,
		FinalReserveRecords:   1,
	}

	normal, ordinaryElided, failureElided := selectMachineOutput(candidates, MachineOutputModeNormal, budget)
	if want := []machineOutputLine{info, debugFailure}; !reflect.DeepEqual(normal, want) {
		t.Fatalf("normal selected records = %#v, want %#v", normal, want)
	}
	if want := (MachineOutputElision{Records: 1, Bytes: int64(len(debug.raw) + 1)}); ordinaryElided != want {
		t.Errorf("normal ordinary elision = %#v, want %#v", ordinaryElided, want)
	}
	if failureElided != (MachineOutputElision{}) {
		t.Errorf("normal failure elision = %#v, want zero", failureElided)
	}

	verbose, ordinaryElided, failureElided := selectMachineOutput(candidates, MachineOutputModeVerbose, budget)
	if !reflect.DeepEqual(verbose, candidates) {
		t.Fatalf("verbose selected records = %#v, want all %#v", verbose, candidates)
	}
	if ordinaryElided != (MachineOutputElision{}) || failureElided != (MachineOutputElision{}) {
		t.Errorf("verbose elisions = ordinary %#v, failure %#v; want zero", ordinaryElided, failureElided)
	}
}

func TestMachineOutputFailurePriorityVocabulary(t *testing.T) {
	tests := []struct {
		name   string
		record SessionStreamRecord
		want   bool
	}{
		{"failed task", SessionStreamRecord{Record: RecordTaskEnd, Task: &TaskRecord{Status: TaskStatusFailed}}, true},
		{"canceled task", SessionStreamRecord{Record: RecordTaskEnd, Task: &TaskRecord{Status: TaskStatusCanceled}}, true},
		{"successful task", SessionStreamRecord{Record: RecordTaskEnd, Task: &TaskRecord{Status: TaskStatusSuccess}}, false},
		{"error level", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"level": "error"}}, true},
		{"diagnostic error", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "diagnostic", "severity": "error"}}, true},
		{"failed phase", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "phase", "status": "failed"}}, true},
		{"failed result", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "result", "data": map[string]any{"status": "FAILED"}}}, true},
		{"failed normalized result", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "result", "status": "FAILED"}}, true},
		{"successful release-set result", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"v": 1, "type": "result", "data": map[string]any{"status": "OK", "data": map[string]any{"releaseSet": map[string]any{}}}}}, true},
		{"ordinary successful result", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"v": 1, "type": "result", "data": map[string]any{"status": "OK"}}}, false},
		{"debug detail", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "log", "level": "debug"}}, false},
		{"debug error diagnostic", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "diagnostic", "level": "debug", "severity": "error"}}, true},
		{"warning diagnostic", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "diagnostic", "severity": "warning"}}, false},
		{"failed test case", SessionStreamRecord{Record: RecordTestCase, TestCase: &TestCase{Status: TestCaseStatusFailed}}, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsMachineOutputFailurePriority(testCase.record); got != testCase.want {
				t.Errorf("IsMachineOutputFailurePriority() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestMachineOutputDebugDetailVocabulary(t *testing.T) {
	tests := []struct {
		name   string
		record SessionStreamRecord
		want   bool
	}{
		{"debug task event", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "log", "level": "debug"}}, true},
		{"debug error diagnostic", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "diagnostic", "level": "debug", "severity": "error"}}, true},
		{"info task event", SessionStreamRecord{Record: RecordTaskEvent, Event: map[string]any{"type": "log", "level": "info"}}, false},
		{"debug task end", SessionStreamRecord{Record: RecordTaskEnd, Event: map[string]any{"level": "debug"}}, false},
		{"passed test case", SessionStreamRecord{Record: RecordTestCase, TestCase: &TestCase{Status: TestCaseStatusPassed}}, true},
		{"skipped test case", SessionStreamRecord{Record: RecordTestCase, TestCase: &TestCase{Status: TestCaseStatusSkipped}}, true},
		{"failed test case", SessionStreamRecord{Record: RecordTestCase, TestCase: &TestCase{Status: TestCaseStatusFailed}}, true},
		{"test case without its payload", SessionStreamRecord{Record: RecordTestCase}, true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsMachineOutputDebugDetail(testCase.record); got != testCase.want {
				t.Errorf("IsMachineOutputDebugDetail() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestMachineOutputClassifiersAgreeOnDecodedRecords pins the producer's
// classifiers to the decoded-record twins whole-stream validation recomputes
// the selection with. A disagreement would make the CLI's own bounded stream
// fail ValidateSessionStream.
func TestMachineOutputClassifiersAgreeOnDecodedRecords(t *testing.T) {
	records := []SessionStreamRecord{
		{Record: RecordTestCase, TestCase: &TestCase{Name: "TestA", Suite: "a", Status: TestCaseStatusPassed}},
		{Record: RecordTestCase, TestCase: &TestCase{Name: "TestA", Suite: "a", Status: TestCaseStatusSkipped, Output: "later"}},
		{Record: RecordTestCase, TestCase: &TestCase{Name: "TestA", Suite: "a", Status: TestCaseStatusFailed, Output: "boom"}},
		{Record: RecordTestCase},
		{Record: RecordTaskEnd, Task: &TaskRecord{Status: TaskStatusFailed, TestCasesDropped: 3}},
		{Record: RecordTaskEvent, Event: map[string]any{"type": "log", "level": "debug"}},
		{Record: RecordTaskEvent, Event: map[string]any{"type": "log", "level": "error"}},
	}
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]any
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
		if got, want := debugDetailRecord(root), IsMachineOutputDebugDetail(record); got != want {
			t.Errorf("%s: debugDetailRecord = %v, IsMachineOutputDebugDetail = %v", data, got, want)
		}
		if got, want := failurePriorityRecord(root), IsMachineOutputFailurePriority(record); got != want {
			t.Errorf("%s: failurePriorityRecord = %v, IsMachineOutputFailurePriority = %v", data, got, want)
		}
	}
}

func TestSelectMachineOutputTreatsEveryTestCaseAsDebugDetail(t *testing.T) {
	line := func(status string) machineOutputLine {
		return machineOutputLine{
			raw:  []byte(status),
			root: map[string]any{"record": RecordTestCase, "testCase": map[string]any{"status": status}},
		}
	}
	passed, skipped, failed := line(TestCaseStatusPassed), line(TestCaseStatusSkipped), line(TestCaseStatusFailed)
	candidates := []machineOutputLine{passed, skipped, failed}
	budget := MachineOutputBudget{
		MaxBytes:              100,
		MaxRecords:            10,
		FailureReserveBytes:   20,
		FailureReserveRecords: 2,
		FinalReserveBytes:     10,
		FinalReserveRecords:   1,
	}

	normal, ordinaryElided, failureElided := selectMachineOutput(candidates, MachineOutputModeNormal, budget)
	if len(normal) != 0 {
		t.Fatalf("normal selected records = %#v, want none", normal)
	}
	wantElided := MachineOutputElision{Records: 3, Bytes: int64(len(passed.raw) + 1 + len(skipped.raw) + 1 + len(failed.raw) + 1)}
	if ordinaryElided != wantElided || failureElided != (MachineOutputElision{}) {
		t.Errorf("normal elisions = ordinary %#v, failure %#v; want ordinary %#v and no failure elision", ordinaryElided, failureElided, wantElided)
	}

	verbose, ordinaryElided, failureElided := selectMachineOutput(candidates, MachineOutputModeVerbose, budget)
	if !reflect.DeepEqual(verbose, candidates) {
		t.Fatalf("verbose selected records = %#v, want every case", verbose)
	}
	if ordinaryElided != (MachineOutputElision{}) || failureElided != (MachineOutputElision{}) {
		t.Errorf("verbose elisions = ordinary %#v, failure %#v; want zero", ordinaryElided, failureElided)
	}
}

func TestSanitizeMachineOutputValue(t *testing.T) {
	secretValues := []string{
		"AK" + "IA" + strings.Repeat("A", 16),
		"AS" + "IA" + strings.Repeat("B", 16),
		"gh" + "p_" + strings.Repeat("a", 36),
		"github" + "_pat_" + strings.Repeat("b", 20),
		"AI" + "za" + strings.Repeat("c", 35),
		"xo" + "xb-" + strings.Repeat("d", 10),
		"sk_" + "live_" + strings.Repeat("e", 16),
		"-----BEGIN " + "PRIVATE KEY-----\nbody",
	}
	for i, secret := range secretValues {
		if got := SanitizeMachineOutputString("before " + secret + " after"); got != "before [REDACTED] after" && got != "[REDACTED]" {
			t.Errorf("secret pattern %d was not redacted: %q", i, got)
		}
	}

	input := map[string]any{
		"Access-Token": "value",
		"label\x00":    "member name retained",
		"nested": map[string]any{
			"client_secret": "value",
			"message":       "\x1b[31mred\x1b[0m\x00ok\u0085\t\n\r",
		},
	}
	want := map[string]any{
		"Access-Token": "[REDACTED]",
		"label\x00":    "member name retained",
		"nested": map[string]any{
			"client_secret": "[REDACTED]",
			"message":       "red\uFFFDok\uFFFD\t\n\r",
		},
	}
	if got := SanitizeMachineOutputValue(input); !reflect.DeepEqual(got, want) {
		t.Errorf("SanitizeMachineOutputValue() = %#v, want %#v", got, want)
	}
	if got := SanitizeMachineOutputString("\x1b]8;;https://example.invalid\ahelp\x1b]8;;\a"); got != "help" {
		t.Errorf("OSC hyperlink sanitization = %q, want help", got)
	}
	if got := SanitizeMachineOutputString("\u009b31mred\u009b0m"); got != "red" {
		t.Errorf("eight-bit CSI sanitization = %q, want red", got)
	}
	if got := SanitizeMachineOutputString("\u009dtitle\u009chelp"); got != "help" {
		t.Errorf("eight-bit OSC sanitization = %q, want help", got)
	}
	invalidUTF8 := string([]byte{'a', 0xff, 'b'})
	if got := SanitizeMachineOutputString(invalidUTF8); got != "a\uFFFDb" {
		t.Errorf("invalid UTF-8 sanitization = %q, want replacement rune", got)
	}
}

func TestUnpairedJSONSurrogateDetection(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{`{"value":"\ud800"}`, true},
		{`{"value":"\udfff"}`, true},
		{`{"value":"\ud800x"}`, true},
		{`{"value":"\ud83d\ude00"}`, false},
		{`{"value":"\\ud800"}`, false},
		{`{"value":"plain"}`, false},
	}
	for _, testCase := range tests {
		if got := hasUnpairedJSONSurrogate([]byte(testCase.raw)); got != testCase.want {
			t.Errorf("hasUnpairedJSONSurrogate(%q) = %v, want %v", testCase.raw, got, testCase.want)
		}
	}
}

func TestValidateSessionStreamRejectsRawInvalidUTF8(t *testing.T) {
	final := testBoundedFinalLine(t)
	invalid := []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'}
	live := append(append([]byte(nil), invalid...), final...)
	artifact := append(append([]byte(nil), invalid...), final...)
	want := []Violation{
		{Code: ViolationUnsanitized, Path: "artifact[0]"},
		{Code: ViolationUnsanitized, Path: "live[0]"},
	}
	if got := ValidateSessionStream(live, artifact); !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidateSessionStream() = %#v, want %#v", got, want)
	}
}

func testBoundedFinalLine(t *testing.T) []byte {
	t.Helper()
	budget, ok := MachineOutputBudgetFor(MachineOutputModeNormal)
	if !ok {
		t.Fatal("normal machine-output mode has no budget")
	}
	record := BoundedSessionEndRecord{
		ProtocolVersion: ResultProtocolVersion,
		Record:          RecordSessionEnd,
		Time:            "2026-08-16T12:00:01Z",
		Run: StreamRunSummary{
			Outcome:    RunOutcomeSuccess,
			ExitCode:   ExitSuccess,
			Counts:     RunCounts{},
			Reuse:      RunReuse{},
			DurationMs: 1,
		},
		MachineOutput: MachineOutputSummary{
			Mode:         MachineOutputModeNormal,
			Sanitization: MachineOutputSanitizationV1,
			Budget:       budget,
			Elided:       MachineOutputElisions{},
			Artifact: MachineOutputArtifact{
				SessionID: "20260816-120000-a1b2c3",
				Path:      MachineOutputArtifactPath,
				Retention: MachineOutputArtifactRetentionSession,
			},
		},
	}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal bounded final: %v", err)
	}
	return append(line, '\n')
}
