package deliverycli

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	protocolcli "go.putnami.dev/protocol/cli"
)

// ciFakeServer stands in for the control plane's CI routes plus the auth
// endpoints WorkspaceAuth needs. Canned responses are keyed by "METHOD path"; an
// unstubbed route 404s. It records every CI request (method/path/query/body) so a
// verb→route test can assert exactly what hit the wire. A raw event-stream body
// (sse) is served verbatim for the tail route.
type ciFakeServer struct {
	requests          []ciCapturedRequest
	responses         map[string]ciStubResponse
	responseSequences map[string][]ciStubResponse
	// tailCalls counts /logs/tail connects so the SSE stub is served only on the
	// FIRST connect; every reconnect then gets an immediately-EOF empty stream, so
	// the follow loop's reconnect budget drains and the test terminates.
	tailCalls int
}

type ciCapturedRequest struct {
	Method string
	Host   string
	Path   string
	Query  string
	Auth   string
	Body   map[string]any
	Raw    []byte
}

type ciStubResponse struct {
	status int
	body   any
	sse    string
}

func (s *ciFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	// Auth endpoints (reused from report_test.go's fixtures).
	switch req.URL.Path {
	case "/.well-known/openid-configuration":
		return reportJSONResponse(http.StatusOK, map[string]any{
			"issuer": reportTestBaseURL, "token_endpoint": reportTestBaseURL + "/token",
			"userinfo_endpoint": reportTestBaseURL + "/userinfo",
		}), nil
	case "/token":
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		extra := map[string]any{}
		if workspaceID := clicore.StringValue(body["workspace_id"]); workspaceID != "" {
			extra["scope_ref"] = map[string]any{"workspace_id": workspaceID}
		}
		return reportJSONResponse(http.StatusOK, reportTokenResponse(extra)), nil
	}

	var body map[string]any
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &body)
	}
	s.requests = append(s.requests, ciCapturedRequest{
		Method: req.Method,
		Host:   req.URL.Host,
		Path:   req.URL.Path,
		Query:  req.URL.RawQuery,
		Auth:   req.Header.Get("Authorization"),
		Body:   body,
		Raw:    bodyBytes,
	})
	key := req.Method + " " + req.URL.Path
	if strings.HasSuffix(req.URL.Path, "/logs/tail") {
		s.tailCalls++
		frame := ""
		if stub, ok := s.responses[key]; ok && s.tailCalls == 1 {
			frame = stub.sse
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(frame)),
		}, nil
	}
	if sequence := s.responseSequences[key]; len(sequence) > 0 {
		stub := sequence[0]
		s.responseSequences[key] = sequence[1:]
		return ciStubReply(stub.status, stub.body), nil
	}
	if stub, ok := s.responses[key]; ok {
		if stub.sse != "" {
			return &http.Response{
				StatusCode: stub.status,
				Status:     http.StatusText(stub.status),
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(stub.sse)),
			}, nil
		}
		return ciStubReply(stub.status, stub.body), nil
	}
	return reportJSONResponse(http.StatusNotFound, map[string]any{"error": "no stub for " + key}), nil
}

// ciStubReply answers a stub the way delivery's records routes write it: a
// refusal carries its reason under both `error` and the first-party envelope's
// `message` (errorResponse.MarshalJSON), the member the CLI's generated client
// reads.
func ciStubReply(status int, body any) *http.Response {
	if status >= http.StatusBadRequest {
		var envelope map[string]any
		if data, err := json.Marshal(body); err == nil && json.Unmarshal(data, &envelope) == nil {
			if reason, ok := envelope["error"].(string); ok {
				if _, has := envelope["message"]; !has {
					envelope["message"] = reason
				}
				return reportJSONResponse(status, envelope)
			}
		}
	}
	return reportJSONResponse(status, body)
}

func newCITestIO(t *testing.T, fake *ciFakeServer) (clicore.IO, string) {
	t.Helper()
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeReportTestAuth(t, home)
	writeReportLinkFile(t, workspaceRoot)
	return clicore.IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL": reportTestBaseURL,
			"NO_COLOR":         "1",
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC) },
	}, workspaceRoot
}

// runCI mirrors the dispatcher's param build (mergeParams over parseFlags) so a
// test drives CI exactly as `putnami cloud ci …` would.
func runCI(ioctx clicore.IO, workspaceRoot string, args []string) error {
	params := clicore.MergeParams(clicore.ParseFlags(args))
	return CI(params, args, workspaceRoot, ioctx.Env, ioctx)
}

// captureCI runs CI and captures the Stdout lines.
func captureCI(t *testing.T, fake *ciFakeServer, args []string) ([]string, error) {
	t.Helper()
	ioctx, root := newCITestIO(t, fake)
	var lines []string
	ioctx.Stdout = func(s string) { lines = append(lines, s) }
	err := runCI(ioctx, root, args)
	return lines, err
}

// --- Wire-key stability (the guard against silent shape drift) ---

func TestCIRequestWireKeys(t *testing.T) {
	assertExactKeys(t, "start", marshalToDoc(t, CIRunStartRequest{
		Provider: "github", Repo: "acme/app", Ref: "refs/heads/main", Branch: "main",
		SHA: "abc", PullRequest: 7, Phases: "lint,test", MaxParallel: 8, NoCache: true, Reason: "manual",
	}), []string{"provider", "repo", "ref", "branch", "sha", "pullRequest", "phases", "maxParallel", "noCache", "reason"})

	assertExactKeys(t, "cancel", marshalToDoc(t, CIRunCancelRequest{Reason: "stop"}), []string{"reason"})

	assertExactKeys(t, "retry", marshalToDoc(t, CIRunRetryRequest{
		Phase: "test", NoCache: true, MaxParallel: 2, Reason: "flake",
	}), []string{"phase", "noCache", "maxParallel", "reason"})

	assertExactKeys(t, "repair", marshalToDoc(t, CIRepairRequest{Reason: "incident"}), []string{"reason"})

	assertExactKeys(t, "pipeline", marshalToDoc(t, PipelineModeRequest{
		Mode: "paused", Reason: "incident", CancelRunning: true, ReviewRef: "r", IncidentRef: "i",
	}), []string{"mode", "reason", "cancelRunning", "reviewRef", "incidentRef"})
}

