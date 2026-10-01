package compose

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// DefaultReadyTimeout bounds each member's wait for its typed ready event.
const DefaultReadyTimeout = 60 * time.Second

// memberStopTimeout bounds how long Close waits for the members to exit after
// they were canceled. The job runner escalates SIGTERM to SIGKILL after five
// seconds, so this is a hang detector, not a grace period.
const memberStopTimeout = 30 * time.Second

// Cleanup states.
const (
	CleanupClean   = "clean"
	CleanupPartial = "partial"
)

// Options configure one composition.
type Options struct {
	// WorkspaceRoot is the workspace the composition runs in.
	WorkspaceRoot string
	// Config is the loaded workspace configuration; nil loads it from
	// WorkspaceRoot.
	Config *wsproto.Config
	// Target is the workload to compose.
	Target *workspace.Project
	// ProxyPort is the target's proxy port; 0 picks an ephemeral port.
	// DefaultTargetProxyPort is the value a caller that was given none uses.
	ProxyPort int
	// WatchTarget restarts the target on file changes. Dependencies never
	// watch.
	WatchTarget bool
	// ReadyTimeout bounds each member's wait for its ready event; zero is
	// DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// Output renders the members' serve logs. Nil discards them.
	Output jobs.Renderer
	// Preparation renders the engine run that prepares the serve pipelines.
	// Nil reports only failures, on stderr.
	Preparation jobs.Renderer
}

// CleanupReport is what a composition's teardown left behind.
type CleanupReport struct {
	State     string   `json:"state"`
	Leftovers []string `json:"leftovers,omitempty"`
}

// preparation is the engine's answer to "prepare these members": the
// workspace it planned against and the serve steps it withheld.
type preparation struct {
	ws       *workspace.Workspace
	withheld []*jobs.ScheduledJob
	versions jobs.RunVersions
}

// upDeps are the effects Up acts through, replaceable in tests.
type upDeps struct {
	loadWorkspace func(root string) (*workspace.Workspace, error)
	prepare       func(ctx context.Context, opts Options, projectIDs []string) (*preparation, error)
	provider      func() databaseProvider
	reap          func(workspaceRoot string) []ReapedLease
}

func defaultUpDeps() upDeps {
	return upDeps{
		loadWorkspace: workspace.Load,
		prepare:       enginePrepare,
		provider:      func() databaseProvider { return dbtestenv.SelectProvider() },
		reap:          ReapOrphans,
	}
}

// Composition is a running composition.
type Composition struct {
	id        string
	root      string
	plan      *Plan
	lease     *leaseHandle
	databases *databaseSet
	proxies   []*proxy
	runtimes  []*memberRuntime
	reaped    []ReapedLease
	cancel    context.CancelFunc

	// pgMu orders every change to the recorded process groups, in memory and
	// in the lease, so a release never removes a record made after its check.
	pgMu  sync.Mutex
	pgids map[int]trackedGroup

	closeOnce sync.Once
	report    CleanupReport
}

// Up starts a composition: it reaps orphaned compositions, prepares every
// member's serve pipeline through the engine, provisions the databases,
// starts one proxy per member, then starts the members in topological order,
// each only once the one before it announced readiness.
//
// On failure everything already started is torn down before Up returns the
// error, a *Error which names the member and the phase and, once anything was
// acquired, carries the teardown report (Error.Cleanup).
func Up(ctx context.Context, opts Options) (*Composition, error) {
	return up(ctx, opts, defaultUpDeps())
}

