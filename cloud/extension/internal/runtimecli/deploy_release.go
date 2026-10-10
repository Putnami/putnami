package runtimecli

// The release render: how a publish-v2 release and its --wait poll print, and
// the exit code a settled release earns.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// generateReleaseID produces a release identifier matching the CPA
// handler's `[A-Za-z0-9_-]{8,64}` shape: a `rel_` prefix plus 32 hex
// chars of crypto/rand bytes (36 chars total). The prefix isn't load-
// bearing on the server — it's there so deploy logs are scannable.
//
// The handler can server-generate when the field is empty in a future
// pass; until then, the CLI always sends one.
func generateReleaseID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", clicore.NewError("generate release id: "+err.Error(), clicore.ExitAPI)
	}
	return "rel_" + hex.EncodeToString(buf), nil
}

// deployTarget is one workload of a submitted release, as the render names
// it: the project and its image selector.
type deployTarget struct {
	app       string
	version   string
	digest    string
	imageName string
}

// renderRelease writes the deploy result and returns a non-zero exit when the
// release settled Partial or Failed. A single-target release keeps the legacy
// single-line message + flat result shape; a multi-target release renders a
// per-project table and carries the projects array in the JSON result.
func renderRelease(params map[string]any, ctx *deployCtx, ioctx clicore.IO, releaseID string, targets []deployTarget, resp *deployResponse) error {
	now := time.Now()
	if ioctx.Now != nil {
		now = ioctx.Now()
	}
	resp = releaseResponseForRender(ctx, resp, now)
	state := resp.State
	results := resp.Projects
	// A timed-out --wait renders Provisioning here too; the follow-up drops the
	// "re-run with --wait" hint in that case (it would start a second rollout).
	wait := clicore.Truthy(clicore.Param(params, "wait"))

	if len(targets) == 1 {
		tgt := targets[0]
		var project releaseProject
		if len(results) > 0 {
			project = results[0]
		}
		out := map[string]any{
			"status":       strings.ToLower(state),
			"workspace":    ctx.WorkspaceID,
			"app":          tgt.app,
			"environment":  ctx.environment,
			"release_id":   resp.ReleaseID,
			"image":        tgt.version,
			"image_digest": tgt.digest,
			"url":          project.URL,
			"revision":     project.Revision,
			"action":       project.Action,
		}
		if project.ImageDigest != "" {
			out["image_digest"] = project.ImageDigest
		}
		out["candidate_revision"] = project.CandidateRevision
		out["ready"] = project.Ready
		out["serving_revision"] = project.ServingRevision
		out["traffic_percent"] = project.TrafficPercent
		out["hold_reason"] = project.HoldReason
		addReleaseObservationOutput(out, ctx, targets, resp)
		if project.Detail != "" {
			out["detail"] = project.Detail
		}
		message := deployReleaseMessage(ctx.WorkspaceID, tgt.app, ctx.environment, state, releaseID, out)
		message += releasePostObservationHuman(resp)
		message += releaseFollowUp(ctx, state, releaseID, results, tgt.app, wait)
		clicore.WriteResult(out, params, ioctx, message)
		return releaseExit(state, releaseID)
	}

	out := map[string]any{
		"status":      strings.ToLower(state),
		"workspace":   ctx.WorkspaceID,
		"environment": ctx.environment,
		"release_id":  resp.ReleaseID,
		"projects":    resp.Projects,
	}
	addReleaseObservationOutput(out, ctx, targets, resp)
	message := releaseTablePostResponse(ctx, resp) + releaseFollowUp(ctx, state, releaseID, results, "", wait)
	clicore.WriteResult(out, params, ioctx, message)
	return releaseExit(state, releaseID)
}

// releaseResponseForRender merges the server's durable observations with the
// two measurements only this CLI invocation owns: submit round-trip and total
// command wall clock. It deliberately leaves build/publish nil because those
// task timings belong to upstream commands, not a standalone cloud-release
// process; null is more useful to CI than an invented zero.
func releaseResponseForRender(ctx *deployCtx, resp *deployResponse, now time.Time) *deployResponse {
	if resp == nil {
		resp = &deployResponse{}
	}
	copy := *resp
	copy.Timings = releaseTimingsForRender(ctx, resp.Timings, now)
	return &copy
}

func releaseTimingsForRender(ctx *deployCtx, server *releaseTimings, now time.Time) *releaseTimings {
	timings := &releaseTimings{}
	if server != nil {
		*timings = *server
	}
	if ctx == nil {
		return timings
	}
	if ctx.releaseSubmitMS != nil {
		timings.ReleaseSubmitMS = ctx.releaseSubmitMS
	}
	if !ctx.releaseWallStarted.IsZero() && !now.Before(ctx.releaseWallStarted) {
		value := now.Sub(ctx.releaseWallStarted).Milliseconds()
		timings.TotalWallClockMS = &value
	}
	return timings
}

