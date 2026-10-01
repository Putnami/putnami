package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	registryproto "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

var (
	orphanPipeDrainDelay  = 2 * time.Second
	processGroupKillDelay = 5 * time.Second
)

// goExtensionName identifies jobs whose subprocesses run Go tooling, for which
// GOMAXPROCS is the canonical lever to bound CPU usage.
const goExtensionName = "@putnami/go"

// applyCPUBudget exports the scheduler-assigned CPU budget to the job's
// subprocess. Every job gets PUTNAMI_CPU_BUDGET so any extension can bound its
// internal parallelism; Go-extension jobs additionally get GOMAXPROCS, the
// canonical lever for Go tools (go test — including its -p default — go build,
// golangci-lint, staticcheck), so concurrent jobs don't each grab every core
// and oversubscribe the host. GOMAXPROCS already present in the environment
// (an explicit operator/CI choice) wins.
//
// PUTNAMI_CPU_BUDGET is appended unconditionally: it is the scheduler's own
// channel, and an entry already in the environment is just an outer putnami's
// allocation leaking into a nested run (putnami testing putnami). os/exec
// keeps the last duplicate, so the append overrides the inherited value.
func applyCPUBudget(env []string, job *ScheduledJob) []string {
	if job == nil || job.CPUBudget <= 0 {
		return env
	}
	env = append(env, fmt.Sprintf("PUTNAMI_CPU_BUDGET=%d", job.CPUBudget))
	if job.Extension == nil || job.Extension.Name != goExtensionName {
		return env
	}
	if _, ok := os.LookupEnv("GOMAXPROCS"); ok || envHasKey(env, "GOMAXPROCS") {
		return env
	}
	return append(env, fmt.Sprintf("GOMAXPROCS=%d", job.CPUBudget))
}

// applyTaskDeadline exports the deadline this child process actually runs
// under. It runs at the exec seam, after cache-key computation, so a scheduler
// tuning remains execution-only while runtimes can derive their own tool timeout
// below the scheduler's clock.
//
// No scheduler deadline is represented by absence, not a sentinel. The input
// starts from os.Environ, so an unbounded nested task must remove a deadline an
// outer putnami left behind rather than accidentally inheriting a false bound.
func applyTaskDeadline(env []string, job *ScheduledJob) []string {
	if job == nil {
		return removeEnvKey(env, extensionproto.TaskDeadlineMsEnv)
	}
	timeoutMs := job.EffectiveTimeoutMs()
	if timeoutMs < 0 {
		return removeEnvKey(env, extensionproto.TaskDeadlineMsEnv)
	}
	if timeoutMs == 0 {
		timeoutMs = DefaultTimeoutMs
	}
	return append(env, fmt.Sprintf("%s=%d", extensionproto.TaskDeadlineMsEnv, boundedDeadlineMs(timeoutMs, 1)))
}

// runtimeEventsAdvertisement is the CLI half of the runtime event protocol
// negotiation (protocols/runtime/negotiation.go): it tells every job subprocess
// the highest event vocabulary this CLI can parse, so an SDK built against a
// newer protocol knows whether readiness may travel.
//
// It is advertised for EVERY job, not only serve jobs, because it states a fact
// about this CLI rather than about the work: a subprocess picks its stream
// version once, before its first line, and since slice B6c the CLI accepts ONLY
// that version (events.go). Making it conditional would turn one honest
// sentence into a per-command lie a reader could not act on.
//
// Precedence, stated exactly (os/exec keeps the LAST duplicate): it is appended
// after os.Environ(), BuildEnvVars, extraEnv, and the job's own Env. No caller,
// manifest, or inherited value can override it, because this CLI parses the
// resulting stream and must not advertise a version it will reject.
func runtimeEventsAdvertisement() string {
	return runtimeproto.AdvertisedVersionEnv(runtimeproto.MaxKnownProtocolVersion)
}

// envHasKey, envLastValue, prependEnvPath and removeEnvKey match variable
// names the way this platform's process environment does (envkeys.Host).
func envHasKey(env []string, key string) bool {
	return envkeys.Host.Has(env, key)
}

func envLastValue(env []string, key string) string {
	return envkeys.Host.Last(env, key)
}

// prependEnvPath makes a trusted executable directory authoritative while
// preserving the effective caller PATH behind it. Empty entries are omitted so
// advertising the CLI never adds an implicit current-directory lookup.
func prependEnvPath(env []string, dir string) []string {
	return prependPathWith(envkeys.Host, env, dir)
}

