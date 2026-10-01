package qualify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	qualifyproto "go.putnami.dev/protocol/qualify"
)

// hangDetector bounds every wait in these tests. It detects a hang; it is never
// a latency assertion.
const hangDetector = 60 * time.Second

const deployedSHA = "3cc91b658a1e0f7d2c4b9e8f6a5d3c2b1a0f9e8d"

// deployment is an httptest workload serving the platform surface and a few
// business routes.
type deployment struct {
	sha          string
	versionCode  int
	notReadyFor  int32 // /readyz answers 503 this many times first
	readyCalls   atomic.Int32
	slowEntered  chan struct{}
	slowOnce     sync.Once
	mu           sync.Mutex
	requestPaths []string
}

func (d *deployment) record(r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requestPaths = append(d.requestPaths, r.Method+" "+r.URL.Path)
}

func (d *deployment) paths() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.requestPaths, ",")
}

func (d *deployment) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/base/":
		w.WriteHeader(http.StatusOK)
	case "/readyz", "/base/readyz":
		if d.readyCalls.Add(1) <= d.notReadyFor {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","checks":{"db":"ok"}}`))
	case "/version", "/base/version":
		if d.versionCode != 0 {
			w.WriteHeader(d.versionCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "example", "version": "1.2.3", "sha": d.sha})
	case "/items", "/whoami", "/base/items":
		d.record(r)
		w.WriteHeader(http.StatusOK)
	case "/private":
		d.record(r)
		w.WriteHeader(http.StatusUnauthorized)
	case "/boom":
		d.record(r)
		w.WriteHeader(http.StatusServiceUnavailable)
	case "/redirect":
		http.Redirect(w, r, "/boom", http.StatusFound)
	case "/slow":
		if d.slowEntered != nil {
			d.slowOnce.Do(func() { close(d.slowEntered) })
		}
		select {
		case <-r.Context().Done():
		case <-time.After(hangDetector):
		}
	default:
		http.NotFound(w, r)
	}
}

func newDeployment(t *testing.T, d *deployment) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(d)
	t.Cleanup(server.Close)
	return server
}

func contractFor(t *testing.T, project string, paths ...string) *qualifyproto.Contract {
	t.Helper()
	contract := &qualifyproto.Contract{
		ProtocolVersion: qualifyproto.ProtocolVersion,
		Project:         project,
		DerivedFrom: []qualifyproto.Source{{
			Kind: qualifyproto.SourceHTTPRoutes, Path: "app/schema/http-routes.json",
			Digest: "sha256:b71b8bcddd7e3f336d3abc1cf4cff56ed3a89536c44de8da65fccb01469681be",
		}},
		Requests: []qualifyproto.Request{},
	}
	for _, path := range paths {
		method, routePath, _ := strings.Cut(path, " ")
		contract.Requests = append(contract.Requests, qualifyproto.Request{
			ID: path, Method: method, Path: routePath, MaxStatus: qualifyproto.DefaultMaxStatus, Provenance: "typed-api",
		})
	}
	contract.Digest = qualifyproto.ContractDigest(contract.Requests)
	return contract
}

func urlTargetFor(t *testing.T, rawURL, sha string) Target {
	t.Helper()
	target, err := NewURLTarget(rawURL, sha)
	if err != nil {
		t.Fatalf("NewURLTarget: %v", err)
	}
	return target
}

func fastOptions() Options {
	return Options{PollInterval: 5 * time.Millisecond, ReadyTimeout: hangDetector, RequestTimeout: hangDetector}
}

// execute runs one contract with a hang detector and holds the producer to the
// protocol: every verdict the CLI emits must pass the strict parser.
func execute(t *testing.T, ctx context.Context, contract *qualifyproto.Contract, target Target, opts Options) *qualifyproto.Verdict {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, hangDetector)
	defer cancel()
	verdict := Execute(ctx, contract, target, opts)
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	if _, diags := qualifyproto.ParseAndValidateVerdict(data); diag.HasErrors(diags) {
		t.Fatalf("the verdict fails the protocol: %v\n%s", diags, data)
	}
	return verdict
}

