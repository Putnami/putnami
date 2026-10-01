package cli

import (
	"reflect"
	"testing"
)

// The cross-language corpus (conformance/manifest.json) owns the contract's
// behavior. These tests cover the Go-only edges the corpus cannot express: an
// unknown document kind, malformed containers, and the derived helpers
// producers use.

func TestValidateDocument_UnknownKind(t *testing.T) {
	got := ValidateDocument(DocumentKind("planFile"), []byte(`{}`))
	want := []Violation{{Code: ViolationInvalidJSON, Path: ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument(unknown kind) = %v, want %v", got, want)
	}
}

func TestValidateDocument_Malformed(t *testing.T) {
	for name, data := range map[string]string{
		"truncated":  `{"protocolVersion":2`,
		"bare-value": `2`,
		"null":       `null`,
	} {
		t.Run(name, func(t *testing.T) {
			got := ValidateDocument(DocumentResultEnvelope, []byte(data))
			want := []Violation{{Code: ViolationInvalidJSON, Path: ""}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ValidateDocument(%s) = %v, want %v", data, got, want)
			}
		})
	}
}

// TestValidateDocument_WrongContainerTypes covers the invalid_type paths for
// every container check: an object, an array, and the open payload another
// protocol owns.
func TestValidateDocument_WrongContainerTypes(t *testing.T) {
	cases := map[string]struct {
		kind     DocumentKind
		document string
		want     []Violation
	}{
		"identity not an object": {
			kind:     DocumentSessionStreamRecord,
			document: `{"protocolVersion":2,"record":"task:start","time":"t","identity":"/a:b"}`,
			want:     []Violation{{Code: ViolationInvalidType, Path: "identity"}},
		},
		"event not an object": {
			kind: DocumentSessionStreamRecord,
			document: `{"protocolVersion":2,"record":"task:event","time":"t","event":[],` +
				`"identity":{"key":"/a:b","scope":"project","project":{"id":"/a","name":"a"},` +
				`"task":{"name":"b","command":"b","kind":"b"},"provider":{"extension":"e"}}}`,
			want: []Violation{{Code: ViolationInvalidType, Path: "event"}},
		},
		"commands not an array": {
			kind:     DocumentMCPResult,
			document: `{"protocolVersion":2,"tool":"plan_jobs","commands":"build","plan":{"dryRun":true,"metrics":{"tasks":0,"edges":0,"projects":0},"tasks":[]}}`,
			want:     []Violation{{Code: ViolationInvalidType, Path: "commands"}},
		},
		"byCommand not an object": {
			kind:     DocumentMCPResult,
			document: `{"protocolVersion":2,"tool":"plan_jobs","commands":["build"],"plan":{"dryRun":true,"metrics":{"tasks":0,"edges":0,"projects":0,"byCommand":[]},"tasks":[]}}`,
			want:     []Violation{{Code: ViolationInvalidType, Path: "plan.metrics.byCommand"}},
		},
		"byCommand value not an integer": {
			kind:     DocumentMCPResult,
			document: `{"protocolVersion":2,"tool":"plan_jobs","commands":["build"],"plan":{"dryRun":true,"metrics":{"tasks":0,"edges":0,"projects":0,"byCommand":{"build":"1"}},"tasks":[]}}`,
			want:     []Violation{{Code: ViolationInvalidType, Path: "plan.metrics.byCommand.build"}},
		},
		"cache not a boolean": {
			kind:     DocumentSessionPlanFile,
			document: `{"protocolVersion":2,"sessionId":"s","commands":["build"],"tasks":[{"identity":{"key":"/a:b","scope":"project","project":{"id":"/a","name":"a"},"task":{"name":"b","command":"b","kind":"b"},"provider":{"extension":"e"}},"cache":"yes"}]}`,
			want:     []Violation{{Code: ViolationInvalidType, Path: "tasks[0].cache"}},
		},
		"fractional duration": {
			kind:     DocumentSessionStreamRecord,
			document: `{"protocolVersion":2,"record":"session:end","time":"t","run":{"outcome":"success","exitCode":0,"counts":{"total":0,"succeeded":0,"failed":0,"canceled":0,"skipped":0},"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":1.5}}`,
			want:     []Violation{{Code: ViolationInvalidType, Path: "run.durationMs"}},
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			got := ValidateDocument(testCase.kind, []byte(testCase.document))
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("violations = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestValidateDocument_IntegerRule pins the contract's integer semantics: any
// JSON number carrying an integral value in IEEE-754's safe range conforms
// (the rule JSON Schema's "integer" implies and Number.isSafeInteger applies),
// and anything fractional or beyond the safe range does not — identically in
// both runtimes, whatever the wire encoding.
func TestValidateDocument_IntegerRule(t *testing.T) {
	document := func(duration string) string {
		return `{"protocolVersion":2,"record":"session:end","time":"t","run":{"outcome":"success","exitCode":0,` +
			`"counts":{"total":0,"succeeded":0,"failed":0,"canceled":0,"skipped":0},` +
			`"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":` + duration + `}}`
	}
	for duration, valid := range map[string]bool{
		"1":                true,
		"1.0":              true,
		"1e2":              true,
		"9007199254740991": true,  // 2^53 - 1, the largest safe integer
		"1.5":              false, // fractional
		"9007199254740993": false, // above the safe range; JavaScript would round it
		"1e300":            false, // integral, but far outside the safe range
	} {
		t.Run(duration, func(t *testing.T) {
			got := ValidateDocument(DocumentSessionStreamRecord, []byte(document(duration)))
			if valid && len(got) != 0 {
				t.Errorf("durationMs %s: violations = %v, want none", duration, got)
			}
			want := []Violation{{Code: ViolationInvalidType, Path: "run.durationMs"}}
			if !valid && !reflect.DeepEqual(got, want) {
				t.Errorf("durationMs %s: violations = %v, want %v", duration, got, want)
			}
		})
	}
}

func TestValidateDocument_LocalCacheAttribution(t *testing.T) {
	document := func(local string) string {
		return `{"protocolVersion":2,"record":"session:end","time":"t","run":{"outcome":"success","exitCode":0,` +
			`"counts":{"total":1,"succeeded":1,"failed":0,"canceled":0,"skipped":0},` +
			`"reuse":{"localCache":1,"remoteCache":0,"coalesced":0},"durationMs":100,` +
			`"cache":{"local":` + local + `}}}`
	}
	valid := `{"hits":1,"misses":0,"servedMs":90,"keysMs":30,"bindingsMs":40,"restoreVerifyMs":20,"spawnedProcesses":4}`
	if got := ValidateDocument(DocumentSessionStreamRecord, []byte(document(valid))); len(got) != 0 {
		t.Fatalf("valid local cache attribution: %v", got)
	}
	rounded := `{"hits":1,"misses":0,"servedMs":90,"keysMs":29,"bindingsMs":39,"restoreVerifyMs":20,"spawnedProcesses":4}`
	if got := ValidateDocument(DocumentSessionStreamRecord, []byte(document(rounded))); len(got) != 0 {
		t.Fatalf("valid rounded local cache attribution: %v", got)
	}
	for name, local := range map[string]string{
		"hits disagree with reuse":     `{"hits":0,"misses":0,"servedMs":90,"keysMs":30,"bindingsMs":40,"restoreVerifyMs":20,"spawnedProcesses":4}`,
		"served exceeds run":           `{"hits":1,"misses":0,"servedMs":101,"keysMs":39,"bindingsMs":40,"restoreVerifyMs":20,"spawnedProcesses":4}`,
		"phase total exceeds served":   `{"hits":1,"misses":0,"servedMs":90,"keysMs":30,"bindingsMs":50,"restoreVerifyMs":20,"spawnedProcesses":4}`,
		"unattributed gap exceeds 2ms": `{"hits":1,"misses":0,"servedMs":90,"keysMs":29,"bindingsMs":38,"restoreVerifyMs":20,"spawnedProcesses":4}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := ValidateDocument(DocumentSessionStreamRecord, []byte(document(local))); len(got) != 1 || got[0].Code != ViolationCountMismatch {
				t.Fatalf("violations = %v, want one count_mismatch", got)
			}
		})
	}
}

// TestValidateDocument_ViolationsAreSorted pins the deterministic order both
// runtimes emit: by path, then by code. Without it the two could report the
// same violations and still disagree.
func TestValidateDocument_ViolationsAreSorted(t *testing.T) {
	got := ValidateDocument(DocumentSessionPlanFile, []byte(`{"zzz":1,"protocolVersion":1,"aaa":2}`))
	want := []Violation{
		{Code: ViolationUnknownField, Path: "aaa"},
		{Code: ViolationMissingField, Path: "commands"},
		{Code: ViolationInvalidProtocolVersion, Path: "protocolVersion"},
		{Code: ViolationMissingField, Path: "sessionId"},
		{Code: ViolationMissingField, Path: "tasks"},
		{Code: ViolationUnknownField, Path: "zzz"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("violations = %v, want %v", got, want)
	}
}

// TestValidateDocument_IndexPaths covers the array index formatting used in
// every nested path, including indexes above nine.
func TestValidateDocument_IndexPaths(t *testing.T) {
	commands := `["a","b","c","d","e","f","g","h","i","j","k",""]`
	document := `{"protocolVersion":2,"sessionId":"s","commands":` + commands + `,"tasks":[]}`
	got := ValidateDocument(DocumentSessionPlanFile, []byte(document))
	want := []Violation{{Code: ViolationInvalidValue, Path: "commands[11]"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("violations = %v, want %v", got, want)
	}
}

func TestTaskIdentityDerivedKey(t *testing.T) {
	identity := TaskIdentity{
		Key:     "/tooling/cli:build~compile",
		Scope:   TaskScopeProject,
		Project: ProjectIdentity{ID: "/tooling/cli", Name: "@putnami/cli"},
		Task:    TaskRef{Name: "build~compile", Command: "build", Step: "compile", Kind: "go-build"},
	}
	if got := identity.DerivedKey(); got != identity.Key {
		t.Errorf("DerivedKey() = %q, want %q", got, identity.Key)
	}
}

func TestRunSummaryDerivations(t *testing.T) {
	summary := RunSummary{
		Outcome: RunOutcomeSuccess,
		Counts:  RunCounts{Total: 4, Succeeded: 3, Canceled: 1},
		Reuse:   RunReuse{LocalCache: 2, RemoteCache: 1, Coalesced: 1},
	}
	if got := summary.Counts.Sum(); got != summary.Counts.Total {
		t.Errorf("Counts.Sum() = %d, want %d", got, summary.Counts.Total)
	}
	if got := summary.Reuse.Sum(); got != 4 {
		t.Errorf("Reuse.Sum() = %d, want 4", got)
	}
	if !summary.Succeeded() {
		t.Error("Succeeded() = false for a run with no failures and no abort")
	}

	// The strict rule: a reused failure sinks the run.
	summary.Counts = RunCounts{Total: 4, Succeeded: 2, Failed: 1, Canceled: 1}
	if summary.Succeeded() {
		t.Error("Succeeded() = true with a failed task — the unified rule is strict")
	}

	// And an abort is never green, whatever the counts say.
	summary.Counts = RunCounts{Total: 1, Succeeded: 1}
	summary.Outcome = RunOutcomeAborted
	if summary.Succeeded() {
		t.Error("Succeeded() = true for an aborted run")
	}
}

// TestResultV2SurfacesShareOneRunSummary is the structural claim behind "one
// verdict on every surface": the run summary a JSON envelope, a session:end
// record, an MCP run_jobs result and a session file report is the same type, so
// the four cannot drift into four vocabularies again.
func TestResultV2SurfacesShareOneRunSummary(t *testing.T) {
	want := reflect.TypeOf(RunSummary{})
	surfaces := map[string]reflect.Type{
		"resultEnvelope.run":      reflect.TypeOf(ResultV2{}.Run).Elem(),
		"sessionStreamRecord.run": reflect.TypeOf(SessionStreamRecord{}.Run).Elem(),
		"mcpResult.run":           reflect.TypeOf(MCPResult{}.Run).Elem(),
		"sessionFile.run":         reflect.TypeOf(SessionFile{}.Run),
	}
	for name, got := range surfaces {
		if got != want {
			t.Errorf("%s is %v, want %v", name, got, want)
		}
	}
}