// removeEnvKey removes every occurrence of a key. Appending a later value is
// enough to override an inherited environment value, but only removal can state
// the task-deadline contract's meaningful absence.
func removeEnvKey(env []string, key string) []string {
	return envkeys.Host.Remove(env, key)
}

type jobReadResult struct {
	events []RawJobEvent
	result *JobResult
}

// clampWallToFirstEvent is the single definition of the startup-latency clamp.
// cmd.Wait can win the scheduling race with the stdout reader even though the
// child necessarily wrote the parsed event before it exited, so a measured wall
// can come out shorter than the startup latency it contains. Both the
// subprocess attempt (RunJob) and the scheduler-level task wall
// (Scheduler.closeTask) clamp the same way; defining it once keeps the two from
// drifting into different timing semantics.
func clampWallToFirstEvent(wall, spawnToFirstEvent time.Duration, firstEventObserved bool) time.Duration {
	if firstEventObserved && wall < spawnToFirstEvent {
		return spawnToFirstEvent
	}
	return wall
}

// jobInvocation is the resolved subprocess setup shared by RunJob and
// RunJobInteractive: the serialized context file, the template-expanded
// command line, and the assembled environment.
type jobInvocation struct {
	jobCtx       *JobCommandContext
	contextFile  string
	command      string
	args         []string
	cwd          string
	projRoot     string
	env          []string
	scratchLease *flock.Lock
	// extensionRuntime is the resolved {extensionRuntime}, or "" when the job
	// does not reference it.
	extensionRuntime string
	// credential is the run credential jobCommand handed the job, or nil. The
	// invocation cleanup closes it.
	credential *jobCredentialHandoff
}

// execCommand passes the context's lease to the child. The invocation cleanup
// closes only the parent's descriptor, so a surviving child stays protected.
func (inv *jobInvocation) execCommand(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, inv.command, inv.args...)
	if flock.Inheritable && inv.scratchLease != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, inv.scratchLease.File())
	}
	return cmd
}

// jobCommand is execCommand in the job's working directory with env. It also
// hands the job the run credential when the job receives one
// (attachJobCredential). The invocation cleanup closes that handoff after the
// process and its pipes, however the job ended.
func (inv *jobInvocation) jobCommand(ctx context.Context, job *ScheduledJob, env []string) (*exec.Cmd, error) {
	cmd := inv.execCommand(ctx)
	cmd.Dir = inv.cwd
	cmd.Env = env
	credential, err := attachJobCredential(ctx, cmd, job, inv.extensionRuntime)
	if err != nil {
		return nil, err
	}
	inv.credential = credential
	credential.send()
	return cmd, nil
}

// startJob runs start, which starts the job's process. A job that received the
// job credential starts as a credential holder (runcredential.StartHolder).
// Every other job runs repository code, which this process records first
// (runcredential.MarkRepositoryCodeStarted): a hosted run hands its credential
// to no process started after it.
func (inv *jobInvocation) startJob(job *ScheduledJob, start func() error) error {
	if inv.credential != nil {
		return runcredential.StartHolder("job "+job.Key(), start)
	}
	runcredential.MarkRepositoryCodeStarted("job " + job.Key())
	return start()
}