func TestCIResponseWireKeys(t *testing.T) {
	started := time.Now()
	finished := started.Add(time.Minute)
	heartbeatCPU, heartbeatCPUDelta := 12.5, 1.25
	heartbeatRSS, heartbeatIORead, heartbeatIOWrite := int64(4096), int64(128), int64(64)
	heartbeatProcs := 7
	runDoc := marshalToDoc(t, CIRunResponse{
		ID: "run-1", Status: "in_progress", Conclusion: "success", Repo: "acme/app",
		Branch: "main", SHA: "abc", PullRequest: 7, Provenance: "github",
		Checks:    map[string]string{"check_run_id": "1"},
		CreatedAt: started, UpdatedAt: finished,
		Phases: []CIRunPhaseResponse{{
			Phase: "test", Status: "completed", Conclusion: "success", Reason: "ok",
			Required: true, Attempt: 1, CreatedAt: started, StartedAt: &started, FinishedAt: &finished,
		}},
		Heartbeat: &CIRunHeartbeat{
			Version: 1, Sequence: 12, Attempt: 1, Session: "gs-1", Phase: "gate",
			ElapsedSeconds: 240, AgeSeconds: 8, ObservedAt: started,
			CPUSeconds: &heartbeatCPU, CPUDeltaSeconds: &heartbeatCPUDelta,
			RSSKB: &heartbeatRSS, IOReadDeltaKB: &heartbeatIORead, IOWriteDeltaKB: &heartbeatIOWrite,
			Processes: &heartbeatProcs,
		},
	})
	assertExactKeys(t, "run", runDoc, []string{
		"id", "status", "conclusion", "repo", "branch", "sha", "pullRequest",
		"provenance", "checks", "createdAt", "updatedAt", "phases", "heartbeat",
	})
	// The liveness twin. The readings are pointers on both sides so an
	// unmeasured one stays absent from the wire; the keys pinned here are the ones
	// a MEASURED beat carries.
	assertExactKeys(t, "run heartbeat", runDoc["heartbeat"], []string{
		"version", "sequence", "attempt", "session", "phase", "elapsedSeconds",
		"ageSeconds", "cpuSeconds", "cpuDeltaSeconds", "rssKb", "ioReadDeltaKb",
		"ioWriteDeltaKb", "processes", "observedAt",
	})
	phases, _ := runDoc["phases"].([]any)
	assertExactKeys(t, "run phase", phases[0], []string{
		"phase", "status", "conclusion", "reason", "required", "attempt", "createdAt", "startedAt", "finishedAt",
	})

	assertExactKeys(t, "run list", marshalToDoc(t, CIRunListResponse{
		Runs: []CIRunResponse{}, Limit: 50, Offset: 0, NextOffset: 50,
	}), []string{"runs", "limit", "offset", "nextOffset"})

	after := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	assertExactKeys(t, "run list filter", marshalToDoc(t, CIRunListFilter{
		Repo: "acme/app", Branch: "main", Ref: "abc", PullRequest: 7, Status: "failure", Phase: "test",
		CreatedAfter: &after, IDPrefix: "aa",
	}), []string{"repo", "branch", "ref", "pullRequest", "status", "phase", "createdAfter", "idPrefix"})

	assertExactKeys(t, "pipeline response", marshalToDoc(t, PipelineModeResponse{
		Scope: "workspace", Workspace: "ws", Mode: "paused", Actor: "iss|sub", Reason: "why",
		CanceledRuns: []string{"run-1"},
	}), []string{"scope", "workspace", "mode", "actor", "reason", "canceledRuns"})

	assertExactKeys(t, "repair response", marshalToDoc(t, CIRepairResult{
		Workspace:            "ws",
		ReapedRuns:           2,
		ReapedPhases:         3,
		CancellationRequests: 2,
		ScannedRuns:          4000,
		ReconciledRuns:       3900,
		ScanLowerBound:       true,
	}), []string{
		"workspace", "reapedRuns", "reapedPhases", "cancellationRequests",
		"scannedRuns", "reconciledRuns", "scanLowerBound",
	})

	// The provenance fields are part of the wire the CLI decodes: the
	// answer to "what actually ran" has to survive `--output=json` as data, not
	// only as prose inside a dimension explanation.
	statusDoc := marshalToDoc(t, CIStatusResponse{
		Workspace: "ws", Healthy: false, QueueDepth: 2, OldestQueuedSeconds: 30, StuckRuns: 1,
		DispatchAxis: "service", LastRunDigest: "sha256:abc", LastRunCLIVersion: "0.1.0-2c62ff1a",
		LastRunRunnerChannel: "stable", LastRunRunnerSelector: "oci.example/runner:stable", LastRunSourceRevision: strings.Repeat("a", 40),
		CheckOutboxPending: 3, CheckOutboxOldestPendingSeconds: 900,
		LiveRun: "run-42", LiveRunPhase: "gate", LiveRunSequence: 12, LiveRunHeartbeatSeconds: 8,
		Dimensions: []CIStatusDimension{{Name: "pipeline", Scope: "workspace", State: "paused", Explanation: "x", Recovery: "y"}},
	})
	assertExactKeys(t, "status", statusDoc, []string{
		"workspace", "healthy", "dimensions", "queueDepth", "oldestQueuedSeconds", "stuckRuns",
		"dispatchAxis", "lastRunDigest", "lastRunCliVersion", "lastRunRunnerChannel", "lastRunRunnerSelector", "lastRunSourceRevision",
		"checkOutboxPending", "checkOutboxOldestPendingSeconds",
		"liveRun", "liveRunPhase", "liveRunSequence", "liveRunHeartbeatSeconds",
	})
	dims, _ := statusDoc["dimensions"].([]any)
	assertExactKeys(t, "status dimension", dims[0], []string{"name", "scope", "state", "explanation", "recovery"})

	logsDoc := marshalToDoc(t, CIRunLogsResponse{
		Entries:     []CILogEntry{{Body: "boom"}},
		NextCursor:  "cur",
		Diagnostics: &CIRunLogDiagnostics{FirstErrors: []string{"boom"}, Reproduce: "putnami …"},
	})
	assertExactKeys(t, "logs", logsDoc, []string{"entries", "nextCursor", "diagnostics"})
	assertExactKeys(t, "log diagnostics", logsDoc["diagnostics"], []string{"firstErrors", "reproduce"})

	// The tail's frame shape (entry / run-state / cursor) is no longer a local
	// twin here: `logs --follow` decodes it through the generated delivery-api
	// client (deliveryapiclient.CIRunLogFrame), and the clientgen contract test
	// pins that shape against the provider.

	usageDoc := marshalToDoc(t, CIUsageResponse{
		Workspace: "ws", WindowHours: 168, ScannedRuns: 10, ScanLowerBound: true,
		Substrates: map[string]int{"gce-spot": 1}, Profiles: map[string]int{"heavy": 1},
		Sources: map[string]int{"explicit": 1}, Outcomes: map[string]int{"success": 1},
		Gate:  map[string]CIUsageWallSummary{"gce-spot": {N: 1, MinSecs: 1, MaxSecs: 1, AvgSecs: 1}},
		Spend: CIUsageSpend{Currency: "EUR", Estimated: true, SpotEUR: 1, CloudRunEUR: 2, TotalEUR: 3, RunsWithDuration: 4},
	})
	assertExactKeys(t, "usage", usageDoc, []string{
		"workspace", "windowHours", "scannedRuns", "scanLowerBound",
		"substrates", "profiles", "sources", "outcomes", "gate", "spend",
	})
	gate, _ := usageDoc["gate"].(map[string]any)
	assertExactKeys(t, "usage gate", gate["gce-spot"], []string{"n", "minSecs", "maxSecs", "avgSecs"})
	assertExactKeys(t, "usage spend", usageDoc["spend"], []string{
		"currency", "estimated", "spotEur", "cloudRunEur", "totalEur", "runsWithDuration",
	})
}

func marshalToDoc(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

// --- Dispatch ---

func TestCIDispatchHelp(t *testing.T) {
	lines, err := captureCI(t, &ciFakeServer{}, []string{"help"})
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"cloud ci list", "cloud ci repair", "cloud ci pause", "sends its filters to the server"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("help missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "cloud ci release") {
		t.Fatalf("help still exposes the retired image-pair release command:\n%s", joined)
	}
	if strings.Contains(joined, "promote-worker") {
		t.Fatalf("main Cloud help still exposes the operator-only promotion verb:\n%s", joined)
	}
}

func TestCIDispatchDoesNotExposeOperatorPromotion(t *testing.T) {
	fake := &ciFakeServer{}
	_, err := captureCI(t, fake, []string{"promote-worker"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "unknown cloud ci sub-verb") {
		t.Fatalf("main Cloud promote-worker dispatch = %v, want unknown usage error", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("removed main Cloud operator verb reached network: %+v", fake.requests)
	}
}

func TestCIDispatchUnknownSubVerb(t *testing.T) {
	_, err := captureCI(t, &ciFakeServer{}, []string{"frobnicate"})
	if err == nil {
		t.Fatal("expected a usage error for an unknown sub-verb")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want %d (%v)", clicore.ExitCode(err), clicore.ExitUsage, err)
	}
}

// --- Verb → request (method + path + body + accepted status) ---

func TestCIListHitsRunsRoute(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{
			Runs: []CIRunResponse{{ID: "run-1", Status: "queued", Repo: "acme/app"}},
		}},
	}}
	if _, err := captureCI(t, fake, []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodGet || got.Path != "/v1/workspaces/ws-acme/runs" {
		t.Fatalf("request = %s %s, want GET the runs list route", got.Method, got.Path)
	}
	if !strings.Contains(got.Query, "limit=") || !strings.Contains(got.Query, "offset=") {
		t.Fatalf("query = %q, want limit+offset paging", got.Query)
	}
	if !strings.HasPrefix(got.Auth, "Bearer ") {
		t.Fatalf("authorization = %q, want a workspace bearer", got.Auth)
	}
}

func TestCIViewHitsByIDRoute(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{ID: "run-9", Status: "completed"}},
	}}
	if _, err := captureCI(t, fake, []string{"view", "run-9"}); err != nil {
		t.Fatalf("view: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodGet || got.Path != "/v1/workspaces/ws-acme/runs/run-9" {
		t.Fatalf("request = %s %s, want GET the by-id run route", got.Method, got.Path)
	}
}

func TestCIStartPostsRunStartAndAccepts202(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs": {status: http.StatusAccepted, body: CIRunResponse{ID: "run-2", Status: "queued", Repo: "acme/app", SHA: "deadbeef"}},
	}}
	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef", "--reason", "manual", "--yes"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodPost || got.Path != "/v1/workspaces/ws-acme/runs" {
		t.Fatalf("request = %s %s, want POST the runs route", got.Method, got.Path)
	}
	if clicore.StringValue(got.Body["repo"]) != "acme/app" || clicore.StringValue(got.Body["sha"]) != "deadbeef" || clicore.StringValue(got.Body["reason"]) != "manual" {
		t.Fatalf("start body = %v, want repo/sha/reason set", got.Body)
	}
}

func TestCICancelPostsCancelAndAccepts200(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs/run-3/cancel": {status: 200, body: CIRunResponse{ID: "run-3", Status: "canceled", Conclusion: "canceled"}},
	}}
	if _, err := captureCI(t, fake, []string{"cancel", "run-3", "--reason", "oops", "--yes"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodPost || got.Path != "/v1/workspaces/ws-acme/runs/run-3/cancel" {
		t.Fatalf("request = %s %s, want POST the cancel route", got.Method, got.Path)
	}
	if clicore.StringValue(got.Body["reason"]) != "oops" {
		t.Fatalf("cancel body = %v, want reason set", got.Body)
	}
}

func TestCIRetryPostsRetryWithBody(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs/run-4/retry": {status: http.StatusAccepted, body: CIRunResponse{ID: "run-5", Status: "queued"}},
	}}
	if _, err := captureCI(t, fake, []string{"retry", "run-4", "--phase", "test", "--no-cache", "--reason", "flake", "--yes"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodPost || got.Path != "/v1/workspaces/ws-acme/runs/run-4/retry" {
		t.Fatalf("request = %s %s, want POST the retry route", got.Method, got.Path)
	}
	if clicore.StringValue(got.Body["phase"]) != "test" || got.Body["noCache"] != true || clicore.StringValue(got.Body["reason"]) != "flake" {
		t.Fatalf("retry body = %v, want phase/noCache/reason", got.Body)
	}
}

func TestCIStatusHitsStatusRoute(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/ci/status": {status: 200, body: CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{}}},
	}}
	if _, err := captureCI(t, fake, []string{"status"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	// The status read comes first; the usage rollup follows it.
	if len(fake.requests) == 0 {
		t.Fatal("no CI request was recorded")
	}
	got := fake.requests[0]
	if got.Method != http.MethodGet || got.Path != "/v1/workspaces/ws-acme/ci/status" {
		t.Fatalf("request = %s %s, want GET the ci status route", got.Method, got.Path)
	}
}

// TestCIUsageHitsUsageRoute pins the verb→request shape AND that --window
// actually reaches the server as a query. The flag was inert once already (read
// from params but never declared in the CLI manifest), and a silently dropped
// window is invisible in the output — every number still renders, just over the
// wrong span.
func TestCIUsageHitsUsageRoute(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/ci/usage": {status: 200, body: CIUsageResponse{
			Workspace: "ws-acme", WindowHours: 24, ScannedRuns: 1,
		}},
	}}
	if _, err := captureCI(t, fake, []string{"usage", "--window", "24h"}); err != nil {
		t.Fatalf("usage: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodGet || got.Path != "/v1/workspaces/ws-acme/ci/usage" {
		t.Fatalf("request = %s %s, want GET the ci usage route", got.Method, got.Path)
	}
	if got.Query != "window=24h" {
		t.Fatalf("query = %q, want the window forwarded", got.Query)
	}
}

