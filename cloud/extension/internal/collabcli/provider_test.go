package collabcli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	feature     = "cloud/collaboration-provider"
	workspaceID = "11111111-2222-4333-8444-555555555555"
	envToken    = "machine-token-from-the-environment"
)

// jwt is an unsigned token whose claims scope it to workspace; the provider
// reads its claims and never verifies it.
func jwt(t *testing.T, workspace, nonce string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"scope_ref": map[string]any{"workspace_id": workspace}, "nonce": nonce})
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none"}`)) + "." + encode(claims) + ".sig"
}

// linkedWorkspace writes a workspace linked to workspaceID and returns its
// root and an environment with a Putnami home but no session.
func linkedWorkspace(t *testing.T) (string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	link := map[string]any{"workspace_id": workspaceID, "control_plane_url": "https://control.example.test"}
	data, _ := json.Marshal(link)
	if err := os.MkdirAll(filepath.Dir(clicore.LinkPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clicore.LinkPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, map[string]string{"PUTNAMI_HOME": t.TempDir(), "HOME": t.TempDir()}
}

// signIn stores a session whose workspace access is fresh, and returns the
// bearer the provider must present.
func signIn(t *testing.T, env map[string]string) string {
	t.Helper()
	scoped := jwt(t, workspaceID, "session")
	err := clicore.WriteAuth(&clicore.StoredToken{
		AccessToken: jwt(t, "", "base"), RefreshToken: "refresh-secret", TokenType: "Bearer", ExpiresAt: "2999-01-01T00:00:00Z",
		WorkspaceAccess: map[string]clicore.StoredWorkspaceAccess{
			workspaceID: {AccessToken: scoped, TokenType: "Bearer", ExpiresAt: "2999-01-01T00:00:00Z"},
		},
	}, env)
	if err != nil {
		t.Fatal(err)
	}
	return scoped
}

type sourceCall struct {
	method string
	input  any
}

// fakeSource answers each method with the document it was given, or fails
// with failures[method]. It records every call.
type fakeSource struct {
	calls    []sourceCall
	failures map[string]error
}

func (f *fakeSource) record(method string, input any) error {
	f.calls = append(f.calls, sourceCall{method: method, input: input})
	return f.failures[method]
}

func proposal(base, head string) collab.Proposal {
	return collab.Proposal{
		Ref: collab.Ref{Source: "github:acme/app", ID: "7"}, Revision: "r1",
		Change: collab.Change{Repository: "acme/app", Base: base, Head: head, HeadCommit: "55e2a8e42"},
		Title:  "feat: a change", State: collab.ProposalStateOpen,
		Labels: []string{}, Assignees: []string{},
	}
}

func task(state collab.TaskState, assignees []string) collab.Task {
	return collab.Task{Ref: collab.Ref{Source: "github:acme/app", ID: "42"}, Revision: "r2", Title: "a task", State: state, Assignees: assignees}
}

func (f *fakeSource) FindProposals(_ context.Context, in *collab.ProposalFindInput) (*collab.ProposalListResult, error) {
	if err := f.record("FindProposals", in); err != nil {
		return nil, err
	}
	return &collab.ProposalListResult{Items: []collab.Proposal{proposal(in.Change.Base, in.Change.Head)}}, nil
}

func (f *fakeSource) UpsertProposal(_ context.Context, in *collab.ProposalUpsertInput) (*collab.ProposalUpsertResult, error) {
	if err := f.record("UpsertProposal", in); err != nil {
		return nil, err
	}
	p := proposal(in.Change.Base, in.Change.Head)
	p.Title, p.Body = in.Title, in.Body
	if in.Draft != nil && *in.Draft {
		p.State = collab.ProposalStateDraft
	}
	return &collab.ProposalUpsertResult{Proposal: p, Created: true}, nil
}

func (f *fakeSource) ProposalStatus(_ context.Context, in *collab.ProposalRefInput) (*collab.ProposalStatusResult, error) {
	if err := f.record("ProposalStatus", in); err != nil {
		return nil, err
	}
	return &collab.ProposalStatusResult{Proposal: proposal("main", "topic"), Checks: collab.Checks{State: collab.ChecksStatePassing, Commit: "55e2a8e42"}}, nil
}

func (f *fakeSource) ProposalComments(_ context.Context, in *collab.ProposalCommentsInput) (*collab.ProposalCommentListResult, error) {
	if err := f.record("ProposalComments", in); err != nil {
		return nil, err
	}
	return &collab.ProposalCommentListResult{}, nil
}

func (f *fakeSource) ReviewProposal(_ context.Context, in *collab.ProposalReviewInput) (*collab.ProposalReviewResult, error) {
	if err := f.record("ReviewProposal", in); err != nil {
		return nil, err
	}
	return &collab.ProposalReviewResult{Review: collab.Review{Ref: collab.Ref{Source: "github:acme/app", ID: "r-1"}, Verdict: in.Verdict}, Created: true}, nil
}

func (f *fakeSource) AutoMergeProposal(_ context.Context, in *collab.ProposalAutoMergeInput) (*collab.ProposalResult, error) {
	if err := f.record("AutoMergeProposal", in); err != nil {
		return nil, err
	}
	return &collab.ProposalResult{Proposal: proposal("main", "topic")}, nil
}

func (f *fakeSource) GetTask(_ context.Context, in *collab.TaskRefInput) (*collab.TaskResult, error) {
	if err := f.record("GetTask", in); err != nil {
		return nil, err
	}
	return &collab.TaskResult{Task: task(collab.TaskStateOpen, nil)}, nil
}

func (f *fakeSource) TransitionTask(_ context.Context, in *collab.TaskTransitionInput) (*collab.TaskResult, error) {
	if err := f.record("TransitionTask", in); err != nil {
		return nil, err
	}
	return &collab.TaskResult{Task: task(in.State, nil)}, nil
}

func (f *fakeSource) AssignTask(_ context.Context, in *collab.TaskAssignInput) (*collab.TaskResult, error) {
	if err := f.record("AssignTask", in); err != nil {
		return nil, err
	}
	return &collab.TaskResult{Task: task(collab.TaskStateOpen, in.Assignees)}, nil
}

// harness is a provider on a fake Source; it records each workspace the
// provider connected.
type harness struct {
	provider  *Provider
	source    *fakeSource
	connected []*clicore.WorkspaceContext
	root      string
}

func newHarness(t *testing.T, root string, env map[string]string) *harness {
	t.Helper()
	h := &harness{source: &fakeSource{failures: map[string]error{}}, root: root}
	h.provider = &Provider{Env: env, Connect: func(workspace *clicore.WorkspaceContext) (Source, error) {
		h.connected = append(h.connected, workspace)
		return h.source, nil
	}}
	return h
}

// call routes one operation through Serve as a bound call does and returns
// the raw answer and its decoded response.
func (h *harness) call(t *testing.T, contract, operation, arguments, settings string) (string, collab.Response) {
	t.Helper()
	request := collab.ProviderRequest{
		Arguments:     json.RawMessage(arguments),
		WorkspaceRoot: h.root,
		Provider:      &collab.ProviderCall{Contract: contract, Version: 1, Operation: operation},
	}
	if settings != "" {
		request.Provider.Settings = json.RawMessage(settings)
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

type servedOp struct {
	contract, operation, arguments, method string
}

var servedOperations = []servedOp{
	{collab.ContractProposals, collab.OperationFind, `{"change":{"base":"main","head":"topic"},"page":{"size":10}}`, "FindProposals"},
	{collab.ContractProposals, collab.OperationUpsert, `{"change":{"base":"main","head":"topic","headCommit":"55e2a8e42"},"title":"feat: a change","draft":false,"expectedRevision":"r0"}`, "UpsertProposal"},
	{collab.ContractProposals, collab.OperationStatus, `{"ref":{"source":"github:acme/app","id":"7"}}`, "ProposalStatus"},
	{collab.ContractProposals, collab.OperationComments, `{"ref":{"source":"github:acme/app","id":"7"},"page":{"size":10}}`, "ProposalComments"},
	{collab.ContractProposals, collab.OperationReview, `{"ref":{"source":"github:acme/app","id":"7"},"verdict":"comment","body":"verified","idempotencyKey":"verify-55e2a8e42"}`, "ReviewProposal"},
	{collab.ContractProposals, collab.OperationAutoMerge, `{"ref":{"source":"github:acme/app","id":"7"},"expectedHeadCommit":"55e2a8e42","method":"squash"}`, "AutoMergeProposal"},
	{collab.ContractTasks, collab.OperationGet, `{"ref":{"source":"github:acme/app","id":"42"}}`, "GetTask"},
	{collab.ContractTasks, collab.OperationTransition, `{"ref":{"source":"github:acme/app","id":"42"},"state":"done","expectedRevision":"r1"}`, "TransitionTask"},
	{collab.ContractTasks, collab.OperationAssign, `{"ref":{"source":"github:acme/app","id":"42"},"assignees":["@me"],"expectedRevision":"r1"}`, "AssignTask"},
}

var notServedOperations = []struct{ contract, operation, arguments string }{
	{collab.ContractTasks, collab.OperationFind, `{"page":{"size":10}}`},
	{collab.ContractTasks, collab.OperationCreate, `{"title":"a task","idempotencyKey":"k-1"}`},
	{collab.ContractTasks, collab.OperationUpdate, `{"ref":{"source":"github:acme/app","id":"42"},"title":"renamed"}`},
	{collab.ContractTasks, collab.OperationLink, `{"ref":{"source":"github:acme/app","id":"42"},"unlink":true}`},
	{collab.ContractProposals, collab.OperationMerge, `{"ref":{"source":"github:acme/app","id":"7"},"expectedHeadCommit":"55e2a8e42"}`},
}

func readOffer(t *testing.T) collab.ProviderOffer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest does not parse strictly: %v", diags)
	}
	if diags := extension.ValidateManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("the manifest fails extension validation: %v", diags)
	}
	offer, diags := collab.ReadProviderOffer(manifest.Providers)
	if len(diags) != 0 {
		t.Fatalf("the provider declarations draw diagnostics: %v", diags)
	}
	return offer
}

// TestTheManifestOffersExactlyTheHandledOperations holds the manifest to the
// extension protocol and to the collaboration offer, and the offer to the
// runtime's dispatch table: every declared operation has a handler and every
// handler is declared, run by the runtime's provider entry point.
func TestTheManifestOffersExactlyTheHandledOperations(t *testing.T) {
	spectest.Proves(t, feature, "declares-the-source-operations", "collab-provider-manifest-offer")
	offer := readOffer(t)
	handlers := New().Handlers()
	for _, contract := range []string{collab.ContractTasks, collab.ContractProposals} {
		if got := offer.Versions(contract); !slices.Equal(got, []int{1}) {
			t.Fatalf("%s versions = %v, want [1]", contract, got)
		}
		operations, _ := offer.Operations(contract, 1)
		for name, op := range operations {
			if _, ok := handlers[key(contract, name)]; !ok {
				t.Errorf("%s.%s is declared and has no handler", contract, name)
			}
			if !slices.Equal(op.Provider.Args, []string{"provider"}) || op.Provider.Command != "{extensionRuntime}" {
				t.Errorf("%s.%s runs %s %v, want the runtime's provider entry point", contract, name, op.Provider.Command, op.Provider.Args)
			}
			if op.Operation.OpenWorld == nil || !*op.Operation.OpenWorld {
				t.Errorf("%s.%s must say it reaches outside the machine", contract, name)
			}
		}
		for handled := range handlers {
			if _, ok := operations[handled.Operation]; handled.Contract == contract && !ok {
				t.Errorf("%s.%s has a handler and is not declared", contract, handled.Operation)
			}
		}
	}
	if len(offer.Versions(collab.ContractMemory)) != 0 || len(offer.Versions(collab.ContractMissions)) != 0 {
		t.Fatal("the provider declares a contract it does not serve")
	}
	tasks, _ := offer.Operations(collab.ContractTasks, 1)
	proposals, _ := offer.Operations(collab.ContractProposals, 1)
	for name, op := range map[string]collab.ProvidedOperation{"tasks.transition": tasks["transition"], "tasks.assign": tasks["assign"], "proposals.upsert": proposals["upsert"]} {
		if op.Operation.Preconditions != string(collab.PreconditionsChecked) {
			t.Errorf("%s preconditions = %q, want checked", name, op.Operation.Preconditions)
		}
	}
}

// TestEachServedOperationCallsSourceOnce routes every served operation to its
// Source method, once, with the caller's document, for the linked workspace,
// and answers Source's document.
func TestEachServedOperationCallsSourceOnce(t *testing.T) {
	spectest.Proves(t, feature, "calls-source-for-each-served-operation", "collab-provider-routes-each-operation")
	root, env := linkedWorkspace(t)
	bearer := signIn(t, env)
	for _, op := range servedOperations {
		t.Run(op.contract+"."+op.operation, func(t *testing.T) {
			h := newHarness(t, root, env)
			_, response := h.call(t, op.contract, op.operation, op.arguments, "")
			if response.Outcome != collab.OutcomeOK {
				t.Fatalf("outcome = %s: %+v", response.Outcome, response.Error)
			}
			if len(h.source.calls) != 1 || h.source.calls[0].method != op.method {
				t.Fatalf("Source calls = %+v, want one %s", h.source.calls, op.method)
			}
			if len(h.connected) != 1 || h.connected[0].WorkspaceID != workspaceID || h.connected[0].ControlPlane != "https://control.example.test" {
				t.Fatalf("connected %+v, want the linked workspace", h.connected)
			}
			if got := bearerValue(h.connected[0].AuthToken); got != bearer {
				t.Fatalf("bearer is not the session's workspace bearer")
			}
		})
	}
	h := newHarness(t, root, env)
	_, response := h.call(t, collab.ContractProposals, collab.OperationReview, servedOperations[4].arguments, "")
	review, _ := h.source.calls[0].input.(*collab.ProposalReviewInput)
	if response.Outcome != collab.OutcomeOK || review == nil || review.IdempotencyKey != "verify-55e2a8e42" || review.Verdict != collab.ReviewVerdictComment {
		t.Fatalf("review reached Source as %+v, want the caller's key and verdict", h.source.calls[0].input)
	}
}

// TestRefusalsKeepTheirOutcome maps each refusal Source answers to the
// outcome that says nothing was written.
func TestRefusalsKeepTheirOutcome(t *testing.T) {
	spectest.Proves(t, feature, "calls-source-for-each-served-operation", "collab-provider-routes-each-operation")
	root, env := linkedWorkspace(t)
	signIn(t, env)
	cases := []struct {
		refusal *Refusal
		outcome collab.Outcome
		reason  string
		current string
	}{
		{&Refusal{Status: 404}, collab.OutcomeNotFound, reasonNotFound, ""},
		{&Refusal{Status: 409, Current: "r9"}, collab.OutcomeConflict, collab.ReasonRevisionConflict, "r9"},
		{&Refusal{Status: 409, Reason: "head.moved", Message: "the head moved"}, collab.OutcomeConflict, "head.moved", ""},
		{&Refusal{Status: 403}, collab.OutcomeDenied, reasonRefused, ""},
		{&Refusal{Status: 422}, collab.OutcomeInvalid, reasonInvalid, ""},
		{&Refusal{Status: 429}, collab.OutcomeUnavailable, reasonRateLimit, ""},
	}
	for _, c := range cases {
		h := newHarness(t, root, env)
		h.source.failures["TransitionTask"] = c.refusal
		_, response := h.call(t, collab.ContractTasks, collab.OperationTransition, servedOperations[7].arguments, "")
		if response.Outcome != c.outcome || response.Error == nil || response.Error.Reason != c.reason || response.Error.Current != c.current {
			t.Errorf("refusal %+v answered %s %+v, want %s %s current %q", c.refusal, response.Outcome, response.Error, c.outcome, c.reason, c.current)
		}
	}
	h := newHarness(t, root, env)
	_, response := h.call(t, collab.ContractTasks, collab.OperationGet, servedOperations[6].arguments, `{"workspace":"other"}`)
	if response.Outcome != collab.OutcomeInvalid || response.Error.Reason != reasonSettings || len(h.connected) != 0 {
		t.Fatalf("a binding with settings answered %s %+v", response.Outcome, response.Error)
	}
	unlinked := newHarness(t, t.TempDir(), env)
	_, response = unlinked.call(t, collab.ContractTasks, collab.OperationGet, servedOperations[6].arguments, "")
	if response.Outcome != collab.OutcomeUnavailable || response.Error.Reason != reasonUnlinked || !strings.Contains(response.Error.Message, "putnami cloud setup") {
		t.Fatalf("an unlinked workspace answered %s %+v", response.Outcome, response.Error)
	}
}

// TestOperationsSourceDoesNotServeSendNothing answers unsupported before any
// credential or workspace is read.
func TestOperationsSourceDoesNotServeSendNothing(t *testing.T) {
	spectest.Proves(t, feature, "answers-unsupported-without-a-call", "collab-provider-unsupported-sends-nothing")
	root, env := linkedWorkspace(t)
	signIn(t, env)
	for _, op := range notServedOperations {
		h := newHarness(t, root, env)
		_, response := h.call(t, op.contract, op.operation, op.arguments, "")
		if response.Outcome != collab.OutcomeUnsupported || response.Error == nil || response.Error.Reason != collab.ReasonOperationUnsupported {
			t.Errorf("%s.%s answered %s %+v, want unsupported", op.contract, op.operation, response.Outcome, response.Error)
		}
		if len(h.connected) != 0 || len(h.source.calls) != 0 {
			t.Errorf("%s.%s reached Source", op.contract, op.operation)
		}
	}
}

// TestTheSignedInSessionWins presents the stored session's bearer even when
// PUTNAMI_CLOUD_TOKEN is set, the token only without a session, and nothing
// without either.
func TestTheSignedInSessionWins(t *testing.T) {
	spectest.Proves(t, feature, "signed-in-session-wins", "collab-provider-credential-precedence")
	get := servedOperations[6]

	root, env := linkedWorkspace(t)
	bearer := signIn(t, env)
	env[cloudTokenVar] = envToken
	h := newHarness(t, root, env)
	if _, response := h.call(t, get.contract, get.operation, get.arguments, ""); response.Outcome != collab.OutcomeOK {
		t.Fatalf("session and token answered %s %+v", response.Outcome, response.Error)
	}
	if bearerValue(h.connected[0].AuthToken) != bearer {
		t.Fatal("PUTNAMI_CLOUD_TOKEN won over a valid session")
	}

	root, env = linkedWorkspace(t)
	env[cloudTokenVar] = envToken
	h = newHarness(t, root, env)
	if _, response := h.call(t, get.contract, get.operation, get.arguments, ""); response.Outcome != collab.OutcomeOK {
		t.Fatalf("token alone answered %s %+v", response.Outcome, response.Error)
	}
	if bearerValue(h.connected[0].AuthToken) != envToken {
		t.Fatal("without a session, the token was not presented")
	}

	root, env = linkedWorkspace(t)
	h = newHarness(t, root, env)
	_, response := h.call(t, get.contract, get.operation, get.arguments, "")
	if response.Outcome != collab.OutcomeDenied || response.Error.Reason != reasonSignIn || !strings.Contains(response.Error.Message, "putnami cloud login") {
		t.Fatalf("no credential answered %s %+v, want denied naming putnami cloud login", response.Outcome, response.Error)
	}
	if len(h.connected) != 0 {
		t.Fatal("Source was connected without a credential")
	}
}

// TestAnUncertainWriteIsNeverSentAgain answers unresolved, with the read that
// settles it, for a write Source did not confirm, and sends it once.
func TestAnUncertainWriteIsNeverSentAgain(t *testing.T) {
	spectest.Proves(t, feature, "never-repeats-an-uncertain-write", "collab-provider-uncertain-write-unresolved")
	root, env := linkedWorkspace(t)
	signIn(t, env)
	writes := map[string]string{
		"UpsertProposal":    "proposals.find",
		"ReviewProposal":    "proposals.review",
		"AutoMergeProposal": "proposals.status",
		"TransitionTask":    "tasks.get",
		"AssignTask":        "tasks.get",
	}
	unknown := []error{errors.New("connection reset after the request left"), context.DeadlineExceeded, &Refusal{Status: 502}}
	for _, op := range servedOperations {
		reconcile, write := writes[op.method]
		for _, cause := range unknown {
			h := newHarness(t, root, env)
			h.source.failures[op.method] = cause
			_, response := h.call(t, op.contract, op.operation, op.arguments, "")
			if len(h.source.calls) != 1 {
				t.Fatalf("%s was sent %d times after %v", op.method, len(h.source.calls), cause)
			}
			switch {
			case write:
				if response.Outcome != collab.OutcomeUnresolved || response.Error.Retryable || !strings.Contains(response.Error.Reconcile, reconcile) {
					t.Errorf("%s after %v answered %s %+v, want unresolved naming %s", op.method, cause, response.Outcome, response.Error, reconcile)
				}
			default:
				if response.Outcome != collab.OutcomeUnavailable || !response.Error.Retryable {
					t.Errorf("read %s after %v answered %s %+v, want unavailable and retryable", op.method, cause, response.Outcome, response.Error)
				}
			}
		}
		h := newHarness(t, root, env)
		h.source.failures[op.method] = NotSent(errors.New("configure the Cloud client"))
		if _, response := h.call(t, op.contract, op.operation, op.arguments, ""); response.Outcome != collab.OutcomeUnavailable {
			t.Errorf("%s that never left answered %s, want unavailable", op.method, response.Outcome)
		}
	}
}

// TestTheBearerNeverLeavesTheProcess replaces the presented bearer and the
// environment token wherever an answer would carry them.
func TestTheBearerNeverLeavesTheProcess(t *testing.T) {
	spectest.Proves(t, feature, "keeps-the-bearer-out-of-the-answer", "collab-provider-redacts-the-bearer")
	root, env := linkedWorkspace(t)
	bearer := signIn(t, env)
	env[cloudTokenVar] = envToken
	h := newHarness(t, root, env)
	h.source.failures["UpsertProposal"] = errors.New("echoed Authorization: Bearer " + bearer + " and " + envToken)
	raw, response := h.call(t, collab.ContractProposals, collab.OperationUpsert, servedOperations[1].arguments, "")
	if response.Outcome != collab.OutcomeUnresolved {
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