// buildJobInvocation resolves the command, working directory, and environment
// shared by execution and cache-key identity calculation. The context-file
// argument is deliberately appended by prepareJobInvocation: direct-extension
// classification needs the same pre-context command shape without creating a
// temporary file during cache precomputation.
func buildJobInvocation(
	ws *workspace.Workspace,
	job *ScheduledJob,
	jobCtx *JobCommandContext,
	extraEnv []string,
	extensionRuntime string,
) *jobInvocation {
	projRoot := filepath.Join(ws.Root, job.Project.Path)
	extensionRoot := jobExtensionRoot(ws, job)
	tmplVars := extension.BuildTemplateVars(
		ws.Root,
		projRoot,
		extensionRoot,
		jobCtx.OutputPath,
	)
	addSelectionTemplateVars(tmplVars, jobCtx.SelectedProjects)
	if extensionRuntime != "" {
		tmplVars[extensionproto.TemplateVarExtensionRuntime] = extensionRuntime
	}
	// {invocationArtifactRoot} expands for exactly the tasks whose context
	// carries the locator — the producer, listed consumers, and finalizer of one
	// finalizes relation. An unrelated task leaves the token unexpanded rather
	// than receiving a path into a private tree it must not read.
	if jobCtx.Invocation != nil && jobCtx.Invocation.ArtifactRoot != "" {
		tmplVars[extensionproto.TemplateVarInvocationArtifactRoot] = jobCtx.Invocation.ArtifactRoot
	}

	args := make([]string, 0, len(job.JobDef.Args)+1)
	for _, arg := range job.JobDef.Args {
		args = append(args, extension.ExpandTemplateVars(arg, tmplVars))
	}

	cwd := projRoot
	// Outside any workspace the project root is the user-scope directory, a
	// place the user never named. The job runs where the command was invoked
	// instead; a task that declares its own cwd still gets it.
	if jobCtx.UserScope != nil {
		cwd = jobCtx.UserScope.CallerDir
	}
	if job.JobDef.Cwd != "" {
		cwd = extension.ExpandTemplateVars(job.JobDef.Cwd, tmplVars)
	}

	// The bound-request channel is orchestrator control data for the pinned
	// entrypoint of a portable execution, never job input: a task that called
	// the CLI again while it was set would be hijacked into a second bound
	// execution. Strip it before the job's own Env can restate it.
	env := removeEnvKey(os.Environ(), runner.BoundRequestEnv)
	// The object-cache socket and trust policy are THIS run's statement about
	// the cache its jobs may consult, and a run that consults none states that
	// by absence (jobProcessEnv withholds both under --no-cache or without a
	// provider). Absence has to be produced, not assumed: a nested run inherits
	// its parent's exported pair through os.Environ, so a withheld variable
	// would otherwise reach the job with the OUTER run's socket. Strip
	// the inherited pair here; extraEnv restates this run's own when it has one.
	env = removeEnvKey(env, cache.ObjectCacheSocketEnv)
	env = removeEnvKey(env, cache.CacheTrustEnv)
	// The bound commit is THIS run's input: the CLI reads it once, into the
	// version snapshot, and every job receives the resulting version through
	// its job context. A job that inherited the variables would bind whatever
	// it runs to the outer run's commit: a test suite would read the bound
	// commit where its fixtures expect their own HEAD, and a nested `putnami`
	// run would stamp a commit its checkout may not even contain. Strip the
	// inherited pair; a manifest that restates one does so on purpose.
	env = removeEnvKey(env, git.SourceRevisionEnv)
	env = removeEnvKey(env, git.SourceCommitTimeEnv)
	// The caller directory is present exactly when THIS job runs outside any
	// workspace. A workspace run nested under a user-scope job would otherwise
	// inherit the outer value and read as a run without a workspace.
	env = removeEnvKey(env, CallerDirEnv)
	// The credential-provider choice belongs to the process that made it.
	env = removeEnvKey(env, ProvidersEnv)
	env = append(env, BuildEnvVars(jobCtx)...)
	env = append(env, extraEnv...)
	if job.JobDef.Env != nil {
		for k, v := range job.JobDef.Env {
			expanded := extension.ExpandTemplateVars(v, tmplVars)
			env = append(env, k+"="+expanded)
		}
	}
	if extensionRuntime != "" {
		env = setEnv(env, "PUTNAMI_EXTENSION_ROOT", extensionRoot)
	}
	// Extensions that call back into the CLI receive this exact process. The
	// explicit variable is the stable contract; prefixing its trusted directory
	// keeps existing extensions that still execute the bare `putnami` name
	// working until they migrate. Toolchain directories are applied afterwards,
	// so a sibling binary beside the CLI cannot shadow an exact lock-resolved
	// compiler or task runtime with the same bare name.
	cliExecutable := ""
	if executable, err := os.Executable(); err == nil && executable != "" {
		cliExecutable = executable
		env = prependEnvPath(env, filepath.Dir(cliExecutable))
	}
	env = applyRuntimeToolchains(env, job.Extension, job.JobDef.Toolchains)
	if cliExecutable != "" {
		env = setEnv(env, registryproto.CLIExecutableEnv, cliExecutable)
	}
	// os/exec uses the last duplicate environment entry. The runtime event
	// protocol is a CLI-owned parser contract, not a task setting, so make the
	// CLI's advertised version authoritative over every task-provided value.
	env = append(env, runtimeEventsAdvertisement())

	// Same rule, same reason: the parent session id states WHO SPAWNED this
	// process. It is a fact about the spawn, never task configuration, so a
	// manifest's own Env — appended above, and therefore winning by default —
	// must not be able to restate it. A task that did would make the run it
	// spawns name a session that never spawned it, and a task that set it to the
	// empty string would make a nested run look top-level and be counted as a
	// gate of its own. Re-assert the scheduler's value last so neither is
	// reachable from a manifest.
	if envHasKey(extraEnv, protocolcli.ParentSessionEnv) {
		env = setEnv(env, protocolcli.ParentSessionEnv, envLastValue(extraEnv, protocolcli.ParentSessionEnv))
	}
	// The credential descriptor names a descriptor of the process that
	// received it, and only the spawn that hands one sets it
	// (attachJobCredential). An inherited or declared value names nothing.
	env = removeEnvKey(env, extensionproto.JobCredentialFDEnv)
	// A hosted run states, last, that the job must not download
	// dependencies, and holds no framework credential in its environment. A
	// workspace-fetch outside a hosted install's dependency fetch is offline
	// too: attachJobCredential states it.
	env = runcredential.ChildEnv(env, fetchesDependencies(job))

	return &jobInvocation{
		jobCtx:           jobCtx,
		command:          extension.ExpandTemplateVars(job.JobDef.Command, tmplVars),
		args:             args,
		cwd:              cwd,
		projRoot:         projRoot,
		env:              env,
		extensionRuntime: extensionRuntime,
	}
}

