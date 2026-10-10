package runtimecli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// DeployDefaultEnvironment matches the secrets/publish-config commands.
const DeployDefaultEnvironment = "prod"

// deployPollTimeout caps the total time the CLI keeps polling
// /v1/workspaces/{workspace}/deploy on a 202 (server-side provisioning).
// Matches the server's 90s poll deadline plus headroom for retries.
const deployPollTimeout = 5 * time.Minute

// DeployPollInterval is the gap between deploy POST retries — both the
// readiness re-poll after a 202 (state=Provisioning) and the compatibility
// back-off for older control planes that returned 409 deploy_in_progress. A var
// so tests can shrink it; production keeps the 10s cadence.
var DeployPollInterval = 10 * time.Second

// Deploy backs `putnami cloud deploy`. It has two subcommands: `status
// <release-id>` re-reads a past release, and `publish-v2 --request-file <path>`
// submits an accepted release-set selection through the continuous deploy. A
// rollback is a publish-v2 request that names an older release set.
func Deploy(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	switch clicore.FirstPositional(args) {
	case "status":
		return DeployStatus(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	case "publish-v2":
		return DeployPublishV2(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	}
	return clicore.NewError(deployUsage, clicore.ExitUsage)
}

// deployUsage names the two deploy subcommands that remain.
const deployUsage = "usage: putnami cloud deploy status <release-id> | putnami cloud deploy publish-v2 --request-file <path>"

// DeployStatus backs `putnami cloud deploy status <release-id>`: it re-fetches a
// past release's per-project detail from the control plane and prints it as a
// status node: the release, then one child per workload, with the
// requested, serving and held counts and the total wall clock as metrics. It
// exits by the rule every status command shares: 1 when the release failed or
// ended Partial, 0 otherwise, and with --strict 1 unless it is Ready, so a CI
// gate never reads an unconverged async release as green. A release set id
// (rs_…) and a missing id are usage errors that say where release ids are
// printed. It reuses the deploy helpers (newDeployCtx, deployStatusRequest)
// rather than hand-rolling HTTP, inheriting the one-shot 401 re-mint.
func DeployStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	releaseID, err := deployStatusReleaseID(args)
	if err != nil {
		return err
	}

	ctx, err := newDeployCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}

	reqCtx, stop := runtimeCommandContext(ioctx)
	defer stop()

	resp, status, err := deployStatusFetch(reqCtx, ctx, releaseID)
	if err != nil {
		// deployStatusFetch surfaces a "deploy status: <message>" error with
		// ExitAPI; re-tag a 401 as ExitAuth so an auth failure reads distinctly.
		if status == http.StatusUnauthorized {
			return clicore.NewError(err.Error(), clicore.ExitAuth)
		}
		return err
	}
	return clicore.WriteStatus(params, ioctx, DeployStatusNodeFrom(releaseID, resp))
}

// deployCtx is the resolved per-invocation context for the deploy commands. It
// embeds the shared *clicore.WorkspaceContext (workspace id, control-plane URL,
// bearer, IO, one-shot re-mint through clicore.CallWithSession) and adds the
// deploy-only fields: the environment and the client-side release timings.
type deployCtx struct {
	*clicore.WorkspaceContext
	environment string
	// localTaskGraphProjectCount is the number of projects the request file
	// names. The control plane reports its own cloud workload count beside it.
	localTaskGraphProjectCount int
	// The deploy command owns these client observations. The server status
	// adds durable queue/worker timings; these fill submit and wall-clock
	// timing without claiming visibility into upstream build/publish.
	releaseWallStarted time.Time
	releaseSubmitMS    *int64
}

func newDeployCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*deployCtx, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	environment := clicore.FirstString(clicore.StringParam(params, "env", "environment"), clicore.StringValue(link["environment"]), DeployDefaultEnvironment)
	wctx, err := clicore.ResolveWorkspaceContextFromLink(params, link, env, ioctx)
	if err != nil {
		return nil, err
	}
	return &deployCtx{WorkspaceContext: wctx, environment: environment}, nil
}