func phaseStates(verdict *qualifyproto.Verdict) string {
	states := make([]string, 0, len(verdict.Phases))
	for _, phase := range verdict.Phases {
		states = append(states, phase.Name+"="+string(phase.State))
	}
	return strings.Join(states, " ")
}

func phaseCode(verdict *qualifyproto.Verdict, name string) string {
	for _, phase := range verdict.Phases {
		if phase.Name == name && len(phase.Diagnostics) > 0 {
			return phase.Diagnostics[0].Code
		}
	}
	return ""
}

func TestExecute_PassedAgainstHttptestServer(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "remote-verdict-binds-to-the-deployed-sha", "a-matching-sha-passes")
	d := &deployment{sha: deployedSHA, notReadyFor: 2}
	server := newDeployment(t, d)
	contract := contractFor(t, "/app", "GET /items", "HEAD /whoami", "GET /private", "GET /redirect")

	verdict := execute(t, context.Background(), contract, urlTargetFor(t, server.URL, deployedSHA[:9]), fastOptions())

	if verdict.State != qualifyproto.StatePassed {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if verdict.Target.Kind != qualifyproto.TargetURL || verdict.Target.URL != server.URL {
		t.Errorf("target = %+v", verdict.Target)
	}
	if b := verdict.Binding; b.Kind != qualifyproto.BindingArtifact || b.ExpectedSHA != deployedSHA[:9] || b.ObservedSHA != deployedSHA || b.Version != "1.2.3" {
		t.Errorf("binding = %+v", b)
	}
	if verdict.Contract.Digest != contract.Digest || verdict.Contract.Requests != 4 || verdict.Cleanup != nil {
		t.Errorf("contract ref %+v cleanup %+v", verdict.Contract, verdict.Cleanup)
	}
	wantStatus := map[string]int{"GET /items": 200, "HEAD /whoami": 200, "GET /private": 401, "GET /redirect": 302}
	for _, result := range verdict.Requests {
		if result.State != qualifyproto.StatePassed || result.Status != wantStatus[result.ID] {
			t.Errorf("request %+v, want passed with %d", result, wantStatus[result.ID])
		}
	}
	// The redirect is recorded, never followed off the contract.
	if d.paths() != "GET /items,HEAD /whoami,GET /private" {
		t.Errorf("requests reaching business routes = %v", d.paths())
	}
	if d.readyCalls.Load() < 3 {
		t.Errorf("readiness polled %d times, want it to wait through two 503s", d.readyCalls.Load())
	}

	// A base URL with a path and a platform prefix both join onto every request.
	prefixed := execute(t, context.Background(), contractFor(t, "/app", "GET /items"),
		urlTargetFor(t, server.URL+"/base", deployedSHA), Options{PollInterval: 5 * time.Millisecond, PlatformPrefix: "/"})
	if prefixed.State != qualifyproto.StatePassed || !strings.Contains(d.paths(), "GET /base/items") {
		t.Errorf("base path: state %s (%s), paths %v", prefixed.State, phaseStates(prefixed), d.paths())
	}
}

func TestExecute_DigestMismatchOnWrongSha(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "remote-verdict-binds-to-the-deployed-sha", "a-wrong-or-missing-sha-is-digest-mismatch")
	cases := map[string]struct {
		deployment *deployment
		expect     string
		code       string
	}{
		"wrong sha":         {&deployment{sha: deployedSHA}, "deadbeef0", qualifyproto.PhaseCodeVersionMismatch},
		"no sha reported":   {&deployment{}, deployedSHA[:7], qualifyproto.PhaseCodeVersionMissing},
		"no version route":  {&deployment{sha: deployedSHA, versionCode: http.StatusNotFound}, deployedSHA[:7], qualifyproto.PhaseCodeVersionMissing},
		"longer than build": {&deployment{sha: deployedSHA[:10]}, deployedSHA, qualifyproto.PhaseCodeVersionMismatch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := newDeployment(t, tc.deployment)
			verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"),
				urlTargetFor(t, server.URL, tc.expect), fastOptions())
			if verdict.State != qualifyproto.StateDigestMismatch {
				t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
			}
			if got := phaseStates(verdict); got != "resolve-target=passed readiness=passed version-binding=digest_mismatch smoke=not_run teardown=passed" {
				t.Errorf("phases = %s", got)
			}
			if code := phaseCode(verdict, qualifyproto.PhaseVersionBinding); code != tc.code {
				t.Errorf("code = %s, want %s", code, tc.code)
			}
			if verdict.Requests[0].State != qualifyproto.StateNotRun || tc.deployment.paths() != "" {
				t.Errorf("a stale deployment must not be smoked: %+v %v", verdict.Requests, tc.deployment.paths())
			}
		})
	}
}

func TestExecute_TargetUnreachableOnClosedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"),
		urlTargetFor(t, "http://"+address, deployedSHA[:7]), fastOptions())
	if verdict.State != qualifyproto.StateTargetUnreachable {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=target_unreachable readiness=not_run version-binding=not_run smoke=not_run teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	if code := phaseCode(verdict, qualifyproto.PhaseResolveTarget); code != qualifyproto.PhaseCodeTargetUnreachable {
		t.Errorf("code = %s", code)
	}
}

func TestExecute_ReadinessDeadlineIsTimedOut(t *testing.T) {
	server := newDeployment(t, &deployment{sha: deployedSHA, notReadyFor: 1 << 30})
	opts := fastOptions()
	opts.ReadyTimeout = 100 * time.Millisecond
	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), urlTargetFor(t, server.URL, deployedSHA[:7]), opts)
	if verdict.State != qualifyproto.StateTimedOut {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=passed readiness=timed_out version-binding=not_run smoke=not_run teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	phase := verdict.Phases[1]
	if phase.Diagnostics[0].Code != qualifyproto.PhaseCodeNotReady || !strings.Contains(phase.Diagnostics[0].Message, "status 503") {
		t.Errorf("diagnostic = %+v, want not_ready naming the last observation", phase.Diagnostics)
	}
}

func TestExecute_CanceledMidSmoke(t *testing.T) {
	d := &deployment{sha: deployedSHA, slowEntered: make(chan struct{})}
	server := newDeployment(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-d.slowEntered:
			cancel()
		case <-time.After(hangDetector):
		}
	}()
	verdict := execute(t, ctx, contractFor(t, "/app", "GET /items", "GET /slow", "GET /whoami"), urlTargetFor(t, server.URL, deployedSHA[:7]), fastOptions())
	if verdict.State != qualifyproto.StateCanceled {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=passed readiness=passed version-binding=passed smoke=canceled teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	states := []qualifyproto.State{verdict.Requests[0].State, verdict.Requests[1].State, verdict.Requests[2].State}
	if states[0] != qualifyproto.StatePassed || states[1] != qualifyproto.StateCanceled || states[2] != qualifyproto.StateNotRun {
		t.Errorf("request states = %v, want passed canceled not_run", states)
	}

	// Canceled before anything opened: resolve-target is canceled, the target is still closed.
	canceled, stop := context.WithCancel(context.Background())
	stop()
	fake := &fakeTarget{}
	early := execute(t, canceled, contractFor(t, "/app", "GET /items"), fake, fastOptions())
	if early.State != qualifyproto.StateCanceled || fake.opened.Load() != 0 || fake.closed.Load() != 1 {
		t.Errorf("state %s opened %d closed %d (%s)", early.State, fake.opened.Load(), fake.closed.Load(), phaseStates(early))
	}
}

func TestExecute_RequestOver499IsFailed(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "every-non-pass-state-exits-non-zero", "a-server-error-fails-the-smoke")
	server := newDeployment(t, &deployment{sha: deployedSHA})
	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /boom", "GET /items"), urlTargetFor(t, server.URL, deployedSHA[:7]), fastOptions())
	if verdict.State != qualifyproto.StateFailed {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	boom := verdict.Requests[0]
	if boom.State != qualifyproto.StateFailed || boom.Status != 503 || !strings.Contains(boom.Reason, "503 exceeds 499") {
		t.Errorf("boom = %+v", boom)
	}
	if verdict.Requests[1].State != qualifyproto.StatePassed {
		t.Errorf("a failing request must not stop the others: %+v", verdict.Requests[1])
	}
	if code := phaseCode(verdict, qualifyproto.PhaseSmoke); code != qualifyproto.PhaseCodeRequestFailed {
		t.Errorf("smoke code = %s", code)
	}

	// A request that exceeds its own deadline is timed_out and fails the verdict.
	opts := fastOptions()
	opts.RequestTimeout = 100 * time.Millisecond
	slow := execute(t, context.Background(), contractFor(t, "/app", "GET /slow"), urlTargetFor(t, server.URL, deployedSHA[:7]), opts)
	if slow.State != qualifyproto.StateFailed || slow.Requests[0].State != qualifyproto.StateTimedOut {
		t.Errorf("slow: state %s request %+v", slow.State, slow.Requests[0])
	}
}

