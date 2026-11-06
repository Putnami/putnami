package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

const feature = "tooling/gitlab-collaboration"

// referenceSettings bind the fake's project with a label for each state,
// GitLab scoped labels under one prefix.
const referenceSettings = `{"project":"` + fakeProject + `","host":"` + fakeHost + `",` +
	`"states":{"open":["status::ready","status::triage"],"in_progress":["status::doing"],"blocked":["status::blocked"],` +
	`"done":["status::done"],"canceled":["status::canceled"]},"stateLabelPrefix":"status::"}`

// source is the reference source the reference settings issue.
const source = "gitlab:" + fakeHost + "/" + fakeProject

// harness drives the provider against a fake GitLab through collab.Serve and
// the same redaction the runtime applies, with the credential in
// GITLAB_TOKEN for the host GITLAB_HOST names.
type harness struct {
	t        *testing.T
	fake     *fakeGitLab
	provider *Provider
	settings string
	env      map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeGitLab(t)
	h := &harness{
		t: t, fake: fake, settings: referenceSettings,
		env: map[string]string{gitlab.OverrideVariable: fake.server.URL, gitlab.TokenVariable: fake.token, gitlab.HostVariable: fakeHost},
	}
	h.provider = &Provider{
		Getenv: func(name string) string { return h.env[name] },
		// No connection is reused, so the transport never repeats a read on
		// its own and every fault meets exactly the request it counts.
		Client:            gitlab.Options{HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, Timeout: 5 * time.Second, Pause: noPause},
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

func (h *harness) get(ref collab.Ref) collab.Task {
	h.t.Helper()
	var got collab.TaskResult
	h.ok("get", `{"ref":`+refJSON(ref)+`}`, &got)
	return got.Task
}

func (h *harness) create(arguments string) collab.TaskCreateResult {
	h.t.Helper()
	var created collab.TaskCreateResult
	h.ok("create", arguments, &created)
	return created
}

// seed adds an issue directly to the fake, as another account would.
func (h *harness) seed(title, description string, labels ...string) *fakeIssue {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return h.fake.newIssue(title, description, labels, otherID)
}

// change edits a fake issue under the fake's lock.
func (h *harness) change(issue *fakeIssue, edit func(*fakeIssue)) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	edit(issue)
}

// issue returns a copy of a fake issue by iid.
func (h *harness) issue(iid int) fakeIssue {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return *h.fake.issues[iid]
}

func (h *harness) issueCount() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return len(h.fake.issues)
}

func refJSON(ref collab.Ref) string {
	encoded, _ := json.Marshal(ref)
	return string(encoded)
}

func refOf(iid int) collab.Ref {
	return collab.Ref{Source: source, ID: strconv.Itoa(iid)}
}

func mustContain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%q does not contain %q", text, want)
	}
}

// statusFault is a fault answering status with GitLab's error document.
func statusFault(status int, message string, before bool) fault {
	body, _ := json.Marshal(map[string]any{"message": message})
	return fault{status: status, body: string(body), before: before}
}
