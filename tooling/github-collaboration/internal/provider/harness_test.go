package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/github-collaboration/internal/github"
)

// referenceStates is Putnami's own status label mapping: the reference
// configuration of a tasks binding, which every state test runs against.
const referenceStates = `"states":{"open":["status/confirmed","status/audit-finding"],"in_progress":["status/in-progress"],` +
	`"blocked":["status/needs-review","status/needs-design"]},"stateLabelPrefix":"status/"`

// harness drives the provider against a fake GitHub through collab.Serve and
// the same redaction the runtime applies, with the credential in GH_TOKEN.
type harness struct {
	t         *testing.T
	fake      *fakeGitHub
	provider  *Provider
	tasks     string
	proposals string
	env       map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeGitHub(t)
	h := &harness{
		t: t, fake: fake,
		tasks:     `{"repository":"acme/app",` + referenceStates + `}`,
		proposals: `{"repository":"acme/app"}`,
		env:       map[string]string{github.OverrideVariable: fake.server.URL, "GH_TOKEN": fake.token},
	}
	h.provider = &Provider{
		Getenv: func(name string) string { return h.env[name] },
		// No connection is reused, so the transport never repeats a read on
		// its own and every fault meets exactly the request it counts.
		Client:            github.Options{HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, Timeout: 5 * time.Second, Pause: noPause},
		ReconcileAttempts: 2,
		ReconcilePause:    noPause,
		HeadLagPause:      noPause,
	}
	return h
}

func noPause(context.Context, int) error { return nil }

func (h *harness) settings(contract string) string {
	if contract == collab.ContractTasks {
		return h.tasks
	}
	return h.proposals
}

// serve answers one routed request the way the runtime does, and returns the
// tool result's text exactly as the runtime writes it.
func (h *harness) serve(contract, operation, arguments string) string {
	h.t.Helper()
	request := extension.ToolCallRequest{
		Name:          "github-collaboration." + contract + "." + operation,
		Arguments:     json.RawMessage(arguments),
		WorkspaceRoot: h.t.TempDir(),
		Provider: &extension.ToolProviderCall{Contract: contract, Version: 1, Operation: operation,
			Settings: json.RawMessage(h.settings(contract))},
	}
	payload, err := json.Marshal(request)
	if err != nil {
		h.t.Fatal(err)
	}
	var out bytes.Buffer
	if err := collab.Serve(context.Background(), bytes.NewReader(payload), &out, h.provider.Handlers()); err != nil {
		h.t.Fatal(err)
	}
	return string(h.provider.Redact(out.Bytes()))
}

func (h *harness) call(contract, operation, arguments string) collab.Response {
	h.t.Helper()
	text := h.serve(contract, operation, arguments)
	var result extension.ToolCallResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		h.t.Fatal(err)
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		h.t.Fatalf("invalid response %s: %v", result.Content[0].Text, diags)
	}
	return *response
}

// ok sends a request that must succeed and decodes its result into into,
// which it zeroes first: a member the answer omits must not keep an earlier
// answer's value.
func (h *harness) ok(contract, operation, arguments string, into any) {
	h.t.Helper()
	response := h.call(contract, operation, arguments)
	if response.Outcome != collab.OutcomeOK {
		h.t.Fatalf("%s.%s %s: %s %+v", contract, operation, arguments, response.Outcome, response.Error)
	}
	reflect.ValueOf(into).Elem().SetZero()
	if err := json.Unmarshal(response.Result, into); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) fails(contract, operation, arguments string, outcome collab.Outcome) collab.Error {
	h.t.Helper()
	response := h.call(contract, operation, arguments)
	if response.Outcome != outcome {
		h.t.Fatalf("%s.%s %s: outcome %s, want %s (%+v)", contract, operation, arguments, response.Outcome, outcome, response.Error)
	}
	return *response.Error
}

func refJSON(ref collab.Ref) string {
	encoded, _ := json.Marshal(ref)
	return string(encoded)
}

// createTask opens a task through the provider.
func (h *harness) createTask(title, key string) collab.Task {
	h.t.Helper()
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"`+title+`","idempotencyKey":"`+key+`"}`, &created)
	return created.Task
}

// openProposal opens a proposal on topic-a against main.
func (h *harness) openProposal(title string) collab.Proposal {
	h.t.Helper()
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic-a"},"title":"`+title+`","body":"b"}`, &created)
	return created.Proposal
}

// seedIssue adds an issue directly to the fake, as another client would.
func (h *harness) seedIssue(title, body string, labels ...string) *fakeIssue {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return h.fake.newIssue(title, body, labels)
}

func (h *harness) issue(number int) fakeIssue {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return *h.fake.issues[number]
}

func (h *harness) issueCount() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return len(h.fake.issues)
}

func number(t *testing.T, ref collab.Ref) int {
	t.Helper()
	var n int
	if err := json.Unmarshal([]byte(ref.ID), &n); err != nil {
		t.Fatalf("ref %+v", ref)
	}
	return n
}

func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%q does not contain %q", text, want)
	}
}

// statusFault is a fault answering status with GitHub's error document.
func statusFault(status int, message string, before bool) fault {
	body, _ := json.Marshal(map[string]any{"message": message})
	return fault{status: status, body: string(body), before: before}
}