// fakeTarget is a scripted Target for the rules that do not need HTTP.
type fakeTarget struct {
	openErr error
	binding qualifyproto.Binding
	cleanup *qualifyproto.Cleanup
	baseURL string
	opened  atomic.Int32
	closed  atomic.Int32
	// worktree is what Worktree observes; nil observes the promised binding.
	worktree    *qualifyproto.Binding
	worktreeErr error
}

func (f *fakeTarget) Worktree(context.Context) (qualifyproto.Binding, error) {
	if f.worktreeErr != nil {
		return qualifyproto.Binding{}, f.worktreeErr
	}
	if f.worktree != nil {
		return *f.worktree, nil
	}
	_, binding := f.Describe()
	return binding, nil
}

// plainTarget hides every method but Target's, like a target that cannot
// re-read a worktree.
type plainTarget struct{ Target }

func (f *fakeTarget) Describe() (qualifyproto.Target, qualifyproto.Binding) {
	binding := f.binding
	if binding.Kind == "" {
		binding = qualifyproto.Binding{Kind: qualifyproto.BindingArtifact, ExpectedSHA: deployedSHA[:7]}
	}
	target := qualifyproto.Target{Kind: qualifyproto.TargetLocal}
	if f.opened.Load() > 0 {
		target.CompositionID = "0f1e2d3c4b5a6978"
	}
	return target, binding
}

func (f *fakeTarget) Open(ctx context.Context) (string, qualifyproto.Binding, error) {
	f.opened.Add(1)
	_, binding := f.Describe()
	return f.baseURL, binding, f.openErr
}

func (f *fakeTarget) Close(context.Context) *qualifyproto.Cleanup {
	f.closed.Add(1)
	return f.cleanup
}

func TestVerdict_NeverPassedWithAnyNonPassPhase(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "no-derivable-request-is-unsupported-not-success", "an-empty-contract-never-passes")
	passed := func() []qualifyproto.Phase {
		phases := make([]qualifyproto.Phase, 0, len(qualifyproto.PhaseNames))
		for _, name := range qualifyproto.PhaseNames {
			phases = append(phases, qualifyproto.Phase{Name: name, State: qualifyproto.StatePassed})
		}
		return phases
	}
	if ReduceState(passed()) != qualifyproto.StatePassed {
		t.Fatal("all phases passed must reduce to passed")
	}
	if ReduceState(nil) != qualifyproto.StateNotRun {
		t.Error("no phase at all must not reduce to passed")
	}
	for index := range qualifyproto.PhaseNames {
		for _, state := range qualifyproto.ValidStates {
			if state.IsPass() {
				continue
			}
			phases := passed()
			phases[index].State = state
			if got := ReduceState(phases); got.IsPass() || got != state {
				t.Errorf("phase %s=%s reduced to %s", phases[index].Name, state, got)
			}
		}
	}
	// The first non-pass phase wins over a later one.
	phases := passed()
	phases[1].State, phases[3].State = qualifyproto.StateTimedOut, qualifyproto.StateFailed
	if got := ReduceState(phases); got != qualifyproto.StateTimedOut {
		t.Errorf("reduced to %s, want the first non-pass state", got)
	}

	// An empty contract opens nothing and is unsupported, whatever the target.
	fake := &fakeTarget{}
	unsupported := &Unsupported{Reason: ReasonNoRouteInventory, Remedy: "run `putnami build --projects /app`"}
	verdict := execute(t, context.Background(), contractFor(t, "/app"), fake, Options{Unsupported: unsupported})
	if verdict.State != qualifyproto.StateUnsupported || fake.opened.Load() != 0 || fake.closed.Load() != 0 {
		t.Fatalf("state %s opened %d closed %d", verdict.State, fake.opened.Load(), fake.closed.Load())
	}
	if code := phaseCode(verdict, qualifyproto.PhaseSmoke); code != qualifyproto.PhaseCodeNoRouteInventory {
		t.Errorf("smoke code = %s", code)
	}
	generic := execute(t, context.Background(), contractFor(t, "/app"), &fakeTarget{}, Options{})
	if generic.State != qualifyproto.StateUnsupported || phaseCode(generic, qualifyproto.PhaseSmoke) != qualifyproto.PhaseCodeNoDerivableRequest {
		t.Errorf("empty contract without an explanation: %s %s", generic.State, phaseStates(generic))
	}
}

