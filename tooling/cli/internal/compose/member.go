package compose

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	modeljobs "go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/watch"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Process environment entries compose owns on a member's serve step.
const (
	envPort              = "PORT=0"
	envProductionNodeEnv = "NODE_ENV=production"
	serveParamPort       = "port"
	serveParamEphemeral  = 0
)

// isServeReadyEvent reports whether a job event is a readiness claim. Copied
// from internal/watch (serve_ready.go), where it is unexported: the type must be
// `ready`, the envelope v2, and the payload a claim inside the closed target
// vocabulary. Anything else fails closed — the member stays not ready.
func isServeReadyEvent(event jobs.RawJobEvent) bool {
	if event.Type != jobs.EventTypeReady || event.Version != runtimeproto.ProtocolVersion2 {
		return false
	}
	data, err := runtimeproto.ExtractReadyData(event.Data)
	if err != nil || data == nil {
		return false
	}
	return data.Target == runtimeproto.ReadyTargetServer || data.Target == runtimeproto.ReadyTargetWorkload
}

// readyPort is the port of the claim's first endpoint, or 0 for a workload
// that binds nothing.
func readyPort(event jobs.RawJobEvent) int {
	data, err := runtimeproto.ExtractReadyData(event.Data)
	if err != nil || data == nil || len(data.Endpoints) == 0 {
		return 0
	}
	return data.Endpoints[0].Port
}

// memberRuntime is a running member: its proxy, the environment its serve step
// receives, and what it announced.
type memberRuntime struct {
	member   *Member
	proxy    *proxy
	env      []string
	sections []string
	watch    bool

	ws             *workspace.Workspace
	versions       jobs.RunVersions
	output         jobs.Renderer
	observeProcess func(pgid int)
	// releaseProcess is told every group a serve step spawned once the step
	// returned.
	releaseProcess func(pgid int)

	startedAt time.Time
	ready     chan struct{}
	readyOnce sync.Once
	readyMs   atomic.Int64
	lastPort  atomic.Int64
	exited    chan struct{}
	// stoppedBeforeReady closes when a serve step ends without ever having
	// announced readiness. Under watch the loop outlives that iteration (it
	// waits for a change), so exited alone would let a failed start run into
	// the ready timeout.
	stoppedBeforeReady chan struct{}
	stoppedOnce        sync.Once

	mu        sync.Mutex
	tail      tailLines
	lastError string
}

func newMemberRuntime(member *Member, p *proxy, env, sections []string, watchMode bool) *memberRuntime {
	return &memberRuntime{
		member:   member,
		proxy:    p,
		env:      env,
		sections: sections,
		watch:    watchMode,
		ready:    make(chan struct{}),
		exited:   make(chan struct{}),

		stoppedBeforeReady: make(chan struct{}),
	}
}

// serveParams are the job params of every serve step: port 0, so the workload
// binds an ephemeral port and announces it. A fresh map per call, because the
// job runner may project aliases into it.
func serveParams() map[string]any { return map[string]any{serveParamPort: serveParamEphemeral} }

// observe captures what a member's event stream says about it: its output
// tail, and every typed readiness claim, which moves the proxy to the announced
// port.
func (rt *memberRuntime) observe(event jobs.RawJobEvent) {
	if event.Type == jobs.EventTypeLog && event.Message != "" {
		rt.mu.Lock()
		rt.tail.add(event.Message)
		rt.mu.Unlock()
	}
	if !isServeReadyEvent(event) {
		return
	}
	port := readyPort(event)
	rt.proxy.setBackend(port)
	if port > 0 {
		rt.lastPort.Store(int64(port))
	}
	rt.readyOnce.Do(func() {
		rt.readyMs.Store(time.Since(rt.startedAt).Milliseconds())
		close(rt.ready)
	})
}