// deployRequest POSTs the deploy intent and returns the parsed body
// alongside the raw status code so the caller can distinguish
// 200 (Ready) from 202 (Provisioning).
//
// The call goes through control-api's generated client (deploy_control.go).
// A refusal prints the control plane's message, else the status line; the
// aggregated preflight rejections replace it, one line per project, and a
// Google Cloud refusal body is appended. control-api declares those two as
// error details, with the 409 retry flag; deployTransport gives the code-less
// answers of older control planes the same shape.
//
// reqCtx wires Ctrl-C / SIGTERM cancellation through the HTTP client —
// without this, a hangup during the POST leaves the connection alive
// until the OS reaps the process at exit.
func deployRequest(reqCtx context.Context, ctx *deployCtx, body map[string]any) (*deployResponse, int, error) {
	target := ctx.WorkspaceURL("/deploy")
	request, err := deployPostBody(body)
	if err != nil {
		return nil, 0, err
	}
	var reply *controlapiclient.DeployPostResponse
	status, exchange, err := deployCall(reqCtx, ctx, func(callCtx context.Context, control *controlapiclient.ControlClient) (int, error) {
		result, err := control.CreateV1WorkspacesDeploy(callCtx, controlapiclient.CreateV1WorkspacesDeployInput{
			Path: controlapiclient.CreateV1WorkspacesDeployPath{Workspace: ctx.WorkspaceID},
			Body: request,
		})
		if err != nil {
			return 0, err
		}
		reply = result.Body
		return result.Status, nil
	})
	if err != nil {
		if status := clicore.ServiceStatus(err); status != 0 {
			return nil, status, deployRefusal(err, status)
		}
		return nil, 0, deployCallFailure(reqCtx, target, exchange, err)
	}
	return deployResponseFrom(reply), status, nil
}

// deployRefusal renders the control plane's refusal of a deploy submit as
// "deploy: <message>": ExitAuth on 401, ExitAPI otherwise, and a
// conflictError the submit loop retries when the 409 says the same request may
// be sent again.
func deployRefusal(err error, status int) error {
	var gcpBody string
	var cloudFailure *controlapiclient.CreateV1WorkspacesDeployControlCloudFailureError
	if errors.As(err, &cloudFailure) && cloudFailure.Payload != nil {
		gcpBody = clicore.Deref(cloudFailure.Payload.GcpResponseBody)
	}
	message := deployRefusalMessage(err, status, gcpBody)
	var rejected *controlapiclient.CreateV1WorkspacesDeployControlDeployRejectedError
	if errors.As(err, &rejected) && rejected.Payload != nil {
		if summary, ok := deployRejectionSummary(clicore.Deref(rejected.Payload.Rejections)); ok {
			message = summary
		}
	}
	code := clicore.ExitAPI
	if status == http.StatusUnauthorized {
		code = clicore.ExitAuth
	}
	deployErr := clicore.NewError("deploy: "+message, code)
	var conflict *controlapiclient.CreateV1WorkspacesDeployControlDeployReleaseConflictError
	if status == http.StatusConflict && errors.As(err, &conflict) && conflict.Payload != nil && clicore.Deref(conflict.Payload.Retryable) {
		return conflictError{err: deployErr}
	}
	return deployErr
}

// deployRejectionSummary renders a control plane's aggregated deploy-preflight
// rejections as one line per rejection, naming each project, under a counted
// header. ok=false when the list is empty or a rejection names no error, so
// the caller keeps the refusal's single message.
func deployRejectionSummary(rejections []controlapiclient.RejectionDetail) (string, bool) {
	if len(rejections) == 0 {
		return "", false
	}
	lines := make([]string, 0, len(rejections))
	for _, rejection := range rejections {
		message := clicore.Deref(rejection.Error)
		if message == "" {
			return "", false
		}
		if project := clicore.Deref(rejection.Project); project != "" {
			lines = append(lines, fmt.Sprintf("  - %s: %s", project, message))
		} else {
			lines = append(lines, "  - "+message)
		}
	}
	return fmt.Sprintf("preflight rejected %d problem(s):\n%s", len(lines), strings.Join(lines, "\n")), true
}