// TestCIInsightsRendersTheGateReport pins the verb→request shape and the two
// facts a deep dive is FOR: the CPU balance sheet renders with its utilization,
// and the report's own 64-task bound is stated rather than presented as a total.
// A truncated list read as a whole one is the only way this verb could lie.
func TestCIInsightsRendersTheGateReport(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{
			ID: "run-9", Status: "completed", Conclusion: "success",
		}},
		"GET /v1/workspaces/ws-acme/runs/run-9/insights": {status: 200, body: protocolcli.ReportFile{
			ProtocolVersion: 2, SessionID: "20260816-112015-bfa4a2", Origin: "cli",
			StartTime: "2026-08-16T11:20:15Z", EndTime: "2026-08-16T11:25:00Z",
			Run: protocolcli.ReportRun{
				Outcome: "success", DurationMs: 285_011,
				Counts: protocolcli.RunCounts{Total: 73, Succeeded: 43, Skipped: 30},
				CPU: &protocolcli.RunCPU{
					AllocatedMillicores: 8000, AllocatedSource: "cgroup-quota",
					AllocatedMs: 2_280_088, ActualMs: 1_140_044, Executions: 73,
				},
			},
			Commands:   []protocolcli.ReportCommand{},
			Jobs:       []protocolcli.ReportJob{{Project: "libs/ci", Task: "test", Outcome: "succeeded", DurationMs: 61_000}},
			ElidedJobs: 9,
		}},
	}}
	lines, err := captureCI(t, fake, []string{"insights", "run-9"})
	if err != nil {
		t.Fatalf("insights: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodGet || got.Path != "/v1/workspaces/ws-acme/runs/run-9/insights" {
		t.Fatalf("request = %s %s, want GET the run insights route", got.Method, got.Path)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "50% utilized") {
		t.Fatalf("insights output did not render the CPU utilization:\n%s", out)
	}
	if !strings.Contains(out, "1 of 10 selected tasks") || !strings.Contains(out, "9 further task(s)") {
		t.Fatalf("insights output did not state the report's own task bound:\n%s", out)
	}
}

// A run with no gate report is a DIFFERENT answer from a run that does not
// exist, and the verb has to say which — the run resolves, the report 404s, and
// the message explains what produces one.
func TestCIInsightsExplainsAMissingReport(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{ID: "run-9", Status: "completed"}},
		"GET /v1/workspaces/ws-acme/runs/run-9/insights": {
			status: http.StatusNotFound, body: map[string]string{"error": "run insights not found"},
		},
	}}
	_, err := captureCI(t, fake, []string{"insights", "run-9"})
	if err == nil {
		t.Fatal("insights on a run with no report succeeded, want an explained failure")
	}
	if !strings.Contains(err.Error(), "carries no gate report") {
		t.Fatalf("error = %q, want it to say the run carries no gate report", err.Error())
	}
}

// The per-task timeline. It is a SEPARATE read from insights because
// the report contract is closed and its job list is capped at 64: absolute
// per-task start and end cannot ride inside it, and they are exactly what a
// critical-path reconstruction needs.
func TestCITimingsRendersThePerTaskTimeline(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{
			ID: "run-9", Status: "completed", Conclusion: "success",
		}},
		"GET /v1/workspaces/ws-acme/runs/run-9/insights/timings": {status: 200, body: map[string]any{
			"runId":     "run-9",
			"sessionId": "20260912-101112-abcdef",
			"tasks": []map[string]any{
				{
					"key": "/go/libs/a:test~test", "project": "/go/libs/a", "task": "test~test",
					"command": "test", "status": "success", "reuse": "none",
					"start": "2026-09-12T10:11:13+02:00", "end": "2026-09-12T10:16:13+02:00",
					"durationMs": 300000, "executionId": "exec-9", "cpuMs": 240000,
				},
				{
					"key": "/go/libs/b:build~compile", "project": "/go/libs/b", "task": "build~compile",
					"command": "build", "status": "success", "reuse": "remote-cache",
					"end": "2026-09-12T10:11:20+02:00", "durationMs": 15,
				},
			},
			"elidedTasks": 3, "maxTasks": 4096, "malformedLines": 0,
		}},
	}}
	lines, err := captureCI(t, fake, []string{"timings", "run-9"})
	if err != nil {
		t.Fatalf("timings: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodGet || got.Path != "/v1/workspaces/ws-acme/runs/run-9/insights/timings" {
		t.Fatalf("request = %s %s, want GET the run timings route", got.Method, got.Path)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "20260912-101112-abcdef") {
		t.Fatalf("timings output did not name the session it was derived from:\n%s", out)
	}
	if !strings.Contains(out, "2 of 5 recorded terminals") || !strings.Contains(out, "dropped the 3 shortest") {
		t.Fatalf("timings output did not state the server's own bound:\n%s", out)
	}
	if !strings.Contains(out, "2026-09-12T10:11:13+02:00 → 2026-09-12T10:16:13+02:00") {
		t.Fatalf("timings output did not render the absolute window:\n%s", out)
	}
	// A task that emitted no start record has NO start, and the row says so
	// instead of back-computing one from end minus duration.
	if !strings.Contains(out, "(no start record)") {
		t.Fatalf("timings output invented a start for a task that recorded none:\n%s", out)
	}
	// A task whose execution was never measured carries no CPU share, never a zero.
	if !strings.Contains(out, "cpu        -") {
		t.Fatalf("timings output defaulted an unmeasured CPU share to zero:\n%s", out)
	}
}

// The default limit keeps the verb readable, and a PRESENT but unusable --limit
// is refused rather than silently folded into that default.
func TestCITimingsBoundsItsOwnOutput(t *testing.T) {
	rows := []map[string]any{}
	for i := 0; i < 60; i++ {
		rows = append(rows, map[string]any{
			"key": "/p:test~test", "project": "/p", "task": "test~test", "command": "test",
			"status": "success", "reuse": "none", "durationMs": 60 - i,
		})
	}
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{ID: "run-9", Status: "completed"}},
		"GET /v1/workspaces/ws-acme/runs/run-9/insights/timings": {status: 200, body: map[string]any{
			"runId": "run-9", "tasks": rows, "elidedTasks": 0, "maxTasks": 4096,
		}},
	}}
	lines, err := captureCI(t, fake, []string{"timings", "run-9"})
	if err != nil {
		t.Fatalf("timings: %v", err)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "Tasks (40 of 60") || !strings.Contains(out, "20 further row(s) not printed") {
		t.Fatalf("timings output did not apply its own default bound:\n%s", out)
	}
}

// `--limit 0` prints every row, a malformed value is REFUSED rather than folded
// into the default, and `--output=json` honors the flag instead of parsing it
// and dropping it.
func TestCITimingsHonoursItsLimitOnEveryPath(t *testing.T) {
	rows := []map[string]any{}
	for i := 0; i < 60; i++ {
		rows = append(rows, map[string]any{
			"key": "/p" + strconv.Itoa(i) + ":test~test", "project": "/p" + strconv.Itoa(i),
			"task": "test~test", "command": "test", "status": "success", "reuse": "none",
			"durationMs": 60 - i,
		})
	}
	newFake := func() *ciFakeServer {
		return &ciFakeServer{responses: map[string]ciStubResponse{
			"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{ID: "run-9", Status: "completed"}},
			"GET /v1/workspaces/ws-acme/runs/run-9/insights/timings": {status: 200, body: map[string]any{
				"runId": "run-9", "tasks": rows, "elidedTasks": 0, "maxTasks": 4096,
			}},
		}}
	}

	lines, err := captureCI(t, newFake(), []string{"timings", "run-9", "--limit", "0"})
	if err != nil {
		t.Fatalf("timings --limit 0: %v", err)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "Tasks (60 of 60") || strings.Contains(out, "not printed") {
		t.Fatalf("--limit 0 did not print every row:\n%s", out)
	}

	// A PRESENT but unusable value is a usage error: a silently ignored flag is
	// indistinguishable from an honored one.
	for _, bad := range []string{"forty", "-1", "1.5"} {
		if _, err := captureCI(t, newFake(), []string{"timings", "run-9", "--limit", bad}); err == nil {
			t.Fatalf("--limit %q was accepted", bad)
		}
	}

	// The JSON path: no flag means the WHOLE document, because a machine consumer
	// asking for the document should get it.
	lines, err = captureCI(t, newFake(), []string{"timings", "run-9", "--output", "json"})
	if err != nil {
		t.Fatalf("timings --output=json: %v", err)
	}
	var whole CIRunTimingsResponse
	if err := json.Unmarshal([]byte(strings.Join(lines, "")), &whole); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if len(whole.Tasks) != 60 || whole.ElidedTasks != 0 {
		t.Fatalf("json without --limit = %d tasks / %d elided, want the whole table",
			len(whole.Tasks), whole.ElidedTasks)
	}

	// With the flag it truncates AND accounts for what it dropped, so the emitted
	// document's own arithmetic stays true.
	lines, err = captureCI(t, newFake(), []string{"timings", "run-9", "--output", "json", "--limit", "10"})
	if err != nil {
		t.Fatalf("timings --output=json --limit 10: %v", err)
	}
	var bounded CIRunTimingsResponse
	if err := json.Unmarshal([]byte(strings.Join(lines, "")), &bounded); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if len(bounded.Tasks) != 10 {
		t.Fatalf("json --limit 10 = %d tasks, want the flag honored rather than dropped", len(bounded.Tasks))
	}
	if len(bounded.Tasks)+bounded.ElidedTasks != 60 {
		t.Fatalf("len(tasks)+elidedTasks = %d, want the run's own 60 terminals",
			len(bounded.Tasks)+bounded.ElidedTasks)
	}
}

