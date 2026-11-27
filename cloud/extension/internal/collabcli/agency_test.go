package collabcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const agencyFeature = "cloud/agency-providers"

// agencyRequest is one request the agency-api stand-in received.
type agencyRequest struct {
	method, path, authorization string
	body                        map[string]any
}

// agencyStandIn is an HTTP stand-in for agency-api's agency routes: it
// answers each route with the document agency-api answers, or with
// refusals[route] when set, and records every request.
type agencyStandIn struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []agencyRequest
	refusals map[string]func(http.ResponseWriter)
}

func newAgencyStandIn(t *testing.T) *agencyStandIn {
	t.Helper()
	s := &agencyStandIn{refusals: map[string]func(http.ResponseWriter){}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

var agencyAnswers = map[string]string{
	"memory/context":                 `{"items":[],"page":{}}`,
	"memory/mission":                 `{"record":{"ref":{"source":"putnami:agency","id":"missions/42"},"revision":"a1b2c3d","kind":"mission","identity":{"workspace":"acme","repository":"acme/app","mission":"42"},"title":"A mission","content":"Task 2 done.","sources":[],"evidence":[],"provenance":{"recordedAt":"2026-09-24T08:10:00Z","recordedBy":"agent"},"freshness":{"updatedAt":"2026-09-24T08:10:00Z","retrievedAt":"2026-09-24T09:00:00Z"}}}`,
	"memory/checkpoint":              `{"record":{"ref":{"source":"putnami:agency","id":"missions/42"},"revision":"b2c3d4e","kind":"mission","identity":{"workspace":"acme","repository":"acme/app","mission":"42"},"title":"A mission","content":"Task 2 gated.","sources":[],"evidence":[],"provenance":{"recordedAt":"2026-09-24T08:10:00Z","recordedBy":"agent"},"freshness":{"updatedAt":"2026-09-24T08:10:00Z","retrievedAt":"2026-09-24T09:00:00Z"}},"replayed":false}`,
	"agent-sessions/host-b:s-1/sync": `{"session":{"state":"working"},"mission":{"key":"issue-596","title":"A mission","plan":[{"key":"t1","text":"Declare","status":"done"}],"open_decisions":[]},"inbox":[],"named_decisions":[],"replies":[],"min_interval_ms":5000,"now":"2026-10-26T09:00:01Z"}`,
	"decisions":                      `{"decision":{"id":"dec_1","state":"open","options":[]},"replayed":false}`,
	"messages/msg_02/reply":          `{"message":{"id":"msg_02"},"replayed":false}`,
	"missions/status":                `{"mission":{"key":"issue-701","title":"","plan":{"steps":[]},"openDecisions":[]},"sessions":[],"page":{}}`,
	"missions/start":                 `{"run":{"id":"R-1","mission":{"repository":"acme/app","key":"issue-7"},"kind":"build","state":"running","input":{"build":{"task":{"source":"github:acme/app","id":"7"},"change":{"base":"main","head":"topic"}}},"startedAt":"2026-11-19T09:00:00Z"},"replayed":false}`,
	"missions/report":                `{"run":{"id":"R-1","mission":{"repository":"acme/app","key":"issue-7"},"kind":"build","state":"failed","input":{"build":{"intention":"Add typed runs."}},"failure":"the gate failed","startedAt":"2026-11-19T09:00:00Z","endedAt":"2026-11-19T09:05:00Z"},"replayed":false}`,
	"missions/runs":                  `{"items":[],"page":{}}`,
}

func (s *agencyStandIn) serve(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	request := agencyRequest{method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization")}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &request.body)
	}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	refuse := s.refusals[r.URL.Path]
	s.mu.Unlock()
	if refuse != nil {
		refuse(w)
		return
	}
	answer, ok := agencyAnswers[strings.TrimPrefix(r.URL.Path, "/v1/workspaces/"+workspaceID+"/agency/")]
	if r.Method != http.MethodPost || !ok {
		http.Error(w, `{"code":"not_found","error":"Not Found","message":"no route"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, answer)
}

func (s *agencyStandIn) received() []agencyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agencyRequest(nil), s.requests...)
}

// refuse answers route with status, the first-party error envelope and the
// agency-api refusal details.
func (s *agencyStandIn) refuse(route string, status int, code, reason, current string) {
	details := map[string]string{}
	if reason != "" {
		details["reason"] = reason
	}
	if current != "" {
		details["current"] = current
	}
	s.refuseWith(route, status, code, details)
}

// refuseWith answers route with status, the first-party error envelope and
// the given refusal details.
func (s *agencyStandIn) refuseWith(route string, status int, code string, details map[string]string) {
	s.refusals["/v1/workspaces/"+workspaceID+"/agency/"+route] = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "error": http.StatusText(status), "message": "refused", "details": details})
	}
}

// agencyHarness is a provider on its default agency-api connection, for a
// workspace named acme, linked to the stand-in's control plane, whose origin
// is github.com/acme/app.
func agencyHarness(t *testing.T, standIn *agencyStandIn, signedIn bool) (*harness, string) {
	t.Helper()
	root, env := linkedWorkspace(t)
	link := map[string]any{"workspace_id": workspaceID, "control_plane_url": standIn.server.URL}
	data, _ := json.Marshal(link)
	if err := os.WriteFile(clicore.LinkPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(`{"name":"acme"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bearer := ""
	if signedIn {
		bearer = signIn(t, env)
	}
	provider := &Provider{Env: env, IO: clicore.IO{Client: standIn.server.Client()},
		OriginRemote: func(string) (string, error) { return "git@github.com:acme/app.git", nil }}
	return &harness{provider: provider, root: root}, bearer
}

// callVersion routes one operation of a contract version through Serve.
func (h *harness) callVersion(t *testing.T, contract string, version int, operation, arguments string) (string, collab.Response) {
	t.Helper()
	request := collab.ProviderRequest{
		Arguments:     json.RawMessage(arguments),
		WorkspaceRoot: h.root,
		Provider:      &collab.ProviderCall{Contract: contract, Version: version, Operation: operation},
	}
	in, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Serve(h.provider, bytes.NewReader(in), &out, &stderr); code != 0 {
		t.Fatalf("%s.%s exited %d: %s", contract, operation, code, stderr.String())
	}
	var response collab.Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("%s.%s answered %q: %v", contract, operation, out.String(), err)
	}
	return out.String(), response
}

type agencyOp struct {
	contract  string
	version   int
	operation string
	arguments string
	route     string
	reconcile string
}

var memoryOperations = []agencyOp{
	{collab.ContractMemory, 1, collab.OperationContext, `{"identity":{"mission":"42"},"kinds":["mission"],"page":{"size":10}}`, "memory/context", ""},
	{collab.ContractMemory, 1, collab.OperationMission, `{"identity":{"mission":"42"},"mission":"42"}`, "memory/mission", ""},
	{collab.ContractMemory, 1, collab.OperationCheckpoint, `{"identity":{"mission":"42"},"mission":"42","precondition":{"expectedRevision":"a1b2c3d"},"idempotencyKey":"42:checkpoint:4","content":"Task 2 gated.","evidence":[]}`, "memory/checkpoint", "memory.mission"},
}

var missionsV1Operations = []agencyOp{
	{collab.ContractMissions, 1, collab.OperationSync, `{"session":{"engine":"host-b","id":"s-1"},"mission":{"repository":"acme/app","key":"issue-596"},"event":"prompt_submit","state":"working","observedAt":"2026-10-26T09:00:00.125Z"}`, "agent-sessions/host-b:s-1/sync", "missions.sync"},
	{collab.ContractMissions, 1, collab.OperationAsk, `{"mission":{"repository":"acme/app","key":"issue-596"},"question":"Which bound?","options":[{"id":"bytes","label":"64 KiB"},{"id":"count","label":"50 messages"}],"idempotencyKey":"ask-1"}`, "decisions", "missions.ask"},
	{collab.ContractMissions, 1, collab.OperationReply, `{"message":"msg_02","reply":"Yes."}`, "messages/msg_02/reply", "missions.reply"},
	{collab.ContractMissions, 1, collab.OperationStatus, `{"mission":{"repository":"acme/app","key":"issue-701"}}`, "missions/status", ""},
}

var missionsRunOperations = []agencyOp{
	{collab.ContractMissions, 2, collab.OperationStart, `{"mission":{"repository":"acme/app","key":"issue-7"},"kind":"build","input":{"build":{"task":{"source":"github:acme/app","id":"7"},"change":{"base":"main","head":"topic"}}},"session":{"engine":"claude","id":"s-1"},"idempotencyKey":"start-1"}`, "missions/start", "missions.start"},
	{collab.ContractMissions, 2, collab.OperationReport, `{"run":"R-1","state":"failed","failure":"the gate failed"}`, "missions/report", "missions.report"},
	{collab.ContractMissions, 2, collab.OperationRuns, `{"mission":{"repository":"acme/app","key":"issue-7"},"kind":"review","page":{"size":20}}`, "missions/runs", ""},
}

func agencyOperations() []agencyOp {
	all := append([]agencyOp{}, memoryOperations...)
	all = append(all, missionsV1Operations...)
	for _, op := range missionsV1Operations {
		op.version = 2
		all = append(all, op)
	}
	return append(all, missionsRunOperations...)
}

// TestTheManifestOffersMemoryAndMissions holds the manifest's memory and
// missions declarations to the contract and to the dispatch table: memory
// version 1 and missions version 2, which also offers version 1, each run by
// the runtime's provider entry point.
func TestTheManifestOffersMemoryAndMissions(t *testing.T) {
	spectest.Proves(t, agencyFeature, "declares-memory-and-missions", "agency-provider-manifest-offer")
	offer := readOffer(t)
	handlers := New().Handlers()
	if got := offer.Versions(collab.ContractMemory); !slices.Equal(got, []int{1}) {
		t.Fatalf("memory versions = %v, want [1]", got)
	}
	if got := offer.Versions(collab.ContractMissions); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("missions versions = %v, want [1 2]", got)
	}
	want := map[collab.OperationKey]bool{}
	for _, op := range agencyOperations() {
		want[collab.OperationKey{Contract: op.contract, Version: op.version, Operation: op.operation}] = true
	}
	for _, contract := range []string{collab.ContractMemory, collab.ContractMissions} {
		for _, version := range offer.Versions(contract) {
			operations, _ := offer.Operations(contract, version)
			for name, op := range operations {
				key := collab.OperationKey{Contract: contract, Version: version, Operation: name}
				if _, ok := handlers[key]; !ok || !want[key] {
					t.Errorf("%s v%d %s is declared and has no handler", contract, version, name)
				}
				if !slices.Equal(op.Provider.Args, []string{"provider"}) || op.Provider.Command != "{extensionRuntime}" {
					t.Errorf("%s.%s runs %s %v, want the runtime's provider entry point", contract, name, op.Provider.Command, op.Provider.Args)
				}
			}
		}
	}
	for key := range want {
		operations, _ := offer.Operations(key.Contract, key.Version)
		if _, ok := operations[key.Operation]; !ok {
			t.Errorf("%s v%d %s has a handler and is not declared", key.Contract, key.Version, key.Operation)
		}
	}
	for _, version := range []int{1, 2} {
		missions, _ := offer.Operations(collab.ContractMissions, version)
		if got := missions[collab.OperationSync].Members(); !slices.Equal(got, collab.MissionMembers) {
			t.Errorf("missions v%d sync declares %v, want %v", version, got, collab.MissionMembers)
		}
		if got := missions[collab.OperationStatus].Members(); !slices.Equal(got, collab.MissionStatusMembers) {
			t.Errorf("missions v%d status declares %v, want %v", version, got, collab.MissionStatusMembers)
		}
	}
	memory, _ := offer.Operations(collab.ContractMemory, 1)
	if memory["checkpoint"].Operation.Preconditions != string(collab.PreconditionsAtomic) {
		t.Errorf("memory.checkpoint preconditions = %q, want atomic", memory["checkpoint"].Operation.Preconditions)
	}
	if _, ok := memory["search"]; ok {
		t.Error("memory.search is declared, and agency-api serves no search")
	}
}

// checkRoutes sends each operation once to the stand-in and checks the route,
// the bearer and the outcome.
func checkRoutes(t *testing.T, operations []agencyOp) {
	t.Helper()
	for _, op := range operations {
		t.Run(op.contract+".v"+string(rune('0'+op.version))+"."+op.operation, func(t *testing.T) {
			standIn := newAgencyStandIn(t)
			h, bearer := agencyHarness(t, standIn, true)
			_, response := h.callVersion(t, op.contract, op.version, op.operation, op.arguments)
			if response.Outcome != collab.OutcomeOK {
				t.Fatalf("outcome = %s: %+v", response.Outcome, response.Error)
			}
			requests := standIn.received()
			want := "/v1/workspaces/" + workspaceID + "/agency/" + op.route
			if len(requests) != 1 || requests[0].method != http.MethodPost || requests[0].path != want {
				t.Fatalf("agency-api received %+v, want one POST %s", requests, want)
			}
			if requests[0].authorization != "Bearer "+bearer {
				t.Fatal("the request does not carry the session's workspace bearer")
			}
		})
	}
}

// TestMemoryCallsAgencyAPI sends each memory operation to its agency-api
// route through the generated client, names the origin's repository in an
// identity without one, and answers another workspace's identity without a
// call.
func TestMemoryCallsAgencyAPI(t *testing.T) {
	spectest.Proves(t, agencyFeature, "memory-calls-agency-api", "agency-memory-routes")
	checkRoutes(t, memoryOperations)

	standIn := newAgencyStandIn(t)
	h, _ := agencyHarness(t, standIn, true)
	if _, response := h.callVersion(t, collab.ContractMemory, 1, collab.OperationCheckpoint, memoryOperations[2].arguments); response.Outcome != collab.OutcomeOK {
		t.Fatalf("checkpoint answered %s %+v", response.Outcome, response.Error)
	}
	identity, _ := standIn.received()[0].body["identity"].(map[string]any)
	if identity["repository"] != "acme/app" {
		t.Fatalf("the checkpoint identity is %v, want the origin's repository acme/app", identity)
	}

	standIn = newAgencyStandIn(t)
	h, _ = agencyHarness(t, standIn, true)
	_, context := h.callVersion(t, collab.ContractMemory, 1, collab.OperationContext, `{"identity":{"workspace":"other","mission":"42"}}`)
	_, mission := h.callVersion(t, collab.ContractMemory, 1, collab.OperationMission, `{"identity":{"workspace":"other","mission":"42"},"mission":"42"}`)
	_, checkpoint := h.callVersion(t, collab.ContractMemory, 1, collab.OperationCheckpoint,
		`{"identity":{"workspace":"other","mission":"42"},"mission":"42","precondition":{"mustNotExist":true},"idempotencyKey":"k","content":"c","evidence":[]}`)
	if context.Outcome != collab.OutcomeOK || mission.Outcome != collab.OutcomeNotFound || checkpoint.Outcome != collab.OutcomeDenied ||
		checkpoint.Error.Reason != reasonWorkspaceOther || len(standIn.received()) != 0 {
		t.Fatalf("another workspace answered %s, %s, %s after %d requests", context.Outcome, mission.Outcome, checkpoint.Outcome, len(standIn.received()))
	}
}

// TestMissionsCallAgencyAPI sends each missions operation of versions 1 and 2
// to its agency-api route through the generated client.
func TestMissionsCallAgencyAPI(t *testing.T) {
	spectest.Proves(t, agencyFeature, "missions-calls-agency-api", "agency-missions-routes")
	operations := append([]agencyOp{}, missionsV1Operations...)
	for _, op := range missionsV1Operations {
		op.version = 2
		operations = append(operations, op)
	}
	checkRoutes(t, operations)

	standIn := newAgencyStandIn(t)
	h, _ := agencyHarness(t, standIn, true)
	raw, response := h.callVersion(t, collab.ContractMissions, 1, collab.OperationSync, missionsV1Operations[0].arguments)
	var result collab.MissionsSyncResult
	if response.Outcome != collab.OutcomeOK || json.Unmarshal(response.Result, &result) != nil ||
		result.Mission.Key != "issue-596" || len(result.Mission.Plan.Steps) != 1 || result.MinIntervalMs != 5000 {
		t.Fatalf("sync answered %s", raw)
	}
	body := standIn.received()[0].body
	if body["engine"] != "host-b" || body["state"] != "working" {
		t.Fatalf("sync sent %v", body)
	}
}

// TestTheSyncForwardsTheSessionURL sends a sync's sessionUrl as session_url,
// an empty one included, and no session_url for a sync that carries none.
func TestTheSyncForwardsTheSessionURL(t *testing.T) {
	spectest.Proves(t, agencyFeature, "missions-calls-agency-api", "agency-sync-forwards-the-session-url")
	sync := missionsV1Operations[0].arguments
	cases := []struct {
		name, arguments string
		want            any
	}{
		{"an address", strings.Replace(sync, `"event"`, `"sessionUrl":"https://claude.ai/code/session_01","event"`, 1), "https://claude.ai/code/session_01"},
		{"an empty address", strings.Replace(sync, `"event"`, `"sessionUrl":"","event"`, 1), ""},
		{"no address", sync, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			standIn := newAgencyStandIn(t)
			h, _ := agencyHarness(t, standIn, true)
			raw, response := h.callVersion(t, collab.ContractMissions, 1, collab.OperationSync, tc.arguments)
			if response.Outcome != collab.OutcomeOK {
				t.Fatalf("sync answered %s", raw)
			}
			got, sent := standIn.received()[0].body["session_url"]
			if tc.want == nil {
				if sent {
					t.Fatalf("a sync without an address sent session_url %v", got)
				}
				return
			}
			if !sent || got != tc.want {
				t.Fatalf("session_url = %v (sent %v), want %q", got, sent, tc.want)
			}
		})
	}
}

// TestTheSyncForwardsTheLastPrompt sends a sync's lastPrompt as last_prompt,
// and no last_prompt for a sync that carries none or an empty one, so the
// route keeps the request it holds.
func TestTheSyncForwardsTheLastPrompt(t *testing.T) {
	spectest.Proves(t, agencyFeature, "missions-calls-agency-api", "agency-sync-forwards-the-last-prompt")
	sync := missionsV1Operations[0].arguments
	cases := []struct {
		name, arguments string
		want            any
	}{
		{"a request", strings.Replace(sync, `"event"`, `"lastPrompt":"Fix the build.\nThen push.","event"`, 1), "Fix the build.\nThen push."},
		{"an empty request", strings.Replace(sync, `"event"`, `"lastPrompt":"","event"`, 1), nil},
		{"no request", sync, nil},
	}
	for _, version := range []int{1, 2} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("v%d %s", version, tc.name), func(t *testing.T) {
				standIn := newAgencyStandIn(t)
				h, _ := agencyHarness(t, standIn, true)
				raw, response := h.callVersion(t, collab.ContractMissions, version, collab.OperationSync, tc.arguments)
				if response.Outcome != collab.OutcomeOK {
					t.Fatalf("sync answered %s", raw)
				}
				got, sent := standIn.received()[0].body["last_prompt"]
				if tc.want == nil {
					if sent {
						t.Fatalf("a sync without a request sent last_prompt %v", got)
					}
					return
				}
				if !sent || got != tc.want {
					t.Fatalf("last_prompt = %v (sent %v), want %q", got, sent, tc.want)
				}
			})
		}
	}
}

