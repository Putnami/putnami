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
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

const feature = "tooling/jira-collaboration"

// harness drives the provider against a fake Jira through collab.Serve and
// the same redaction the runtime applies, with the credential in
// JIRA_EMAIL and JIRA_API_TOKEN.
type harness struct {
	t        *testing.T
	fake     *fakeJira
	provider *Provider
	settings string
	env      map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeJira(t)
	h := &harness{
		t: t, fake: fake,
		settings: `{"site":"acme.atlassian.net","project":"ENG"}`,
		env:      map[string]string{jira.OverrideVariable: fake.server.URL, jira.EmailVariable: fake.email, jira.TokenVariable: fake.token},
	}
	h.provider = &Provider{
		Getenv: func(name string) string { return h.env[name] },
		// No connection is reused, so the transport never repeats a read on
		// its own and every fault meets exactly the request it counts.
		Client:            jira.Options{HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, Timeout: 5 * time.Second, Pause: noPause},
		ReconcileAttempts: 2,
		ReconcilePause:    noPause,
	}
	return h
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

func (h *harness) create(arguments string) collab.TaskCreateResult {
	h.t.Helper()
	var created collab.TaskCreateResult
	h.ok("create", arguments, &created)
	return created
}

// seed adds an issue directly to the fake, as another client would.
func (h *harness) seed(summary, description string, labels ...string) *fakeIssue {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return h.fake.newIssue(summary, description, labels, "557058:someone-else")
}

// issue returns a copy of a fake issue by id.
func (h *harness) issue(id string) fakeIssue {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return *h.fake.issues[id]
}

func (h *harness) issueCount() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return len(h.fake.issues)
}

func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%q does not contain %q", text, want)
	}
}

// statusFault is a fault answering status with Jira's error document.
func statusFault(status int, message string, before bool) fault {
	body, _ := json.Marshal(jiraError(message))
	return fault{status: status, body: string(body), before: before}
}