func TestExecute_TargetFailuresAndTeardown(t *testing.T) {
	server := newDeployment(t, &deployment{sha: deployedSHA})
	tree := qualifyproto.Binding{Kind: qualifyproto.BindingTree, Fingerprint: strings.Repeat("a", 64), HeadSHA: deployedSHA}

	// A composition failure is its own state, and teardown still runs.
	composition := &fakeTarget{
		binding: tree,
		openErr: &TargetError{State: qualifyproto.StateCompositionFailed, Code: qualifyproto.PhaseCodeCompositionFailed, Err: errors.New("member /app never became ready")},
		cleanup: &qualifyproto.Cleanup{State: qualifyproto.CleanupClean},
	}
	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), composition, fastOptions())
	if verdict.State != qualifyproto.StateCompositionFailed || composition.closed.Load() != 1 || verdict.Cleanup == nil || verdict.Cleanup.Leftovers == nil {
		t.Fatalf("state %s closed %d cleanup %+v", verdict.State, composition.closed.Load(), verdict.Cleanup)
	}
	if verdict.Target.CompositionID == "" {
		t.Error("the target identity must be re-read after Open")
	}

	// A plain Open error is target_unreachable.
	plain := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), &fakeTarget{openErr: errors.New("dial refused")}, fastOptions())
	if plain.State != qualifyproto.StateTargetUnreachable {
		t.Errorf("plain open error: %s", plain.State)
	}

	// A tree binding passes version-binding without HTTP, and a partial teardown fails the verdict.
	partial := &fakeTarget{baseURL: server.URL, binding: tree, cleanup: &qualifyproto.Cleanup{State: qualifyproto.CleanupPartial, Leftovers: []string{"database compose_x_app_main"}}}
	failed := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), partial, fastOptions())
	if got := phaseStates(failed); got != "resolve-target=passed readiness=passed version-binding=passed smoke=passed teardown=failed" {
		t.Errorf("phases = %s", got)
	}
	if failed.State != qualifyproto.StateFailed || phaseCode(failed, qualifyproto.PhaseTeardown) != qualifyproto.PhaseCodeTeardownPartial {
		t.Errorf("partial teardown: %s %s", failed.State, phaseCode(failed, qualifyproto.PhaseTeardown))
	}
}

func TestExecute_TreeBindingIsProvenAgainOnceTheTargetIsReady(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-verdict-binds-to-the-exact-worktree",
		"a-worktree-that-changes-before-the-workload-is-ready-is-digest-mismatch")
	server := newDeployment(t, &deployment{sha: deployedSHA})
	tree := qualifyproto.Binding{Kind: qualifyproto.BindingTree, Fingerprint: strings.Repeat("a", 64), HeadSHA: deployedSHA}
	moved := tree
	moved.Fingerprint, moved.Dirty = strings.Repeat("c", 64), true
	clean := &qualifyproto.Cleanup{State: qualifyproto.CleanupClean}

	cases := map[string]struct {
		target Target
		state  qualifyproto.State
		code   string
	}{
		"unchanged tree": {&fakeTarget{baseURL: server.URL, binding: tree, cleanup: clean}, qualifyproto.StatePassed, ""},
		"changed tree": {&fakeTarget{baseURL: server.URL, binding: tree, cleanup: clean, worktree: &moved},
			qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMismatch},
		"unreadable tree": {&fakeTarget{baseURL: server.URL, binding: tree, cleanup: clean, worktreeErr: errors.New("git: not a repository")},
			qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMissing},
		"a target that cannot re-read its tree": {plainTarget{&fakeTarget{baseURL: server.URL, binding: tree, cleanup: clean}},
			qualifyproto.StateDigestMismatch, qualifyproto.PhaseCodeVersionMissing},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), tc.target, fastOptions())
			if verdict.State != tc.state {
				t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
			}
			if code := phaseCode(verdict, qualifyproto.PhaseVersionBinding); code != tc.code {
				t.Errorf("version-binding code = %q, want %q", code, tc.code)
			}
			if verdict.Binding != tree {
				t.Errorf("binding = %+v, want the promised tree whatever was observed", verdict.Binding)
			}
			if tc.state != qualifyproto.StatePassed && verdict.Requests[0].State != qualifyproto.StateNotRun {
				t.Errorf("a tree that was not proven must not be smoked: %+v", verdict.Requests[0])
			}
		})
	}
}

