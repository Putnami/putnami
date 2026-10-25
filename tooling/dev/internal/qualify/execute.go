package qualify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	protocolplatform "go.putnami.dev/protocol/platform"
	qualifyproto "go.putnami.dev/protocol/qualify"
)

// Execution defaults.
const (
	DefaultReadyTimeout   = 60 * time.Second
	DefaultRequestTimeout = 5 * time.Second
	DefaultPollInterval   = 250 * time.Millisecond
	// teardownTimeout bounds Close, which runs even after cancellation.
	teardownTimeout = 60 * time.Second
)

// Options tunes one execution.
type Options struct {
	// PlatformPrefix is where /readyz and /version are mounted; "" is the root.
	PlatformPrefix string
	// ReadinessRoute reports that the workload serves GET
	// <PlatformPrefix>/readyz (Platform.ReadinessRoute). Readiness polls it
	// then, and always on a target that is not a StartupTarget; otherwise
	// readiness is the target's completed-startup report.
	ReadinessRoute bool
	// ReadyTimeout bounds the readiness phase; zero means DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// RequestTimeout bounds each request; zero means DefaultRequestTimeout.
	RequestTimeout time.Duration
	// PollInterval spaces readiness attempts; zero means DefaultPollInterval.
	PollInterval time.Duration
	// Unsupported explains an empty contract; Derive returns it.
	Unsupported *Unsupported
	// Now stamps StartedAt and FinishedAt; nil means time.Now.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = DefaultReadyTimeout
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// execution carries one run's state between its phases.
type execution struct {
	opts     Options
	contract *qualifyproto.Contract
	verdict  *qualifyproto.Verdict
	client   *http.Client
	base     *url.URL
}

// Execute runs contract against target and reduces the run to one verdict.
//
// Phases run in order — resolve-target, readiness, version-binding, smoke,
// teardown — and each runs only when every earlier phase passed; the rest are
// not_run. Teardown runs whenever resolve-target was reached, so a target that
// failed or was canceled while opening is still closed. An empty contract opens
// nothing and is unsupported. Cancellation of ctx makes the active phase
// canceled, and the verdict state is the first phase state that is neither
// passed nor not_run.
func Execute(ctx context.Context, contract *qualifyproto.Contract, target Target, opts Options) *qualifyproto.Verdict {
	opts = opts.withDefaults()
	run := &execution{
		opts:     opts,
		contract: contract,
		client:   newClient(opts.RequestTimeout),
		verdict: &qualifyproto.Verdict{
			ProtocolVersion: qualifyproto.ProtocolVersion,
			Project:         contract.Project,
			Contract: qualifyproto.ContractRef{
				Digest:      contract.Digest,
				Requests:    len(contract.Requests),
				DerivedFrom: append([]qualifyproto.Source{}, contract.DerivedFrom...),
			},
			Phases:    make([]qualifyproto.Phase, 0, len(qualifyproto.PhaseNames)),
			Requests:  make([]qualifyproto.RequestResult, 0, len(contract.Requests)),
			StartedAt: stamp(opts.Now()),
		},
	}
	defer run.client.CloseIdleConnections()
	for _, name := range qualifyproto.PhaseNames {
		run.verdict.Phases = append(run.verdict.Phases, qualifyproto.Phase{Name: name, State: qualifyproto.StateNotRun})
	}
	for _, request := range contract.Requests {
		run.verdict.Requests = append(run.verdict.Requests, qualifyproto.RequestResult{ID: request.ID, State: qualifyproto.StateNotRun})
	}
	run.verdict.Target, run.verdict.Binding = target.Describe()

	if len(contract.Requests) == 0 {
		run.unsupported()
		return run.finish()
	}

	if run.phase(qualifyproto.PhaseResolveTarget, func(p *qualifyproto.Phase) { run.resolve(ctx, target, p) }) {
		if run.phase(qualifyproto.PhaseReadiness, func(p *qualifyproto.Phase) { run.readiness(ctx, target, p) }) &&
			run.phase(qualifyproto.PhaseVersionBinding, func(p *qualifyproto.Phase) { run.versionBinding(ctx, target, p) }) {
			run.phase(qualifyproto.PhaseSmoke, func(p *qualifyproto.Phase) { run.smoke(ctx, p) })
		}
	}
	// Release whatever Open may hold, whether it succeeded, failed, or never ran.
	run.phase(qualifyproto.PhaseTeardown, func(p *qualifyproto.Phase) { run.teardown(ctx, target, p) })
	return run.finish()
}

// phase times body against the named phase and reports whether it passed.
func (r *execution) phase(name string, body func(*qualifyproto.Phase)) bool {
	phase := r.phaseNamed(name)
	started := time.Now()
	body(phase)
	phase.DurationMs = time.Since(started).Milliseconds()
	return phase.State.IsPass()
}

func (r *execution) phaseNamed(name string) *qualifyproto.Phase {
	for index := range r.verdict.Phases {
		if r.verdict.Phases[index].Name == name {
			return &r.verdict.Phases[index]
		}
	}
	panic("qualify: unknown phase " + name)
}

func (r *execution) unsupported() {
	smoke := r.phaseNamed(qualifyproto.PhaseSmoke)
	smoke.State = qualifyproto.StateUnsupported
	explanation := r.opts.Unsupported
	if explanation == nil {
		explanation = &Unsupported{Reason: ReasonNoDerivableRequest, Remedy: "the contract holds no request, so nothing proves the workload serves one"}
	}
	smoke.Diagnostics = []diag.Diagnostic{explanation.Diagnostic()}
}

func (r *execution) finish() *qualifyproto.Verdict {
	r.verdict.State = ReduceState(r.verdict.Phases)
	if r.verdict.State.IsPass() && len(r.verdict.Requests) == 0 {
		r.verdict.State = qualifyproto.StateUnsupported
	}
	r.verdict.FinishedAt = stamp(r.opts.Now())
	return r.verdict
}

// ReduceState is the verdict rule: the first phase state that is neither passed
// nor not_run. Phases that all passed give passed; a phase set holding a
// not_run and nothing worse gives not_run, never passed.
func ReduceState(phases []qualifyproto.Phase) qualifyproto.State {
	if len(phases) == 0 {
		return qualifyproto.StateNotRun
	}
	notRun := false
	for _, phase := range phases {
		switch phase.State {
		case qualifyproto.StatePassed:
		case qualifyproto.StateNotRun:
			notRun = true
		default:
			return phase.State
		}
	}
	if notRun {
		return qualifyproto.StateNotRun
	}
	return qualifyproto.StatePassed
}

func (r *execution) resolve(ctx context.Context, target Target, phase *qualifyproto.Phase) {
	if interrupted(ctx, phase) {
		return
	}
	baseURL, binding, err := target.Open(ctx)
	r.verdict.Target, _ = target.Describe()
	if err != nil {
		if interrupted(ctx, phase) {
			return
		}
		var targetErr *TargetError
		if errors.As(err, &targetErr) {
			fail(phase, targetErr.State, targetErr.Code, "%v", targetErr.Err)
			return
		}
		fail(phase, qualifyproto.StateTargetUnreachable, qualifyproto.PhaseCodeTargetUnreachable, "%v", err)
		return
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		fail(phase, qualifyproto.StateTargetUnreachable, qualifyproto.PhaseCodeTargetUnreachable, "target base URL: %v", err)
		return
	}
	r.base = base
	r.verdict.Binding = binding
	phase.State = qualifyproto.StatePassed
}

// endpoint is the absolute URL of routePath under the platform prefix when
// platform is true. Route paths are already percent-encoded, so the URL is
// assembled as text rather than through url.URL.Path, which would encode them
// twice.
func (r *execution) endpoint(routePath string, platform bool) string {
	prefix := ""
	if platform {
		prefix = r.opts.PlatformPrefix
	}
	return r.base.Scheme + "://" + r.base.Host + joinURLPath(r.base.EscapedPath(), prefix, routePath)
}

// readiness waits, within ReadyTimeout, until the target can be smoked. A
// workload that serves a readiness route proves it there. A StartupTarget whose
// workload declares none proves it with its application's completed-startup
// report instead: an answer from any other route proves only that a listener is
// bound (an auth denial or a 404 answers long before every starter and start
// hook returned), so it is never taken for readiness.
func (r *execution) readiness(ctx context.Context, target Target, phase *qualifyproto.Phase) {
	if startup, ok := target.(StartupTarget); ok && !r.opts.ReadinessRoute {
		r.awaitStartup(ctx, startup, phase)
		return
	}
	r.pollReadiness(ctx, phase)
}

// awaitStartup is the readiness of a StartupTarget whose workload declares no
// readiness route.
func (r *execution) awaitStartup(ctx context.Context, target StartupTarget, phase *qualifyproto.Phase) {
	if interrupted(ctx, phase) {
		return
	}
	startupCtx, cancel := context.WithTimeout(ctx, r.opts.ReadyTimeout)
	defer cancel()
	err := target.AwaitStartup(startupCtx)
	if err == nil {
		phase.State = qualifyproto.StatePassed
		return
	}
	if interrupted(ctx, phase) {
		return
	}
	var targetErr *TargetError
	switch {
	case errors.As(err, &targetErr):
		fail(phase, targetErr.State, targetErr.Code, "%v", targetErr.Err)
	case startupCtx.Err() != nil:
		fail(phase, qualifyproto.StateTimedOut, qualifyproto.PhaseCodeNotReady,
			"the application never reported completed startup (a typed ready event with target workload) within %s; the route inventory declares no GET %s to poll instead",
			r.opts.ReadyTimeout, protocolplatform.JoinPrefix(r.opts.PlatformPrefix, protocolplatform.PathReadyz))
	default:
		fail(phase, qualifyproto.StateFailed, qualifyproto.PhaseCodeNotReady,
			"waiting for the application to report completed startup failed: %v", err)
	}
}

// pollReadiness polls GET <prefix>/readyz until it answers 200 with status ok.
func (r *execution) pollReadiness(ctx context.Context, phase *qualifyproto.Phase) {
	readyCtx, cancel := context.WithTimeout(ctx, r.opts.ReadyTimeout)
	defer cancel()
	endpoint := r.endpoint(protocolplatform.PathReadyz, true)
	last := "no answer yet"
	for {
		ready, observation := r.probeReady(readyCtx, endpoint)
		if ready {
			phase.State = qualifyproto.StatePassed
			return
		}
		// A probe cut short by the deadline observed the deadline, not the target;
		// keep the last answer the target actually gave.
		if readyCtx.Err() == nil {
			last = observation
		}
		select {
		case <-readyCtx.Done():
		case <-time.After(r.opts.PollInterval):
			continue
		}
		if interrupted(ctx, phase) {
			return
		}
		fail(phase, qualifyproto.StateTimedOut, qualifyproto.PhaseCodeNotReady,
			"GET %s did not answer 200 with status ok within %s; last: %s", endpoint, r.opts.ReadyTimeout, last)
		return
	}
}

func (r *execution) probeReady(ctx context.Context, endpoint string) (bool, string) {
	request, err := newRequest(ctx, http.MethodGet, endpoint)
	if err != nil {
		return false, err.Error()
	}
	response, err := r.client.Do(request) //nolint:gosec // G704: the destination is the qualify target the user named; reaching it is the command's purpose, and redirects are never followed
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = drain(response) }()
	var envelope protocolplatform.Envelope
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(&envelope)
	if response.StatusCode == http.StatusOK && decodeErr == nil && envelope.Status == protocolplatform.StatusOK {
		return true, ""
	}
	return false, fmt.Sprintf("status %d, envelope status %q", response.StatusCode, envelope.Status)
}