// TestMissionsRunsCallAgencyAPI sends start, report and runs of version 2 to
// their agency-api routes with the contract's document, and answers the run.
func TestMissionsRunsCallAgencyAPI(t *testing.T) {
	spectest.Proves(t, agencyFeature, "missions-runs-call-agency-api", "agency-missions-run-routes")
	checkRoutes(t, missionsRunOperations)

	standIn := newAgencyStandIn(t)
	h, _ := agencyHarness(t, standIn, true)
	_, response := h.callVersion(t, collab.ContractMissions, 2, collab.OperationStart, missionsRunOperations[0].arguments)
	var run collab.MissionsRunResult
	if response.Outcome != collab.OutcomeOK || json.Unmarshal(response.Result, &run) != nil || run.Run.ID != "R-1" || run.Run.State != collab.RunStateRunning {
		t.Fatalf("start answered %s %+v", response.Outcome, response.Error)
	}
	body := standIn.received()[0].body
	if body["kind"] != "build" || body["idempotencyKey"] != "start-1" {
		t.Fatalf("start sent %v, want the contract's document", body)
	}

	standIn = newAgencyStandIn(t)
	h, _ = agencyHarness(t, standIn, true)
	_, response = h.callVersion(t, collab.ContractMissions, 1, collab.OperationStart, missionsRunOperations[0].arguments)
	if response.Outcome == collab.OutcomeOK || len(standIn.received()) != 0 {
		t.Fatalf("start of version 1 answered %s after %d requests", response.Outcome, len(standIn.received()))
	}
}