// A run with no derived table is a DIFFERENT answer from a run that does not
// exist, and the verb has to say which.
func TestCITimingsExplainsAMissingTable(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{ID: "run-9", Status: "completed"}},
		"GET /v1/workspaces/ws-acme/runs/run-9/insights/timings": {
			status: http.StatusNotFound, body: map[string]string{"error": "run timings not found"},
		},
	}}
	_, err := captureCI(t, fake, []string{"timings", "run-9"})
	if err == nil {
		t.Fatal("timings on a run with no table succeeded, want an explained failure")
	}
	if !strings.Contains(err.Error(), "carries no per-task timings") {
		t.Fatalf("error = %q, want it to say the run carries no timings", err.Error())
	}
	if _, err := captureCI(t, fake, []string{"timings"}); err == nil {
		t.Fatal("timings with no run id succeeded, want a usage error")
	}
}

// `timings --against` reads why a task executed: the cache key tells an
// input that changed from an entry that went missing, and a task with no key
// from both.
func TestCITimingsAgainstNamesWhyEachTaskExecuted(t *testing.T) {
	row := func(project, status, reuse, digest string, durationMs int) map[string]any {
		out := map[string]any{
			"key": project + ":test~test", "project": project, "task": "test~test", "command": "test",
			"status": status, "reuse": reuse, "durationMs": durationMs,
		}
		if digest != "" {
			out["inputDigest"] = digest
		}
		return out
	}
	newFake := func() *ciFakeServer {
		return &ciFakeServer{responses: map[string]ciStubResponse{
			"GET /v1/workspaces/ws-acme/runs/run-9": {status: 200, body: CIRunResponse{ID: "run-9", Status: "completed"}},
			"GET /v1/workspaces/ws-acme/runs/run-8": {status: 200, body: CIRunResponse{ID: "run-8", Status: "completed"}},
			"GET /v1/workspaces/ws-acme/runs/run-7": {status: 200, body: CIRunResponse{ID: "run-7", Status: "completed"}},
			"GET /v1/workspaces/ws-acme/runs/run-9/insights/timings": {status: 200, body: map[string]any{
				"runId": "run-9", "maxTasks": 4096, "tasks": []map[string]any{
					row("/changed", "success", "none", "sha256:bbbbbbbbbbbbbbbb", 9000),
					row("/missing", "success", "none", "sha256:cccc", 7000),
					row("/other-failed", "success", "none", "sha256:dddd", 5000),
					row("/uncached", "success", "none", "", 3000),
					row("/new", "success", "none", "sha256:eeee", 1000),
					row("/hit", "success", "remote-cache", "sha256:ffff", 4),
					row("/skipped", "skipped", "none", "", 2),
				},
			}},
			"GET /v1/workspaces/ws-acme/runs/run-8/insights/timings": {status: 200, body: map[string]any{
				"runId": "run-8", "maxTasks": 4096, "tasks": []map[string]any{
					row("/changed", "success", "none", "sha256:aaaaaaaaaaaaaaaa", 9000),
					row("/missing", "success", "remote-cache", "sha256:cccc", 3),
					row("/other-failed", "failed", "none", "sha256:dddd", 5000),
					row("/uncached", "success", "none", "", 3000),
					row("/hit", "success", "none", "sha256:ffff", 800),
				},
			}},
			// A table the control plane derived before it recorded the key.
			"GET /v1/workspaces/ws-acme/runs/run-7/insights/timings": {status: 200, body: map[string]any{
				"runId": "run-7", "maxTasks": 4096, "tasks": []map[string]any{
					row("/changed", "success", "none", "", 9000),
				},
			}},
		}}
	}

	lines, err := captureCI(t, newFake(), []string{"timings", "run-9", "--against", "run-8", "--output", "json"})
	if err != nil {
		t.Fatalf("timings --against: %v", err)
	}
	var causes CIRunTimingCauses
	if err := json.Unmarshal([]byte(strings.Join(lines, "")), &causes); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if causes.RunID != "run-9" || causes.AgainstRunID != "run-8" || causes.Tasks != 7 || causes.Served != 1 {
		t.Fatalf("header = %+v", causes)
	}
	want := map[string]string{
		"/changed":      ciCauseKeyChanged,
		"/missing":      ciCauseEntryMissing,
		"/other-failed": ciCauseOtherFailed,
		"/uncached":     ciCauseNoKey,
		"/new":          ciCauseNotInOtherRun,
	}
	if len(causes.Executed) != len(want) {
		t.Fatalf("executed = %d rows, want %d: a served or skipped task is not an executed one", len(causes.Executed), len(want))
	}
	for _, got := range causes.Executed {
		if want[got.Project] != got.Cause {
			t.Fatalf("%s cause = %q, want %q", got.Project, got.Cause, want[got.Project])
		}
	}
	if causes.Executed[0].Project != "/changed" || causes.Executed[0].AgainstInputDigest != "sha256:aaaaaaaaaaaaaaaa" {
		t.Fatalf("first row = %+v, want the longest wall with the other run's key", causes.Executed[0])
	}
	if len(causes.Classes) != 5 || causes.Classes[0].Cause != ciCauseKeyChanged || causes.Classes[0].DurationMs != 9000 {
		t.Fatalf("classes = %+v, want one per cause, heaviest wall first", causes.Classes)
	}

	// --limit bounds the rows and accounts for them; the weights still describe
	// the whole run.
	lines, err = captureCI(t, newFake(), []string{"timings", "run-9", "--against", "run-8", "--output", "json", "--limit", "2"})
	if err != nil {
		t.Fatalf("timings --against --limit 2: %v", err)
	}
	var bounded CIRunTimingCauses
	if err := json.Unmarshal([]byte(strings.Join(lines, "")), &bounded); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if len(bounded.Executed) != 2 || bounded.ElidedExecuted != 3 || len(bounded.Classes) != 5 {
		t.Fatalf("bounded = %d rows / %d elided / %d classes", len(bounded.Executed), bounded.ElidedExecuted, len(bounded.Classes))
	}

	lines, err = captureCI(t, newFake(), []string{"timings", "run-9", "--against", "run-8"})
	if err != nil {
		t.Fatalf("timings --against (human): %v", err)
	}
	out := strings.Join(lines, "\n")
	for _, fragment := range []string{
		"run-9 against run-8", "7 recorded, 1 served by the cache, 5 executed",
		"aaaaaaaaaaaa → bbbbbbbbbbbb", ciCauseEntryMissing,
	} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("output lacks %q:\n%s", fragment, out)
		}
	}

	// A table without any key cannot be compared, and the verb says so instead of
	// reporting every task as uncacheable.
	_, err = captureCI(t, newFake(), []string{"timings", "run-9", "--against", "run-7"})
	if err == nil || !strings.Contains(err.Error(), "without cache keys") {
		t.Fatalf("error = %v, want the keyless table refused", err)
	}
}

func TestCIRepairPostsWorkspaceRepair(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/ci/repair": {
			status: http.StatusOK,
			body: CIRepairResult{
				Workspace:            "ws-acme",
				ReapedRuns:           2,
				ReapedPhases:         4,
				CancellationRequests: 2,
				ScannedRuns:          1200,
				ReconciledRuns:       900,
			},
		},
	}}
	if _, err := captureCI(t, fake, []string{
		"repair", "--reason", "recover stale rows", "--yes",
	}); err != nil {
		t.Fatalf("repair: %v", err)
	}
	got := lastCIRequest(t, fake)
	if got.Method != http.MethodPost || got.Path != "/v1/workspaces/ws-acme/ci/repair" {
		t.Fatalf("request = %s %s, want POST the workspace repair route", got.Method, got.Path)
	}
	if clicore.StringValue(got.Body["reason"]) != "recover stale rows" {
		t.Fatalf("repair body = %v, want reason", got.Body)
	}
}

func TestCIPipelineVerbsPostWorkspacePipeline(t *testing.T) {
	for _, tc := range []struct {
		verb string
		mode string
	}{{"pause", "paused"}, {"drain", "draining"}, {"resume", "active"}} {
		t.Run(tc.verb, func(t *testing.T) {
			fake := &ciFakeServer{responses: map[string]ciStubResponse{
				"POST /v1/workspaces/ws-acme/pipeline": {status: 200, body: PipelineModeResponse{Scope: "workspace", Workspace: "ws-acme", Mode: tc.mode, Actor: "sub"}},
			}}
			if _, err := captureCI(t, fake, []string{tc.verb, "--reason", "incident", "--yes"}); err != nil {
				t.Fatalf("%s: %v", tc.verb, err)
			}
			got := lastCIRequest(t, fake)
			if got.Method != http.MethodPost || got.Path != "/v1/workspaces/ws-acme/pipeline" {
				t.Fatalf("request = %s %s, want POST the WORKSPACE pipeline route", got.Method, got.Path)
			}
			if clicore.StringValue(got.Body["mode"]) != tc.mode {
				t.Fatalf("mode = %q, want %q", clicore.StringValue(got.Body["mode"]), tc.mode)
			}
		})
	}
}

// A 409 (gated) and a 404 (unknown/foreign) must render honest, specific messages.
func TestCIStartGatedConflictRendersSpecificMessage(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs": {status: http.StatusConflict, body: map[string]any{"error": "no run opened: the pipeline is paused or the change was gated"}},
	}}
	_, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "abc", "--reason", "x", "--yes"})
	if err == nil {
		t.Fatal("expected an error for the 409 gated start")
	}
	if !strings.Contains(err.Error(), "paused") || !strings.Contains(err.Error(), "409") {
		t.Fatalf("409 message should be specific about the gate: %v", err)
	}
}

// The THIRD re-run door's refusal, end to end. A commit that has used
// every run-attempt id is a deterministic, permanent, client-side condition; the
// API answers 412 carrying the bound and the (repo, sha), and this asserts the
// operator actually reads them. Before the API mapped the class it fell to the
// generic 500 branch and this command printed "delivery operation failed" — no
// bound, no commit, and a status that says the server is broken.
func TestCIRetryExhaustedAttemptsRendersTheBoundAndTheCommit(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs/run-1/retry": {status: http.StatusPreconditionFailed, body: map[string]any{
			"error": "no free run attempt for acme/app@abc123 — all 1000 attempt ids in the probe's range are taken; only a new commit can be run again",
		}},
	}}
	_, err := captureCI(t, fake, []string{"retry", "run-1", "--reason", "flaky dep", "--yes"})
	if err == nil {
		t.Fatal("expected an error for the 412 exhausted retry")
	}
	for _, want := range []string{"acme/app@abc123", "1000", "412"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("412 message %q never names %q", err.Error(), want)
		}
	}
	if clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("exit = %d, want ExitAPI (%v)", clicore.ExitCode(err), err)
	}
}