// prepareJobInvocation builds the shared setup for spawning a job subprocess:
// it writes the context file, expands template vars in command/args/cwd,
// applies the job timeout to ctx, and assembles the environment (extraEnv
// entries are appended before the job's own Env). The caller must remove
// contextFile when done and defer cancel.
//
// extraEnv carries ONLY facts about how the CLI is spawning the process:
// RunJobInteractive's PUTNAMI_INTERACTIVE=1 and the scheduler's object-cache
// socket + trust policy (remote_provider.go). Neither describes the work, so
// neither belongs in a cache key. The one entry that DID describe the work is
// gone: a cleanup deleted the synthesized DATABASE_TEST_BINDINGS the
// test-infra planner injected here. Its whole purpose
// was to reach a task with a value the task's own declared inputs could not see,
// which is exactly why the cache key had to be patched separately to notice it.
// A credential reaches a task through the invocation-scoped `sensitive` artifact
// its producer declares now, and the consumer's key folds the producing action's
// digest.
func prepareJobInvocation(
	ctx context.Context,
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
	versions RunVersions,
	extraEnv []string,
) (context.Context, context.CancelFunc, *jobInvocation, error) {
	if err := ensureJobRuntimeToolchains(ws, job); err != nil {
		return nil, nil, nil, err
	}
	runtimePath, runtimeErr := resolvedExtensionRuntime(job)
	if runtimeErr != nil {
		return nil, nil, nil, runtimeErr
	}

	jobCtx := BuildJobContext(ws, job, commandParams, configDefaults, versions)
	scratchLease, err := store.AcquireScratch(ws.Root)
	if err != nil {
		return nil, nil, nil, err
	}

	contextFile, err := WriteContextFile(jobCtx, jobCtx.CacheRoot)
	if err != nil {
		_ = scratchLease.Close()
		return nil, nil, nil, fmt.Errorf("write context file: %w", err)
	}

	// Set timeout: positive → use that value, zero → use default, negative → no
	// timeout. The resolver is shared with coalescing, batching and finalization.
	timeoutMs := job.EffectiveTimeoutMs()
	if timeoutMs == 0 {
		timeoutMs = DefaultTimeoutMs
	}
	cancel := context.CancelFunc(func() {})
	if timeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(boundedDeadlineMs(timeoutMs, 1))*time.Millisecond)
	}

	inv := buildJobInvocation(ws, job, jobCtx, extraEnv, runtimePath)
	inv.contextFile = contextFile
	inv.scratchLease = scratchLease
	inv.args = append(inv.args, "--putnamiContext", contextFile)
	closeScratch := sync.OnceFunc(func() { _ = scratchLease.Close() })
	return ctx, func() { cancel(); inv.credential.close(); closeScratch() }, inv, nil
}