// declaredCode is the stable code agency-api's schema declares for a status;
// only a refusal under its declared code carries its details.
var declaredCode = map[int]string{
	http.StatusBadRequest: "validation", http.StatusUnauthorized: "unauthorized", http.StatusForbidden: "forbidden",
	http.StatusNotFound: "not_found", http.StatusConflict: "conflict", http.StatusTooManyRequests: "rate_limited",
}

// TestAgencyRefusalsKeepTheirOutcome maps agency-api's refusals to the
// contract's outcomes, with the reason and current revision it names, and
// refuses binding settings and an unlinked workspace before any request.
func TestAgencyRefusalsKeepTheirOutcome(t *testing.T) {
	spectest.Proves(t, agencyFeature, "refusals-keep-their-outcome", "agency-provider-refusals")
	cases := []struct {
		op                      agencyOp
		status                  int
		reason, current         string
		outcome                 collab.Outcome
		wantReason, wantCurrent string
	}{
		{memoryOperations[2], http.StatusConflict, collab.ReasonRevisionConflict, "c0ffee", collab.OutcomeConflict, collab.ReasonRevisionConflict, "c0ffee"},
		{memoryOperations[1], http.StatusNotFound, "", "", collab.OutcomeNotFound, "mission.missing", ""},
		{missionsV1Operations[1], http.StatusConflict, collab.ReasonIdempotencyMismatch, "", collab.OutcomeConflict, collab.ReasonIdempotencyMismatch, ""},
		{missionsV1Operations[2], http.StatusForbidden, agencyOwnerOnly, "", collab.OutcomeDenied, collab.ReasonMessageDenied, ""},
		{missionsV1Operations[2], http.StatusConflict, "", "", collab.OutcomeConflict, collab.ReasonMessageClosed, ""},
		{missionsRunOperations[1], http.StatusConflict, collab.ReasonRunClosed, "", collab.OutcomeConflict, collab.ReasonRunClosed, ""},
		{missionsRunOperations[1], http.StatusBadRequest, collab.ReasonRunKindMismatch, "", collab.OutcomeInvalid, collab.ReasonRunKindMismatch, ""},
		{missionsV1Operations[3], http.StatusUnauthorized, "", "", collab.OutcomeDenied, reasonAgencyRejected, ""},
		{missionsV1Operations[3], http.StatusTooManyRequests, "", "", collab.OutcomeUnavailable, reasonAgencyRateLimit, ""},
	}
	for _, c := range cases {
		standIn := newAgencyStandIn(t)
		standIn.refuse(c.op.route, c.status, declaredCode[c.status], c.reason, c.current)
		h, _ := agencyHarness(t, standIn, true)
		_, response := h.callVersion(t, c.op.contract, c.op.version, c.op.operation, c.op.arguments)
		if response.Outcome != c.outcome || response.Error == nil || response.Error.Reason != c.wantReason || response.Error.Current != c.wantCurrent {
			t.Errorf("%s %d answered %s %+v, want %s %s", c.op.operation, c.status, response.Outcome, response.Error, c.outcome, c.wantReason)
		}
		if c.status == http.StatusUnauthorized && !strings.Contains(response.Error.Message, "putnami cloud login") {
			t.Errorf("a rejected credential does not name putnami cloud login: %s", response.Error.Message)
		}
	}

	// An invalid refusal names the member and the rule, so the session hook
	// can tell a sync refused for its lastPrompt.
	invalid := newAgencyStandIn(t)
	invalid.refuseWith(missionsV1Operations[0].route, http.StatusBadRequest, declaredCode[http.StatusBadRequest],
		map[string]string{"reason": "invalid", "field": "last_prompt", "problem": "too_long"})
	invalidHarness, _ := agencyHarness(t, invalid, true)
	_, refused := invalidHarness.callVersion(t, collab.ContractMissions, 1, collab.OperationSync, missionsV1Operations[0].arguments)
	if refused.Outcome != collab.OutcomeInvalid || refused.Error == nil || !strings.Contains(refused.Error.Message, "last_prompt: too_long") {
		t.Errorf("a sync refused for last_prompt answered %s %+v, want invalid naming last_prompt", refused.Outcome, refused.Error)
	}

	standIn := newAgencyStandIn(t)
	h, _ := agencyHarness(t, standIn, true)
	op := memoryOperations[0]
	request := collab.ProviderRequest{Arguments: json.RawMessage(op.arguments), WorkspaceRoot: h.root,
		Provider: &collab.ProviderCall{Contract: op.contract, Version: 1, Operation: op.operation, Settings: json.RawMessage(`{"url":"x"}`)}}
	in, _ := json.Marshal(request)
	var out, stderr bytes.Buffer
	Serve(h.provider, bytes.NewReader(in), &out, &stderr)
	var response collab.Response
	_ = json.Unmarshal(out.Bytes(), &response)
	if response.Outcome != collab.OutcomeInvalid || len(standIn.received()) != 0 {
		t.Fatalf("binding settings answered %s after %d requests", response.Outcome, len(standIn.received()))
	}

	root, env := linkedWorkspace(t)
	if err := os.Remove(clicore.LinkPath(root)); err != nil {
		t.Fatal(err)
	}
	unlinked := &harness{provider: &Provider{Env: env}, root: root}
	if _, response := unlinked.callVersion(t, op.contract, 1, op.operation, op.arguments); response.Outcome != collab.OutcomeUnavailable ||
		!strings.Contains(response.Error.Message, "putnami cloud setup") {
		t.Fatalf("an unlinked workspace answered %s %+v", response.Outcome, response.Error)
	}
}

