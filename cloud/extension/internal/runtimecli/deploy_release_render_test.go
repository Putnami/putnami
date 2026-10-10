package runtimecli

import (
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// TestReleaseTable_ShowsAction pins that the multi-project human summary surfaces
// the per-project deploy action (skip|roll) so an operator can see which projects
// reused a revision vs rolled a fresh one.
func TestReleaseTable_ShowsAction(t *testing.T) {
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{WorkspaceID: "ws-acme"}, environment: "prod"}
	results := []releaseProject{
		{Name: "accounts/workloads/auth-server", Status: "Ready", Action: "roll", URL: "https://a.example"},
		{Name: "config/server", Status: "Ready", Action: "skip", URL: "https://c.example"},
	}
	out := releaseTableTyped(ctx, "Ready", "rel_x", results)
	if !strings.Contains(out, "roll") {
		t.Errorf("table missing 'roll' action:\n%s", out)
	}
	if !strings.Contains(out, "skip") {
		t.Errorf("table missing 'skip' action:\n%s", out)
	}
}

// TestDeployReleaseMessage_SkipVsRoll pins that the single-project line tells the
// truth: a digest-travel skip says "Unchanged", a real rollout says "Deployed".
func TestDeployReleaseMessage_SkipVsRoll(t *testing.T) {
	roll := deployReleaseMessage("ws-acme", "accounts/workloads/auth-server", "prod", "Ready", "rel_x",
		map[string]any{"url": "https://a.example", "revision": "rev-2", "action": "roll"})
	if !strings.Contains(roll, "Deployed") {
		t.Errorf("roll message = %q, want it to say Deployed", roll)
	}

	skip := deployReleaseMessage("ws-acme", "accounts/workloads/auth-server", "prod", "Ready", "rel_x",
		map[string]any{"url": "https://a.example", "revision": "rev-1", "action": "skip"})
	if !strings.Contains(skip, "Unchanged") {
		t.Errorf("skip message = %q, want it to say Unchanged", skip)
	}
}

// TestReleaseFollowUp_NamesCommandsOnFailure pins that a Partial/Failed release's
// human output names both follow-up commands (per failed project): the logs
// `--since deploy:last --level error` line and the `deploy status <release-id>`
// line. Ready adds nothing.
func TestReleaseFollowUp_NamesCommandsOnFailure(t *testing.T) {
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{WorkspaceID: "ws-acme"}, environment: "prod"}
	results := []releaseProject{
		{Name: "accounts/workloads/auth-server", Status: "Ready", Action: "roll"},
		{Name: "config/server", Status: "Failed", Action: "roll"},
	}

	failed := releaseFollowUp(ctx, "Partial", "rel_x", results, "", false)
	for _, want := range []string{
		"putnami cloud logs config/server --env prod --since deploy:last --level error",
		"putnami cloud deploy status rel_x",
	} {
		if !strings.Contains(failed, want) {
			t.Errorf("follow-up missing %q:\n%s", want, failed)
		}
	}
	// Only the non-Ready project gets a logs line.
	if strings.Contains(failed, "logs accounts/workloads/auth-server") {
		t.Errorf("follow-up should not name the Ready project:\n%s", failed)
	}

	if got := releaseFollowUp(ctx, "Ready", "rel_x", results, "", false); got != "" {
		t.Errorf("Ready release must add no follow-up, got %q", got)
	}
}

// TestReleaseFollowUp_SingleTargetFallbackApp: a Failed single-target release with
// no per-project row still names the logs follow-up using the release's own app.
func TestReleaseFollowUp_SingleTargetFallbackApp(t *testing.T) {
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{WorkspaceID: "ws-acme"}, environment: "staging"}
	got := releaseFollowUp(ctx, "Failed", "rel_y", nil, "payments/workloads/api", false)
	if !strings.Contains(got, "putnami cloud logs payments/workloads/api --env staging --since deploy:last --level error") {
		t.Errorf("fallback-app logs line missing:\n%s", got)
	}
	if !strings.Contains(got, "putnami cloud deploy status rel_y") {
		t.Errorf("deploy status line missing:\n%s", got)
	}
}