// runOnce executes the member's serve step once and returns its result. The
// proxy answers 503 again as soon as the process is gone.
func (rt *memberRuntime) runOnce(ctx context.Context, sink jobs.Renderer) (*jobs.JobResult, error) {
	job := rt.member.ServeJob
	if sink != nil {
		sink.JobStart(job)
	}
	// The workspace's command defaults, exactly as the scheduler resolves them
	// for a planned serve step (up guarantees the configuration is loaded).
	var spawnedMu sync.Mutex
	var spawned []int
	observe := func(pgid int) {
		spawnedMu.Lock()
		spawned = append(spawned, pgid)
		spawnedMu.Unlock()
		if rt.observeProcess != nil {
			rt.observeProcess(pgid)
		}
	}
	result, err := jobs.RunJobWithProcessEnv(
		jobs.WithProcessGroupObserver(ctx, observe),
		rt.ws, job, serveParams(), rt.ws.Config.GetCommandDefaults(modeljobs.JobCommandName(job), job.Extension.Name),
		rt.versions, rt.env,
		func(event jobs.RawJobEvent) {
			rt.observe(event)
			if sink != nil {
				sink.JobEvent(job, event)
			}
		},
	)
	rt.proxy.setBackend(0)
	spawnedMu.Lock()
	returned := append([]int(nil), spawned...)
	spawnedMu.Unlock()
	if rt.releaseProcess != nil {
		for _, pgid := range returned {
			rt.releaseProcess(pgid)
		}
	}
	if err != nil {
		rt.recordExit(err.Error())
	} else {
		if sink != nil {
			sink.JobComplete(job, result)
		}
		// The status says how the step ended. The job's error text is the
		// workload's own stderr, so it joins the output tail, never the message.
		status := result.Status
		if result.ExitCode != 0 {
			status += ", exit code " + strconv.Itoa(result.ExitCode)
		}
		rt.recordExit(status)
		if result.Error != nil && result.Error.Message != "" {
			rt.mu.Lock()
			for _, line := range strings.Split(result.Error.Message, "\n") {
				rt.tail.add(line)
			}
			rt.mu.Unlock()
		}
	}
	// Signaled after the exit is recorded, so a waiter reads the status.
	select {
	case <-rt.ready:
	default:
		rt.stoppedOnce.Do(func() { close(rt.stoppedBeforeReady) })
	}
	return result, err
}

func (rt *memberRuntime) recordExit(message string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.lastError = message
}

func (rt *memberRuntime) exitDetail() (string, []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.lastError, rt.tail.snapshot()
}

// start runs the member until ctx is canceled: once for a dependency or a
// target without watch, under the watch loop for a watched target.
func (rt *memberRuntime) start(ctx context.Context) {
	rt.startedAt = time.Now()
	if !rt.watch {
		go func() {
			defer close(rt.exited)
			_, _ = rt.runOnce(ctx, rt.output)
		}()
		return
	}
	target := rt.member.Project
	session := watch.NewSession(watch.SessionConfig{
		Workspace:        rt.ws,
		SelectedProjects: []*workspace.Project{target},
		CommandParams:    serveParams(),
		ServeMode:        true,
		Renderer:         rt.output,
		BoundPort:        func() int { return int(rt.lastPort.Load()) },
		RunIteration: func(ctx context.Context, _ []*workspace.Project, sink jobs.Renderer) watch.IterationResult {
			result, err := rt.runOnce(ctx, sink)
			switch {
			case err != nil:
				return watch.IterationResult{ExitCode: protocolcli.ExitFailure}
			case result.Status == "canceled":
				return watch.IterationResult{Aborted: true}
			case result.Status == "success":
				return watch.IterationResult{ExitCode: protocolcli.ExitSuccess}
			default:
				return watch.IterationResult{ExitCode: protocolcli.ExitFailure}
			}
		},
	})
	go func() {
		defer close(rt.exited)
		session.Run(ctx)
	}()
}

// waitReady blocks until the member announced readiness, exited, or timeout
// elapsed.
func (rt *memberRuntime) waitReady(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	id := rt.member.Project.ID
	select {
	case <-rt.ready:
		return nil
	case <-rt.stoppedBeforeReady:
		return rt.exitedBeforeReady(id)
	case <-rt.exited:
		return rt.exitedBeforeReady(id)
	case <-timer.C:
		_, tail := rt.exitDetail()
		err := newError(CodeReadyTimeout, id, PhaseReadiness,
			"no typed ready event within "+timeout.String())
		err.Detail = tail
		return err
	case <-ctx.Done():
		return newError(CodeStartFailed, id, PhaseReadiness, "canceled while waiting for the ready event: "+ctx.Err().Error())
	}
}

// exitedBeforeReady is the failure of a serve step that ended without a ready
// event, with its exit status and the tail of its output.
func (rt *memberRuntime) exitedBeforeReady(id string) error {
	status, tail := rt.exitDetail()
	err := newError(CodeMemberExited, id, PhaseReadiness, "the serve step exited before its ready event")
	if status != "" {
		err.Message += " (" + status + ")"
	}
	err.Detail = tail
	return err
}

// memberEnv is the process environment of a member's serve step: an
// ephemeral port, production mode for a dependency and for an unwatched
// target, and the CONFIG_DATA document when there is one.
func memberEnv(production bool, document []byte) []string {
	env := []string{envPort}
	if production {
		env = append(env, envProductionNodeEnv)
	}
	if len(document) > 0 {
		env = append(env, ConfigDataEnv+"="+string(document))
	}
	return env
}

// describeProcessGroup is how a leftover process group is named.
func describeProcessGroup(member string, pgid int) string {
	return fmt.Sprintf("process group %s of %s", strconv.Itoa(pgid), member)
}