// TestAnUncertainAgencyWriteIsNeverSentAgain answers unresolved, naming its
// reconcile, for a write agency-api did not confirm, sends it once, and
// answers a retryable unavailable for a read.
func TestAnUncertainAgencyWriteIsNeverSentAgain(t *testing.T) {
	spectest.Proves(t, agencyFeature, "never-repeats-an-uncertain-write", "agency-provider-uncertain-writes")
	for _, op := range agencyOperations() {
		standIn := newAgencyStandIn(t)
		standIn.refuse(op.route, http.StatusBadGateway, "http.bad_gateway", "", "")
		h, _ := agencyHarness(t, standIn, true)
		_, response := h.callVersion(t, op.contract, op.version, op.operation, op.arguments)
		if len(standIn.received()) != 1 {
			t.Fatalf("%s was sent %d times", op.operation, len(standIn.received()))
		}
		if op.reconcile != "" {
			if response.Outcome != collab.OutcomeUnresolved || !strings.Contains(response.Error.Reconcile, op.reconcile) {
				t.Errorf("%s v%d answered %s %+v, want unresolved naming %s", op.operation, op.version, response.Outcome, response.Error, op.reconcile)
			}
			continue
		}
		if response.Outcome != collab.OutcomeUnavailable || !response.Error.Retryable {
			t.Errorf("read %s answered %s %+v, want a retryable unavailable", op.operation, response.Outcome, response.Error)
		}
	}
}