func TestParseTargetURLAndExpectSHA(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "remote-verdict-binds-to-the-deployed-sha", "a-url-target-without-expect-sha-is-a-usage-error")
	for _, raw := range []string{"http://127.0.0.1:3910", "https://pr-1.preview.example/app/"} {
		if _, err := ParseTargetURL(raw); err != nil {
			t.Errorf("%s refused: %v", raw, err)
		}
	}
	for _, raw := range []string{"ftp://host", "localhost:3000", "http://", "https://user:token@host", "http://host/?a=1", "http://host/?", "http://host/#x", "http://%zz"} {
		if _, err := ParseTargetURL(raw); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	for _, sha := range []string{"", "abc123", "DEADBEEF", "deadbeefz", strings.Repeat("a", 65)} {
		if err := ValidateExpectSHA(sha); err == nil {
			t.Errorf("expect-sha %q accepted", sha)
		}
		if _, err := NewURLTarget("http://127.0.0.1:1", sha); err == nil {
			t.Errorf("NewURLTarget accepted expect-sha %q", sha)
		}
	}
	if _, err := NewURLTarget("ssh://host", deployedSHA); err == nil {
		t.Error("NewURLTarget accepted a non-http URL")
	}
}

func TestRender(t *testing.T) {
	server := newDeployment(t, &deployment{sha: deployedSHA})
	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /boom"), urlTargetFor(t, server.URL, deployedSHA[:7]), fastOptions())
	var out bytes.Buffer
	if err := RenderVerdictText(&out, verdict); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"✓ resolve-target ", "✗ smoke failed ", "qualify.request_failed", "✗ GET /boom 503 failed ", "(status 503 exceeds 499)",
		"verdict: failed (/app, url, artifact expected 3cc91b6 observed " + deployedSHA + ")"} {
		if !strings.Contains(text, want) {
			t.Errorf("verdict text lacks %q:\n%s", want, text)
		}
	}

	tree := &qualifyproto.Verdict{State: qualifyproto.StateNotRun, Project: "/app", Target: qualifyproto.Target{Kind: "local"},
		Binding: qualifyproto.Binding{Kind: qualifyproto.BindingTree, Fingerprint: strings.Repeat("b", 64), Dirty: true},
		Phases:  []qualifyproto.Phase{{Name: qualifyproto.PhaseSmoke, State: qualifyproto.StateNotRun}}}
	out.Reset()
	if err := RenderVerdictText(&out, tree); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "- smoke not_run 0ms") || !strings.Contains(out.String(), "tree bbbbbbbbbbbb dirty") {
		t.Errorf("tree verdict text:\n%s", out.String())
	}
	out.Reset()
	tree.Binding = qualifyproto.Binding{Kind: "other"}
	_ = RenderVerdictText(&out, tree)
	if !strings.Contains(out.String(), "(/app, local, other)") {
		t.Errorf("unknown binding text:\n%s", out.String())
	}

	out.Reset()
	contract := contractFor(t, "/app", "GET /items")
	if err := RenderContractText(&out, contract, &Unsupported{Reason: ReasonNoDerivableRequest, Remedy: "declare a GET route"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"contract: /app (sha256:", "derived from: app/schema/http-routes.json", "GET /items  maxStatus 499  provenance typed-api", "requests: 1", "unsupported: no_derivable_request — declare a GET route"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("contract text lacks %q:\n%s", want, out.String())
		}
	}
}