func TestCICancelNotFoundRendersSpecificMessage(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs/ghost/cancel": {status: http.StatusNotFound, body: map[string]any{"error": "run not found"}},
	}}
	_, err := captureCI(t, fake, []string{"cancel", "ghost", "--reason", "x", "--yes"})
	if err == nil {
		t.Fatal("expected an error for the 404 cancel")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "run not found") {
		t.Fatalf("404 message should be specific: %v", err)
	}
}

// --- reason required (usage before any request) ---

func TestCIMutatingVerbRequiresReason(t *testing.T) {
	fake := &ciFakeServer{}
	_, err := captureCI(t, fake, []string{"cancel", "run-1", "--yes"})
	if err == nil {
		t.Fatal("expected a usage error without --reason")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage (%v)", clicore.ExitCode(err), err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want none — reason is checked before any request", fake.requests)
	}
	_, err = captureCI(t, fake, []string{"repair", "--yes"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("repair without reason err = %v, want usage", err)
	}
}

// --- JSONL shape: one object per line ---

func TestCIListJSONLOneObjectPerLine(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs": {status: 200, body: CIRunListResponse{Runs: []CIRunResponse{
			{ID: "run-1", Status: "queued", Repo: "acme/app"},
			{ID: "run-2", Status: "completed", Conclusion: "success", Repo: "acme/app"},
		}}},
	}}
	lines, err := captureCI(t, fake, []string{"list", "--output=jsonl"})
	if err != nil {
		t.Fatalf("list jsonl: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("jsonl lines = %d, want one object per run (2)", len(lines))
	}
	for i, line := range lines {
		var run CIRunResponse
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			t.Fatalf("line %d not a stable run object: %v (%q)", i, err, line)
		}
		if run.ID == "" {
			t.Fatalf("line %d missing id: %q", i, line)
		}
	}
}

func TestCILogsJSONLOneEntryPerLine(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs": {status: 200, body: CIRunLogsResponse{
			Entries: []CILogEntry{{Body: "line one"}, {Body: "line two", Severity: ciSeverityError}},
		}},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--output=jsonl"})
	if err != nil {
		t.Fatalf("logs jsonl: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("jsonl lines = %d, want one object per entry (2)", len(lines))
	}
	for i, line := range lines {
		var entry CILogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d not a stable log entry: %v (%q)", i, err, line)
		}
	}
}

// --- Pipeline consequence note (D2) is TABLE-only; JSONL emits the raw response ---

func TestCIPipelineTableNotesConsequenceButJSONLDoesNot(t *testing.T) {
	stub := map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/pipeline": {status: 200, body: PipelineModeResponse{Scope: "workspace", Workspace: "ws-acme", Mode: "paused", Actor: "sub"}},
	}
	tableLines, err := captureCI(t, &ciFakeServer{responses: stub}, []string{"pause", "--reason", "incident", "--yes"})
	if err != nil {
		t.Fatalf("pause table: %v", err)
	}
	joined := strings.Join(tableLines, "\n")
	if !strings.Contains(joined, "Putnami CI") || !strings.Contains(joined, "ci status") {
		t.Fatalf("table output should carry the ruleset consequence note + status pointer:\n%s", joined)
	}

	jsonlLines, err := captureCI(t, &ciFakeServer{responses: stub}, []string{"pause", "--reason", "incident", "--yes", "--output=jsonl"})
	if err != nil {
		t.Fatalf("pause jsonl: %v", err)
	}
	if len(jsonlLines) != 1 {
		t.Fatalf("jsonl lines = %d, want exactly the raw PipelineModeResponse", len(jsonlLines))
	}
	var resp PipelineModeResponse
	if err := json.Unmarshal([]byte(jsonlLines[0]), &resp); err != nil {
		t.Fatalf("jsonl not the raw PipelineModeResponse: %v (%q)", err, jsonlLines[0])
	}
	if strings.Contains(jsonlLines[0], "Putnami CI") || strings.Contains(jsonlLines[0], "consequence") {
		t.Fatalf("jsonl must NOT carry a synthesized consequence field: %q", jsonlLines[0])
	}
}

// The pause and resume notes say what happens to queued runs and to the
// pushes received while paused.
func TestCIPipelineNotesNameHeldWork(t *testing.T) {
	for _, tc := range []struct {
		verb, mode, want string
	}{
		{"pause", "paused", "pushes received while paused are held"},
		{"resume", "active", "each push held while paused gets a run"},
	} {
		stub := map[string]ciStubResponse{
			"POST /v1/workspaces/ws-acme/pipeline": {status: 200, body: PipelineModeResponse{Scope: "workspace", Workspace: "ws-acme", Mode: tc.mode, Actor: "sub"}},
		}
		lines, err := captureCI(t, &ciFakeServer{responses: stub}, []string{tc.verb, "--reason", "incident", "--yes"})
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		if joined := strings.Join(lines, "\n"); !strings.Contains(joined, tc.want) {
			t.Fatalf("%s output does not say %q:\n%s", tc.verb, tc.want, joined)
		}
	}
}

// SSE frame parsing (heartbeat comments, the data: framing, per-frame decode)
// is now the generated delivery-api client's job (go.putnami.dev/client
// server_stream.go), covered by the framework's own tests. What stays here is
// the follow loop's re-open policy, exercised end-to-end below through
// ciFakeServer's raw SSE bodies — the same wire format, now read by the
// generated client instead of a hand-written scanner.

// The follow loop replays the last frame's cursor on reconnect and resumes.
func TestCIFollowLogsReplaysCursorOnReconnect(t *testing.T) {
	prevBackoff := ciFollowBackoff
	prevMax := ciFollowMaxReconnects
	ciFollowBackoff = time.Millisecond
	ciFollowMaxReconnects = 3
	defer func() { ciFollowBackoff = prevBackoff; ciFollowMaxReconnects = prevMax }()

	// First tail connection yields one frame then EOF; the reconnect must carry
	// cursor=c1. After the second (cursorless-of-more) empty connection the drop
	// budget drains and follow returns.
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs/tail": {status: 200, sse: `data: {"entry":{"body":"one"},"cursor":"c1"}` + "\n\n"},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--follow"})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "one") {
		t.Fatalf("expected the streamed entry to print, got %v", lines)
	}
	// A reconnect after the first frame must replay cursor=c1.
	sawCursor := false
	for _, r := range fake.requests {
		if strings.HasSuffix(r.Path, "/logs/tail") && strings.Contains(r.Query, "cursor=c1") {
			sawCursor = true
		}
	}
	if !sawCursor {
		t.Fatalf("expected a reconnect replaying cursor=c1; requests = %+v", fake.requests)
	}
}

// A run-state frame carries no `entry`. The renderer must skip it — printing a
// zero entry would emit a blank timestamped line for every run event — while the
// cursor it carries is still tracked for the reconnect.
func TestCIFollowLogsSkipsEntrylessFramesButKeepsTheirCursor(t *testing.T) {
	prevBackoff := ciFollowBackoff
	prevMax := ciFollowMaxReconnects
	ciFollowBackoff = time.Millisecond
	ciFollowMaxReconnects = 1
	defer func() { ciFollowBackoff = prevBackoff; ciFollowMaxReconnects = prevMax }()

	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs/tail": {status: 200, sse: "" +
			`data: {"run":{"status":"in_progress","final":false},"cursor":"cichunk1:0:0"}` + "\n\n" +
			`data: {"entry":{"body":"one"},"cursor":"cichunk1:0:1"}` + "\n\n" +
			`data: {"run":{"status":"in_progress","final":false},"cursor":"cichunk1:0:1"}` + "\n\n"},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--follow"})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "one") != 1 {
		t.Fatalf("expected exactly the one entry line, got %v", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, "0001-01-01") {
			t.Fatalf("a run frame rendered as a zero-valued entry line: %q", line)
		}
	}
	sawCursor := false
	for _, r := range fake.requests {
		if strings.Contains(r.Query, "cursor=cichunk1%3A0%3A1") {
			sawCursor = true
		}
	}
	if !sawCursor {
		t.Fatalf("expected the reconnect to replay the run frame's cursor; requests = %+v", fake.requests)
	}
}

// A run frame with final=true ends follow immediately: the server has said the
// log is complete, so reconnecting would only re-open a stream with nothing left.
func TestCIFollowLogsStopsOnFinalRunFrame(t *testing.T) {
	prevBackoff := ciFollowBackoff
	prevMax := ciFollowMaxReconnects
	ciFollowBackoff = time.Second // a reconnect would be observable as a hang
	ciFollowMaxReconnects = 5
	defer func() { ciFollowBackoff = prevBackoff; ciFollowMaxReconnects = prevMax }()

	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/runs/run-1/logs/tail": {status: 200, sse: "" +
			`data: {"entry":{"body":"one"},"cursor":"cichunk1:0:1"}` + "\n\n" +
			`data: {"run":{"status":"completed","conclusion":"success","final":true},"cursor":"cichunk1:1:0"}` + "\n\n"},
	}}
	lines, err := captureCI(t, fake, []string{"logs", "run-1", "--follow"})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "one") {
		t.Fatalf("expected the streamed entry to print, got %v", lines)
	}
	tails := 0
	for _, r := range fake.requests {
		if strings.HasSuffix(r.Path, "/logs/tail") {
			tails++
		}
	}
	if tails != 1 {
		t.Fatalf("tail connections = %d, want exactly 1 (final must stop the follow loop)", tails)
	}
}

// --- Confirmation (declined aborts, sends nothing) ---