type releaseOutputCounts struct {
	requested      int
	servingNew     int
	reused         int
	held           int
	classification string
}

func releaseCountsForOutput(state string, projects []releaseProject, requested, servingNew, reused, held int, classification string) releaseOutputCounts {
	if requested == 0 && len(projects) > 0 {
		for _, project := range projects {
			requested++
			switch {
			case projectReusesServingRevisionForOutput(project):
				reused++
			case projectServesNewRevisionForOutput(project):
				servingNew++
			default:
				held++
			}
		}
	}
	if classification == "" {
		switch state {
		case "Ready":
			if held > 0 {
				classification = "held"
			} else {
				classification = "serving"
			}
		case "Partial":
			classification = "held"
		case "Failed":
			classification = "failed"
		case "Provisioning":
			classification = "in_progress"
		default:
			classification = "unknown"
		}
	}
	return releaseOutputCounts{
		requested: requested, servingNew: servingNew, reused: reused,
		held: held, classification: classification,
	}
}

func projectServesNewRevisionForOutput(project releaseProject) bool {
	if project.Status != "Ready" || project.Action != "roll" {
		return false
	}
	// Old control planes did not send serving observations. Preserve their
	// existing Ready/roll meaning instead of rendering a false hold.
	if project.CandidateRevision == "" && project.ServingRevision == "" && project.TrafficPercent == nil {
		return true
	}
	return project.CandidateRevision != "" && project.ServingRevision != "" &&
		project.TrafficPercent != nil && *project.TrafficPercent == 100 &&
		sameCloudRunRevisionForOutput(project.CandidateRevision, project.ServingRevision)
}

func projectReusesServingRevisionForOutput(project releaseProject) bool {
	if project.Status != "Ready" || project.Action != "skip" {
		return false
	}
	// Old control planes did not send serving observations. Preserve their
	// existing Ready/skip meaning instead of rendering a false hold.
	if project.CandidateRevision == "" && project.ServingRevision == "" && project.TrafficPercent == nil {
		return true
	}
	return project.CandidateRevision != "" && project.ServingRevision != "" &&
		project.TrafficPercent != nil && *project.TrafficPercent == 100 &&
		sameCloudRunRevisionForOutput(project.CandidateRevision, project.ServingRevision)
}

func sameCloudRunRevisionForOutput(left, right string) bool {
	return cloudRunRevisionLeafForOutput(left) != "" && cloudRunRevisionLeafForOutput(left) == cloudRunRevisionLeafForOutput(right)
}

func cloudRunRevisionLeafForOutput(revision string) string {
	if slash := strings.LastIndexByte(revision, '/'); slash >= 0 {
		return revision[slash+1:]
	}
	return revision
}

// addReleaseObservationOutput makes the JSON/JSONL terminal object explicit
// about execution, counts, and timing while retaining its pre-existing flat
// result fields. The current API response is copied wholesale only for
// `deploy status`; deploy itself preserves the long-standing result-map shape.
func addReleaseObservationOutput(out map[string]any, ctx *deployCtx, targets []deployTarget, resp *deployResponse) {
	if out == nil || resp == nil {
		return
	}
	counts := releaseCountsForOutput(resp.State, resp.Projects, resp.RequestedWorkloadCount, resp.ServingNewRevisionCount, resp.ReusedWorkloadCount, resp.HeldWorkloadCount, resp.TerminalClassification)
	cloudWorkloads := resp.CloudWorkloadCount
	if cloudWorkloads == 0 {
		cloudWorkloads = len(resp.Projects)
	}
	if cloudWorkloads == 0 {
		cloudWorkloads = len(targets)
	}
	localProjects := len(targets)
	if ctx != nil && ctx.localTaskGraphProjectCount > 0 {
		localProjects = ctx.localTaskGraphProjectCount
	}
	out["cloud_workload_count"] = cloudWorkloads
	out["local_task_graph_project_count"] = localProjects
	out["requested_workload_count"] = counts.requested
	out["serving_new_revision_count"] = counts.servingNew
	out["reused_workload_count"] = counts.reused
	out["held_workload_count"] = counts.held
	out["terminal_classification"] = counts.classification
	out["execution_backend"] = resp.ExecutionBackend
	out["worker_service"] = resp.WorkerService
	out["worker_revision"] = resp.WorkerRevision
	out["attempt_number"] = resp.AttemptNumber
	out["attempt_status"] = resp.AttemptStatus
	out["attempts"] = resp.Attempts
	out["timings"] = resp.Timings
}