func (r *execution) versionBinding(ctx context.Context, target Target, phase *qualifyproto.Phase) {
	binding := &r.verdict.Binding
	if binding.Kind != qualifyproto.BindingArtifact {
		r.treeBinding(ctx, target, phase)
		return
	}
	endpoint := r.endpoint(protocolplatform.PathVersion, true)
	request, err := newRequest(ctx, http.MethodGet, endpoint)
	if err != nil {
		fail(phase, qualifyproto.StateTargetUnreachable, qualifyproto.PhaseCodeTargetUnreachable, "%v", err)
		return
	}
	response, err := r.client.Do(request) //nolint:gosec // G704: the destination is the qualify target the user named; reaching it is the command's purpose, and redirects are never followed
	if err != nil {
		switch {
		case interrupted(ctx, phase):
		case isTimeout(err):
			fail(phase, qualifyproto.StateTimedOut, qualifyproto.PhaseCodeVersionMissing, "GET %s: %v", endpoint, err)
		default:
			fail(phase, qualifyproto.StateTargetUnreachable, qualifyproto.PhaseCodeTargetUnreachable, "GET %s: %v", endpoint, err)
		}
		return
	}
	defer func() { _ = drain(response) }()
	var info protocolplatform.VersionInfo
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(&info)
	if response.StatusCode != http.StatusOK || decodeErr != nil {
		fail(phase, qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMissing,
			"GET %s answered %d without a version document, so the deployed build is unknown", endpoint, response.StatusCode)
		return
	}
	binding.ObservedSHA = info.SHA
	binding.Version = info.Version
	switch {
	case info.SHA == "":
		fail(phase, qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMissing,
			"GET %s reports no sha, so the deployed build cannot be matched to %s", endpoint, binding.ExpectedSHA)
	case len(binding.ExpectedSHA) < minExpectSHA || !strings.HasPrefix(info.SHA, binding.ExpectedSHA):
		fail(phase, qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMismatch,
			"GET %s reports sha %s, which does not start with the expected %s", endpoint, info.SHA, binding.ExpectedSHA)
	default:
		phase.State = qualifyproto.StatePassed
	}
}

