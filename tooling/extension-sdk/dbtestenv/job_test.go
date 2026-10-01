package dbtestenv

import (
	"go.putnami.dev/protocol/features/spectest"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/jsonl"
)

// captureStdout runs fn with os.Stdout redirected to a file and returns what
// was written. The emitter binds to os.Stdout at emit time, so this is the only
// way to read the event stream a real task produces.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	defer func() {
		os.Stdout = original
		_ = file.Close()
	}()
	fn()
	_ = file.Sync()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The single most important property of the producer's job wrapper: it reports
// SUCCESS even when it deliberately provisions nothing.
//
// The orchestrator reads a producer's terminal status as the invocation
// relation's setup verdict — anything else marks the relation setupFailed and
// SKIPS every listed consumer with `sensitive.setup_failed`. A unit-only
// project would then have its whole test job skipped because it has no
// database, which is the exact opposite of the zero-cost path.
func TestUpJob_ReportsSuccessWhenItProvisionsNothing(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	// The job wrapper is the one path that SELECTS a provider from the
	// environment, so the ambient one must be neutralized or a developer's own
	// PUTNAMI_TEST_PG_URL would decide this case.
	t.Setenv(EnvProvidedServer, "")
	f := newFixture(t, nil)

	var status string
	var data map[string]any
	var err error
	out := captureStdout(t, func() {
		status, data, err = UpJob()(f.ctx, jsonl.NewForVersion(1), nil)
	})

	if err != nil {
		t.Fatalf("producer returned an error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK — a non-success producer skips every consumer", status)
	}
	if data["outcome"] != string(OutcomeNoDatabases) {
		t.Errorf("data[outcome] = %v, want %q", data["outcome"], OutcomeNoDatabases)
	}
	if !strings.Contains(out, PhaseName) {
		t.Errorf("event stream does not report the %q phase:\n%s", PhaseName, out)
	}
}

// The environment could not satisfy `require`: the finding is a WARNING and the
// status stays successful, so the test task reports the real failure instead of
// the producer masking it with a skipped frontier.
func TestUpJob_AdvisoryNeverFailsTheTask(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	t.Setenv(EnvProvidedServer, "")
	f := newFixture(t, map[string]any{ParamInfra: "require"})
	f.declareDatabases(t, "svc", postgresRequirements)

	var status string
	var err error
	out := captureStdout(t, func() {
		status, _, err = UpJob()(f.ctx, jsonl.NewForVersion(1), nil)
	})

	if err != nil || status != "OK" {
		t.Fatalf("status/err = %q/%v, want OK/nil — a setup advisory must never fail the task", status, err)
	}
	if !strings.Contains(out, DiagnosticNoProvider) {
		t.Errorf("event stream does not carry the %s advisory:\n%s", DiagnosticNoProvider, out)
	}
}

// The finalizer reports its outcome and never fails the run it tears down.
func TestDownJob_ReportsRetention(t *testing.T) {
	f := newFixture(t, nil)
	if err := writeLease(f.artifacts, Lease{
		Version: LeaseVersion, InvocationID: "inv-fixture", Provider: providerID,
		Label: labelKey, Digest: "0123456789abcdef", Retain: true,
	}); err != nil {
		t.Fatal(err)
	}

	var status string
	var data map[string]any
	var err error
	captureStdout(t, func() {
		status, data, err = DownJob()(f.ctx, jsonl.NewForVersion(1), nil)
	})

	if err != nil || status != "OK" {
		t.Fatalf("status/err = %q/%v, want OK/nil", status, err)
	}
	if data["outcome"] != string(OutcomeRetained) {
		t.Errorf("data[outcome] = %v, want %q", data["outcome"], OutcomeRetained)
	}
}

// Nothing the producer publishes may carry the credential — the event stream
// included, because the orchestrator's leak guard fails a task that emits one
// and a producer failing is a skipped consumer frontier.
func TestUpJob_EventStreamIsCredentialFree(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "credential-confinement", "the-event-stream-is-credential-free")
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	// Provision through the stub so a credential genuinely exists on disk.
	out := captureStdout(t, func() {
		result := Up(f.ctx, stubProvisioner(nil))
		reportDiagnostics(jsonl.NewForVersion(1), result.Diagnostics)
		if result.Outcome != OutcomeProvisioned {
			t.Errorf("outcome = %q, want provisioned", result.Outcome)
		}
	})

	if strings.Contains(out, fixturePassword) {
		t.Errorf("the producer's event stream leaked the credential:\n%s", out)
	}
	if binding, ok := BindingFrom(f.ctx); !ok || !strings.Contains(binding, fixturePassword) {
		t.Error("the fixture never wrote a credential, so this proves nothing")
	}
}
