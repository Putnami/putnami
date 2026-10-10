package deliverycli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- Fix 1: a red run names what broke, in file:line form ---

// ciSessionEndBody is a real v2 `session:end` record, trimmed to the members the
// renderer reads. It is the shape that used to reach the terminal verbatim.
const ciSessionEndBody = `{"protocolVersion":2,"record":"session:end","runId":"run-1","run":{` +
	`"counts":{"canceled":617,"failed":2,"skipped":38,"succeeded":140,"total":799},` +
	`"durationMs":170038,"exitCode":1,"outcome":"failure","failures":[` +
	`{"error":{"code":"failure","message":"task failed"},` +
	`"identity":{"key":"/apps/admin:lint~check-only",` +
	`"project":{"id":"/apps/admin","name":"apps/admin"},` +
	`"task":{"command":"lint","name":"lint~check-only"}},` +
	`"diagnostics":[{"code":"assist/source/organizeImports","column":1,` +
	`"file":"apps/admin/src/app/layout.tsx","line":3,` +
	`"message":"Sort these imports.","severity":"error"}]}]}}`

func TestCIViewRendersTheFailingStepTasksAndDiagnostics(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1": {status: 200, body: CIRunResponse{
			ID: "run-1", Status: "completed", Conclusion: "failure",
			Failure:     &CIRunFailure{Step: "gate", Detail: "lint"},
			FailedTasks: []CIRunFailedTask{{Project: "apps/admin", Task: "lint~check-only"}},
		}},
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{},
			Diagnostics: &CIRunLogDiagnostics{Failures: []CIRunLogFailure{{
				Project: "apps/admin", Task: "lint~check-only",
				Diagnostics: []CIRunLogDiagnostic{{
					File: "apps/admin/src/app/layout.tsx", Line: 3, Column: 1,
					Severity: "error", Code: "assist/source/organizeImports", Message: "Sort these imports.",
				}},
				Truncated: 9,
			}}},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"view", "run-1"})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	out := strings.Join(lines, "\n")
	for _, want := range []string{
		"Failed:     gate (lint)",
		"Failing tasks:",
		"apps/admin  lint~check-only",
		"apps/admin/src/app/layout.tsx:3:1: Sort these imports. [assist/source/organizeImports]",
		"… and 9 more in this task",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view output never showed %q:\n%s", want, out)
		}
	}
}

// A green run must not pay the diagnostics request: there is nothing to show, and
// the view is polled while a run is live.
func TestCIViewFetchesNoDiagnosticsForANonFailedRun(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1": {status: 200, body: CIRunResponse{
			ID: "run-1", Status: "completed", Conclusion: "success",
		}},
	}}
	if _, err := captureCI(t, fake, []string{"view", "run-1"}); err != nil {
		t.Fatalf("view: %v", err)
	}
	for _, req := range fake.requests {
		if strings.HasSuffix(req.Path, "/logs") {
			t.Fatalf("a successful run fetched its logs: %s %s", req.Method, req.Path)
		}
	}
}

// The view must survive an unreadable log surface: the run facts are the answer
// and the diagnostics are the enrichment.
func TestCIViewSurvivesAnUnreadableLogSurface(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1": {status: 200, body: CIRunResponse{
			ID: "run-1", Status: "completed", Conclusion: "failure",
			Failure: &CIRunFailure{Step: "gate", Detail: "lint"},
		}},
		// /logs is deliberately unstubbed, so it 404s.
	}}
	lines, err := captureCI(t, fake, []string{"view", "run-1"})
	if err != nil {
		t.Fatalf("view failed on an unreadable log surface: %v", err)
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "Failed:     gate (lint)") {
		t.Fatalf("view lost the run facts when logs were unreadable:\n%s", out)
	}
}

