package infraagg

import (
	"go.putnami.dev/protocol/features/spectest"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/jsonl"
)

// captureStdout runs fn with os.Stdout redirected to a file and returns what
// was written. The emitter binds to os.Stdout at emit time, so this is the
// only way to read the event stream a real task produces.
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

func TestJob_EmitsTheManifestAndReportsOK(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)

	var status string
	var data map[string]any
	var err error
	out := captureStdout(t, func() {
		status, data, err = Job(Options{})(ctx, jsonl.NewForVersion(1), nil)
	})

	if err != nil {
		t.Fatalf("job returned an error: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK", status)
	}
	if data["outcome"] != string(OutcomeEmitted) {
		t.Errorf("data[outcome] = %v, want %q", data["outcome"], OutcomeEmitted)
	}
	readAggregated(t, root, "app")
	if !strings.Contains(out, `"phase"`) || !strings.Contains(out, PhaseName) {
		t.Errorf("event stream does not report the %q phase:\n%s", PhaseName, out)
	}
	if !strings.Contains(out, `"artifact"`) || !strings.Contains(out, "requirements.json") {
		t.Errorf("event stream does not report the emitted artifact:\n%s", out)
	}
}

// A library reports SKIP: it is not a failure and not a no-op the reader has to
// infer from an empty result.
func TestJob_SkipsLibraries(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "lib", "lib", "library")

	var status string
	captureStdout(t, func() {
		status, _, _ = Job(Options{})(ctx, jsonl.NewForVersion(1), nil)
	})

	if status != "SKIP" {
		t.Errorf("status = %q, want SKIP", status)
	}
}

// Findings never fail the task: a build that produced binaries must not be
// failed by an advisory deployability finding.
func TestJob_FindingsAreWarningsAndNeverFail(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "findings-never-fail-the-task")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2,"databazes":[]}`)

	var status string
	var err error
	out := captureStdout(t, func() {
		status, _, err = Job(Options{})(ctx, jsonl.NewForVersion(1), nil)
	})

	if err != nil || status != "OK" {
		t.Fatalf("status = %q, err = %v; an advisory finding must not fail the task", status, err)
	}
	if !strings.Contains(out, `"warn"`) || !strings.Contains(out, "infra requirements:") {
		t.Errorf("finding was not surfaced as a warning:\n%s", out)
	}
	if strings.Contains(out, `"level":"error"`) {
		t.Errorf("finding was escalated to an error level:\n%s", out)
	}
}

// A workload that declares nothing reports SKIP: its run removes an earlier
// manifest, which a cache entry of the task cannot replay, so the language's
// required manifest output keeps the run uncached.
func TestJob_AWorkloadThatDeclaresNothingIsASkip(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	writeProjectFile(t, root, "app", AggregatedManifestFile, `{"protocolVersion":2,"workload":"app"}`)
	writeProjectFile(t, root, "app", "infra/runtime.json", `{ this is not json`)

	var status string
	var data map[string]any
	var err error
	out := captureStdout(t, func() {
		status, data, err = Job(Options{})(ctx, jsonl.NewForVersion(1), nil)
	})

	if err != nil || status != "SKIP" || data["outcome"] != string(OutcomeCleared) {
		t.Fatalf("job = %q %v %v, want a cleared SKIP that does not fail", status, data, err)
	}
	if _, statErr := os.Stat(aggregatedPath(root, "app")); !os.IsNotExist(statErr) {
		t.Errorf("the earlier manifest survived, stat err = %v", statErr)
	}
	if strings.Contains(out, `"artifact"`) {
		t.Errorf("a cleared run reported an artifact:\n%s", out)
	}
}

// A failed write reports SKIP and never fails: the files on disk can be an
// earlier run's, so the run must not be cached, and a deployability manifest
// never fails a build. The failure reaches the reader as a warning.
func TestJob_AFailedWriteIsASkipThatNeverFails(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "findings-never-fail-the-task")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)
	writeProjectFile(t, root, "app", AggregatedManifestFile+"/blocker", "x")

	var status string
	var data map[string]any
	var err error
	out := captureStdout(t, func() {
		status, data, err = Job(Options{})(ctx, jsonl.NewForVersion(1), nil)
	})

	if err != nil || status != "SKIP" {
		t.Fatalf("job = %q %v, want a SKIP that does not fail", status, err)
	}
	if data["outcome"] != string(OutcomeEmitted) {
		t.Errorf("data[outcome] = %v, want %q", data["outcome"], OutcomeEmitted)
	}
	if !strings.Contains(out, `"warn"`) || !strings.Contains(out, "write aggregated infra manifest") {
		t.Errorf("the failed write was not surfaced as a warning:\n%s", out)
	}
	if strings.Contains(out, `"artifact"`) {
		t.Errorf("a failed write reported an artifact:\n%s", out)
	}
}