func TestCIMutatingVerbConsultsConfirmAndAbortsOnDecline(t *testing.T) {
	fake := &ciFakeServer{}
	ioctx, root := newCITestIO(t, fake)
	consulted := false
	ioctx.Confirm = func(string) (string, bool) { consulted = true; return "n", true }

	err := runCI(ioctx, root, []string{"cancel", "run-1", "--reason", "stop"})
	if err == nil {
		t.Fatal("expected an abort when the confirmation is declined")
	}
	if !consulted {
		t.Fatal("the mutating verb did not consult ioctx.Confirm")
	}
	for _, r := range fake.requests {
		if strings.Contains(r.Path, "/cancel") {
			t.Fatalf("a declined confirmation must send NO cancel request; got %+v", r)
		}
	}
}

func TestCIConfirmBypassedByYes(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs/run-1/cancel": {status: 200, body: CIRunResponse{ID: "run-1", Status: "canceled"}},
	}}
	ioctx, root := newCITestIO(t, fake)
	ioctx.Confirm = func(string) (string, bool) { t.Fatal("--yes must bypass the prompt"); return "", false }
	if err := runCI(ioctx, root, []string{"cancel", "run-1", "--reason", "stop", "--yes"}); err != nil {
		t.Fatalf("cancel --yes: %v", err)
	}
}

func lastCIRequest(t *testing.T, fake *ciFakeServer) ciCapturedRequest {
	t.Helper()
	if len(fake.requests) == 0 {
		t.Fatal("no CI request was recorded")
	}
	return fake.requests[len(fake.requests)-1]
}

// TestCIHeartbeatLine covers the run-detail renderer's liveness line: an absent
// heartbeat renders nothing (never a zero-age beat), the measured readings ride
// along when the substrate took them, and an unmeasured one is simply not shown.
func TestCIHeartbeatLine(t *testing.T) {
	if got := ciHeartbeatLine(nil); got != "" {
		t.Fatalf("an unbeaten run rendered %q, want no line", got)
	}

	cpuDelta := 1.5
	rss := int64(2048)
	procs := 6
	full := ciHeartbeatLine(&CIRunHeartbeat{
		Version: 1, Sequence: 9, Attempt: 2, Phase: "gate", ElapsedSeconds: 195,
		AgeSeconds: 75, CPUDeltaSeconds: &cpuDelta, RSSKB: &rss, Processes: &procs,
	})
	for _, want := range []string{"gate #9", "1m15s ago", "attempt 2", "elapsed 3m15s", "cpu +1.5s", "rss 2MiB", "procs 6"} {
		if !strings.Contains(full, want) {
			t.Fatalf("heartbeat line %q omits %q", full, want)
		}
	}

	bare := ciHeartbeatLine(&CIRunHeartbeat{Version: 1, Sequence: 1, Attempt: 1, Phase: "gate", AgeSeconds: 5})
	if strings.Contains(bare, "cpu") || strings.Contains(bare, "rss") || strings.Contains(bare, "procs") {
		t.Fatalf("an unmeasured beat rendered readings: %q", bare)
	}
}

// Durations are rendered from whole seconds served by the control plane, so the
// CLI never reconciles two clocks. The three magnitudes an operator sees during
// a run all have to read correctly.
func TestCIFormatDurationSeconds(t *testing.T) {
	cases := map[int]string{0: "0s", -3: "0s", 45: "45s", 60: "1m00s", 195: "3m15s", 3600: "1h00m", 5415: "1h30m"}
	for seconds, want := range cases {
		if got := ciFormatDurationSeconds(seconds); got != want {
			t.Fatalf("ciFormatDurationSeconds(%d) = %q, want %q", seconds, got, want)
		}
	}
}

// TestCITimingLine pins the wall-clock decomposition line: stage labels
// in execution order, compact durations (sub-10s readings keep one decimal),
// discriminators in a trailing note, absent readings absent.
func TestCITimingLine(t *testing.T) {
	if got := ciTimingLine(nil); got != "" {
		t.Fatalf("nil timing rendered %q", got)
	}
	i64 := func(v int64) *int64 { return &v }
	full := ciTimingLine(&CIRunTiming{
		QueueUsec: i64(2_000_000), ColdStartUsec: i64(3_700_000),
		FetchUsec: i64(5_000_000), InstallUsec: i64(39_000_000),
		GateUsec: i64(83_000_000), TailUsec: i64(13_000_000),
		RunDurationUsec: i64(140_000_000),
		CheckoutMode:    "full", CLIVersion: "0.1.0-abc123",
	})
	want := "queue 2s · cold-start 3.7s · fetch 5s · bootstrap 39s · gate 1m23s · tail 13s · total 2m20s (checkout full, cli 0.1.0-abc123)"
	if full != want {
		t.Fatalf("timing line = %q\nwant          %q", full, want)
	}
	if bare := ciTimingLine(&CIRunTiming{GateUsec: i64(61_000_000)}); bare != "gate 1m01s" {
		t.Fatalf("bare timing line = %q, want just the gate", bare)
	}
}

// The run detail renders the timing decomposition beside the liveness line.
func TestCIRenderRunDetail_ShowsTiming(t *testing.T) {
	io, out := renderIO()
	gate := int64(83_000_000)
	ciRenderRunDetail(io, &CIRunResponse{
		ID: "run-1", Status: "completed",
		Timing: &CIRunTiming{GateUsec: &gate, CheckoutMode: "full"},
	})
	if !strings.Contains(out.String(), "Timing:     gate 1m23s (checkout full)") {
		t.Fatalf("run detail omits the timing line:\n%s", out)
	}

	io2, out2 := renderIO()
	ciRenderRunDetail(io2, &CIRunResponse{ID: "run-2", Status: "completed"})
	if strings.Contains(out2.String(), "Timing:") {
		t.Fatalf("a run with no timing rendered a timing line:\n%s", out2)
	}
}

// TestCIDiskLine pins the runner disk line: the peak against the disk
// it was measured on, its phase and clock, then the resident readings. Absent
// readings stay absent, and a run that measured nothing renders no line.
func TestCIDiskLine(t *testing.T) {
	if got := ciDiskLine(nil); got != "" {
		t.Fatalf("nil disk rendered %q", got)
	}
	if got := ciDiskLine(&CIRunDisk{}); got != "" {
		t.Fatalf("an empty disk block rendered %q", got)
	}
	f := func(v float64) *float64 { return &v }
	full := ciDiskLine(&CIRunDisk{
		PeakGiB: f(41.5), PeakAt: "2026-09-18T10:03:12Z", PeakPhase: "gate",
		TotalGiB: f(60), EndGiB: f(30), GoCacheGiB: f(9.8), GoModcacheGiB: f(0),
	})
	want := "peak 41.5 GiB of 60.0 GiB (69%, phase gate, 2026-09-18T10:03:12Z) · end 30.0 GiB · Go build cache 9.8 GiB · Go module cache 0.0 GiB"
	if full != want {
		t.Fatalf("disk line = %q\nwant        %q", full, want)
	}
	if bare := ciDiskLine(&CIRunDisk{PeakGiB: f(12.26)}); bare != "peak 12.3 GiB" {
		t.Fatalf("bare peak = %q", bare)
	}
	if noPeak := ciDiskLine(&CIRunDisk{TotalGiB: f(100), EndGiB: f(17.04)}); noPeak != "size 100.0 GiB · end 17.0 GiB" {
		t.Fatalf("no-peak line = %q", noPeak)
	}
}

// The run detail renders the disk line beside the timing, and none for a run
// whose runner measured nothing.
func TestCIRenderRunDetail_ShowsDisk(t *testing.T) {
	io, out := renderIO()
	peak, total := 41.5, 60.0
	ciRenderRunDetail(io, &CIRunResponse{
		ID: "run-1", Status: "completed",
		Disk: &CIRunDisk{PeakGiB: &peak, TotalGiB: &total, PeakPhase: "gate"},
	})
	if !strings.Contains(out.String(), "Disk:       peak 41.5 GiB of 60.0 GiB (69%, phase gate)") {
		t.Fatalf("run detail omits the disk line:\n%s", out)
	}

	io2, out2 := renderIO()
	ciRenderRunDetail(io2, &CIRunResponse{ID: "run-2", Status: "completed"})
	if strings.Contains(out2.String(), "Disk:") {
		t.Fatalf("a run with no disk reading rendered a disk line:\n%s", out2)
	}
}