func TestCILogsRendersProtocolRecordsForHumans(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{
				{Body: ciSessionEndBody},
				{Body: `{"protocolVersion":2,"record":"task:end","runId":"run-1",` +
					`"identity":{"project":{"name":"libs/ci"},"task":{"name":"lint~staticcheck"}},` +
					`"task":{"durationMs":39353,"reuse":"remote-cache","status":"success"}}`},
				{Body: "plain runner output, not a record"},
			},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1"})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	out := strings.Join(lines, "\n")
	for _, want := range []string{
		"session end  failure  140 succeeded, 2 failed, 617 canceled, 38 skipped of 799",
		"task success",
		"libs/ci  lint~staticcheck",
		"remote-cache",
		"plain runner output, not a record",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs output never showed %q:\n%s", want, out)
		}
	}
	// The raw JSON must be gone — that is the entire point of the rendering.
	if strings.Contains(out, `"protocolVersion"`) {
		t.Fatalf("logs still printed the raw protocol JSON:\n%s", out)
	}
}

func TestCILogsRawKeepsTheProtocolRecords(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{{Body: ciSessionEndBody}},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--raw"})
	if err != nil {
		t.Fatalf("logs --raw: %v", err)
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, `"protocolVersion":2`) {
		t.Fatalf("--raw did not print the verbatim record:\n%s", out)
	}
}

// The regression this whole slice exists for: `--level error` on a run that just
// failed used to answer "No logs found", because every protocol line carries the
// severity of the pipe (LOG) rather than of the finding inside it.
func TestCILogsLevelMatchesTheFindingNotTheTransport(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{
				{Body: ciSessionEndBody},
				{Body: `{"protocolVersion":2,"record":"task:end","runId":"run-1",` +
					`"identity":{"project":{"name":"libs/ci"},"task":{"name":"build"}},` +
					`"task":{"durationMs":10,"status":"success"}}`},
			},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--level", "error"})
	if err != nil {
		t.Fatalf("logs --level error: %v", err)
	}
	out := strings.Join(lines, "\n")
	if strings.Contains(out, "No logs found") {
		t.Fatalf("--level error still reported no logs about a failed run:\n%s", out)
	}
	if !strings.Contains(out, "session end  failure") {
		t.Fatalf("--level error dropped the failing session record:\n%s", out)
	}
	if strings.Contains(out, "build") {
		t.Fatalf("--level error kept a successful task:\n%s", out)
	}
	// The server's own severity filter must not be consulted: it would have
	// removed the very record the findings live in.
	for _, req := range fake.requests {
		if strings.Contains(req.Query, "level=") {
			t.Fatalf("--level was forwarded to the server: %q", req.Query)
		}
	}
}

// An empty page after filtering must say which of the two empties it is.
func TestCILogsDistinguishesFilteredEmptyFromNoLogs(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{{Body: `{"protocolVersion":2,"record":"task:end","runId":"run-1",` +
				`"identity":{"project":{"name":"p"},"task":{"name":"t"}},` +
				`"task":{"durationMs":1,"status":"success"}}`}},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--level", "error"})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "No logs at or above --level error") || !strings.Contains(out, "1 entries below it") {
		t.Fatalf("a filtered-empty page did not distinguish itself from an empty run:\n%s", out)
	}
}

// The filter moved client-side, so the refusal has to move with it: a typo'd
// level that silently printed everything would read as "nothing was filtered".
func TestCILogsRefusesAnUnknownLevel(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{{Body: "hello"}},
		}},
	}}
	_, err := captureCI(t, fake, []string{"logs", "run-1", "--level", "erro"})
	if err == nil {
		t.Fatal("an unknown --level was silently ignored")
	}
	if !strings.Contains(err.Error(), "trace, debug, info, warn, error, fatal") {
		t.Fatalf("the refusal does not name the accepted levels: %q", err)
	}
}

