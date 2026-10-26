package compose

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// launchWarnings are the warnings a test hands the command as putnami hands
// them in the job context.
var launchWarnings = []protocolcli.WorkspaceWarning{
	{Code: "workspace.jobs_ignored", Message: "project /app declares jobs, which putnami ignores"},
}

// assertCarriesLaunchWarnings fails unless document is a result envelope whose
// workspaceWarnings are exactly launchWarnings.
func assertCarriesLaunchWarnings(t *testing.T, document string) {
	t.Helper()
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentResultEnvelope, []byte(document)); len(violations) != 0 {
		t.Fatalf("the envelope violates the contract: %v\n%s", violations, document)
	}
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(document), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, document)
	}
	if !reflect.DeepEqual(envelope.WorkspaceWarnings, launchWarnings) {
		t.Errorf("workspaceWarnings = %+v, want %+v\n%s", envelope.WorkspaceWarnings, launchWarnings, document)
	}
}

// TestRun_EveryEnvelopeCarriesTheLaunchWarnings holds compose to carrying the
// warnings of the load that launched it in each envelope it writes: a usage
// refusal, the start and exit documents and a failure. putnami already wrote
// them on stderr, so compose writes none of them there.
func TestRun_EveryEnvelopeCarriesTheLaunchWarnings(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", "compose-and-qualify-carry-their-launch-warnings",
		"every-envelope-carries-the-launch-warnings")

	t.Run("usage refusal", func(t *testing.T) {
		fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app"})
		var stdout, stderr bytes.Buffer
		code := Run(Request{
			Ctx: t.Context(), Workspace: fixture.ws, Args: []string{"missing"}, Output: protocolcli.OutputJSON,
			Executable: fixture.executable, Warnings: launchWarnings, Stdout: &stdout, Stderr: &stderr,
		})
		if code != protocolcli.ExitUsage {
			t.Fatalf("exit %d, want %d; stderr %q", code, protocolcli.ExitUsage, stderr.String())
		}
		assertCarriesLaunchWarnings(t, stdout.String())
		if strings.Contains(stderr.String(), "putnami: warning: ") {
			t.Errorf("compose wrote a launch warning on stderr: %q", stderr.String())
		}
	})

	t.Run("start and exit", func(t *testing.T) {
		fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app"})
		run := startRequest(t, fixture, Request{
			Args: []string{"app", "--port", "0", "--no-watch"}, Output: protocolcli.OutputJSONL, Warnings: launchWarnings,
		})
		assertCarriesLaunchWarnings(t, run.line(t))
		run.cancel()
		code, rest := run.wait(t)
		if code != 0 || len(rest) != 1 {
			t.Fatalf("exit %d with %d more documents %q\nstderr:\n%s", code, len(rest), rest, run.stderr.String())
		}
		assertCarriesLaunchWarnings(t, rest[0])
		if strings.Contains(run.stderr.String(), "putnami: warning: ") {
			t.Errorf("compose wrote a launch warning on stderr:\n%s", run.stderr.String())
		}
	})

	t.Run("failure", func(t *testing.T) {
		fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", mode: fixtureExitEarly})
		run := startRequest(t, fixture, Request{
			Args: []string{"app", "--port", "0", "--no-watch"}, Output: protocolcli.OutputJSONL, Warnings: launchWarnings,
		})
		code, documents := run.wait(t)
		if code == 0 || len(documents) != 1 {
			t.Fatalf("exit %d with documents %q\nstderr:\n%s", code, documents, run.stderr.String())
		}
		assertCarriesLaunchWarnings(t, documents[0])
		if strings.Contains(run.stderr.String(), "putnami: warning: ") {
			t.Errorf("compose wrote a launch warning on stderr:\n%s", run.stderr.String())
		}
	})
}
