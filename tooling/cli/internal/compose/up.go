package compose

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// DefaultReadyTimeout bounds each member's wait for its typed ready event.
const DefaultReadyTimeout = 60 * time.Second

// memberStopTimeout bounds how long Close waits for a member's serve run to
// exit after SIGTERM, before it sends SIGKILL. The run stops its serve step
// with SIGTERM and SIGKILL after five seconds, and forces its own exit after
// ten, so this is a hang detector, not a grace period.
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
	// Executable is the putnami CLI each member's serve run starts; empty is
	// this process's own executable.
	Executable string
	// Logs receives each member's serve log, the text its serve run renders
	// on stderr as `putnami serve` renders it. Nil discards it; a failure keeps
	// its last lines either way (Error.Detail).
	Logs io.Writer
	// Verbose, Quiet and NoColor are the display flags each serve run renders
	// its serve log with.
	Verbose bool
	Quiet   bool
	NoColor bool
}

// CleanupReport is what a composition's teardown left behind.
type CleanupReport struct {
	State     string   `json:"state"`
	Leftovers []string `json:"leftovers,omitempty"`
}

// upDeps are the effects Up acts through, replaceable in tests.
type upDeps struct {
	loadWorkspace func(root string) (*workspace.Workspace, error)
	// discover loads the workspace's extensions, whose resolved traits name the
	// serve-workload commands every member runs.
	discover func(ws *workspace.Workspace, cfg *wsproto.Config) ([]*extension.ExtensionDescription, error)
	provider func() databaseProvider
	reap     func(workspaceRoot string) []ReapedLease
}