// deployStatusRequest GETs the async deploy status for a release id — the
// operation id the POST returns when the control plane runs provisioning in a
// detached Job. The --wait loop polls this instead of re-POSTing the payload:
// re-POSTing would re-drive a synchronous deploy, but an async deploy is already
// being driven by its Job, so the client only reads state.
//
// The poll reads the members a status shares with a submit reply; it uses
// state/projects/async only. The status view (deployStatusFetch) keeps the
// fuller status with migrations.
func deployStatusRequest(reqCtx context.Context, ctx *deployCtx, releaseID string) (*deployResponse, int, error) {
	reply, status, err := deployStatusRead(reqCtx, ctx, releaseID)
	if err != nil {
		return nil, status, err
	}
	return deployResponseFromStatus(reply), status, nil
}

// deployStatusFetch is the typed GET .../deploy/{id} read backing `deploy
// status`. It reads the reply into deployStatusResponse, which keeps the
// member order and the members `deploy status --output` has always printed.
func deployStatusFetch(reqCtx context.Context, ctx *deployCtx, releaseID string) (*deployStatusResponse, int, error) {
	reply, status, err := deployStatusRead(reqCtx, ctx, releaseID)
	if err != nil {
		return nil, status, err
	}
	return deployStatusFrom(reply), status, nil
}

// deployStatusRead reads one release's status through control-api's generated
// client. A refusal reads "deploy status: <message>" with ExitAPI, and the
// status it carried.
func deployStatusRead(reqCtx context.Context, ctx *deployCtx, releaseID string) (*controlapiclient.DeployStatusResponse, int, error) {
	target := ctx.WorkspaceURL("/deploy/" + url.PathEscape(releaseID))
	var reply *controlapiclient.DeployStatusResponse
	status, exchange, err := deployCall(reqCtx, ctx, func(callCtx context.Context, control *controlapiclient.ControlClient) (int, error) {
		result, err := control.GetV1WorkspacesWorkspaceDeployRelease(callCtx, controlapiclient.GetV1WorkspacesWorkspaceDeployReleaseInput{
			Path: controlapiclient.GetV1WorkspacesWorkspaceDeployReleasePath{Workspace: ctx.WorkspaceID, Release: releaseID},
		})
		if err != nil {
			return 0, err
		}
		reply = result
		return http.StatusOK, nil
	})
	if err != nil {
		if status := clicore.ServiceStatus(err); status != 0 {
			var gcpBody string
			var cloudFailure *controlapiclient.GetV1WorkspacesWorkspaceDeployReleaseControlCloudFailureError
			if errors.As(err, &cloudFailure) && cloudFailure.Payload != nil {
				gcpBody = clicore.Deref(cloudFailure.Payload.GcpResponseBody)
			}
			return nil, status, clicore.NewError("deploy status: "+deployRefusalMessage(err, status, gcpBody), clicore.ExitAPI)
		}
		return nil, 0, deployCallFailure(reqCtx, target, exchange, err)
	}
	return reply, status, nil
}

// releaseTableTyped renders the multi-project human summary for `deploy status`
// from the typed release projects. Same layout as releaseTable's map-based twin,
// which the write-side render still uses pending the S6 write-side cutover.
func releaseTableTyped(ctx *deployCtx, state, releaseID string, projects []releaseProject) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Release %s for workspace %s (%s) — %s",
		releaseID, ctx.WorkspaceID, ctx.environment, state)
	for _, p := range projects {
		// A terminal policy hold needs to be visible in human output even when the
		// service still has a URL. The explicit reason explains why a Ready
		// candidate was not reported as fully deployed.
		detail := clicore.FirstString(p.HoldReason, p.Detail, p.URL)
		fmt.Fprintf(&b, "\n  %-32s %-12s %-5s %s", p.Name, p.Status, p.Action, detail)
		if observation := projectServingObservationHuman(p); observation != "" {
			fmt.Fprintf(&b, "\n    %s", observation)
		}
	}
	return b.String()
}

