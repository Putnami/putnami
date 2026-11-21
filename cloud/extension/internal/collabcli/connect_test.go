package collabcli

import (
	"encoding/json"
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
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
)

// sourceRequest is one request the Source stand-in received.
type sourceRequest struct {
	method, path, query, authorization string
	body                               map[string]any
}

// sourceStandIn is an HTTP stand-in for Putnami Source's collaboration
// routes: it answers each route with a collaboration document, or with
// refusals[route] when set, and records every request.
type sourceStandIn struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []sourceRequest
	refusals map[string]func(http.ResponseWriter)
}

func newSourceStandIn(t *testing.T) *sourceStandIn {
	t.Helper()
	s := &sourceStandIn{refusals: map[string]func(http.ResponseWriter){}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *sourceStandIn) serve(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	request := sourceRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, authorization: r.Header.Get("Authorization")}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &request.body)
	}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	refuse := s.refusals[r.Method+" "+r.URL.Path]
	s.mu.Unlock()
	if refuse != nil {
		refuse(w)
		return
	}
	var answer any
	prefix := "/v1/workspaces/" + workspaceID + "/source/"
	route := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case r.Method == http.MethodGet && route == "proposals":
		answer = collab.ProposalListResult{Items: []collab.Proposal{sourceProposal()}}
	case r.Method == http.MethodPost && route == "proposals":
		answer = collab.ProposalUpsertResult{Proposal: sourceProposal(), Created: true}
	case r.Method == http.MethodGet && route == "proposals/7":
		answer = collab.ProposalStatusResult{Proposal: sourceProposal(), Checks: collab.Checks{State: collab.ChecksStatePassing, Commit: "55e2a8e42"}}
	case r.Method == http.MethodGet && route == "proposals/7/comments":
		answer = collab.ProposalCommentListResult{Items: []collab.ProposalComment{}}
	case r.Method == http.MethodPost && route == "proposals/7/reviews":
		answer = collab.ProposalReviewResult{Review: collab.Review{Ref: collab.Ref{Source: "github:acme/app", ID: "r-1"}, Verdict: collab.ReviewVerdictComment}, Created: true}
	case r.Method == http.MethodPost && route == "proposals/7/automerge":
		answer = collab.ProposalResult{Proposal: sourceProposal()}
	case r.Method == http.MethodGet && route == "tasks/42":
		answer = collab.TaskResult{Task: task(collab.TaskStateOpen, []string{})}
	case r.Method == http.MethodPost && route == "tasks/42/transition":
		answer = collab.TaskResult{Task: task(collab.TaskStateDone, []string{})}
	case r.Method == http.MethodPost && route == "tasks/42/assign":
		answer = collab.TaskResult{Task: task(collab.TaskStateOpen, []string{"octocat"})}
	default:
		http.Error(w, `{"code":"not_found","error":"Not Found","message":"no route"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// changePage is the change page the Source stand-in answers on a proposal.
const changePage = "https://putnami.app/w/" + workspaceID + "/pr-7"

// sourceProposal is the proposal the Source stand-in answers: Source fills
// pageUrl with the proposal's change page on putnami.app.
func sourceProposal() collab.Proposal {
	p := proposal("main", "topic")
	p.PageURL = changePage
	return p
}

func (s *sourceStandIn) received() []sourceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sourceRequest(nil), s.requests...)
}

// refuse answers route with status and the first-party error envelope.
func (s *sourceStandIn) refuse(route string, status int, code, message string) {
	s.refusals[route] = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": http.StatusText(status), "message": message})
	}
}

// sourceHarness is a provider on its default Source connection, for a
// workspace linked to the stand-in's control plane.
func sourceHarness(t *testing.T, standIn *sourceStandIn) (*harness, string) {
	t.Helper()
	root, env := linkedWorkspace(t)
	link := map[string]any{"workspace_id": workspaceID, "control_plane_url": standIn.server.URL}
	data, _ := json.Marshal(link)
	if err := os.MkdirAll(filepath.Dir(clicore.LinkPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clicore.LinkPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	bearer := signIn(t, env)
	return &harness{provider: &Provider{Env: env, IO: clicore.IO{Client: standIn.server.Client()}}, root: root}, bearer
}

// TestTheSourceConnectionCallsEachRoute sends every served operation to its
// route of Putnami Source, once, for the linked workspace and under the
// session's workspace bearer, and answers the document Source answered.
func TestTheSourceConnectionCallsEachRoute(t *testing.T) {
	spectest.Proves(t, feature, "calls-source-for-each-served-operation", "collab-provider-routes-each-operation")
	prefix := "/v1/workspaces/" + workspaceID + "/source/"
	routes := map[string]string{
		"FindProposals":     "GET " + prefix + "proposals",
		"UpsertProposal":    "POST " + prefix + "proposals",
		"ProposalStatus":    "GET " + prefix + "proposals/7",
		"ProposalComments":  "GET " + prefix + "proposals/7/comments",
		"ReviewProposal":    "POST " + prefix + "proposals/7/reviews",
		"AutoMergeProposal": "POST " + prefix + "proposals/7/automerge",
		"GetTask":           "GET " + prefix + "tasks/42",
		"TransitionTask":    "POST " + prefix + "tasks/42/transition",
		"AssignTask":        "POST " + prefix + "tasks/42/assign",
	}
	for _, op := range servedOperations {
		t.Run(op.contract+"."+op.operation, func(t *testing.T) {
			standIn := newSourceStandIn(t)
			h, bearer := sourceHarness(t, standIn)
			_, response := h.call(t, op.contract, op.operation, op.arguments, "")
			if response.Outcome != collab.OutcomeOK {
				t.Fatalf("outcome = %s: %+v", response.Outcome, response.Error)
			}
			requests := standIn.received()
			if len(requests) != 1 || requests[0].method+" "+requests[0].path != routes[op.method] {
				t.Fatalf("Source received %+v, want one %s", requests, routes[op.method])
			}
			if requests[0].authorization != "Bearer "+bearer {
				t.Fatal("the request does not carry the session's workspace bearer")
			}
			if requests[0].method == http.MethodPost {
				var arguments map[string]any
				_ = json.Unmarshal([]byte(op.arguments), &arguments)
				for name, value := range arguments {
					if got, _ := json.Marshal(requests[0].body[name]); string(got) != mustJSON(t, value) {
						t.Errorf("body member %s = %s, want %s", name, got, mustJSON(t, value))
					}
				}
			}
		})
	}

	standIn := newSourceStandIn(t)
	h, _ := sourceHarness(t, standIn)
	_, response := h.call(t, collab.ContractProposals, collab.OperationFind,
		`{"change":{"repository":"acme/app","base":"main","head":"topic"},"states":["draft","open"],"page":{"size":10}}`, "")
	requests := standIn.received()
	if response.Outcome != collab.OutcomeOK || len(response.Result) == 0 || len(requests) != 1 ||
		requests[0].query != "base=main&head=topic&repository=acme%2Fapp&size=10&state=draft%2Copen" {
		t.Fatalf("find sent %+v and answered %s %+v", requests, response.Outcome, response.Error)
	}
}

// TestTheSourceConnectionAnswersTheChangePage carries the pageUrl Source
// answers on a proposal through the generated client into the result of
// proposals.find, upsert, status and automerge, the four operations whose
// declaration lists the pageUrl member.
func TestTheSourceConnectionAnswersTheChangePage(t *testing.T) {
	spectest.Proves(t, feature, "answers-the-change-page", "collab-provider-change-page")
	operations, _ := readOffer(t).Operations(collab.ContractProposals, 1)
	pageOperations := []string{collab.OperationFind, collab.OperationUpsert, collab.OperationStatus, collab.OperationAutoMerge}
	for name, op := range operations {
		want := slices.Contains(pageOperations, name)
		if got := slices.Contains(op.Members(), collab.ProposalMemberPageURL); got != want {
			t.Errorf("proposals.%s declares pageUrl = %v, want %v", name, got, want)
		}
	}
	for _, op := range servedOperations {
		if op.contract != collab.ContractProposals || !slices.Contains(pageOperations, op.operation) {
			continue
		}
		t.Run(op.operation, func(t *testing.T) {
			standIn := newSourceStandIn(t)
			h, _ := sourceHarness(t, standIn)
			_, response := h.call(t, op.contract, op.operation, op.arguments, "")
			if response.Outcome != collab.OutcomeOK {
				t.Fatalf("outcome = %s: %+v", response.Outcome, response.Error)
			}
			var result struct {
				Items    []collab.Proposal `json:"items"`
				Proposal *collab.Proposal  `json:"proposal"`
			}
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			proposal := result.Proposal
			if op.operation == collab.OperationFind && len(result.Items) == 1 {
				proposal = &result.Items[0]
			}
			if proposal == nil {
				t.Fatalf("no proposal in %s", response.Result)
			}
			if problem := providertest.PageURLProblem(*proposal, operations[op.operation].Members()); problem != "" || proposal.PageURL != changePage {
				t.Fatalf("pageUrl = %q (%s), want %q", proposal.PageURL, problem, changePage)
			}
		})
	}
}

// TestTheSourceConnectionKeepsARefusal reads Source's refusal, its stable
// code and its message, leaves the reason to the status when Source declared
// no code, and sends a write Source did not confirm only once.
func TestTheSourceConnectionKeepsARefusal(t *testing.T) {
	spectest.Proves(t, feature, "keeps-the-outcome-of-a-refusal", "collab-provider-refusal-outcomes")
	transition := servedOperations[7]
	route := "POST /v1/workspaces/" + workspaceID + "/source/tasks/42/transition"

	standIn := newSourceStandIn(t)
	standIn.refuse(route, http.StatusConflict, "source.task_moved", "the task moved")
	h, _ := sourceHarness(t, standIn)
	_, response := h.call(t, transition.contract, transition.operation, transition.arguments, "")
	if response.Outcome != collab.OutcomeConflict || response.Error == nil || response.Error.Reason != "source.task_moved" ||
		!strings.Contains(response.Error.Message, "the task moved") {
		t.Fatalf("a conflict answered %s %+v", response.Outcome, response.Error)
	}

	standIn = newSourceStandIn(t)
	standIn.refuse(route, http.StatusTooManyRequests, "", "slow down")
	h, _ = sourceHarness(t, standIn)
	_, response = h.call(t, transition.contract, transition.operation, transition.arguments, "")
	if response.Outcome != collab.OutcomeUnavailable || response.Error == nil || response.Error.Reason != reasonRateLimit || !response.Error.Retryable {
		t.Fatalf("an undeclared rate limit answered %s %+v", response.Outcome, response.Error)
	}

	standIn = newSourceStandIn(t)
	standIn.refuse(route, http.StatusBadGateway, "http.bad_gateway", "GitHub did not answer")
	h, _ = sourceHarness(t, standIn)
	_, response = h.call(t, transition.contract, transition.operation, transition.arguments, "")
	if response.Outcome != collab.OutcomeUnresolved || response.Error == nil || response.Error.Reconcile == "" || len(standIn.received()) != 1 {
		t.Fatalf("an unconfirmed write answered %s %+v after %d requests", response.Outcome, response.Error, len(standIn.received()))
	}
}

// TestTheSourceConnectionSendsNoCursor refuses a page cursor on find, which
// Source does not take, as an invalid request, before any request.
func TestTheSourceConnectionSendsNoCursor(t *testing.T) {
	spectest.Proves(t, feature, "calls-source-for-each-served-operation", "collab-provider-routes-each-operation")
	standIn := newSourceStandIn(t)
	h, _ := sourceHarness(t, standIn)
	_, response := h.call(t, collab.ContractProposals, collab.OperationFind,
		`{"change":{"base":"main","head":"topic"},"page":{"cursor":"c-2"}}`, "")
	if response.Outcome != collab.OutcomeInvalid || response.Error == nil || response.Error.Reason != collab.ReasonRequestInvalid ||
		!strings.Contains(response.Error.Message, "cursor") || len(standIn.received()) != 0 {
		t.Fatalf("a cursor answered %s %+v after %d requests", response.Outcome, response.Error, len(standIn.received()))
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
