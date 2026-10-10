package runtimecli

import (
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// captureStderr returns an IO whose Stderr appends each progress line to lines.
func captureStderr(lines *[]string) clicore.IO {
	return clicore.IO{Stderr: func(s string) { *lines = append(*lines, s) }}
}

// In human mode the --wait loop emits the first observed state and each
// subsequent overall-state transition to stderr; a poll that observes no change
// stays silent (the 10s cadence must not spam identical lines).
func TestEmitDeployProgress_HumanEmitsStateTransitions(t *testing.T) {
	var lines []string
	io := captureStderr(&lines)
	params := map[string]any{} // human mode: no --output / --json

	p := emitDeployProgress(params, io, deployProgress{}, &deployResponse{State: "Provisioning"})
	p = emitDeployProgress(params, io, p, &deployResponse{State: "Provisioning"}) // unchanged → silent
	p = emitDeployProgress(params, io, p, &deployResponse{State: "Ready"})
	_ = p

	if len(lines) != 2 {
		t.Fatalf("stderr lines = %v, want exactly 2 (provisioning, ready)", lines)
	}
	if !strings.Contains(strings.ToLower(lines[0]), "provisioning") {
		t.Errorf("line 0 = %q, want a provisioning line", lines[0])
	}
	if !strings.Contains(strings.ToLower(lines[1]), "ready") {
		t.Errorf("line 1 = %q, want a ready line", lines[1])
	}
}

// Per-project status/action transitions each emit once and dedupe on the
// status(+action) key, so a pending→ready and a roll→skip both register but a
// repeat of the same key does not.
func TestEmitDeployProgress_HumanEmitsPerProjectTransitions(t *testing.T) {
	var lines []string
	io := captureStderr(&lines)
	params := map[string]any{}

	provisioning := &deployResponse{
		State:    "Provisioning",
		Projects: []releaseProject{{Name: "api", Status: "provisioning"}},
	}
	ready := &deployResponse{
		State:    "Ready",
		Projects: []releaseProject{{Name: "api", Status: "ready", Action: "roll"}},
	}

	p := emitDeployProgress(params, io, deployProgress{}, provisioning)
	p = emitDeployProgress(params, io, p, provisioning) // identical → no new project line
	p = emitDeployProgress(params, io, p, ready)
	_ = p

	var projectLines []string
	for _, l := range lines {
		if strings.Contains(l, "api:") {
			projectLines = append(projectLines, l)
		}
	}
	if len(projectLines) != 2 {
		t.Fatalf("per-project lines = %v, want 2 (provisioning, then ready (roll))", projectLines)
	}
	if !strings.Contains(projectLines[1], "roll") {
		t.Errorf("second project line = %q, want the action-suffixed ready key", projectLines[1])
	}
}

// A detached release is advanced by deploy-worker, not by the CLI's poll.
// `--wait` must therefore surface the durable worker attempt identity and a
// retry as distinct transitions instead of looking like a stalled Provisioning
// message forever.
func TestEmitDeployProgress_HumanShowsWorkerAttemptTransitions(t *testing.T) {
	var lines []string
	io := captureStderr(&lines)
	params := map[string]any{}
	first := &deployResponse{
		State:            "Provisioning",
		ExecutionBackend: "service",
		WorkerService:    "deploy-worker",
		WorkerRevision:   "deploy-worker-00001",
		AttemptNumber:    1,
		AttemptStatus:    "running",
	}
	second := &deployResponse{
		State:            "Provisioning",
		ExecutionBackend: "service",
		WorkerService:    "deploy-worker",
		WorkerRevision:   "deploy-worker-00002",
		AttemptNumber:    2,
		AttemptStatus:    "running",
	}

	p := emitDeployProgress(params, io, deployProgress{}, first)
	p = emitDeployProgress(params, io, p, first) // identical attempt → silent
	_ = emitDeployProgress(params, io, p, second)

	var workerLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "worker:") {
			workerLines = append(workerLines, line)
		}
	}
	if len(workerLines) != 2 {
		t.Fatalf("worker progress = %v, want one line per attempt", workerLines)
	}
	for _, want := range []string{"deploy-worker", "attempt 1", "deploy-worker-00002", "attempt 2"} {
		if !strings.Contains(strings.Join(workerLines, "\n"), want) {
			t.Errorf("worker progress missing %q: %v", want, workerLines)
		}
	}
}

// The machine contract: in structured mode (--output=jsonl / --output=json
// / --json) NO progress line is emitted — stdout stays exactly the single
// terminal object. The snapshot is still advanced so no stale transition would
// replay if the mode ever flipped mid-run.
func TestEmitDeployProgress_StructuredEmitsNothing(t *testing.T) {
	for _, params := range []map[string]any{
		{"output": "jsonl"},
		{"output": "json"},
		{"json": true},
	} {
		var lines []string
		io := captureStderr(&lines)

		p := emitDeployProgress(params, io, deployProgress{}, &deployResponse{
			State:    "Provisioning",
			Projects: []releaseProject{{Name: "api", Status: "provisioning"}},
		})
		p = emitDeployProgress(params, io, p, &deployResponse{State: "Ready"})

		if len(lines) != 0 {
			t.Fatalf("params %v: structured mode emitted %v, want no progress lines", params, lines)
		}
		if p.state != "Ready" {
			t.Errorf("params %v: snapshot state = %q, want Ready (observation must still be folded)", params, p.state)
		}
	}
}

// A nil Stderr sink is tolerated: emission is skipped without a panic, and the
// snapshot still advances.
func TestEmitDeployProgress_NilStderrIsSafe(t *testing.T) {
	params := map[string]any{}
	p := emitDeployProgress(params, clicore.IO{}, deployProgress{}, &deployResponse{State: "Provisioning"})
	if p.state != "Provisioning" {
		t.Fatalf("snapshot state = %q, want Provisioning even with a nil stderr sink", p.state)
	}
}