// TestReleaseFollowUp_ProvisioningNamesStatusCommand pins that a non-wait
// deploy that returns 202 Provisioning is accepted but NOT applied, so its human
// output names the release-status command that confirms it converged — plus the
// --wait alternative — without the per-project failure-logs guidance (nothing has
// failed yet). Ready still adds nothing.
func TestReleaseFollowUp_ProvisioningNamesStatusCommand(t *testing.T) {
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{WorkspaceID: "ws-acme"}, environment: "prod"}
	got := releaseFollowUp(ctx, "Provisioning", "rel_x", nil, "payments/workloads/api", false)
	if !strings.Contains(got, "putnami cloud deploy status rel_x") {
		t.Errorf("Provisioning follow-up must name the status command:\n%s", got)
	}
	if !strings.Contains(got, "--wait") {
		t.Errorf("a non-wait Provisioning submit should offer the --wait alternative:\n%s", got)
	}
	if strings.Contains(got, "--since deploy:last") {
		t.Errorf("Provisioning follow-up must not emit failure-logs guidance:\n%s", got)
	}
	if empty := releaseFollowUp(ctx, "Ready", "rel_x", nil, "", false); empty != "" {
		t.Errorf("Ready release must add no follow-up, got %q", empty)
	}
}

// TestReleaseFollowUp_TimedOutWaitOmitsRerun pins the timeout path: a --wait that
// times out ALSO renders Provisioning, but must NOT suggest re-running with
// --wait — a fresh invocation mints a new release_id and the control plane
// accepts concurrent releases, so it would start a second rollout. It is directed
// solely to `deploy status <release-id>`.
func TestReleaseFollowUp_TimedOutWaitOmitsRerun(t *testing.T) {
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{WorkspaceID: "ws-acme"}, environment: "prod"}
	got := releaseFollowUp(ctx, "Provisioning", "rel_x", nil, "payments/workloads/api", true)
	if !strings.Contains(got, "putnami cloud deploy status rel_x") {
		t.Errorf("timed-out --wait follow-up must still name the status command:\n%s", got)
	}
	if strings.Contains(got, "--wait") {
		t.Errorf("timed-out --wait must not advise re-running with --wait (starts a second rollout):\n%s", got)
	}
}

// TestDeployReleaseMessage_ProvisioningNotYetApplied pins that the single-target
// Provisioning line labels the outcome accepted-but-not-applied rather than
// reading as a completed deploy, and keeps the "still provisioning" phrase the
// wait-timeout render asserts on.
func TestDeployReleaseMessage_ProvisioningNotYetApplied(t *testing.T) {
	msg := deployReleaseMessage("ws-acme", "payments/workloads/api", "prod", "Provisioning", "rel_x", map[string]any{})
	if !strings.Contains(msg, "not yet applied") {
		t.Errorf("Provisioning message must label the outcome not-yet-applied:\n%s", msg)
	}
	if strings.Contains(msg, "Deployed") {
		t.Errorf("Provisioning must not read as a completed deploy:\n%s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "still provisioning") {
		t.Errorf("Provisioning message should still say 'still provisioning':\n%s", msg)
	}
}

// TestRenderRelease_ProvisioningNonWaitNamesHandoff pins end-to-end that a
// non-wait deploy whose submit returns 202 Provisioning still exits 0 (a
// successful async submit) but its human output labels the outcome not-applied
// and names the status handoff, while structured (--output) mode leaks none of
// that text and still reports the provisioning status field.
func TestRenderRelease_ProvisioningNonWaitNamesHandoff(t *testing.T) {
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{WorkspaceID: "ws-acme"}, environment: "prod"}
	targets := []deployTarget{{app: "payments/workloads/api", version: "1.0.0"}}
	resp := &deployResponse{
		ReleaseID: "rel_x",
		State:     "Provisioning",
		Projects:  []releaseProject{{Name: "payments/workloads/api", Status: "provisioning"}},
	}

	cap := &captureIO{}
	if err := renderRelease(map[string]any{}, ctx, cap.io(nil), "rel_x", targets, resp); err != nil {
		t.Fatalf("non-wait Provisioning must exit 0, got %v", err)
	}
	human := strings.Join(cap.stdout, "\n")
	if !strings.Contains(human, "not yet applied") {
		t.Errorf("human output must label the outcome not-yet-applied:\n%s", human)
	}
	if !strings.Contains(human, "putnami cloud deploy status rel_x") {
		t.Errorf("human output must name the status handoff command:\n%s", human)
	}

	cap.stdout = nil
	if err := renderRelease(map[string]any{"output": "jsonl"}, ctx, cap.io(nil), "rel_x", targets, resp); err != nil {
		t.Fatalf("non-wait Provisioning --output must exit 0, got %v", err)
	}
	structured := strings.Join(cap.stdout, "\n")
	if strings.Contains(structured, "deploy status rel_x") || strings.Contains(structured, "not yet applied") {
		t.Fatalf("structured output leaked human handoff text:\n%s", structured)
	}
	if !strings.Contains(structured, "provisioning") {
		t.Fatalf("structured result missing the provisioning status:\n%s", structured)
	}
}