// The wire twin decodes the server's disk block member for member.
func TestCIRunResponse_DecodesTheDiskBlock(t *testing.T) {
	body := `{"id":"run-1","status":"completed","createdAt":"2026-09-18T10:00:00Z","updatedAt":"2026-09-18T10:10:00Z",` +
		`"disk":{"peakGiB":41.5,"peakAt":"2026-09-18T10:03:12Z","peakPhase":"gate","totalGiB":60,"endGiB":30,` +
		`"goCacheGiB":9.8,"goModcacheGiB":1.9}}`
	var run CIRunResponse
	if err := json.Unmarshal([]byte(body), &run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := run.Disk
	if d == nil || d.PeakGiB == nil || *d.PeakGiB != 41.5 || d.PeakAt != "2026-09-18T10:03:12Z" || d.PeakPhase != "gate" ||
		d.TotalGiB == nil || *d.TotalGiB != 60 || d.EndGiB == nil || *d.EndGiB != 30 ||
		d.GoCacheGiB == nil || *d.GoCacheGiB != 9.8 || d.GoModcacheGiB == nil || *d.GoModcacheGiB != 1.9 {
		t.Fatalf("disk = %+v", d)
	}
}

// TestCITestedLine pins the two shapes of the "what did this run execute" line
// and, above all, the third case: a run that recorded no tested tree
// renders NOTHING, because every run before the squash existed is that case and
// none of them may start claiming a provenance they never reported.
func TestCITestedLine(t *testing.T) {
	head := "abc123def4567890abc123def4567890abc123de"
	base := "1482257ec34cdb740f93cc86e212f7e7b7888183"
	tree := "0ad9f3c1a2b74e5d68c1f0b93e2a4d7c5f8e1b60"

	squashed := ciTestedLine(&CIRunResponse{
		SHA:    head,
		Checks: map[string]string{"base_sha": base, "ci_tested_tree": tree},
	})
	if want := "base 1482257ec34c + head abc123def456 → tree 0ad9f3c1a2b7"; squashed != want {
		t.Fatalf("tested line = %q\nwant         %q", squashed, want)
	}

	noBase := ciTestedLine(&CIRunResponse{SHA: head, Checks: map[string]string{"ci_tested_tree": tree}})
	if want := "head abc123def456 → tree 0ad9f3c1a2b7"; noBase != want {
		t.Fatalf("no-base tested line = %q, want %q", noBase, want)
	}

	// An explicit squash reads like the record before it.
	explicit := ciTestedLine(&CIRunResponse{
		SHA:    head,
		Checks: map[string]string{"base_sha": base, "ci_tested_tree": tree, "ci_tested_checkout": "squash"},
	})
	if want := "base 1482257ec34c + head abc123def456 → tree 0ad9f3c1a2b7"; explicit != want {
		t.Fatalf("squash tested line = %q, want %q", explicit, want)
	}

	// A head tree never borrows the saga's base, because that run (a
	// publishing pull request here) merged nothing onto it.
	headTree := ciTestedLine(&CIRunResponse{
		SHA:    head,
		Checks: map[string]string{"base_sha": base, "ci_tested_tree": tree, "ci_tested_checkout": "head"},
	})
	if want := "head abc123def456 → tree 0ad9f3c1a2b7"; headTree != want {
		t.Fatalf("head tested line = %q, want %q", headTree, want)
	}

	// A run that stopped before its checkout, and every record written before
	// the tested tree existed.
	if got := ciTestedLine(&CIRunResponse{SHA: head}); got != "" {
		t.Fatalf("a run that tested its checkout as fetched rendered %q", got)
	}
	if got := ciTestedLine(&CIRunResponse{SHA: head, Checks: map[string]string{"base_sha": base}}); got != "" {
		t.Fatalf("a run with a base but no tested tree rendered %q", got)
	}
}

// The run detail prints the tested line under the refs that name the run, and
// prints nothing extra for a run that carries no tree — an older record must
// render byte-identically to the way it renders today.
func TestCIRenderRunDetail_ShowsTested(t *testing.T) {
	io, out := renderIO()
	ciRenderRunDetail(io, &CIRunResponse{
		ID: "run-1", Status: "completed", SHA: "abc123def4567890abc123def4567890abc123de",
		Checks: map[string]string{
			"base_sha":       "1482257ec34cdb740f93cc86e212f7e7b7888183",
			"ci_tested_tree": "0ad9f3c1a2b74e5d68c1f0b93e2a4d7c5f8e1b60",
		},
	})
	if !strings.Contains(out.String(), "Tested:     base 1482257ec34c + head abc123def456 → tree 0ad9f3c1a2b7") {
		t.Fatalf("run detail omits the tested line:\n%s", out)
	}

	io2, out2 := renderIO()
	ciRenderRunDetail(io2, &CIRunResponse{ID: "run-2", Status: "completed", SHA: "abc123def4567890abc123def4567890abc123de"})
	if strings.Contains(out2.String(), "Tested:") {
		t.Fatalf("a run that recorded no tested tree rendered a tested line:\n%s", out2)
	}
}

// The wire twin carries the run's annotations, which is where the tested tree and
// the saga's base sha both arrive (`checks` on the run get, unchanged).
func TestCIRunResponse_DecodesTheTestedAnnotations(t *testing.T) {
	body := `{"id":"run-1","status":"completed","sha":"abc123def4567890abc123def4567890abc123de",` +
		`"createdAt":"2026-09-20T10:00:00Z","updatedAt":"2026-09-20T10:10:00Z",` +
		`"checks":{"base_sha":"1482257ec34cdb740f93cc86e212f7e7b7888183","base_ref":"main",` +
		`"ci_tested_tree":"0ad9f3c1a2b74e5d68c1f0b93e2a4d7c5f8e1b60"}}`
	var run CIRunResponse
	if err := json.Unmarshal([]byte(body), &run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if run.Checks["ci_tested_tree"] != "0ad9f3c1a2b74e5d68c1f0b93e2a4d7c5f8e1b60" ||
		run.Checks["base_sha"] != "1482257ec34cdb740f93cc86e212f7e7b7888183" ||
		run.Checks["base_ref"] != "main" {
		t.Fatalf("checks = %+v", run.Checks)
	}
}

// The run detail renders the liveness line beside the run's own facts.
func TestCIRenderRunDetail_ShowsHeartbeat(t *testing.T) {
	io, out := renderIO()
	ciRenderRunDetail(io, &CIRunResponse{
		ID: "run-1", Status: "in_progress", Repo: "acme/app",
		Heartbeat: &CIRunHeartbeat{Version: 1, Sequence: 4, Attempt: 1, Phase: "gate", AgeSeconds: 12},
	})
	if !strings.Contains(out.String(), "Heartbeat:  gate #4, 12s ago") {
		t.Fatalf("run detail omits the heartbeat:\n%s", out)
	}

	io2, out2 := renderIO()
	ciRenderRunDetail(io2, &CIRunResponse{ID: "run-2", Status: "completed"})
	if strings.Contains(out2.String(), "Heartbeat:") {
		t.Fatalf("a run that never beat rendered a heartbeat line:\n%s", out2)
	}
}

// renderIO is a stdout-only IO for the pure renderers: they take an IO and write
// lines, so a buffer is the whole fixture.
func renderIO() (clicore.IO, *strings.Builder) {
	var buf strings.Builder
	return clicore.IO{Stdout: func(s string) { buf.WriteString(s); buf.WriteString("\n") }}, &buf
}

// --- per-run gate fan-out -------------------------------------------------------

// TestCIMaxParallelParsing pins the three outcomes `--max-parallel` must have,
// and the third is the one that matters.
//
//  1. A positive whole number is the requested width.
//  2. ABSENCE — no flag, an empty value, or the literal `auto` the host CLI's
//     default context commonly supplies for this well-known flag name — is zero,
//     which the wire omits and the control plane forwards as "no CI_MAX_PARALLEL".
//     The runner's own default then applies, so the default lives in exactly one
//     place. Reading `auto` as a width would silently re-dispatch every run.
//  3. Anything else is a USAGE error, never a fallback: a ladder cell that quietly
//     ran at another width yields a number that is wrong without looking wrong.
func TestCIMaxParallelParsing(t *testing.T) {
	for _, valid := range []struct {
		raw  any
		want int
	}{
		{"1", 1}, {"8", 8}, {8, 8}, {float64(16), 16}, {"32", 32},
	} {
		got, err := ciMaxParallel(map[string]any{"max-parallel": valid.raw}, "start")
		if err != nil {
			t.Fatalf("--max-parallel %v: %v", valid.raw, err)
		}
		if got != valid.want {
			t.Fatalf("--max-parallel %v = %d, want %d", valid.raw, got, valid.want)
		}
	}

	for name, params := range map[string]map[string]any{
		"absent": {},
		"empty":  {"max-parallel": ""},
		"auto":   {"max-parallel": "auto"},
		"AUTO":   {"max-parallel": "AUTO"},
	} {
		got, err := ciMaxParallel(params, "start")
		if err != nil {
			t.Fatalf("%s must not be an error: %v", name, err)
		}
		if got != 0 {
			t.Fatalf("%s = %d, want 0 so the runner's own default applies", name, got)
		}
	}

	for _, invalid := range []any{"0", "-1", "33", "1024", "abc", "4x", "1.5", true} {
		if _, err := ciMaxParallel(map[string]any{"max-parallel": invalid}, "retry"); err == nil {
			t.Fatalf("--max-parallel %v must be a usage error, not a silent default", invalid)
		}
	}
}

// The flag has to survive the real dispatcher param path (ParseFlags →
// MergeParams), not just a hand-built map, and it has to land on the wire under
// the key the control plane decodes.
func TestCIStartAndRetrySendTheRequestedGateFanOut(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs": {status: http.StatusAccepted,
			body: CIRunResponse{ID: "run-2", Status: "queued", Repo: "acme/app", SHA: "deadbeef"}},
		"POST /v1/workspaces/ws-acme/runs/run-1/retry": {status: http.StatusAccepted,
			body: CIRunResponse{ID: "run-3", Status: "queued"}},
	}}

	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "ladder cell", "--max-parallel", "8", "--yes"}); err != nil {
		t.Fatalf("ci start: %v", err)
	}
	if got := clicore.NumberParam(lastCIRequest(t, fake).Body, "maxParallel"); got == nil || *got != 8 {
		t.Fatalf("start body maxParallel = %v, want 8", got)
	}

	if _, err := captureCI(t, fake, []string{"retry", "run-1", "--reason", "ladder cell",
		"--max-parallel", "2", "--yes"}); err != nil {
		t.Fatalf("ci retry: %v", err)
	}
	if got := clicore.NumberParam(lastCIRequest(t, fake).Body, "maxParallel"); got == nil || *got != 2 {
		t.Fatalf("retry body maxParallel = %v, want 2", got)
	}

	// Omitted stays omitted end to end: the key must not appear on the wire at all,
	// so the control plane forwards absence and the runner applies its own default.
	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "ordinary", "--yes"}); err != nil {
		t.Fatalf("ci start: %v", err)
	}
	if _, present := lastCIRequest(t, fake).Body["maxParallel"]; present {
		t.Fatalf("an unrequested fan-out must be omitted from the wire body: %v", lastCIRequest(t, fake).Body)
	}

	// An unparseable width never reaches the control plane.
	before := len(fake.requests)
	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "typo", "--max-parallel", "eight", "--yes"}); err == nil {
		t.Fatal("an unparseable --max-parallel must fail as a usage error")
	}
	if len(fake.requests) != before {
		t.Fatal("an invalid --max-parallel still dispatched a run")
	}
}