func TestCIEntrySeverityReadsTheDiagnosticNotTheEntry(t *testing.T) {
	// Severity LOG on the wire, an `error` diagnostic inside.
	entry := CILogEntry{Body: ciSessionEndBody}
	if got := ciEntrySeverity(entry); got < ciSeverityError {
		t.Fatalf("effective severity = %v, want at least error", got)
	}
	// A canceled task is not a failure: a fail-fast gate cancels most of its work,
	// and rendering those as errors would bury the few that really broke.
	canceled := CILogEntry{Body: `{"protocolVersion":2,"record":"task:end",` +
		`"identity":{"project":{"name":"p"},"task":{"name":"t"}},` +
		`"task":{"status":"canceled"}}`}
	if got := ciEntrySeverity(canceled); got >= ciSeverityError {
		t.Fatalf("a canceled task read as severity %v, want below error", got)
	}
}

// --- Fix 2: run ids abbreviate ---

func TestCIListPrintsShortRunIDs(t *testing.T) {
	full := strings.Repeat("a", 64)
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{
			Runs: []CIRunResponse{{ID: full, Status: "completed", Conclusion: "success"}},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := strings.Join(lines, "\n")
	if strings.Contains(out, full) {
		t.Fatalf("list printed the full 64-character id:\n%s", out)
	}
	if !strings.Contains(out, full[:ciShortRunIDLength]) {
		t.Fatalf("list did not print the short id:\n%s", out)
	}
}

func TestCIViewResolvesAnAbbreviatedRunID(t *testing.T) {
	full := strings.Repeat("b", 64)
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{
			Runs: []CIRunResponse{{ID: full}, {ID: strings.Repeat("c", 64)}},
		}},
		"GET /v1/workspaces/ws-acme/runs/" + full: {status: 200, body: CIRunResponse{
			ID: full, Status: "completed", Conclusion: "success",
		}},
	}}
	if _, err := captureCI(t, fake, []string{"view", "bbbbbbbbbbbb"}); err != nil {
		t.Fatalf("view by prefix: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Path != "/v1/workspaces/ws-acme/runs/"+full {
		t.Fatalf("final request = %s, want the resolved by-id route", got.Path)
	}
}

// A full id must never consult the run window — the abbreviation feature is not
// allowed to add a request to the path everything else takes.
func TestCIFullRunIDCostsNoLookup(t *testing.T) {
	full := strings.Repeat("d", 64)
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/" + full: {status: 200, body: CIRunResponse{ID: full}},
	}}
	if _, err := captureCI(t, fake, []string{"view", full}); err != nil {
		t.Fatalf("view: %v", err)
	}
	for _, req := range fake.requests {
		if req.Path == "/v1/workspaces/ws-acme/runs" {
			t.Fatalf("a full run id still walked the list window")
		}
	}
}

func TestCIAmbiguousRunPrefixIsRefusedAndNamesTheCandidates(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{
			Runs: []CIRunResponse{{ID: "eeee1111" + strings.Repeat("0", 56)}, {ID: "eeee2222" + strings.Repeat("0", 56)}},
		}},
	}}
	_, err := captureCI(t, fake, []string{"view", "eeee"})
	if err == nil {
		t.Fatal("an ambiguous prefix was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "at least 2 runs") {
		t.Fatalf("ambiguity error does not explain itself: %q", msg)
	}
	if !strings.Contains(msg, "eeee1111") {
		t.Fatalf("ambiguity error does not name the candidates: %q", msg)
	}
}

// A cancel must never reach a run the operator did not name. The literal `ffff`
// is offered first and 404s — mutating nothing, which is what makes offering it
// safe — and the ambiguity is then refused rather than resolved to a guess, so
// neither candidate is ever posted to.
func TestCICancelRefusesAnAmbiguousPrefixWithoutMutating(t *testing.T) {
	first := "ffff1111" + strings.Repeat("0", 56)
	second := "ffff2222" + strings.Repeat("0", 56)
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{
			Runs: []CIRunResponse{{ID: first}, {ID: second}},
		}},
	}}
	if _, err := captureCI(t, fake, []string{"cancel", "ffff", "--reason", "x", "--yes"}); err == nil {
		t.Fatal("cancel accepted an ambiguous prefix")
	}
	for _, req := range fake.requests {
		if req.Method != http.MethodPost {
			continue
		}
		if strings.Contains(req.Path, first) || strings.Contains(req.Path, second) {
			t.Fatalf("cancel reached a guessed candidate: %s %s", req.Method, req.Path)
		}
	}
}