// The deploy terminal JSONL result is intentionally protocol-shaped (the shared
// result envelope) but the data object must carry enough release observation to
// gate CI without scraping human output: worker attempt, separate workload vs
// local task-graph counts, terminal classification, and named timing fields.
func TestRenderRelease_JSONLIncludesWorkerOutcomeAndTimings(t *testing.T) {
	ready := true
	traffic := 100
	queueWait := int64(1200)
	total := int64(5400)
	ctx := &deployCtx{
		WorkspaceContext:           &clicore.WorkspaceContext{WorkspaceID: "ws-acme"},
		environment:                "prod",
		localTaskGraphProjectCount: 4,
	}
	resp := &deployResponse{
		ReleaseID:               "rel_x",
		State:                   "Partial",
		CloudWorkloadCount:      2,
		RequestedWorkloadCount:  2,
		ServingNewRevisionCount: 1,
		HeldWorkloadCount:       1,
		TerminalClassification:  "held",
		ExecutionBackend:        "service",
		WorkerService:           "deploy-worker",
		WorkerRevision:          "deploy-worker-00002",
		AttemptNumber:           2,
		AttemptStatus:           "failed",
		Timings: &releaseTimings{
			QueueWaitMS:      &queueWait,
			TotalWallClockMS: &total,
		},
		Projects: []releaseProject{
			{
				Name:              "payments/workloads/api",
				Status:            "Ready",
				Action:            "roll",
				ImageDigest:       "sha256:abc123",
				CandidateRevision: "api-00002",
				ServingRevision:   "api-00002",
				Ready:             &ready,
				TrafficPercent:    &traffic,
			},
			{Name: "config/server", Status: "Skipped", HoldReason: "held after preflight failure"},
		},
	}
	targets := []deployTarget{{app: "payments/workloads/api"}, {app: "config/server"}}
	cap := &captureIO{}
	_ = renderRelease(map[string]any{"output": "jsonl"}, ctx, cap.io(nil), "rel_x", targets, resp)

	var data map[string]any
	if err := decodeResultData([]byte(strings.Join(cap.stdout, "\n")), &data); err != nil {
		t.Fatalf("decode JSONL result: %v\n%s", err, strings.Join(cap.stdout, "\n"))
	}
	for key, want := range map[string]any{
		"release_id":                     "rel_x",
		"execution_backend":              "service",
		"worker_service":                 "deploy-worker",
		"worker_revision":                "deploy-worker-00002",
		"terminal_classification":        "held",
		"cloud_workload_count":           float64(2),
		"local_task_graph_project_count": float64(4),
		"attempt_number":                 float64(2),
	} {
		if got := data[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	timings, ok := data["timings"].(map[string]any)
	if !ok {
		t.Fatalf("timings = %#v, want object", data["timings"])
	}
	for _, key := range []string{
		"build_ms", "publish_ms", "release_submit_ms", "queue_wait_ms",
		"worker_preflight_ms", "provisioning_tiers_ms", "readiness_ms",
		"traffic_convergence_ms", "total_wall_clock_ms",
	} {
		if _, ok := timings[key]; !ok {
			t.Errorf("timings missing %q: %#v", key, timings)
		}
	}

	human := releaseTablePostResponse(ctx, resp)
	for _, want := range []string{
		"Execution: service", "deploy-worker", "attempt 2", "Terminal classification: held",
		"2 requested · 1 serving new revision · 1 held", "sha256:abc123", "candidate api-00002",
		"serving api-00002", "traffic 100%", "hold: held after preflight failure",
	} {
		if !strings.Contains(human, want) {
			t.Errorf("human release output missing %q:\n%s", want, human)
		}
	}
}

func TestReleaseCountsForOutput_HoldsSkippedCandidateWithoutServingTraffic(t *testing.T) {
	ready := true
	traffic := 0
	counts := releaseCountsForOutput("Ready", []releaseProject{{
		Name:              "payments/workloads/api",
		Status:            "Ready",
		Action:            "skip",
		CandidateRevision: "api-00002",
		ServingRevision:   "api-00001",
		Ready:             &ready,
		TrafficPercent:    &traffic,
	}}, 0, 0, 0, 0, "")

	if counts.requested != 1 || counts.servingNew != 0 || counts.reused != 0 || counts.held != 1 || counts.classification != "held" {
		t.Fatalf("counts = %+v, want requested=1 servingNew=0 reused=0 held=1 classification=held", counts)
	}
}
