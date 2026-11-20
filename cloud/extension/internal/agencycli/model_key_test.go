package agencycli

import (
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

const modelKeySeed = "sk-ant-api03-0123456789abcdefghij"

func modelKeyRoute(suffix string) string {
	return "/v1/workspaces/" + workspaceID + "/config/organization-model-credential" + suffix
}

const keyStatus = `{"organizationId":"org_1","provider":"anthropic","state":"active","generation":3,"budget":{"tokens":500000,"modelSeconds":600},"updatedBy":"ada","updatedAt":"2026-11-19T09:00:00Z"}`

// TestModelKeySetReadsTheKeyFromStandardInput stores the key read from
// standard input through config-api's generated client, sends it once, and
// prints the redacted status.
func TestModelKeySetReadsTheKeyFromStandardInput(t *testing.T) {
	spectest.Proves(t, feature, "model-key-is-read-from-stdin", "agency-model-key-set")
	s := newStandIn(t)
	s.answer("PUT "+modelKeyRoute(""), http.StatusOK, keyStatus)
	run := newRun(t, s)
	if err := ModelKey(params("stdin", true, "max-tokens", "500000", "max-model-minutes", "10"), []string{"set"}, run.root, run.env, run.ioctx, stdin(modelKeySeed+"\r\n")); err != nil {
		t.Fatalf("set: %v", err)
	}
	requests := s.got()
	if len(requests) != 1 || requests[0].body["key"] != modelKeySeed || requests[0].body["provider"] != "anthropic" ||
		requests[0].authorization != "Bearer "+run.bearer {
		t.Fatalf("Config received %+v", requests)
	}
	budget, _ := requests[0].body["budget"].(map[string]any)
	if budget["tokens"] != float64(500000) || budget["modelSeconds"] != float64(600) {
		t.Fatalf("budget = %v", budget)
	}
	if strings.Contains(run.text(), modelKeySeed) || !strings.Contains(run.text(), "generation 3") {
		t.Fatalf("output = %q", run.text())
	}

	for _, flag := range []string{"key", "api-key", "value", "token", "from-file"} {
		err := ModelKey(params("stdin", true, flag, modelKeySeed), []string{"set"}, run.root, run.env, run.ioctx, noInput{t})
		if clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("--%s: err = %v", flag, err)
		}
	}
	for _, key := range []string{"", "short", "has a space in the model key value"} {
		if err := ModelKey(params("stdin", true), []string{"set"}, run.root, run.env, run.ioctx, stdin(key)); clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("key %q: err = %v", key, err)
		}
	}
	if len(s.got()) != 1 {
		t.Fatalf("a refused key was sent: %+v", s.got())
	}
}

// TestModelKeyStatusAndRevoke prints the status, says when no key is set,
// revokes at the generation given, and prints Config's refusal with exit 1.
func TestModelKeyStatusAndRevoke(t *testing.T) {
	spectest.Proves(t, feature, "model-key-status-and-revoke", "agency-model-key-status-revoke")
	s := newStandIn(t)
	s.answer("GET "+modelKeyRoute(""), http.StatusOK, keyStatus)
	run := newRun(t, s)
	if err := ModelKey(params(), []string{"status"}, run.root, run.env, run.ioctx, nil); err != nil ||
		!strings.Contains(run.text(), "Budget per run: 500000 tokens and 10 minutes") {
		t.Fatalf("status: %v, %q", err, run.text())
	}

	s = newStandIn(t)
	s.answer("GET "+modelKeyRoute(""), http.StatusNotFound, envelope(404))
	run = newRun(t, s)
	if err := ModelKey(params(), []string{"status"}, run.root, run.env, run.ioctx, nil); err != nil || !strings.Contains(run.text(), "no Anthropic API key") {
		t.Fatalf("status without a key: %v, %q", err, run.text())
	}

	s = newStandIn(t)
	s.answer("POST "+modelKeyRoute("/revoke"), http.StatusOK, strings.Replace(keyStatus, `"active"`, `"revoked"`, 1))
	run = newRun(t, s)
	if err := ModelKey(params("expected-generation", "3", "confirm", true), []string{"revoke"}, run.root, run.env, run.ioctx, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if body := s.got()[0].body; body["expectedGeneration"] != float64(3) || body["provider"] != "anthropic" {
		t.Fatalf("revoke sent %v", body)
	}
	if err := ModelKey(params("expected-generation", "3"), []string{"revoke"}, run.root, run.env, run.ioctx, nil); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("revoke without --confirm: %v", err)
	}

	s = newStandIn(t)
	s.answer("GET "+modelKeyRoute(""), http.StatusForbidden, envelope(403))
	run = newRun(t, s)
	err := ModelKey(params(), []string{"status"}, run.root, run.env, run.ioctx, nil)
	if clicore.ExitCode(err) != clicore.ExitAuth || !strings.Contains(err.Error(), "platform.organization.manage") {
		t.Fatalf("a refused status: %v", err)
	}
}

// TestModelKeyIsSentOnce sends the key once even when Config answers a
// status the generated client would retry.
func TestModelKeyIsSentOnce(t *testing.T) {
	spectest.Proves(t, feature, "model-key-is-read-from-stdin", "agency-model-key-set")
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		s := newStandIn(t)
		s.answer("PUT "+modelKeyRoute(""), status, envelope(status))
		run := newRun(t, s)
		err := ModelKey(params("stdin", true), []string{"set"}, run.root, run.env, run.ioctx, stdin(modelKeySeed))
		if err == nil || strings.Contains(err.Error(), modelKeySeed) {
			t.Fatalf("HTTP %d: err = %v", status, err)
		}
		if got := len(s.got()); got != 1 {
			t.Fatalf("HTTP %d: the key was sent %d times", status, got)
		}
	}
}

// TestModelKeyBudgetBounds refuses a budget outside Config's bounds, or one
// flag without the other, before anything is read or sent.
func TestModelKeyBudgetBounds(t *testing.T) {
	spectest.Proves(t, feature, "model-key-budget-flags", "agency-model-key-budget-bounds")
	s := newStandIn(t)
	run := newRun(t, s)
	for _, budget := range [][2]string{{"999", "10"}, {"50000001", "10"}, {"5000", "0"}, {"5000", "121"}, {"5000", ""}, {"", "10"}, {"many", "10"}} {
		flags := params("stdin", true)
		if budget[0] != "" {
			flags["max-tokens"] = budget[0]
		}
		if budget[1] != "" {
			flags["max-model-minutes"] = budget[1]
		}
		if err := ModelKey(flags, []string{"set"}, run.root, run.env, run.ioctx, noInput{t}); clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("budget %v: err = %v", budget, err)
		}
	}
	if len(s.got()) != 0 {
		t.Fatalf("Config received %+v", s.got())
	}
}