func TestCIStartAndRetrySendTheRequestedRunnerSize(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs": {status: http.StatusAccepted,
			body: CIRunResponse{ID: "run-2", Status: "queued", Repo: "acme/app", SHA: "deadbeef"}},
		"POST /v1/workspaces/ws-acme/runs/run-1/retry": {status: http.StatusAccepted,
			body: CIRunResponse{ID: "run-3", Status: "queued"}},
	}}
	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "small canary", "--size", "small", "--yes"}); err != nil {
		t.Fatalf("ci start: %v", err)
	}
	if got := clicore.StringParam(lastCIRequest(t, fake).Body, "size"); got != "small" {
		t.Fatalf("start body size = %q, want small", got)
	}
	if _, err := captureCI(t, fake, []string{"retry", "run-1", "--reason", "large canary",
		"--size", "large", "--yes"}); err != nil {
		t.Fatalf("ci retry: %v", err)
	}
	if got := clicore.StringParam(lastCIRequest(t, fake).Body, "size"); got != "large" {
		t.Fatalf("retry body size = %q, want large", got)
	}
	// The 32-vCPU class is a size like any other on the wire.
	if _, err := captureCI(t, fake, []string{"retry", "run-1", "--reason", "32 vCPU cold run",
		"--size", "XLarge", "--yes"}); err != nil {
		t.Fatalf("ci retry --size xlarge: %v", err)
	}
	if got := clicore.StringParam(lastCIRequest(t, fake).Body, "size"); got != "xlarge" {
		t.Fatalf("retry body size = %q, want xlarge", got)
	}
	before := len(fake.requests)
	_, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "typo", "--size", "medium", "--yes"})
	if err == nil {
		t.Fatal("an unknown --size must fail as a usage error")
	}
	// The refusal lists the same names the server accepts.
	if want := "xsmall, small, standard, large, xlarge"; !strings.Contains(err.Error(), want) {
		t.Fatalf("--size refusal = %q, want it to list %q", err.Error(), want)
	}
	if len(fake.requests) != before {
		t.Fatal("an invalid --size still dispatched a run")
	}
}

// --- per-run forced-cold gate ---------------------------------------------------

// TestCINoCacheParsing pins the two outcomes `--no-cache` must have at this layer.
// It is a boolean here, so unlike `--max-parallel` there is no third, invalid
// outcome to reject: the value a runner could fail to honor is the ENV token, and
// the entrypoint refuses that one at bootstrap rather than degrading to a warm run.
//
// What matters is that ABSENCE is false, which the wire omits and the control
// plane forwards as "no CI_NO_CACHE" — so the runner's own default is the only
// default there is, and an ordinary run's gate argv is unchanged.
func TestCINoCacheParsing(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"absent":          {},
		"explicit cache":  {"cache": true},
		"cache as string": {"cache": "true"},
	} {
		if ciNoCache(params) {
			t.Fatalf("%s must leave the run warm so the runner's own default applies", name)
		}
	}
	// The extension parser supplies cache=false; the host forwards its global
	// flag as no-cache=true. Both shapes force the same cold-run wire field.
	for name, params := range map[string]map[string]any{
		"extension --no-cache": {"cache": false},
		"host --no-cache":      {"no-cache": true},
		"host camel case":      {"noCache": "true"},
		"cache=false":          {"cache": "false"},
		"cache=0 string":       {"cache": "0"},
	} {
		if !ciNoCache(params) {
			t.Fatalf("%s must force the gate cold", name)
		}
	}
}

// The flag has to survive the real dispatcher param path on BOTH doors and land
// on the wire under the key the control plane decodes. `start` is the new door;
// `retry` once accepted the flag without passing it to the gate.
func TestCIStartAndRetrySendTheForcedColdGate(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"POST /v1/workspaces/ws-acme/runs": {status: http.StatusAccepted,
			body: CIRunResponse{ID: "run-2", Status: "queued", Repo: "acme/app", SHA: "deadbeef"}},
		"POST /v1/workspaces/ws-acme/runs/run-1/retry": {status: http.StatusAccepted,
			body: CIRunResponse{ID: "run-3", Status: "queued"}},
	}}

	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "cold ladder cell", "--no-cache", "--yes"}); err != nil {
		t.Fatalf("ci start: %v", err)
	}
	if got := lastCIRequest(t, fake).Body["noCache"]; got != true {
		t.Fatalf("start body noCache = %v, want true", got)
	}

	if _, err := captureCI(t, fake, []string{"retry", "run-1", "--reason", "cold retry",
		"--no-cache", "--yes"}); err != nil {
		t.Fatalf("ci retry: %v", err)
	}
	if got := lastCIRequest(t, fake).Body["noCache"]; got != true {
		t.Fatalf("retry body noCache = %v, want true", got)
	}

	// Omitted stays omitted end to end: the key must not appear on the wire at all,
	// so the control plane forwards absence and the runner's gate stays warm.
	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "ordinary", "--yes"}); err != nil {
		t.Fatalf("ci start: %v", err)
	}
	if _, present := lastCIRequest(t, fake).Body["noCache"]; present {
		t.Fatalf("an unrequested cold run must be omitted from the wire body: %v", lastCIRequest(t, fake).Body)
	}

	// The two per-run ladder knobs are independent on the wire too.
	if _, err := captureCI(t, fake, []string{"start", "--repo", "acme/app", "--sha", "deadbeef",
		"--reason", "cold w=8", "--no-cache", "--max-parallel", "8", "--yes"}); err != nil {
		t.Fatalf("ci start: %v", err)
	}
	body := lastCIRequest(t, fake).Body
	if body["noCache"] != true {
		t.Fatalf("start body noCache = %v, want true", body["noCache"])
	}
	if got := clicore.NumberParam(body, "maxParallel"); got == nil || *got != 8 {
		t.Fatalf("start body maxParallel = %v, want 8", got)
	}
}

// Cache economics on the two human surfaces. Cache reuse used to be readable
// only in log lines; these pin that `ci view` and `ci status`
// now state it, and that a run which measured nothing prints NO line rather than a
// zeroed one that would read as a cold cache.
func TestCICacheLine(t *testing.T) {
	if got := ciCacheLine(nil); got != "" {
		t.Fatalf("an unmeasured run rendered %q, want no line", got)
	}
	if got := ciCacheLine(&CIRunCache{}); got != "" {
		t.Fatalf("a zero-task report rendered %q, want no line", got)
	}
	line := ciCacheLine(&CIRunCache{Tasks: 4, Hits: 3, Misses: 1, HitPercent: 75, ServedMS: 21, ExecutedMS: 41000})
	for _, want := range []string{"3/4 tasks reused (75%)", "1 executed", "21ms served from cache", "41s executed"} {
		if !strings.Contains(line, want) {
			t.Fatalf("cache line %q omits %q", line, want)
		}
	}
}

func TestCIRenderRunDetail_ShowsCacheEconomics(t *testing.T) {
	io, out := renderIO()
	ciRenderRunDetail(io, &CIRunResponse{
		ID: "run-1", Status: "completed", Conclusion: "success",
		Cache: &CIRunCache{Tasks: 2, Hits: 2, HitPercent: 100, ServedMS: 12},
	})
	if !strings.Contains(out.String(), "Cache:      2/2 tasks reused (100%)") {
		t.Fatalf("run detail omits the cache economics:\n%s", out)
	}

	io2, out2 := renderIO()
	ciRenderRunDetail(io2, &CIRunResponse{ID: "run-2", Status: "completed"})
	if strings.Contains(out2.String(), "Cache:") {
		t.Fatalf("a run with no builds report rendered a cache line:\n%s", out2)
	}
}

// A 503 from a service Delivery depends on reaches the operator as its
// sentence, through the generated client: the body's code and
// details match the contract the four run-operation routes declare.
//
// A 503 that Delivery did not write (the platform in front of it answers with
// its own body and no code) must stay a readable 503 too. The routes declare a
// second, schema-less 503 code for that, so the client does not hold the
// platform body to the typed error's details schema and report a contract
// violation in place of the outage.
func TestCIRunOperations503ReachesTheOperator(t *testing.T) {
	typed := map[string]any{
		"error": "Source refused the request (403)", "message": "Source refused the request (403)",
		"code": "delivery.dependency_unavailable", "dependency": "Source",
		"details": map[string]any{"dependency": "Source"},
	}
	for verb, tc := range map[string]struct {
		route string
		args  []string
	}{
		"start":  {"POST /v1/workspaces/ws-acme/runs", []string{"start", "--repo", "acme/app", "--sha", "abc", "--reason", "x", "--yes"}},
		"retry":  {"POST /v1/workspaces/ws-acme/runs/run-1/retry", []string{"retry", "run-1", "--reason", "x", "--yes"}},
		"cancel": {"POST /v1/workspaces/ws-acme/runs/run-1/cancel", []string{"cancel", "run-1", "--reason", "x", "--yes"}},
		"repair": {"POST /v1/workspaces/ws-acme/ci/repair", []string{"repair", "--reason", "x", "--yes"}},
	} {
		fake := &ciFakeServer{responses: map[string]ciStubResponse{tc.route: {status: http.StatusServiceUnavailable, body: typed}}}
		_, err := captureCI(t, fake, tc.args)
		if err == nil || !strings.Contains(err.Error(), "Source refused the request (403)") {
			t.Fatalf("%s: typed 503 rendered as %v, want the sentence that names Source", verb, err)
		}

		// The platform's own 503: a body with no code, not Delivery's envelope.
		fake = &ciFakeServer{responses: map[string]ciStubResponse{tc.route: {status: http.StatusServiceUnavailable, body: "Service Unavailable"}}}
		_, err = captureCI(t, fake, tc.args)
		if err == nil {
			t.Fatalf("%s: expected an error for the code-less 503", verb)
		}
		if strings.Contains(err.Error(), "invalid JSON response") || !strings.Contains(err.Error(), "503") {
			t.Fatalf("%s: code-less 503 rendered as %q, want a plain 503 and never a contract violation", verb, err.Error())
		}
		if clicore.ExitCode(err) != clicore.ExitAPI {
			t.Fatalf("%s: exit = %d, want ExitAPI (%v)", verb, clicore.ExitCode(err), err)
		}
	}
}