func addSelectionTemplateVars(vars map[string]string, projects []JobContextSelectedProject) {
	if len(projects) == 0 {
		return
	}
	vars["selectedProjects"] = strings.Join(selectedProjectField(projects, "name"), ",")
	vars["selectedProjectIDs"] = strings.Join(selectedProjectField(projects, "id"), ",")
	vars["selectedProjectPaths"] = strings.Join(selectedProjectField(projects, "path"), ",")
	vars["selectedProjectRoots"] = strings.Join(selectedProjectField(projects, "fullPath"), ",")
}

// missingCommandBinary reports whether an error from cmd.Start() means the
// command executable itself was not found — an absolute path that does not exist
// (a reaped extension binary behind a dangling stable symlink) or a bare name
// absent from PATH. Other startup ENOENTs, such as a missing cmd.Dir, must keep
// their original diagnostic.
func missingCommandBinary(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr) && pathErr.Op == "fork/exec" && errors.Is(pathErr.Err, os.ErrNotExist)
}

func missingWorkingDir(cwd string) bool {
	if cwd == "" {
		return false
	}
	_, err := os.Stat(cwd)
	return errors.Is(err, os.ErrNotExist)
}

// missingBinaryError turns a bare fork/exec ENOENT into an actionable diagnostic.
// The usual cause is that the machine-global artifact store entry backing an
// extension binary was reclaimed concurrently (e.g. `putnami cache clean --all` in
// a sibling worktree) and could not self-heal, leaving the stable symlink
// dangling — far more useful than a raw "start subprocess …: no such file or
// directory".
func missingBinaryError(job *ScheduledJob, command string, cause error) error {
	subject := "a required extension binary"
	if job != nil && job.Extension != nil && job.Extension.Name != "" {
		subject = fmt.Sprintf("the %s extension binary", job.Extension.Name)
	}
	return fmt.Errorf(
		"%s is missing (%s): the shared artifact store may have been reclaimed "+
			"concurrently (e.g. `putnami cache clean --all` in another worktree); "+
			"run `putnami install` to re-materialize it, then retry: %w",
		subject, command, cause,
	)
}

// RunJob executes a single scheduled job as a subprocess.
// It serializes the context, spawns the extension command, parses JSONL
// events from stdout, and returns the structured result.
//
// It takes no subprocess-only environment: every value a task consumes travels
// through its declared inputs, its job context, or — for a credential — the
// invocation-scoped `sensitive` artifact its producer wrote. The scheduler
// reaches runJob directly when it has run-level
// process facts to add (the object-cache socket).
func RunJob(
	ctx context.Context,
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
	versions RunVersions,
	onEvent EventHandler,
) (*JobResult, error) {
	return runJob(ctx, ws, job, commandParams, configDefaults, versions, nil, onEvent)
}

// RunJobWithProcessEnv is RunJob plus process-spawn facts the caller owns
// (compose: PORT, NODE_ENV, CONFIG_DATA). It is not a cache input: serve jobs
// are cache:false.
//
// The entries reach the subprocess environment and nothing else — not the job
// context file, not the task params, not a key — exactly like the scheduler's
// own run-level entries. A caller that also needs the spawned process group
// (to record it for orphan recovery) derives ctx with
// WithProcessGroupObserver.
func RunJobWithProcessEnv(
	ctx context.Context,
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
	versions RunVersions,
	processEnv []string,
	onEvent EventHandler,
) (*JobResult, error) {
	return runJob(ctx, ws, job, commandParams, configDefaults, versions, processEnv, onEvent)
}

// processGroupObserverKey carries the callback runJob reports a spawned
// process group to.
type processGroupObserverKey struct{}

// WithProcessGroupObserver returns a context under which every job subprocess
// runJob starts reports its process group id to observe, once, right after the
// spawn and before its first event is read. The group id is the only handle
// that outlives the CLI: a supervisor that dies without canceling its jobs
// leaves them running, and the next invocation can only terminate what it
// recorded.
func WithProcessGroupObserver(ctx context.Context, observe func(pgid int)) context.Context {
	if observe == nil {
		return ctx
	}
	return context.WithValue(ctx, processGroupObserverKey{}, observe)
}

// observeProcessGroup reports pgid to the context's observer, if any.
func observeProcessGroup(ctx context.Context, pgid int) {
	if observe, ok := ctx.Value(processGroupObserverKey{}).(func(pgid int)); ok {
		observe(pgid)
	}
}