// treeBinding proves a tree binding: once the workload answers, the worktree
// must still be the one the binding named when the target opened. Between the
// two readings the serve pipelines were prepared and every member was built and
// started, so an equal fingerprint is what lets the verdict say the workload it
// smoked was built from that exact tree. A changed or unreadable tree is
// digest_mismatch, and no smoke request is sent.
func (r *execution) treeBinding(ctx context.Context, target Target, phase *qualifyproto.Phase) {
	if interrupted(ctx, phase) {
		return
	}
	promised := r.verdict.Binding
	worktree, ok := target.(WorktreeTarget)
	if !ok {
		fail(phase, qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMissing,
			"the target cannot re-read the worktree it serves, so the verdict cannot name the tree it proved")
		return
	}
	observed, err := worktree.Worktree(ctx)
	switch {
	case err != nil:
		fail(phase, qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMissing,
			"re-reading the worktree failed, so the verdict cannot name the tree it proved: %v", err)
	case observed.Fingerprint != promised.Fingerprint || observed.HeadSHA != promised.HeadSHA:
		fail(phase, qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMismatch,
			"the worktree changed while the workload was composed (tree %s at %s when qualify started, tree %s at %s once it was ready), so the verdict cannot name the tree it served",
			shortDigest(promised.Fingerprint), shortDigest(promised.HeadSHA), shortDigest(observed.Fingerprint), shortDigest(observed.HeadSHA))
	default:
		phase.State = qualifyproto.StatePassed
	}
}