// --- Fix 3: list is bounded, and says when it is ---

func TestCIListDefaultsToABoundedPage(t *testing.T) {
	runs := make([]CIRunResponse, 0, 50)
	for i := 0; i < 50; i++ {
		runs = append(runs, CIRunResponse{ID: strings.Repeat("a", 63) + string(rune('a'+i%26)), Status: "completed"})
	}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{Runs: runs}},
	}}
	lines, err := captureCI(t, fake, []string{"list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// header + ciListDefaultLimit rows + the truncation notice
	if len(lines) != ciListDefaultLimit+2 {
		t.Fatalf("list printed %d lines, want %d (header + %d rows + notice)", len(lines), ciListDefaultLimit+2, ciListDefaultLimit)
	}
	if !strings.Contains(lines[len(lines)-1], "30 more in the window") {
		t.Fatalf("list did not say how much it withheld: %q", lines[len(lines)-1])
	}
}

func TestCIListLimitIsHonored(t *testing.T) {
	runs := make([]CIRunResponse, 0, 10)
	for i := 0; i < 10; i++ {
		runs = append(runs, CIRunResponse{ID: strings.Repeat("b", 63) + string(rune('a'+i)), Status: "completed"})
	}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{Runs: runs}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--limit", "3"})
	if err != nil {
		t.Fatalf("list --limit: %v", err)
	}
	if len(lines) != 3+2 {
		t.Fatalf("--limit 3 printed %d lines, want 5 (header + 3 rows + notice):\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// `--limit 0` is the escape hatch for a caller that really wants the lot.
func TestCIListLimitZeroPrintsTheWholeWindow(t *testing.T) {
	runs := make([]CIRunResponse, 0, 25)
	for i := 0; i < 25; i++ {
		runs = append(runs, CIRunResponse{ID: strings.Repeat("c", 62) + string(rune('a'+i)) + "z", Status: "completed"})
	}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{Runs: runs}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--limit", "0"})
	if err != nil {
		t.Fatalf("list --limit 0: %v", err)
	}
	if len(lines) != 25+1 {
		t.Fatalf("--limit 0 printed %d lines, want 26 (header + 25 rows)", len(lines))
	}
}

// The structured surface is a machine contract: bounding it would silently
// truncate a scripted read, so --limit shapes it the same way and nothing else.
func TestCIListJSONLIsStillOnePerLine(t *testing.T) {
	runs := []CIRunResponse{{ID: strings.Repeat("e", 64)}, {ID: strings.Repeat("f", 64)}}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{Runs: runs}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--output=jsonl"})
	if err != nil {
		t.Fatalf("list jsonl: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("jsonl printed %d lines, want 2", len(lines))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "{") {
			t.Fatalf("jsonl line is not an object: %q", line)
		}
	}
}

// --- Wire twins for the new fields ---

func TestCIFailureAndDiagnosticsWireKeys(t *testing.T) {
	assertExactKeys(t, "failure", marshalToDoc(t, CIRunFailure{Step: "gate", Detail: "lint"}),
		[]string{"step", "detail"})

	assertExactKeys(t, "failedTask", marshalToDoc(t, CIRunFailedTask{Project: "p", Task: "t"}),
		[]string{"project", "task"})

	assertExactKeys(t, "logDiagnostic", marshalToDoc(t, CIRunLogDiagnostic{
		File: "a.ts", Line: 1, Column: 2, Severity: "error", Code: "c", Message: "m",
	}), []string{"file", "line", "column", "severity", "code", "message"})

	assertExactKeys(t, "logFailure", marshalToDoc(t, CIRunLogFailure{
		Project: "p", Task: "t", Message: "m",
		Diagnostics: []CIRunLogDiagnostic{{File: "a.ts"}}, Truncated: 3,
	}), []string{"project", "task", "message", "diagnostics", "truncated"})

	assertExactKeys(t, "logDiagnostics", marshalToDoc(t, CIRunLogDiagnostics{
		FirstErrors: []string{"e"}, Failures: []CIRunLogFailure{{Project: "p"}}, Reproduce: "putnami lint",
	}), []string{"firstErrors", "failures", "reproduce"})
}

func TestCIRunResponseCarriesTheFailureFields(t *testing.T) {
	doc := marshalToDoc(t, CIRunResponse{
		ID: "run-1", Status: "completed", Conclusion: "failure",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Failure:     &CIRunFailure{Step: "gate"},
		FailedTasks: []CIRunFailedTask{{Project: "p", Task: "t"}},
	})
	for _, key := range []string{"failure", "failedTasks"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("run response lost the %q field", key)
		}
	}
}

// --- Review follow-ups: the live tail and the machine surface obey the same
// flags as the history page, and every bound states itself ---

// The fix for `--level error` had been applied to the history page only. The tail
// built its OWN query and kept forwarding the level to a server that filters on
// the severity each entry arrived with — LOG for every protocol line — so
// `--follow --level error` still dropped the session record the findings live in.
func TestCIFollowDoesNotForwardLevelAndFiltersOnFindings(t *testing.T) {
	prevBackoff, prevMax := ciFollowBackoff, ciFollowMaxReconnects
	ciFollowBackoff, ciFollowMaxReconnects = time.Millisecond, 2
	defer func() { ciFollowBackoff, ciFollowMaxReconnects = prevBackoff, prevMax }()

	frame := func(body string) string {
		payload, err := json.Marshal(map[string]any{"entry": map[string]any{"body": body}, "cursor": "c1"})
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		return "data: " + string(payload) + "\n\n"
	}
	green := `{"protocolVersion":2,"record":"task:end",` +
		`"identity":{"project":{"name":"p"},"task":{"name":"build"}},` +
		`"task":{"durationMs":5,"status":"success"}}`

	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs/tail": {
			status: 200, sse: frame(ciSessionEndBody) + frame(green),
		},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--follow", "--level", "error"})
	if err != nil {
		t.Fatalf("follow --level error: %v", err)
	}
	for _, req := range fake.requests {
		if strings.HasSuffix(req.Path, "/logs/tail") && strings.Contains(req.Query, "level=") {
			t.Fatalf("the tail forwarded --level to the server: %q", req.Query)
		}
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "session end  failure") {
		t.Fatalf("the tail dropped the failing session record:\n%s", out)
	}
	if strings.Contains(out, "build") {
		t.Fatalf("the tail kept a successful task under --level error:\n%s", out)
	}
}

// The tail must also RENDER like the history page: before this it always used the
// raw formatter, so `--follow` printed protocol JSON and `--raw` changed nothing.
func TestCIFollowRendersForHumansAndHonorsRaw(t *testing.T) {
	prevBackoff, prevMax := ciFollowBackoff, ciFollowMaxReconnects
	ciFollowBackoff, ciFollowMaxReconnects = time.Millisecond, 2
	defer func() { ciFollowBackoff, ciFollowMaxReconnects = prevBackoff, prevMax }()

	payload, err := json.Marshal(map[string]any{"entry": map[string]any{"body": ciSessionEndBody}, "cursor": "c1"})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	sse := "data: " + string(payload) + "\n\n"

	newFake := func() *ciFakeServer {
		return &ciFakeServer{responses: map[string]ciStubResponse{
			"GET /v1/workspaces/ws-acme/runs/run-1/logs/tail": {status: 200, sse: sse},
		}}
	}

	human, err := captureCI(t, newFake(), []string{"logs", "run-1", "--follow"})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if out := strings.Join(human, "\n"); !strings.Contains(out, "session end  failure") ||
		strings.Contains(out, `"protocolVersion"`) {
		t.Fatalf("the tail did not render for humans:\n%s", out)
	}

	raw, err := captureCI(t, newFake(), []string{"logs", "run-1", "--follow", "--raw"})
	if err != nil {
		t.Fatalf("follow --raw: %v", err)
	}
	if out := strings.Join(raw, "\n"); !strings.Contains(out, `"protocolVersion":2`) {
		t.Fatalf("--raw did not reach the tail:\n%s", out)
	}
}

// `--level` is a selector, so the machine surface has to honor it too — it used
// to return before the filter ran, emitting every entry.
func TestCILogsJSONLHonorsLevel(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{
				{Body: ciSessionEndBody},
				{Body: `{"protocolVersion":2,"record":"task:end",` +
					`"identity":{"project":{"name":"p"},"task":{"name":"build"}},` +
					`"task":{"durationMs":5,"status":"success"}}`},
			},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--level", "error", "--output=jsonl"})
	if err != nil {
		t.Fatalf("logs jsonl --level: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("jsonl emitted %d lines, want only the failing one:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "session:end") {
		t.Fatalf("jsonl kept the wrong entry: %s", lines[0])
	}
}

// An unusable level must be refused on every surface, not just the human one.
func TestCILogsRefusesAnUnknownLevelOnEverySurface(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "run-1", "--level", "erro", "--output=jsonl"},
		{"logs", "run-1", "--level", "erro", "--follow"},
	} {
		fake := &ciFakeServer{responses: map[string]ciStubResponse{
			"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{}},
		}}
		if _, err := captureCI(t, fake, args); err == nil {
			t.Fatalf("%v accepted an unknown --level", args)
		}
	}
}

// A present-but-unusable --limit is refused rather than folded into the default:
// NumberParam returns nil for `--limit foo`, which is indistinguishable from "not
// supplied" and would print 20 rows as if the flag had been honored.
func TestCIListRefusesAMalformedLimit(t *testing.T) {
	for _, bad := range []string{"foo", "1.5", "-3"} {
		fake := &ciFakeServer{responses: map[string]ciStubResponse{
			"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{
				Runs: []CIRunResponse{{ID: strings.Repeat("a", 64)}},
			}},
		}}
		_, err := captureCI(t, fake, []string{"list", "--limit", bad})
		if err == nil {
			t.Fatalf("--limit %q was accepted", bad)
		}
		if !strings.Contains(err.Error(), "non-negative whole number") {
			t.Fatalf("--limit %q gave an unhelpful refusal: %v", bad, err)
		}
	}
}