// runJob is RunJob with the run-level process environment the scheduler owns.
// extraEnv carries facts about HOW this process spawns the job — never task
// inputs — so it reaches the subprocess and nothing else: not the job context,
// not taskParams, not a cache key.
func runJob(
	ctx context.Context,
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
	versions RunVersions,
	extraEnv []string,
	onEvent EventHandler,
) (*JobResult, error) {
	// Production reaches this point with App/Engine already captured. Keep this
	// direct API fail-closed too: package tests and the interactive adapter may
	// invoke a job without going through the scheduler.
	ctx = CaptureProcessCapabilities(ctx)
	onEvent = processCapabilityEventSink(ctx, job, onEvent)
	start := time.Now()

	ctx, cancel, inv, err := prepareJobInvocation(ctx, ws, job, commandParams, configDefaults, versions, extraEnv)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { _ = os.Remove(inv.contextFile) }()

	// Ensure output directory exists. A prior cache hit may have left
	// OutputPath as a symlink into the immutable CAS. Detach it into a writable
	// real copy before the job starts: every pipeline step shares this command
	// directory, so deleting the link would also delete sibling outputs (and can
	// make a cached executable vanish from under a concurrent job). A dangling
	// link is removed and recreated empty.
	//
	// The scheduler normally detaches first while coordinating this command
	// output with sibling tasks and has more context about the shared directory.
	// This call stays unconditional anyway, because it is the last guard before the
	// subprocess starts and it also covers jobs the scheduler skips (noOutput and
	// capture tasks still receive this OutputPath) and callers that run a job
	// without a scheduler. On an already-detached path it costs one Lstat.
	if err := store.MaterializeDirSymlink(inv.jobCtx.OutputPath); err != nil {
		return nil, fmt.Errorf("materialize cached output symlink: %w", err)
	}
	if err := os.MkdirAll(inv.jobCtx.OutputPath, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	// Spawn subprocess
	cmd, err := inv.jobCommand(ctx, job, applyHeldOutputLocks(ctx,
		scopeProcessCapabilities(ctx, applyTaskDeadline(applyCPUBudget(inv.env, job), job), job)))
	if err != nil {
		return nil, err
	}
	waitDone := make(chan struct{})
	// Start the subprocess as the root of its own process tree so cancellation
	// can terminate the whole tree (parent + children) reliably.
	tree := proctree.New(cmd)

	// On context cancellation, ask the tree to exit (SIGTERM, not SIGKILL) so
	// it can shut down gracefully. Force-kill after 5 seconds if still running.
	cmd.Cancel = func() error {
		err := tree.Terminate()
		go forceKillProcessGroupAfter(waitDone, tree, processGroupKillDelay)
		return err
	}
	cmd.WaitDelay = 5 * time.Second

	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	defer func() { _ = stdout.Close() }()

	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	defer func() { _ = stderr.Close() }()

	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	// Timestamp immediately before Start so this includes the fork/exec boundary,
	// while excluding context construction and output-directory preparation.
	spawnedAt := time.Now()
	if err := inv.startJob(job, tree.Start); err != nil {
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		if missingWorkingDir(inv.cwd) {
			return nil, fmt.Errorf("start subprocess %s in %s: %w", inv.command, inv.cwd, err)
		}
		if missingCommandBinary(err) {
			return nil, missingBinaryError(job, inv.command, err)
		}
		return nil, fmt.Errorf("start subprocess %s: %w", inv.command, err)
	}
	// Closing the tree ends nothing on Unix; on Windows it ends every process
	// the job left behind once its pipes are drained.
	defer func() { _ = tree.Close() }()
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	observeProcessGroup(ctx, tree.ID())

	// Read stdout (JSONL events) and stderr concurrently
	eventsCh := make(chan jobReadResult, 1)
	stderrCh := make(chan string, 1)
	waitCh := make(chan error, 1)
	var spawnToFirstEvent time.Duration
	firstEventObserved := false
	observeValidEvent := func() {
		if !firstEventObserved {
			spawnToFirstEvent = time.Since(spawnedAt)
			firstEventObserved = true
		}
	}

	go func() {
		events, result := readJSONLEventsObserved(
			stdout,
			onEvent,
			workspacePathEventMapper(ws.Root, inv.projRoot, inv.cwd),
			observeValidEvent,
		)
		eventsCh <- jobReadResult{events: events, result: result}
	}()

	go func() {
		stderrCh <- iox.ReadCapped(stderr, iox.DefaultReadCap)
	}()

	go func() {
		waitCh <- cmd.Wait()
		close(waitDone)
	}()

	// Wait for process exit. Two walls are taken here, not one: `duration` is the
	// TASK's attempt (it starts before the context file and the output directory
	// are prepared), while `spawnWall` is the SUBPROCESS's own, which is what the
	// physical execution record reports because it is the wall the rusage below
	// accounts for.
	waitErr := <-waitCh
	duration := time.Since(start)
	spawnWall := time.Since(spawnedAt)

	// A job that starts a background process and exits can leave stdout/stderr
	// inherited by the descendant. Without this cleanup the parent runner waits
	// forever for EOF even though the job process already exited.
	_ = tree.Terminate()
	evResult := waitForJobRead(eventsCh, stdout, tree)
	stderrText := waitForJobStderr(stderrCh, stderr, tree)

	// Build result
	result := evResult.result
	if result == nil {
		// No result event was parsed from stdout. Default based on exit code:
		// exit 0 → success, non-zero → failed.
		if waitErr == nil {
			result = &JobResult{Status: "success"}
		} else {
			result = &JobResult{Status: "failed"}
		}
	}
	result.Events = evResult.events
	result.Duration = clampWallToFirstEvent(duration, spawnToFirstEvent, firstEventObserved)
	result.SpawnToFirstEvent = spawnToFirstEvent
	result.FirstEventObserved = firstEventObserved
	// The ONE spawn site is the only place a process's resource accounting still
	// exists, so it is where the physical execution is recorded — id, wall, CPU
	// split into user and system, peak RSS, block IO, and the concurrency this
	// process was granted. CPUTime keeps its own field because the learned task
	// weights read it per task; the execution is what a cost roll-up reads,
	// exactly once per subprocess however many task records it produces.
	if ps := cmd.ProcessState; ps != nil {
		result.Execution = captureExecution(ps, spawnWall, job.CPUBudget)
		result.CPUTime = result.Execution.CPUTime()
	}

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}

		// Context timeout/cancel
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				result.Status = "failed"
				// The structural marker every consumer of "did this fail
				// because of its inputs?" reads instead of the message. The
				// failure cache refuses to record a timeout, because the
				// deadline depends on host load and not on the cache key.
				result.TimedOut = true
				if result.Error == nil {
					result.Error = &JobError{Message: "job timed out"}
				}
			} else {
				result.Status = "canceled"
				result.Error = nil // drop any subprocess noise
			}
			return redactProcessCapabilityResult(ctx, job, result), nil
		}

		// Non-zero process exits always fail the job. A result event is useful
		// structured context, but it cannot downgrade the subprocess exit code.
		if result.Status != "failed" {
			result.Status = "failed"
		}
		if result.Error == nil {
			result.Error = jobErrorFromWaitFailure(waitErr, stderrText)
		}
	}

	return redactProcessCapabilityResult(ctx, job, result), nil
}

