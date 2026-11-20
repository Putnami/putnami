package agencycli

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// buildsRoute is the agency gateway's build start route.
const buildsRoute = "/v1/workspaces/" + workspaceID + "/agency/builds"

// withOrigin makes the checkout's origin remote remote for one test.
func withOrigin(t *testing.T, remote string, err error) {
	t.Helper()
	previous := originRemote
	originRemote = func(string) (string, error) { return remote, err }
	t.Cleanup(func() { originRemote = previous })
}

// TestBuildStartsTheRoutine asks the agency gateway's build route to start the
// caller's routine on the issue of the origin's repository, or of
// --repository, and prints the session link.
func TestBuildStartsTheRoutine(t *testing.T) {
	spectest.Proves(t, feature, "build-starts-the-routine", "agency-build-starts")
	withOrigin(t, "git@github.com:Acme/Shop.git", nil)
	s := newStandIn(t)
	s.answer("POST "+buildsRoute, http.StatusOK, `{"started":true,"sessionUrl":"https://claude.ai/code/session_01"}`)
	run := newRun(t, s)
	if err := Build(params("issue", "4745"), run.root, run.env, run.ioctx); err != nil {
		t.Fatalf("build: %v", err)
	}
	request := s.got()[0]
	if request.path != buildsRoute || request.body["repository"] != "Acme/Shop" || request.body["issue"] != float64(4745) ||
		request.authorization != "Bearer "+run.bearer {
		t.Fatalf("the gateway received %+v", request)
	}
	if !strings.Contains(run.text(), "https://claude.ai/code/session_01") || !strings.Contains(run.text(), "#4745") {
		t.Fatalf("output = %q", run.text())
	}

	s = newStandIn(t)
	s.answer("POST "+buildsRoute, http.StatusOK, `{"started":true}`)
	run = newRun(t, s)
	if err := Build(params("issue", "7", "repository", "acme/widgets"), run.root, run.env, run.ioctx); err != nil ||
		s.got()[0].body["repository"] != "acme/widgets" || !strings.Contains(run.text(), "started on issue #7 of acme/widgets") {
		t.Fatalf("build --repository: %v, %+v, %q", err, s.got(), run.text())
	}
}

// TestBuildSaysWhyNothingStarted prints the reason and its fix with exit 1,
// keeps the result in the failure, and refuses a bad issue or repository
// before anything is sent.
func TestBuildSaysWhyNothingStarted(t *testing.T) {
	spectest.Proves(t, feature, "build-says-why-nothing-started", "agency-build-refusals")
	for reason, want := range map[string]string{
		"no_trigger":  "putnami agency trigger register --url",
		"disabled":    "not enabled",
		"unavailable": "did not accept",
		"":            "no reason",
	} {
		s := newStandIn(t)
		s.answer("POST "+buildsRoute, http.StatusOK, `{"started":false,"reason":"`+reason+`"}`)
		run := newRun(t, s)
		err := Build(params("issue", "12", "repository", "acme/widgets"), run.root, run.env, run.ioctx)
		if clicore.ExitCode(err) != clicore.ExitFailure || !strings.Contains(err.Error(), want) || len(*run.out) != 0 {
			t.Fatalf("%q: err = %v, output = %v", reason, err, *run.out)
		}
		if data, _ := protocolcli.ResultData(err).(map[string]any); data["reason"] != reason || data["started"] != false {
			t.Fatalf("%q: result = %v", reason, protocolcli.ResultData(err))
		}
	}

	for status, want := range map[int]string{
		http.StatusForbidden:          "cannot start builds",
		http.StatusBadRequest:         "owner/name",
		http.StatusServiceUnavailable: "nothing is known to have started",
	} {
		s := newStandIn(t)
		body := envelope(status)
		if status == http.StatusForbidden {
			// The route declares the details of its 403.
			body = `{"code":"forbidden","error":"Forbidden","message":"refused","details":{"reason":"denied"}}`
		}
		s.answer("POST "+buildsRoute, status, body)
		run := newRun(t, s)
		err := Build(params("issue", "12", "repository", "acme/widgets"), run.root, run.env, run.ioctx)
		if clicore.ExitCode(err) != clicore.ExitFailure || !strings.Contains(err.Error(), want) {
			t.Fatalf("HTTP %d: err = %v", status, err)
		}
	}

	withOrigin(t, "/srv/git/widgets", nil)
	s := newStandIn(t)
	run := newRun(t, s)
	for _, input := range []map[string]any{
		{"issue": "0"}, {"issue": "-3"}, {"issue": "12a"}, {},
		{"issue": "12", "repository": "widgets"}, {"issue": "12", "repository": "acme/widgets/extra"},
		{"issue": "12"},
	} {
		if err := Build(input, run.root, run.env, run.ioctx); clicore.ExitCode(err) != clicore.ExitFailure {
			t.Fatalf("%v: err = %v", input, err)
		}
	}
	withOrigin(t, "", errors.New("no origin"))
	if err := Build(params("issue", "12"), run.root, run.env, run.ioctx); clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("no origin: err = %v", err)
	}
	if len(s.got()) != 0 {
		t.Fatalf("the gateway received %+v", s.got())
	}
}