func defaultUpDeps() upDeps {
	return upDeps{
		loadWorkspace: workspace.Load,
		discover: func(ws *workspace.Workspace, cfg *wsproto.Config) ([]*extension.ExtensionDescription, error) {
			return extension.DiscoverExtensions(ws.Root, cfg, ws.ProjectPaths())
		},
		provider: func() databaseProvider { return dbtestenv.SelectProvider() },
		reap:     ReapOrphans,
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

	// pgMu orders every change to the recorded process groups, in memory and
	// in the lease, so a release never removes a record made after its check.
	pgMu  sync.Mutex
	pgids map[int]trackedGroup

	closeOnce sync.Once
	report    CleanupReport
}

// Up starts a composition: it reaps orphaned compositions, provisions the
// databases, starts one proxy per member, then starts each member as an
// ordinary serve run of the putnami CLI, in topological order, each only once
// the one before it announced readiness. The serve run prepares the member's
// serve pipeline and starts its serve step: only the engine starts a serve
// step.
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

	loaded, err := deps.loadWorkspace(opts.WorkspaceRoot)
	if err != nil {
		return nil, newError(CodePrepareFailed, opts.Target.ID, PhasePlan, "load the workspace: "+err.Error())
	}
	target := loaded.ProjectByID(opts.Target.ID)
	if target == nil {
		return nil, newError(CodeUnknownMember, opts.Target.ID, PhasePlan, "the target is not a project of this workspace")
	}
	plan, serveCommands, err := planComposition(opts, deps, loaded, target)
	if err != nil {
		return nil, err
	}
	if err := plan.BindServeCommands(serveCommands); err != nil {
		return nil, err
	}
	executable := opts.Executable
	if executable == "" {
		if executable, err = os.Executable(); err != nil {
			return nil, newError(CodeStartFailed, target.ID, PhasePlan, "locate the putnami executable: "+err.Error())
		}
	}
	display := serveRunDisplay{verbose: opts.Verbose, quiet: opts.Quiet, noColor: opts.NoColor}

	lease, err := createLease(opts.WorkspaceRoot, target.ID)
	if err != nil {
		return nil, newError(CodeLeaseFailed, target.ID, PhasePlan, err.Error())
	}
	c := &Composition{
		id:     lease.lease.ID,
		root:   opts.WorkspaceRoot,
		plan:   plan,
		lease:  lease,
		reaped: reaped,
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

	for i, member := range plan.Members {
		document, sections, err := configData(member, plan)
		if err != nil {
			return fail(err)
		}
		isTarget := member == plan.Target
		watchMode := isTarget && opts.WatchTarget
		rt := newMemberRuntime(member, c.proxies[i], memberEnv(!watchMode, document), sections, watchMode)
		rt.executable, rt.root, rt.logs = executable, opts.WorkspaceRoot, opts.Logs
		rt.args = serveRunArgs(member.ServeCommand, member.Project.ID, watchMode, display)
		memberID := member.Project.ID
		rt.observeProcess = func(pgid int, leaderStart string) { c.recordProcessGroup(memberID, pgid, leaderStart) }
		rt.releaseProcess = c.releaseProcessGroup
		c.runtimes = append(c.runtimes, rt)

		if err := rt.start(); err != nil {
			return fail(newError(CodeStartFailed, memberID, PhaseStart, err.Error()))
		}
		if err := rt.waitReady(ctx, opts.ReadyTimeout); err != nil {
			return fail(err)
		}
		member.BackendPort = rt.proxy.backendPort()
	}
	return c, nil
}

// trackedGroup is a member's process group as the composition recorded it: its
// serve run's, or a serve step's.
type trackedGroup struct {
	member string
	// leaderStart identifies the group's leader (proctree.StartTime); empty when
	// it could not be read, which leaves the group unconfirmable.
	leaderStart string
}

// recordProcessGroup adds a member's process group to the lease, with its
// leader's start time, before anything else can happen to it: the serve run's
// own group as it starts, and each serve step's as the run's session stream
// names it.
func (c *Composition) recordProcessGroup(member string, pgid int, leaderStart string) {
	c.pgMu.Lock()
	defer c.pgMu.Unlock()
	c.pgids[pgid] = trackedGroup{member: member, leaderStart: leaderStart}
	_ = c.lease.update(func(l *Lease) { l.recordGroup(pgid, leaderStart) })
}

// releaseProcessGroup forgets a member's process group once its process ended
// and nothing of the group runs any more, so a composition never keeps an id
// the host can hand to another process: every watch restart releases the
// group of the serve step it replaces. A group that outlives its process stays
// recorded; its leader is gone, so a later teardown or reap names it instead of
// signaling it.
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

// Wait returns when the target's serve run ends or ctx is done.
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

// WaitStartup returns once a member's application reported completed startup:
// a typed ready event whose target is workload, which the Go and TypeScript
// application frameworks emit only after every plugin starter and module start
// hook returned. The member's readiness (its first claim, which is all Up
// waits for) is not that report: a server claim states only that a listener is
// bound. WaitStartup fails with CodeMemberExited when the member's serve step
// ends first, returns ctx's error when ctx is done first, and fails with
// CodeUnknownMember for a project that is not a member.
func (c *Composition) WaitStartup(ctx context.Context, projectID string) error {
	for _, rt := range c.runtimes {
		if rt.member.Project.ID == projectID {
			return rt.waitStartup(ctx)
		}
	}
	return newError(CodeUnknownMember, projectID, PhaseReadiness, "the project is not a member of this composition")
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

// Close tears the composition down: it stops the members' serve runs in
// reverse start order (each run stops its serve step), closes the proxies,
// drops the databases, and releases the lease. Anything that survives is named
// in the report, whose state is then "partial", and stays recorded in the
// lease for the next invocation to reap. Close is idempotent.
func (c *Composition) Close(ctx context.Context) CleanupReport {
	c.closeOnce.Do(func() {
		c.report = c.close(ctx)
	})
	return c.report
}

func (c *Composition) close(ctx context.Context) CleanupReport {
	var leftovers []string
	for i := len(c.runtimes) - 1; i >= 0; i-- {
		if leftover := c.runtimes[i].stop(ctx); leftover != "" {
			leftovers = append(leftovers, leftover)
		}
	}

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

// planComposition plans target against the loaded workspace ws, and returns
// the plan with the loaded commands whose resolved traits declare the serve
// workload. A member that cannot serve is refused first, then a workspace whose
// loaded extensions declare no serve-workload command
// (compose.no_serve_command).
func planComposition(opts Options, deps upDeps, ws *workspace.Workspace, target *workspace.Project) (*Plan, []string, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = wsproto.Load(opts.WorkspaceRoot)
	}
	extensions, err := deps.discover(ws, cfg)
	if err != nil {
		return nil, nil, newError(CodePrepareFailed, opts.Target.ID, PhasePlan, "discover the workspace's extensions: "+err.Error())
	}
	serveCommands := jobs.CommandsOfWorkload(extensions, extension.WorkloadServe)
	plan, err := PlanFor(ws, target, serveCommands)
	if err != nil {
		return nil, nil, err
	}
	if len(serveCommands) == 0 {
		return nil, nil, newError(CodeNoServeCommand, opts.Target.ID, PhasePlan,
			"no loaded extension declares a command whose traits declare the serve workload")
	}
	return plan, serveCommands, nil
}
