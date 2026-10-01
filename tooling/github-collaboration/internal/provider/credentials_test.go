package provider

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/github-collaboration/internal/github"
)

func TestACredentialNeverReachesAnAnswer(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-credential-never-reaches-an-answer")
	h := newHarness(t)
	// The credential comes from gh, not from the environment the orchestrator
	// redacts, so only the provider can keep it out of its answers.
	delete(h.env, "GH_TOKEN")
	h.provider.TokenCommand = func(_ context.Context, host string) (string, error) {
		if host != "github.com" {
			t.Errorf("gh was asked for host %q", host)
		}
		return fakeToken, nil
	}
	echo := `{"message":"Not Found: token ` + fakeToken + ` was used","errors":["` + fakeToken + `"]}`
	h.fake.fail("GET", `/issues/[0-9]+$`, fault{before: true, status: 404, body: echo})
	text := h.serve("tasks", "get", `{"ref":{"source":"github:acme/app","id":"1"}}`)
	if strings.Contains(text, fakeToken) || !strings.Contains(text, github.RedactedMarker) {
		t.Fatalf("a GitHub error echoing the credential reached the answer: %s", text)
	}

	other := "ghp_" + strings.Repeat("Z", 36)
	seeded := h.seedIssue("Leaked "+fakeToken, "Someone pasted "+other)
	text = h.serve("tasks", "get", `{"ref":{"source":"github:acme/app","id":"`+strconv.Itoa(seeded.number)+`"}}`)
	if strings.Contains(text, fakeToken) || strings.Contains(text, other) {
		t.Fatalf("a credential inside an issue reached the answer: %s", text)
	}
	var task collab.TaskResult
	h.ok("tasks", "get", `{"ref":{"source":"github:acme/app","id":"`+strconv.Itoa(seeded.number)+`"}}`, &task)
	if task.Task.Title != "Leaked "+github.RedactedMarker {
		t.Fatalf("title %q", task.Task.Title)
	}

	h.fake.fail("POST", `/issues$`, fault{before: true, status: 422, body: echo})
	failure := h.fails("tasks", "create", `{"title":"t","idempotencyKey":"k"}`, collab.OutcomeInvalid)
	if strings.Contains(failure.Message, fakeToken) {
		t.Fatalf("a refused write echoed the credential: %s", failure.Message)
	}
}