// projectServingObservationHuman renders the per-workload facts that make a
// completed release auditable: immutable image, candidate, readiness, serving
// revision, candidate traffic, and an explicit hold reason. Fields remain
// absent for historical releases that predate the durable observation keys.
func projectServingObservationHuman(project releaseProject) string {
	parts := make([]string, 0, 6)
	if project.ImageDigest != "" {
		parts = append(parts, "image "+project.ImageDigest)
	}
	if project.CandidateRevision != "" {
		parts = append(parts, "candidate "+project.CandidateRevision)
	}
	if project.Ready != nil {
		parts = append(parts, fmt.Sprintf("Ready=%t", *project.Ready))
	}
	if project.ServingRevision != "" {
		parts = append(parts, "serving "+project.ServingRevision)
	}
	if project.TrafficPercent != nil {
		parts = append(parts, fmt.Sprintf("traffic %d%%", *project.TrafficPercent))
	}
	if project.HoldReason != "" {
		parts = append(parts, "hold: "+project.HoldReason)
	}
	return strings.Join(parts, " · ")
}

func deployTransportError(target string, err error) error {
	// Tag transport errors with a sentinel so the wait loop can retry them.
	// ExitAPI surface stays the same for end-user messages; the sentinel is
	// checked via errors.As.
	return transportError{err: clicore.NewError(fmt.Sprintf("request failed for %s: %s", target, err.Error()), clicore.ExitAPI)}
}

// transportError wraps an error that originated from the HTTP client
// (DNS, TCP, TLS, connection reset) rather than from a server-side
// response body. The deploy wait loop retries these until the user's
// deadline; server-reported errors (4xx/5xx) exit immediately.
type transportError struct{ err error }

func (e transportError) Error() string { return e.err.Error() }
func (e transportError) Unwrap() error { return e.err }

// isTransientDeployError reports whether err is a transport-layer
// failure we should retry during the deploy wait loop. The conservative
// definition: anything wrapped by transportError or matching the
// net-package's Temporary/Timeout interfaces. Server-reported HTTP
// errors (which come through as a plain cliError with the body
// surface) are intentionally NOT retried — those represent the server
// having decided.
func isTransientDeployError(err error) bool {
	if err == nil {
		return false
	}
	var te transportError
	if errors.As(err, &te) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// conflictError wraps a retryable "deploy in progress" rejection from older
// control planes (HTTP 409 deploy_in_progress). It is not a deploy failure —
// the submit loop backs off and re-POSTs until the in-flight release settles or
// the deploy timeout fires.
type conflictError struct{ err error }

func (e conflictError) Error() string { return e.err.Error() }
func (e conflictError) Unwrap() error { return e.err }

// isRetryableConflict reports whether err is an in-progress deploy conflict the
// submit loop should retry rather than surface as a failure.
func isRetryableConflict(err error) bool {
	var ce conflictError
	return errors.As(err, &ce)
}

// submitRelease POSTs the release and keeps compatibility with older control
// planes that answered 409 deploy_in_progress by backing off and re-POSTing.
// Current control planes accept concurrent submissions and queue server-side.
// Transport blips and other server errors keep their existing semantics (the
// former retried by the wait loop, the latter fatal).
func submitRelease(reqCtx context.Context, ctx *deployCtx, body map[string]any, deadline time.Time, ioctx clicore.IO) (*deployResponse, int, error) {
	for {
		resp, status, err := deployRequest(reqCtx, ctx, body)
		if err == nil {
			return resp, status, nil
		}
		if !isRetryableConflict(err) || !ioctx.Now().Before(deadline) {
			return nil, status, err
		}
		if ioctx.Stderr != nil {
			ioctx.Stderr("another deploy is in progress for this workspace; retrying soon")
		}
		if sErr := sleepCtx(reqCtx, DeployPollInterval); sErr != nil {
			return nil, 0, clicore.NewError("deploy wait canceled by user", clicore.ExitUsage)
		}
	}
}

// sleepCtx blocks for d or until ctx is canceled, whichever comes
// first. Returns ctx.Err() on cancellation so callers can distinguish
// timeout from interrupt.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func parseDeployTimeout(params map[string]any) time.Duration {
	if raw := clicore.StringParam(params, "timeout"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			return d
		}
	}
	return deployPollTimeout
}