func (r *execution) smoke(ctx context.Context, phase *qualifyproto.Phase) {
	failed := 0
	for index, request := range r.contract.Requests {
		result := &r.verdict.Requests[index]
		if interrupted(ctx, phase) {
			return
		}
		started := time.Now()
		r.send(ctx, request, result)
		result.DurationMs = time.Since(started).Milliseconds()
		if result.State == qualifyproto.StateCanceled || result.State == qualifyproto.StateTimedOut && ctx.Err() != nil {
			interrupted(ctx, phase)
			return
		}
		if !result.State.IsPass() {
			failed++
		}
	}
	if failed > 0 {
		fail(phase, qualifyproto.StateFailed, qualifyproto.PhaseCodeRequestFailed,
			"%d of %d requests did not pass", failed, len(r.contract.Requests))
		return
	}
	phase.State = qualifyproto.StatePassed
}

// send runs one request and records its outcome on result.
func (r *execution) send(ctx context.Context, request qualifyproto.Request, result *qualifyproto.RequestResult) {
	endpoint := r.endpoint(request.Path, false)
	httpRequest, err := newRequest(ctx, request.Method, endpoint)
	if err != nil {
		result.State, result.Reason = qualifyproto.StateFailed, err.Error()
		return
	}
	response, err := r.client.Do(httpRequest) //nolint:gosec // G704: the destination is the qualify target the user named; reaching it is the command's purpose, and redirects are never followed
	if err == nil {
		result.Status = response.StatusCode
		err = drain(response)
	}
	switch {
	case ctx.Err() != nil:
		result.State, result.Reason = ctxState(ctx), ctx.Err().Error()
	case err != nil && isTimeout(err):
		result.State, result.Reason = qualifyproto.StateTimedOut, fmt.Sprintf("no complete answer within %s", r.opts.RequestTimeout)
	case err != nil && result.Status == 0:
		result.State, result.Reason = qualifyproto.StateFailed, err.Error()
	case result.Status > request.MaxStatus:
		result.State, result.Reason = qualifyproto.StateFailed, fmt.Sprintf("status %d exceeds %d", result.Status, request.MaxStatus)
	default:
		result.State = qualifyproto.StatePassed
	}
}

func (r *execution) teardown(ctx context.Context, target Target, phase *qualifyproto.Phase) {
	teardownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	defer cancel()
	cleanup := target.Close(teardownCtx)
	if cleanup != nil {
		if cleanup.Leftovers == nil {
			cleanup.Leftovers = []string{}
		}
		r.verdict.Cleanup = cleanup
		if cleanup.State != qualifyproto.CleanupClean {
			fail(phase, qualifyproto.StateFailed, qualifyproto.PhaseCodeTeardownPartial,
				"teardown left %s behind", strings.Join(cleanup.Leftovers, ", "))
			return
		}
	}
	phase.State = qualifyproto.StatePassed
}

// interrupted marks phase canceled or timed_out when ctx is done.
func interrupted(ctx context.Context, phase *qualifyproto.Phase) bool {
	if ctx.Err() == nil {
		return false
	}
	fail(phase, ctxState(ctx), qualifyproto.PhaseCodeCanceled, "%v", ctx.Err())
	return true
}

func ctxState(ctx context.Context) qualifyproto.State {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return qualifyproto.StateTimedOut
	}
	return qualifyproto.StateCanceled
}

func fail(phase *qualifyproto.Phase, state qualifyproto.State, code, format string, args ...any) {
	phase.State = state
	phase.Diagnostics = append(phase.Diagnostics, diag.Errorf(code, "", format, args...))
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