func releaseTablePostResponse(ctx *deployCtx, resp *deployResponse) string {
	if resp == nil {
		return ""
	}
	counts := releaseCountsForOutput(resp.State, resp.Projects, resp.RequestedWorkloadCount, resp.ServingNewRevisionCount, resp.ReusedWorkloadCount, resp.HeldWorkloadCount, resp.TerminalClassification)
	return releaseTableTyped(ctx, resp.State, resp.ReleaseID, resp.Projects) +
		releaseObservationHuman(resp.ExecutionBackend, resp.WorkerService, resp.WorkerRevision, resp.AttemptNumber, resp.AttemptStatus, counts, resp.Timings)
}

func releasePostObservationHuman(resp *deployResponse) string {
	if resp == nil {
		return ""
	}
	counts := releaseCountsForOutput(resp.State, resp.Projects, resp.RequestedWorkloadCount, resp.ServingNewRevisionCount, resp.ReusedWorkloadCount, resp.HeldWorkloadCount, resp.TerminalClassification)
	return releaseObservationHuman(resp.ExecutionBackend, resp.WorkerService, resp.WorkerRevision, resp.AttemptNumber, resp.AttemptStatus, counts, resp.Timings)
}

func releaseObservationHuman(backend, workerService, workerRevision string, attemptNumber int64, attemptStatus string, counts releaseOutputCounts, timings *releaseTimings) string {
	var b strings.Builder
	if backend != "" || attemptNumber > 0 {
		fmt.Fprintf(&b, "\nExecution: %s", firstNonEmptyStringForOutput(backend, "unknown"))
		if workerService != "" {
			fmt.Fprintf(&b, " · worker %s", workerService)
		}
		if workerRevision != "" {
			fmt.Fprintf(&b, " · revision %s", workerRevision)
		}
		if attemptNumber > 0 {
			fmt.Fprintf(&b, " · attempt %d", attemptNumber)
		}
		if attemptStatus != "" {
			fmt.Fprintf(&b, " · %s", attemptStatus)
		}
	}
	if counts.classification != "" {
		fmt.Fprintf(&b, "\nTerminal classification: %s", counts.classification)
	}
	if timings != nil {
		fmt.Fprintf(&b, "\nTimings: build %s · publish %s · release submit %s · queue wait %s · worker preflight %s · provisioning tiers %s · readiness %s · traffic convergence %s · total wall clock %s",
			formatReleaseTiming(timings.BuildMS),
			formatReleaseTiming(timings.PublishMS),
			formatReleaseTiming(timings.ReleaseSubmitMS),
			formatReleaseTiming(timings.QueueWaitMS),
			formatReleaseTiming(timings.WorkerPreflightMS),
			formatReleaseTiming(timings.ProvisioningTiersMS),
			formatReleaseTiming(timings.ReadinessMS),
			formatReleaseTiming(timings.TrafficConvergenceMS),
			formatReleaseTiming(timings.TotalWallClockMS))
	}
	fmt.Fprintf(&b, "\n%d requested · %d serving new revision · %d held", counts.requested, counts.servingNew, counts.held)
	if counts.reused > 0 {
		fmt.Fprintf(&b, " · %d reused", counts.reused)
	}
	return b.String()
}

func firstNonEmptyStringForOutput(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func formatReleaseTiming(value *int64) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%dms", *value)
}

// releaseFollowUp appends the next-command guidance a Provisioning or
// Partial/Failed release should name in its HUMAN output (never the structured
// object — WriteResult emits the result map under --output, this string only in
// human mode). For Partial/Failed it names the logs follow-up per failed/partial
// project (using the server-side `--since deploy:last` resolution) plus the
// release-detail command. For a non-wait Provisioning submit — accepted but not
// yet applied — it names only the release-status command that surfaces the
// terminal verdict, so a deploy that later fails the migrate gate server-side is
// not left reading as a completed rollout. Ready releases add nothing. A
// single-target release with no per-project row falls back to the release's own
// app for the logs line. wait is whether the operator passed --wait, so a
// timed-out --wait (which also renders Provisioning) is not told to re-run.
func releaseFollowUp(ctx *deployCtx, state, releaseID string, results []releaseProject, fallbackApp string, wait bool) string {
	if state == "Provisioning" {
		// A non-wait deploy exits 0 on this 202, so the handoff line is what keeps
		// the outcome from being mistaken for a completed deploy: name the status
		// command that confirms it converged before the operator relies on it.
		var b strings.Builder
		b.WriteString("\n\nThe deploy is not confirmed — the release can still fail server-side. Confirm it converged:")
		fmt.Fprintf(&b, "\n  putnami cloud deploy status %s", releaseID)
		// Only a non-wait submission gets the --wait alternative. A --wait that
		// timed out ALSO renders Provisioning here, but suggesting "re-run with
		// --wait" then is wrong: a fresh invocation mints a new release_id and the
		// control plane accepts concurrent releases, so it would start a SECOND
		// rollout instead of continuing to observe this one. A timed-out --wait is
		// directed solely to `deploy status <release-id>`.
		if !wait {
			b.WriteString("\n  (or re-run with --wait to block until the release settles)")
		}
		return b.String()
	}
	if state != "Partial" && state != "Failed" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nNext steps:")
	apps := failedProjectApps(results)
	if len(apps) == 0 && fallbackApp != "" {
		apps = []string{fallbackApp}
	}
	for _, app := range apps {
		fmt.Fprintf(&b, "\n  putnami cloud logs %s --env %s --since deploy:last --level error", app, ctx.environment)
	}
	fmt.Fprintf(&b, "\n  putnami cloud deploy status %s", releaseID)
	return b.String()
}