// jobErrorFromWaitFailure builds the JobError to surface for a non-zero
// process exit. When stderr captured any output, the last 50 lines are used
// for context. When stderr is empty (e.g. the subprocess was killed by a
// signal before it could write anything), the wait error itself is surfaced
// so the job doesn't render as "(no details emitted)".
func jobErrorFromWaitFailure(waitErr error, stderrText string) *JobError {
	if stderrText == "" {
		return &JobError{Message: waitErr.Error()}
	}

	// Take last 50 lines of stderr for better error context
	lines := strings.Split(strings.TrimSpace(stderrText), "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	return &JobError{Message: strings.Join(lines, "\n")}
}

func waitForJobRead(ch <-chan jobReadResult, pipe io.Closer, tree *proctree.Tree) jobReadResult {
	select {
	case result := <-ch:
		return result
	case <-time.After(orphanPipeDrainDelay):
		_ = tree.Kill()
		_ = pipe.Close()
		return <-ch
	}
}

func waitForJobStderr(ch <-chan string, pipe io.Closer, tree *proctree.Tree) string {
	select {
	case result := <-ch:
		return result
	case <-time.After(orphanPipeDrainDelay):
		_ = tree.Kill()
		_ = pipe.Close()
		return <-ch
	}
}

func forceKillProcessGroupAfter(done <-chan struct{}, tree *proctree.Tree, delay time.Duration) {
	select {
	case <-done:
	case <-time.After(delay):
		_ = tree.Kill()
	}
}
