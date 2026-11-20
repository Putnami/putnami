package agencycli

import (
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	fireURL     = "https://api.anthropic.com/v1/claude_code/routines/trig_01ABC/fire"
	routineSeed = "routine-token-0123456789abcdef"
)

func triggerRoute(skill string) string {
	return "/v1/workspaces/" + workspaceID + "/config/agent-triggers/" + skill
}

const registered = `{"skill":"putnami","routineId":"trig_01ABC","generation":2,"updatedAt":"2026-11-19T09:00:00Z"}`

// TestTriggerRegisterReadsTheTokenFromStandardInput registers the routine
// through config-api's generated client with the token read from standard
// input, sends it once, and never prints it.
func TestTriggerRegisterReadsTheTokenFromStandardInput(t *testing.T) {
	spectest.Proves(t, feature, "trigger-token-is-read-from-stdin", "agency-trigger-register")
	for _, skill := range []string{"", "rebase", "build"} {
		s := newStandIn(t)
		want := skill
		if want == "" {
			want = triggerPutnamiRoute
		}
		s.answer("PUT "+triggerRoute(want), http.StatusOK, strings.Replace(registered, `"putnami"`, `"`+want+`"`, 1))
		run := newRun(t, s)
		flags := params("url", fireURL, "token-stdin", true)
		if skill != "" {
			flags["skill"] = skill
		}
		if err := Trigger(flags, []string{"register"}, run.root, run.env, run.ioctx, stdin(routineSeed+"\n")); err != nil {
			t.Fatalf("register %q: %v", skill, err)
		}
		requests := s.got()
		last := requests[len(requests)-1]
		if last.method != http.MethodPut || last.path != triggerRoute(want) || last.body["token"] != routineSeed || last.body["url"] != fireURL {
			t.Fatalf("Config received %+v", last)
		}
		if last.authorization != "Bearer "+run.bearer {
			t.Fatal("the request does not carry the session's workspace bearer")
		}
		if strings.Contains(run.text(), routineSeed) || !strings.Contains(run.text(), "trig_01ABC") {
			t.Fatalf("output = %q", run.text())
		}
	}

	s := newStandIn(t)
	run := newRun(t, s)
	for _, flag := range []string{"token", "value", "key", "from-file"} {
		err := Trigger(params("url", fireURL, "token-stdin", true, flag, routineSeed), []string{"register"}, run.root, run.env, run.ioctx, noInput{t})
		if clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("--%s: err = %v", flag, err)
		}
	}
	if len(s.got()) != 0 {
		t.Fatal("a refused flag sent a request")
	}
}

// triggerPutnamiRoute is the skill the Putnami routine is kept under.
const triggerPutnamiRoute = triggerPutnamiRoutine

// TestTriggerShowAndRemove prints the redacted trigger, says when none is
// registered, removes it, and prints Config's refusal with exit 1.
func TestTriggerShowAndRemove(t *testing.T) {
	spectest.Proves(t, feature, "trigger-show-and-remove", "agency-trigger-show-remove")
	s := newStandIn(t)
	s.answer("GET "+triggerRoute("putnami"), http.StatusOK, registered)
	run := newRun(t, s)
	if err := Trigger(params(), []string{"show"}, run.root, run.env, run.ioctx, nil); err != nil || !strings.Contains(run.text(), "routine trig_01ABC (generation 2)") {
		t.Fatalf("show: %v, %q", err, run.text())
	}

	s = newStandIn(t)
	s.answer("GET "+triggerRoute("rebase"), http.StatusNotFound, envelope(404))
	run = newRun(t, s)
	if err := Trigger(params("skill", "rebase"), []string{"show"}, run.root, run.env, run.ioctx, nil); err != nil ||
		!strings.Contains(run.text(), "putnami agency trigger register --skill rebase") {
		t.Fatalf("show without a trigger: %v, %q", err, run.text())
	}

	s = newStandIn(t)
	s.answer("DELETE "+triggerRoute("build"), http.StatusNoContent, "")
	run = newRun(t, s)
	if err := Trigger(params("skill", "build"), []string{"remove"}, run.root, run.env, run.ioctx, nil); err != nil || !strings.Contains(run.text(), "build trigger is removed") {
		t.Fatalf("remove: %v, %q", err, run.text())
	}

	s = newStandIn(t)
	s.answer("DELETE "+triggerRoute("putnami"), http.StatusForbidden, envelope(403))
	run = newRun(t, s)
	err := Trigger(params(), []string{"remove"}, run.root, run.env, run.ioctx, nil)
	if clicore.ExitCode(err) != clicore.ExitAuth || !strings.Contains(err.Error(), "member of the workspace") {
		t.Fatalf("a refused remove: %v", err)
	}

	s = newStandIn(t)
	s.answer("GET "+triggerRoute("putnami"), http.StatusConflict, envelope(409))
	run = newRun(t, s)
	if err := Trigger(params(), []string{"show"}, run.root, run.env, run.ioctx, nil); clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("another refusal: %v", err)
	}
}

// TestTriggerTokenIsSentOnce sends the token once even when Config answers
// a status the generated client would retry.
func TestTriggerTokenIsSentOnce(t *testing.T) {
	spectest.Proves(t, feature, "trigger-token-is-read-from-stdin", "agency-trigger-register")
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusBadGateway} {
		s := newStandIn(t)
		s.answer("PUT "+triggerRoute("putnami"), status, envelope(status))
		run := newRun(t, s)
		err := Trigger(params("url", fireURL, "token-stdin", true), []string{"register"}, run.root, run.env, run.ioctx, stdin(routineSeed))
		if err == nil || strings.Contains(err.Error(), routineSeed) {
			t.Fatalf("HTTP %d: err = %v", status, err)
		}
		if got := len(s.got()); got != 1 {
			t.Fatalf("HTTP %d: the token was sent %d times", status, got)
		}
		if status == http.StatusServiceUnavailable && !strings.Contains(err.Error(), "not enabled") {
			t.Fatalf("HTTP 503: err = %v", err)
		}
	}
}

// TestTriggerRefusesBeforeSending refuses a URL that is not a routine fire
// endpoint, an unknown skill and a token of the wrong shape, before anything
// is sent.
func TestTriggerRefusesBeforeSending(t *testing.T) {
	spectest.Proves(t, feature, "trigger-url-is-checked-before-sending", "agency-trigger-refuses-before-sending")
	s := newStandIn(t)
	run := newRun(t, s)
	for _, bad := range []string{"", "http://api.anthropic.com/v1/claude_code/routines/r/fire", "https://evil.example/v1/claude_code/routines/r/fire",
		fireURL + "?x=1", "https://api.anthropic.com/v1/claude_code/routines/r/run"} {
		if err := Trigger(params("url", bad, "token-stdin", true), []string{"register"}, run.root, run.env, run.ioctx, stdin(routineSeed)); clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("url %q: err = %v", bad, err)
		}
	}
	for _, token := range []string{"", "short", "has a space in the routine token", strings.Repeat("x", 1025)} {
		if err := Trigger(params("url", fireURL, "token-stdin", true), []string{"register"}, run.root, run.env, run.ioctx, stdin(token)); clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("token %q: err = %v", token, err)
		}
	}
	if err := Trigger(params("skill", "deploy"), []string{"show"}, run.root, run.env, run.ioctx, nil); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("unknown skill: err = %v", err)
	}
	if err := Trigger(params("url", fireURL), []string{"register"}, run.root, run.env, run.ioctx, stdin(routineSeed)); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("no --token-stdin: err = %v", err)
	}
	if len(s.got()) != 0 {
		t.Fatalf("Config received %+v", s.got())
	}
}