func TestCredentialsFollowGHPrecedence(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "credentials-follow-gh-precedence")
	h := newHarness(t)
	asked := 0
	h.provider.TokenCommand = func(context.Context, string) (string, error) {
		asked++
		return "", errors.New("gh auth token --hostname github.com failed; run gh auth login --hostname github.com")
	}
	fresh := func() {
		h.provider = &Provider{Getenv: h.provider.Getenv, TokenCommand: h.provider.TokenCommand, Client: h.provider.Client, ReconcilePause: noPause}
	}

	h.env["GITHUB_TOKEN"] = "the-wrong-one"
	fresh()
	var page collab.TaskListResult
	h.ok("tasks", "find", `{}`, &page)

	delete(h.env, "GH_TOKEN")
	h.env["GITHUB_TOKEN"] = fakeToken
	fresh()
	h.ok("tasks", "find", `{}`, &page)
	if asked != 0 {
		t.Fatal("gh was asked while a variable named a credential")
	}

	delete(h.env, "GITHUB_TOKEN")
	fresh()
	calls := h.fake.count("GET", `.`)
	failure := h.fails("tasks", "find", `{}`, collab.OutcomeDenied)
	if failure.Reason != reasonUnauthenticated || asked != 1 || h.fake.count("GET", `.`) != calls {
		t.Fatalf("no credential anywhere: %+v, gh asked %d times", failure, asked)
	}
	mustContain(t, failure.Message, "gh auth login")

	h.provider.TokenCommand = nil
	fresh()
	h.fails("tasks", "find", `{}`, collab.OutcomeDenied)

	h.env["GH_TOKEN"] = fakeToken + "\r\nX-Injected: yes"
	fresh()
	if failure := h.fails("tasks", "find", `{}`, collab.OutcomeDenied); strings.Contains(failure.Message, fakeToken) {
		t.Fatalf("a malformed credential: %+v", failure)
	}

	// An enterprise host takes GH_ENTERPRISE_TOKEN only when GH_HOST names it.
	h.env["GH_TOKEN"] = fakeToken
	h.env["GH_ENTERPRISE_TOKEN"] = "enterprise-token-value"
	h.tasks = `{"repository":"acme/app","host":"ghe.example.com"}`
	var hosts []string
	h.provider.TokenCommand = func(_ context.Context, host string) (string, error) {
		hosts = append(hosts, host)
		return fakeToken, nil
	}
	fresh()
	var got collab.TaskListResult
	h.ok("tasks", "find", `{}`, &got)
	if len(hosts) != 1 || hosts[0] != "ghe.example.com" {
		t.Fatalf("the enterprise credential without GH_HOST: gh asked for %v", hosts)
	}
	h.env["GH_HOST"] = "ghe.example.com"
	h.env["GH_ENTERPRISE_TOKEN"] = fakeToken
	fresh()
	h.ok("tasks", "find", `{}`, &got)
	if len(hosts) != 1 {
		t.Fatalf("GH_HOST names the host, and gh was asked again: %v", hosts)
	}
	var task collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"On the enterprise host","idempotencyKey":"ghe"}`, &task)
	if task.Task.Ref.Source != "github:ghe.example.com/acme/app" {
		t.Fatalf("an enterprise reference names its host: %+v", task.Task.Ref)
	}
}

func TestTheOverrideReachesOnlyTheLoopbackInterface(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "the-override-reaches-only-the-loopback-interface")
	h := newHarness(t)
	for _, override := range []string{"https://api.evil.example", "http://user:secret@127.0.0.1:1", "ftp://127.0.0.1", "http://127.0.0.1:1/?q=1"} {
		h.env[github.OverrideVariable] = override
		h.provider = &Provider{Getenv: h.provider.Getenv, Client: h.provider.Client}
		failure := h.fails("tasks", "find", `{}`, collab.OutcomeInvalid)
		if failure.Reason != reasonSettings || strings.Contains(failure.Message, "secret") {
			t.Fatalf("override %s: %+v", override, failure)
		}
	}
	if h.fake.count("GET", `.`) != 0 {
		t.Fatal("a refused override still sent a request")
	}
	h.env[github.OverrideVariable] = strings.Replace(h.fake.server.URL, "127.0.0.1", "localhost", 1)
	h.provider = &Provider{Getenv: h.provider.Getenv, Client: h.provider.Client}
	var page collab.TaskListResult
	h.ok("tasks", "find", `{}`, &page)
}

func TestSettingsAreStrict(t *testing.T) {
	h := newHarness(t)
	for _, settings := range []string{
		`{}`,
		`{"repository":"acme"}`,
		`{"repository":"acme/app/extra"}`,
		`{"repository":"acme/.."}`,
		`{"repository":"acme/app","token":"x"}`,
		`{"repository":"acme/app","host":"https://github.com"}`,
		`{"repository":"acme/app","states":{"open":["a"],"blocked":["A"]}}`,
		`{"repository":"acme/app","states":{"open":[" padded"]}}`,
		`{"repository":"acme/app","states":{"review":["x"]}}`,
		`{"repository":"acme/app","stateLabelPrefix":" status/"}`,
		`{"repository":"acme/app","keyScanPages":11}`,
		`{"repository":"acme/app","allowMerge":true}`,
		`[]`,
	} {
		h.tasks = settings
		if failure := h.fails("tasks", "find", `{}`, collab.OutcomeInvalid); failure.Reason != reasonSettings {
			t.Errorf("tasks settings %s: %+v", settings, failure)
		}
	}
	for _, settings := range []string{
		`{"repository":"acme/app","states":{}}`,
		`{"repository":"acme/app","inheritLabelPrefixes":[" "]}`,
	} {
		h.proposals = settings
		if failure := h.fails("proposals", "find", `{"change":{"base":"main","head":"x"}}`, collab.OutcomeInvalid); failure.Reason != reasonSettings {
			t.Errorf("proposals settings %s: %+v", settings, failure)
		}
	}
	if h.fake.count("GET", `.`) != 0 {
		t.Fatal("invalid settings still sent a request")
	}
}