// TestTheAgencySessionPrecedence presents the stored session over
// PUTNAMI_CLOUD_TOKEN, the token without a session, and sends nothing
// without either.
func TestTheAgencySessionPrecedence(t *testing.T) {
	spectest.Proves(t, agencyFeature, "signed-in-session-wins", "agency-provider-credentials")
	op := missionsV1Operations[3]

	standIn := newAgencyStandIn(t)
	h, bearer := agencyHarness(t, standIn, true)
	h.provider.Env[cloudTokenVar] = envToken
	if _, response := h.callVersion(t, op.contract, 1, op.operation, op.arguments); response.Outcome != collab.OutcomeOK ||
		standIn.received()[0].authorization != "Bearer "+bearer {
		t.Fatalf("session and token answered %s, sent %q", response.Outcome, standIn.received()[0].authorization)
	}

	standIn = newAgencyStandIn(t)
	h, _ = agencyHarness(t, standIn, false)
	h.provider.Env[cloudTokenVar] = envToken
	if _, response := h.callVersion(t, op.contract, 1, op.operation, op.arguments); response.Outcome != collab.OutcomeOK ||
		standIn.received()[0].authorization != "Bearer "+envToken {
		t.Fatalf("token alone answered %s", response.Outcome)
	}

	standIn = newAgencyStandIn(t)
	h, _ = agencyHarness(t, standIn, false)
	_, response := h.callVersion(t, op.contract, 1, op.operation, op.arguments)
	if response.Outcome != collab.OutcomeDenied || response.Error.Reason != reasonSignIn ||
		!strings.Contains(response.Error.Message, "putnami cloud login") || len(standIn.received()) != 0 {
		t.Fatalf("no credential answered %s %+v after %d requests", response.Outcome, response.Error, len(standIn.received()))
	}
}

// TestTheAgencyAnswerCarriesNoBearer replaces the presented bearer and the
// environment token wherever an answer would carry them.
func TestTheAgencyAnswerCarriesNoBearer(t *testing.T) {
	spectest.Proves(t, agencyFeature, "keeps-the-bearer-out-of-the-answer", "agency-provider-redacts-the-bearer")
	standIn := newAgencyStandIn(t)
	h, bearer := agencyHarness(t, standIn, true)
	h.provider.Env[cloudTokenVar] = envToken
	h.provider.ConnectAgency = func(*clicore.WorkspaceContext) (Agency, error) {
		return nil, errors.New("echoed Authorization: Bearer " + bearer + " and " + envToken)
	}
	raw, response := h.callVersion(t, collab.ContractMissions, 1, collab.OperationStatus, missionsV1Operations[3].arguments)
	if response.Outcome != collab.OutcomeUnavailable {
		t.Fatalf("outcome = %s", response.Outcome)
	}
	for name, secret := range map[string]string{"bearer": bearer, cloudTokenVar: envToken} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the answer carries the %s", name)
		}
	}
	if !strings.Contains(raw, "[REDACTED]") {
		t.Fatalf("the answer %s does not mark what it removed", raw)
	}
}