// failedProjectApps returns the names of the per-project rows that did not
// succeed (anything other than a Ready status), so the logs follow-up targets
// only the projects an operator needs to investigate. Order follows the result
// rows; empty names are skipped.
func failedProjectApps(results []releaseProject) []string {
	apps := make([]string, 0, len(results))
	for _, r := range results {
		if r.Name == "" {
			continue
		}
		if r.Status == "Ready" {
			continue
		}
		apps = append(apps, r.Name)
	}
	return apps
}

// releaseExit maps the aggregate release state to an exit code: Ready and
// Provisioning (still converging) exit 0; Partial and Failed exit non-zero so
// CI fails after the per-project breakdown has been rendered. This is the
// NON-strict verdict used by the non-wait submit render, where a 202
// Provisioning is a SUCCESSFUL async submit (exit 0).
func releaseExit(state, releaseID string) error {
	switch state {
	case "Partial", "Failed":
		return clicore.NewError(fmt.Sprintf("release %s ended %s — see the per-project results", releaseID, state), clicore.ExitAPI)
	default:
		return nil
	}
}

// releaseExitStrict is the terminal verdict: only a converged Ready release is
// exit 0. Partial/Failed exit non-zero (same as releaseExit); a NON-terminal
// result (Provisioning, or a blank/unknown state) is ALSO non-zero, so a caller
// that explicitly asked for the terminal outcome — `deploy status <id>` or a
// timed-out `--wait` — never reads an in-flight, unconverged release as green.
func releaseExitStrict(state, releaseID string) error {
	switch state {
	case "Ready":
		return nil
	case "Partial", "Failed":
		return clicore.NewError(fmt.Sprintf("release %s ended %s — see the per-project results", releaseID, state), clicore.ExitAPI)
	default:
		return clicore.NewError(
			fmt.Sprintf("release %s is still provisioning (not terminal) — re-check with 'putnami cloud deploy status %s'", releaseID, releaseID),
			clicore.ExitAPI)
	}
}

// deployReleaseMessage formats the single-project human line shown when --json
// is off. Mirrors deployMessage but includes the release_id so operators can
// correlate against the deployments table without grepping the JSON.
func deployReleaseMessage(workspaceID, app, environment, state, releaseID string, out map[string]any) string {
	switch state {
	case "Ready":
		if clicore.ValueString(out, "action") == "skip" {
			// Digest travel: nothing rolled — the existing revision already serves
			// this content. Say so rather than implying a fresh deploy.
			return fmt.Sprintf("Unchanged: %s/%s on workspace %s already serves %s (revision %s; release %s).",
				app, environment, workspaceID,
				clicore.ValueString(out, "url"), clicore.ValueString(out, "revision"), releaseID)
		}
		return fmt.Sprintf("Deployed %s/%s for workspace %s — %s (revision %s; release %s).",
			app, environment, workspaceID,
			clicore.ValueString(out, "url"), clicore.ValueString(out, "revision"), releaseID)
	case "Provisioning":
		// Accepted, NOT applied: the control plane still has to clear the migrate
		// gate, provision, and promote server-side — any of which can fail with no
		// revision rolled while the CLI has already exited 0 (a successful async
		// submit). Label it so a non-wait deploy is never misread as a completed
		// rollout; the follow-up block names the confirm command.
		return fmt.Sprintf("Accepted (not yet applied): %s/%s on workspace %s (release %s) — still provisioning server-side; the release may still fail with no revision rolled.",
			app, environment, workspaceID, releaseID)
	default:
		return fmt.Sprintf("Deploy completed for %s/%s on workspace %s (state=%s; release %s).",
			app, environment, workspaceID, state, releaseID)
	}
}