func up(ctx context.Context, opts Options, deps upDeps) (composition *Composition, err error) {
	var reaped []ReapedLease
	defer func() {
		var composeErr *Error
		if err != nil && errors.As(err, &composeErr) {
			composeErr.Reaped = reaped
		}
	}()
	if opts.Target == nil {
		return nil, newError(CodeUnknownMember, "", PhasePlan, "no target project")
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultReadyTimeout
	}
	if os.Getenv(ConfigDataEnv) != "" {
		return nil, newError(CodeConfigDataConflict, opts.Target.ID, PhasePlan,
			ConfigDataEnv+" is already set in this environment; compose injects it and refuses to merge or replace a value it does not own")
	}

	reaped = deps.reap(opts.WorkspaceRoot)

	// Plan once against the loaded workspace to know which projects to
	// prepare, then again against the workspace the engine prepared, whose
	// probe view is the one the serve steps run with.
	loaded, err := deps.loadWorkspace(opts.WorkspaceRoot)
	if err != nil {
		return nil, newError(CodePrepareFailed, opts.Target.ID, PhasePlan, "load the workspace: "+err.Error())
	}
	target := loaded.ProjectByID(opts.Target.ID)
	if target == nil {
		return nil, newError(CodeUnknownMember, opts.Target.ID, PhasePlan, "the target is not a project of this workspace")
	}
	draft, err := PlanFor(loaded, target)
	if err != nil {
		return nil, err
	}
	prepared, err := deps.prepare(ctx, opts, draft.ProjectIDs())
	if err != nil {
		return nil, err
	}
	if prepared.ws.Config == nil {
		prepared.ws.Config = &wsproto.Config{}
	}
	target = prepared.ws.ProjectByID(opts.Target.ID)
	if target == nil {
		return nil, newError(CodePrepareFailed, opts.Target.ID, PhasePrepare, "the prepared workspace no longer contains the target")
	}
	plan, err := PlanFor(prepared.ws, target)
	if err != nil {
		return nil, err
	}
	if strings.Join(plan.ProjectIDs(), ",") != strings.Join(draft.ProjectIDs(), ",") {
		return nil, newError(CodePrepareFailed, opts.Target.ID, PhasePrepare,
			"the runsWith closure changed while the composition was prepared: "+
				strings.Join(draft.ProjectIDs(), ",")+" became "+strings.Join(plan.ProjectIDs(), ","))
	}
	if err := plan.BindServeJobs(prepared.withheld); err != nil {
		return nil, err
	}
	for _, member := range plan.Members {
		if err := checkConfigDataConflict(member); err != nil {
			return nil, err
		}
	}

	lease, err := createLease(opts.WorkspaceRoot, target.ID)
	if err != nil {
		return nil, newError(CodeLeaseFailed, target.ID, PhasePlan, err.Error())
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Composition{
		id:     lease.lease.ID,
		root:   opts.WorkspaceRoot,
		plan:   plan,
		lease:  lease,
		reaped: reaped,
		cancel: cancel,
		pgids:  make(map[int]trackedGroup),
	}
	// fail tears down what the start acquired and reports that teardown on the
	// error, so a caller that records cleanup (qualify --target local) states
	// what a failed start left behind instead of assuming nothing.
	fail := func(err error) (*Composition, error) {
		teardown, stop := context.WithTimeout(context.Background(), memberStopTimeout+time.Minute)
		defer stop()
		report := c.Close(teardown)
		var composeErr *Error
		if !errors.As(err, &composeErr) {
			composeErr = newError(CodeStartFailed, target.ID, PhaseStart, err.Error())
			err = composeErr
		}
		composeErr.ID, composeErr.Cleanup = c.id, &report
		return nil, err
	}

	c.databases, err = provisionDatabases(ctx, c.id, plan, lease, deps.provider())
	if err != nil {
		return fail(err)
	}

	for _, member := range plan.Members {
		port := 0
		if member == plan.Target {
			port = opts.ProxyPort
		}
		p, err := startProxy(member.Project.ID, port)
		if err != nil {
			return fail(err)
		}
		c.proxies = append(c.proxies, p)
		member.ProxyURL = p.URL()
		if err := lease.update(func(l *Lease) { l.ProxyPorts = append(l.ProxyPorts, p.port) }); err != nil {
			return fail(newError(CodeLeaseFailed, member.Project.ID, PhaseProxies, err.Error()))
		}
	}

	renderer := opts.Output
	if renderer == nil {
		renderer = output.NewTextRenderer(io.Discard, io.Discard, output.TextRendererConfig{ServeMode: true})
	}
	for i, member := range plan.Members {
		document, sections, err := configData(member, plan)
		if err != nil {
			return fail(err)
		}
		isTarget := member == plan.Target
		watchMode := isTarget && opts.WatchTarget
		rt := newMemberRuntime(member, c.proxies[i], memberEnv(!watchMode, document), sections, watchMode)
		rt.ws = prepared.ws
		rt.versions = prepared.versions
		rt.output = renderer
		memberID := member.Project.ID
		rt.observeProcess = func(pgid int) { c.recordProcessGroup(memberID, pgid) }
		rt.releaseProcess = c.releaseProcessGroup
		c.runtimes = append(c.runtimes, rt)

		rt.start(runCtx)
		if err := rt.waitReady(ctx, opts.ReadyTimeout); err != nil {
			return fail(err)
		}
		member.BackendPort = rt.proxy.backendPort()
	}
	return c, nil
}

// trackedGroup is a serve step's process group as the composition recorded it.
type trackedGroup struct {
	member string
	// leaderStart identifies the group's leader (processStartTime); empty when
	// it could not be read, which leaves the group unconfirmable.
	leaderStart string
}

// recordProcessGroup adds a spawned serve step's process group to the lease,
// with its leader's start time, before anything else can happen to it.
func (c *Composition) recordProcessGroup(member string, pgid int) {
	start, _ := processStartTime(pgid)
	c.pgMu.Lock()
	defer c.pgMu.Unlock()
	c.pgids[pgid] = trackedGroup{member: member, leaderStart: start}
	_ = c.lease.update(func(l *Lease) { l.recordGroup(pgid, start) })
}

// releaseProcessGroup forgets a serve step's process group once its job has
// returned and nothing of the group runs any more, so a composition never
// keeps an id the host can hand to another process: every watch restart
// releases the group of the run it replaces. A group that outlives its job
// stays recorded; its leader is gone, so a later teardown or reap names it
// instead of signaling it.
func (c *Composition) releaseProcessGroup(pgid int) {
	if len(waitForGroups([]int{pgid}, groupReleaseWait)) > 0 {
		return
	}
	c.pgMu.Lock()
	defer c.pgMu.Unlock()
	// Checked again under the lock: a member that spawned since and was handed
	// the same id recorded its own group, which is running.
	if processGroupAlive(pgid) {
		return
	}
	delete(c.pgids, pgid)
	_ = c.lease.update(func(l *Lease) { l.forgetGroup(pgid) })
}

// ID is the composition's 16-hex identity; its databases and lease carry it.
func (c *Composition) ID() string { return c.id }

// Isolation reports how the members' databases are isolated.
func (c *Composition) Isolation() string {
	if c.databases == nil || c.databases.isolation == "" {
		return IsolationNone
	}
	return c.databases.isolation
}

// Endpoint returns the stable proxy URL of a member.
func (c *Composition) Endpoint(projectID string) (string, bool) {
	member, ok := c.plan.Member(projectID)
	if !ok || member.ProxyURL == "" {
		return "", false
	}
	return member.ProxyURL, true
}

// Wait returns when the target's serve loop ends or ctx is done.
func (c *Composition) Wait(ctx context.Context) error {
	if len(c.runtimes) == 0 {
		return nil
	}
	target := c.runtimes[len(c.runtimes)-1]
	select {
	case <-target.exited:
		status, tail := target.exitDetail()
		err := newError(CodeMemberExited, target.member.Project.ID, PhaseStart, "the target's serve step exited")
		if status != "" {
			err.Message += " (" + status + ")"
		}
		err.Detail = tail
		return err
	case <-ctx.Done():
		return nil
	}
}

// MemberStatus is one member as a composition reports it. It carries no
// configuration value, connection string or password.
type MemberStatus struct {
	Project        string   `json:"project"`
	ProxyURL       string   `json:"proxyUrl"`
	BackendPort    int      `json:"backendPort"`
	Databases      []string `json:"databases"`
	ConfigSections []string `json:"configSections"`
	ReadyMs        int64    `json:"readyMs"`
	Notes          []string `json:"notes,omitempty"`
}

// Status is the structured projection of a composition.
type Status struct {
	ID        string         `json:"id"`
	Target    string         `json:"target"`
	Members   []MemberStatus `json:"members"`
	Isolation string         `json:"isolation"`
	Cleanup   *CleanupReport `json:"cleanup,omitempty"`
	Reaped    []ReapedLease  `json:"reaped,omitempty"`
}

// Status reports the composition's members as they are now.
func (c *Composition) Status() Status {
	status := Status{
		ID:        c.id,
		Target:    c.plan.Target.Project.ID,
		Members:   make([]MemberStatus, 0, len(c.plan.Members)),
		Isolation: c.Isolation(),
		Reaped:    c.reaped,
	}
	for i, member := range c.plan.Members {
		entry := MemberStatus{
			Project:        member.Project.ID,
			ProxyURL:       member.ProxyURL,
			Databases:      []string{},
			ConfigSections: []string{},
			Notes:          member.notes,
		}
		for _, binding := range member.Databases {
			entry.Databases = append(entry.Databases, binding.Datasource)
		}
		if i < len(c.runtimes) {
			rt := c.runtimes[i]
			entry.BackendPort = rt.proxy.backendPort()
			entry.ReadyMs = rt.readyMs.Load()
			entry.ConfigSections = append(entry.ConfigSections, rt.sections...)
		}
		status.Members = append(status.Members, entry)
	}
	return status
}

// Close tears the composition down: it cancels the members (the job runner
// sends SIGTERM to each process group and SIGKILL after five seconds), closes
// the proxies, drops the databases, and releases the lease. Anything that
// survives is named in the report, whose state is then "partial", and stays
// recorded in the lease for the next invocation to reap. Close is idempotent.
func (c *Composition) Close(ctx context.Context) CleanupReport {
	c.closeOnce.Do(func() {
		c.report = c.close(ctx)
	})
	return c.report
}

func (c *Composition) close(ctx context.Context) CleanupReport {
	var leftovers []string
	c.cancel()

	stopCtx, stop := context.WithTimeout(ctx, memberStopTimeout)
	for _, rt := range c.runtimes {
		select {
		case <-rt.exited:
		case <-stopCtx.Done():
			leftovers = append(leftovers, "serve step of "+rt.member.Project.ID+" did not stop within "+memberStopTimeout.String())
		}
	}
	stop()

	for _, p := range c.proxies {
		if err := p.close(ctx); err != nil {
			leftovers = append(leftovers, err.Error())
		}
	}

	dropCtx, drop := context.WithTimeout(ctx, reapDropTimeout)
	dropLeftovers, undropped := c.databases.drop(dropCtx)
	leftovers = append(leftovers, dropLeftovers...)
	drop()

	// A recorded group whose id now names another process's group is not a
	// leftover of this composition; one whose leader is gone cannot be
	// confirmed and is named, never signaled.
	c.pgMu.Lock()
	pgids := make([]int, 0, len(c.pgids))
	for pgid := range c.pgids {
		pgids = append(pgids, pgid)
	}
	sort.Ints(pgids)
	survivors := make(map[int]bool)
	for _, pgid := range pgids {
		group := c.pgids[pgid]
		switch identifyGroup(pgid, group.leaderStart) {
		case groupOwned:
			survivors[pgid] = true
			leftovers = append(leftovers, describeProcessGroup(group.member, pgid))
		case groupUnverified:
			survivors[pgid] = true
			leftovers = append(leftovers, describeProcessGroup(group.member, pgid)+" (its leader is gone; not signaled)")
		}
	}
	c.pgMu.Unlock()

	if len(leftovers) == 0 {
		if err := c.lease.release(); err != nil {
			leftovers = append(leftovers, "lease "+c.lease.dir+" ("+err.Error()+")")
		}
		if len(leftovers) == 0 {
			return CleanupReport{State: CleanupClean}
		}
		return CleanupReport{State: CleanupPartial, Leftovers: leftovers}
	}

	// Keep what survived on record and let the lock go: the next invocation
	// reaps it once this process is gone.
	_ = c.lease.update(func(l *Lease) {
		l.keepGroups(func(pgid int) bool { return survivors[pgid] })
		l.Databases = append([]string{}, undropped...)
		l.ProxyPorts = []int{}
	})
	_ = c.lease.lock.Release()
	return CleanupReport{State: CleanupPartial, Leftovers: leftovers}
}

// enginePrepare runs the members' serve pipelines through the engine without
// their serve steps, and returns the steps it withheld (engine.Request
// WithholdServeSteps).
func enginePrepare(ctx context.Context, opts Options, projectIDs []string) (*preparation, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = wsproto.Load(opts.WorkspaceRoot)
	}
	sink := opts.Preparation
	if sink == nil {
		sink = output.NewTextRenderer(io.Discard, os.Stderr, output.TextRendererConfig{Quiet: true})
	}
	result, err := engine.New().Run(ctx, engine.Request{
		WorkspaceRoot:      opts.WorkspaceRoot,
		Config:             cfg,
		Commands:           []string{serveCommand},
		Global:             engine.GlobalFlags{Projects: strings.Join(projectIDs, ",")},
		WithholdServeSteps: true,
		Preflight:          doctor.DoctorPreflight,
		Stdout:             io.Discard,
	}, sink)
	if err != nil {
		return nil, newError(CodePrepareFailed, opts.Target.ID, PhasePrepare, err.Error())
	}
	if result.ExitCode != engine.ExitSuccess || result.Workspace == nil {
		member := failedProject(result)
		if member == "" {
			member = opts.Target.ID
		}
		return nil, newError(CodePrepareFailed, member, PhasePrepare,
			"preparing the serve pipelines failed with exit code "+strconv.Itoa(result.ExitCode)+"; the task output above names the cause")
	}
	versions, err := engine.BuildVersionInfo(result.Workspace)
	if err != nil {
		return nil, newError(CodePrepareFailed, opts.Target.ID, PhasePrepare, err.Error())
	}
	return &preparation{
		ws:       result.Workspace,
		withheld: result.Withheld,
		versions: versions,
	}, nil
}

// failedProject names the project of the first failed job of a preparation,
// in key order, so a failure is attributed to a member.
func failedProject(result engine.SessionResult) string {
	keys := make([]string, 0, len(result.Results))
	for key, res := range result.Results {
		if res != nil && res.Status == "failed" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, job := range result.Plan {
			if job != nil && job.Key() == key && job.Project != nil {
				return job.Project.ID
			}
		}
	}
	return ""
}