// The client's own paging cap is invisible from the output — the last row printed
// looks like the oldest run there is — so it has to state itself. It matters most
// under `--limit 0`, whose whole promise is "no page bound".
func TestCIListSaysWhenTheWindowWalkWasCapped(t *testing.T) {
	runs := make([]CIRunResponse, 0, ciListPageLimit)
	for i := 0; i < ciListPageLimit; i++ {
		runs = append(runs, CIRunResponse{ID: strings.Repeat("a", 60) + string(rune('a'+i%26)) + "bcd", Status: "completed"})
	}
	// NextOffset always advances, so the walk can only end at the page cap.
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{
			Runs: runs, NextOffset: 1,
		}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--limit", "0"})
	if err != nil {
		t.Fatalf("list --limit 0: %v", err)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "older history was not scanned") {
		t.Fatalf("a capped window walk did not say so:\n%s", out[max(0, len(out)-400):])
	}
}

// A gate that failed 30 tasks must not render as one that failed 10.
func TestCIViewReportsFailingTasksItDidNotShow(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1": {status: 200, body: CIRunResponse{
			ID: "run-1", Status: "completed", Conclusion: "failure",
		}},
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{},
			Diagnostics: &CIRunLogDiagnostics{
				Failures:          []CIRunLogFailure{{Project: "p", Task: "lint"}},
				TruncatedFailures: 20,
			},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"view", "run-1"})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "20 more failing task(s) not shown") {
		t.Fatalf("view presented a bounded task list as complete:\n%s", out)
	}
}