// deployProgress is the dedupe snapshot the --wait loop threads across polls:
// the last-emitted overall state plus the last-emitted "status/action" per
// project. A poll whose state and every project row match the snapshot emits
// nothing, so the 10s cadence stays silent while nothing changes.
type deployProgress struct {
	state     string
	execution string
	projects  map[string]string
}

// emitDeployProgress prints a human progress line for each transition observed
// in resp relative to last, sending them to ioctx.Stderr (stdout stays reserved
// for the terminal WriteResult). It returns the updated snapshot.
//
// Two guards keep the machine contract intact: nothing is emitted in
// structured mode (--output=json|jsonl / --json), and every line goes to
// stderr, so `--output=jsonl` remains exactly one terminal object on stdout.
func emitDeployProgress(params map[string]any, ioctx clicore.IO, last deployProgress, resp *deployResponse) deployProgress {
	next := deployProgress{state: last.state, execution: last.execution, projects: last.projects}
	if next.projects == nil {
		next.projects = map[string]string{}
	}
	// Never interleave progress with a structured object, and never emit when
	// there is no stderr sink to write to.
	if clicore.StructuredOutput(params) || ioctx.Stderr == nil {
		// Still fold the observation into the snapshot so a later human-mode
		// emission (should the gate flip) doesn't replay stale transitions.
		next.state = resp.State
		next.execution = releaseExecutionProgressKey(resp)
		for _, p := range resp.Projects {
			if p.Name != "" {
				next.projects[p.Name] = projectProgressKey(p)
			}
		}
		return next
	}

	if state := resp.State; state != "" && state != next.state {
		ioctx.Stderr(fmt.Sprintf("deploy: %s", strings.ToLower(state)))
		next.state = state
	}
	if execution := releaseExecutionProgressKey(resp); execution != "" && execution != next.execution {
		ioctx.Stderr(releaseExecutionProgressLine(resp))
		next.execution = execution
	}
	for _, p := range resp.Projects {
		if p.Name == "" {
			continue
		}
		key := projectProgressKey(p)
		if next.projects[p.Name] == key {
			continue
		}
		ioctx.Stderr(fmt.Sprintf("  %s: %s", p.Name, key))
		next.projects[p.Name] = key
	}
	return next
}

// releaseExecutionProgressKey is the stable observation key for the latest
// durable service-worker attempt. An attempt retry changes the number; a worker
// revision or lifecycle transition changes the key too, so `--wait` visibly
// follows the worker rather than printing the same opaque Provisioning line.
func releaseExecutionProgressKey(resp *deployResponse) string {
	if resp == nil || resp.ExecutionBackend == "" {
		return ""
	}
	return strings.Join([]string{
		resp.ExecutionBackend,
		resp.WorkerService,
		resp.WorkerRevision,
		fmt.Sprintf("%d", resp.AttemptNumber),
		resp.AttemptStatus,
	}, "\x00")
}

func releaseExecutionProgressLine(resp *deployResponse) string {
	parts := []string{resp.ExecutionBackend}
	if resp.WorkerService != "" {
		parts = append(parts, resp.WorkerService)
	}
	if resp.WorkerRevision != "" {
		parts = append(parts, "revision "+resp.WorkerRevision)
	}
	if resp.AttemptNumber > 0 {
		parts = append(parts, fmt.Sprintf("attempt %d", resp.AttemptNumber))
	}
	if resp.AttemptStatus != "" {
		parts = append(parts, resp.AttemptStatus)
	}
	return "worker: " + strings.Join(parts, " · ")
}

// projectProgressKey is the per-project dedupe token: its status, suffixed with
// the action (skip|roll) once the control plane has classified it, so a
// pending→ready and a roll→skip both register as a transition.
func projectProgressKey(project releaseProject) string {
	if project.Action != "" {
		return project.Status + " (" + project.Action + ")"
	}
	return project.Status
}
