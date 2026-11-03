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
	"go.putnami.dev/tooling/linear-collaboration/internal/linear"
)

const feature = "tooling/linear-collaboration"

// harness drives the provider against a fake Linear through collab.Serve and
// the same redaction the runtime applies, with the key in LINEAR_API_KEY.
type harness struct {
	t        *testing.T
	fake     *fakeLinear
	provider *Provider
	settings string
	env      map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeLinear(t)
	h := &harness{
		t: t, fake: fake, settings: `{"team":"ENG"}`,
		env: map[string]string{linear.OverrideVariable: fake.server.URL, linear.KeyVariable: fake.key},
	}
	h.provider = h.newProvider()
	return h
}

// newProvider is a provider on the harness's environment. No connection is
// reused, so the transport never repeats a request on its own and every fault
// meets exactly the request it counts.
func (h *harness) newProvider() *Provider {
	return &Provider{
		Getenv:            func(name string) string { return h.env[name] },
		Client:            linear.Options{HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, Timeout: 5 * time.Second, Pause: noPause},
		ReconcileAttempts: 2,
		ReconcilePause:    noPause,
	}
}

func noPause(context.Context, int) error { return nil }

// serve answers one routed request the way the runtime does, and returns the
// response exactly as the runtime writes it.
func (h *harness) serve(operation, arguments string) string {
	h.t.Helper()
	request := collab.ProviderRequest{
		Arguments:     json.RawMessage(arguments),
		WorkspaceRoot: h.t.TempDir(),
		Provider: &collab.ProviderCall{Contract: collab.ContractTasks, Version: 1, Operation: operation,
			Settings: json.RawMessage(h.settings)},
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

func (h *harness) call(operation, arguments string) collab.Response {
	h.t.Helper()
	text := h.serve(operation, arguments)
	response, diags := collab.ParseResponse([]byte(text))
	if diags != nil {
		h.t.Fatalf("invalid response %s: %v", text, diags)
	}
	return *response
}

// ok sends a request that must succeed and decodes its result into into,
// which it zeroes first.
func (h *harness) ok(operation, arguments string, into any) {
	h.t.Helper()
	response := h.call(operation, arguments)
	if response.Outcome != collab.OutcomeOK {
		h.t.Fatalf("tasks.%s %s: %s %+v", operation, arguments, response.Outcome, response.Error)
	}
	reflect.ValueOf(into).Elem().SetZero()
	if err := json.Unmarshal(response.Result, into); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) fails(operation, arguments string, outcome collab.Outcome) collab.Error {
	h.t.Helper()
	response := h.call(operation, arguments)
	if response.Outcome != outcome {
		h.t.Fatalf("tasks.%s %s: outcome %s, want %s (%+v)", operation, arguments, response.Outcome, outcome, response.Error)
	}
	return *response.Error
}

func refJSON(ref collab.Ref) string {
	encoded, _ := json.Marshal(ref)
	return string(encoded)
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// createTask opens a task through the provider.
func (h *harness) createTask(arguments string) collab.Task {
	h.t.Helper()
	var created collab.TaskCreateResult
	h.ok("create", arguments, &created)
	if !created.Created {
		h.t.Fatalf("a new key answered created: false: %+v", created)
	}
	return created.Task
}

// stored returns a copy of the fake's issue of a task.
func (h *harness) stored(ref collab.Ref) fakeIssue {
	h.t.Helper()
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	issue := h.fake.issue(ref.ID)
	if issue == nil {
		h.t.Fatalf("the fake holds no issue %s", ref.ID)
	}
	return *issue
}

func (h *harness) issueCount() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return len(h.fake.issues)
}

// seedIssue adds an issue directly to the fake, as another client would.
// seedIssue stores an issue in the team its state belongs to.
func (h *harness) seedIssue(id, title, state string, labels ...string) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	team := teamID
	if strings.HasPrefix(state, "st-"+otherTeam[:4]+"-") {
		team = otherTeam
	}
	h.fake.newIssue(id, team, title, "", state, labels)
}

func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%q does not contain %q", text, want)
	}
}

// statusFault answers a request with a GraphQL error document.
func statusFault(status int, message, code string, before bool) fault {
	return fault{status: status, body: string(graphQLErrors(message, code)), before: before}
}
