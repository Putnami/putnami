package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

// The cache lifecycle.
//
// `putnami cache clean` and `putnami cache gc` used to know the caches: the CLI
// shipped a Go collector and a Bun collector and ran them by name. They are now
// fan-outs to the extensions that own those caches, and an extension declares
// its participation the same way it declares everything else — a hidden command
// running a typed task, not a hook.
//
// Three properties of this fan-out are load-bearing:
//
//   - COMPLETE AGGREGATION. Every declaring extension is asked, and every
//     failure is reported. The pre-C5 fan-out returned on the first error, so
//     one broken extension meant the caches of every extension sorted after it
//     were silently never collected — while the command still looked like it
//     had done its job for them.
//
//   - DETERMINISTIC ORDER. Extensions are asked in name order, so two runs on
//     one machine print the same lines in the same order.
//
//   - NO PARTIAL SUCCESS CLAIM. An extension that exits non-zero contributes an
//     error, never a byte count. A purge that failed must not be counted as
//     bytes freed.

// CacheCommandEnv names the environment variable carrying the phase to the
// subprocess, so an extension whose runtime dispatches on argv can also branch
// on the environment.
const CacheCommandEnv = "PUTNAMI_CACHE_COMMAND"

// CacheCommandOutcome is one extension's answer for one cache phase.
type CacheCommandOutcome struct {
	// Extension is the extension asked.
	Extension string
	// FreedBytes is what it reported reclaiming. Always 0 when Err is set.
	FreedBytes int64
	// Err is why this extension's collection failed, or nil.
	Err error
}

// ExtensionsDeclaringCacheCommand returns the extensions declaring the reserved
// cache command, in name order.
//
// Declaration is the manifest command, not a hook and not a name convention: an
// extension participates in `cache clean` exactly when it declares the
// `cache-clean` command, which is a fact a manifest reader can check.
func ExtensionsDeclaringCacheCommand(
	extensions []*extension.ExtensionDescription,
	command string,
) []*extension.ExtensionDescription {
	if !extensionproto.IsCacheCommand(command) {
		return nil
	}
	declaring := make([]*extension.ExtensionDescription, 0, len(extensions))
	for _, ext := range extensions {
		if ext == nil || ext.Jobs == nil {
			continue
		}
		if def, ok := ext.Jobs[command]; ok && def != nil && def.Command != "" {
			declaring = append(declaring, ext)
		}
	}
	sort.Slice(declaring, func(i, j int) bool { return declaring[i].Name < declaring[j].Name })
	return declaring
}

// RunCacheCommands asks every extension in the set to run one cache phase and
// returns one outcome per extension, in the order they were asked.
//
// It never stops early. The caller renders the outcomes and decides what a
// failure means for the exit code; this function's contract is that every
// extension was given its chance.
func RunCacheCommands(
	ctx context.Context,
	ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription,
	command string,
	debug bool,
) []CacheCommandOutcome {
	outcomes := make([]CacheCommandOutcome, 0, len(extensions))
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		freed, err := RunCacheCommand(ctx, ws, ext, command, debug)
		outcomes = append(outcomes, CacheCommandOutcome{Extension: ext.Name, FreedBytes: freed, Err: err})
	}
	return outcomes
}

// RunCacheCommand runs one extension's reserved cache command and returns the
// bytes it reported freeing.
//
// The subprocess receives three roots, and the difference between them is the
// whole point of the C5 contract:
//
//   - PUTNAMI_WORKSPACE_ROOT — this worktree.
//   - PUTNAMI_CACHE_ROOT — the per-worktree mutable scratch every extension
//     shares (<ws>/.putnami/cache).
//   - PUTNAMI_EXTENSION_CACHE_ROOT — the MACHINE-global directory this one
//     extension owns, shared across every repository and worktree on the host.
//
// Core creates the machine root and never looks inside it. What an extension
// keeps there, and how it evicts from it, is the extension's business — that is
// the subtraction this slice performs.
func RunCacheCommand(
	ctx context.Context,
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
	command string,
	debug bool,
) (int64, error) {
	if ws == nil || ext == nil {
		return 0, nil
	}
	if !extensionproto.IsCacheCommand(command) {
		return 0, fmt.Errorf("unknown cache command %q", command)
	}
	def := ext.Jobs[command]
	if def == nil || def.Command == "" {
		return 0, nil
	}

	scratchRoot := filepath.Join(ws.Root, ".putnami", "cache")
	machineRoot := extensionproto.MachineCacheRoot(ext.Name, ws.Root)
	if machineRoot != "" {
		// Create it before the call so the extension can assume it exists — the
		// alternative is every extension re-implementing "mkdir -p my own root".
		// A failure is not fatal: the extension may collect caches that live
		// elsewhere entirely (Bun's native ~/.bun/install/cache does).
		_ = os.MkdirAll(machineRoot, 0o755)
	}

	tmplVars := buildHookTemplateVars(ws.Root, ws.Root, ext, "")
	tmplVars["cacheRoot"] = scratchRoot

	resolvedCommand := extension.ExpandTemplateVars(def.Command, tmplVars)
	resolvedArgs := make([]string, len(def.Args))
	for i, arg := range def.Args {
		resolvedArgs[i] = extension.ExpandTemplateVars(arg, tmplVars)
	}
	resolvedCwd := ws.Root
	if def.Cwd != "" {
		resolvedCwd = extension.ExpandTemplateVars(def.Cwd, tmplVars)
	}

	timeoutMs := def.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = DefaultHookTimeoutMs
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, resolvedCommand, resolvedArgs...)
	childLease, err := store.AttachScratch(cmd, ws.Root)
	if err != nil {
		return 0, err
	}
	defer func() { _ = childLease.Close() }()
	cmd.Dir = resolvedCwd
	cmd.Env = cacheCommandEnv(ws, ext, def, command, scratchRoot, machineRoot, tmplVars)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("%s stdout pipe: %w", command, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, fmt.Errorf("%s stderr pipe: %w", command, err)
	}
	// A hosted run hands its credential to no process started after this one.
	runcredential.MarkRepositoryCodeStarted("job " + ext.Name + " " + command)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s %s: %w", ext.Name, command, err)
	}

	resultCh := make(chan *HookResult, 1)
	go func() { resultCh <- readHookEvents(stdout, debug) }()
	stderrCh := make(chan string, 1)
	go func() { stderrCh <- iox.ReadCapped(stderr, iox.DefaultReadCap) }()

	result := <-resultCh
	stderrText := <-stderrCh

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("%s %s timed out after %dms", ext.Name, command, timeoutMs)
		}
		errMsg := fmt.Sprintf("%s %s failed", ext.Name, command)
		if stderrText != "" {
			lines := strings.Split(strings.TrimSpace(stderrText), "\n")
			if len(lines) > 50 {
				lines = lines[len(lines)-50:]
			}
			errMsg += ": " + strings.Join(lines, "\n")
		}
		return 0, fmt.Errorf("%s", errMsg)
	}

	if result == nil {
		return 0, nil
	}
	return result.FreedBytes, nil
}

// StartDetachedCacheGC launches an extension's cache-gc command as a detached
// background process and returns whether it started.
//
// Detached is the point. An exact collection walks the cache, which can be
// gigabytes; doing that on the way out of a build would put a multi-second walk
// on the foreground path of a run that has already produced its outputs. The
// caller throttles (see OpportunisticCacheGCDue), so at most one of these
// starts per extension per interval, and the extension's own collector lock
// collapses any that overlap anyway.
func StartDetachedCacheGC(
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
) bool {
	resolved, tmplVars, ok := resolveCacheGCCommand(ws, ext)
	if !ok {
		return false
	}
	def := ext.Jobs[extensionproto.CommandCacheGC]
	scratchRoot := tmplVars["cacheRoot"]
	machineRoot := extensionproto.MachineCacheRoot(ext.Name, ws.Root)

	cmd := exec.Command(resolved.command, resolved.args...)
	childLease, err := store.AttachScratch(cmd, ws.Root)
	if err != nil {
		return false
	}
	defer func() { _ = childLease.Close() }()
	cmd.Dir = ws.Root
	cmd.Env = cacheCommandEnv(ws, ext, def, extensionproto.CommandCacheGC, scratchRoot, machineRoot, tmplVars)
	// A hosted run hands its credential to no process started after this one.
	runcredential.MarkRepositoryCodeStarted("job " + ext.Name + " " + extensionproto.CommandCacheGC)
	// Outside every process tree this CLI runs in: on Windows a tree's Job
	// Object would otherwise end the collection when that tree closes.
	started, err := proctree.StartDetached(cmd)
	if err != nil {
		return false
	}
	_ = started.Process.Release()
	return true
}

// resolvedCacheCommand is one extension's fully expanded cache-gc invocation.
type resolvedCacheCommand struct {
	command string
	args    []string
}

// resolveCacheGCCommand expands an extension's cache-gc command, reporting
// whether it can be run at all.
//
// A command still carrying a template token — {extensionRuntime}, in practice —
// means this extension's runtime was never prepared in this run. Preparing one
// HERE, after the build, for a best-effort collection, is exactly the kind of
// surprise cost the opportunistic path exists to avoid.
func resolveCacheGCCommand(
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
) (resolvedCacheCommand, map[string]string, bool) {
	if ws == nil || ext == nil {
		return resolvedCacheCommand{}, nil, false
	}
	def := ext.Jobs[extensionproto.CommandCacheGC]
	if def == nil || def.Command == "" {
		return resolvedCacheCommand{}, nil, false
	}

	tmplVars := buildHookTemplateVars(ws.Root, ws.Root, ext, "")
	tmplVars["cacheRoot"] = filepath.Join(ws.Root, ".putnami", "cache")

	command := extension.ExpandTemplateVars(def.Command, tmplVars)
	if command == "" || strings.Contains(command, "{") {
		return resolvedCacheCommand{}, nil, false
	}
	args := make([]string, len(def.Args))
	for i, arg := range def.Args {
		args[i] = extension.ExpandTemplateVars(arg, tmplVars)
	}
	return resolvedCacheCommand{command: command, args: args}, tmplVars, true
}

// OpportunisticGCStampFile is the throttle stamp core keeps in each extension's
// machine cache root. Core owns the SCHEDULE (how often a collection may start)
// and the extension owns the POLICY (what a collection does) — which is what
// lets the throttle cost one os.Stat per extension instead of one subprocess.
const OpportunisticGCStampFile = ".putnami-gc-stamp"

// opportunisticGCStampLockFile serializes the check-and-reserve operation
// across worktrees that share one extension cache root. The stamp itself is a
// timestamp for cheap non-due checks; it cannot atomically represent both an
// expired-window check and a new reservation.
const opportunisticGCStampLockFile = ".putnami-gc-stamp.lock"

// OpportunisticCacheGCInterval bounds how often a given extension's collector
// may be started from a finishing run. It matches the interval the deleted
// Go/Bun collectors used, so C5 changes who collects, not how often.
const OpportunisticCacheGCInterval = time.Hour

// MaybeOpportunisticCacheGC starts a detached `cache-gc` for every extension
// whose throttle window has elapsed, and returns the ones it started.
//
// Two properties keep this off the hot path: the throttle check is ONE os.Stat
// per extension, so a run that is not due spawns nothing at all; and a run that
// IS due starts a detached process rather than waiting for a cache walk that
// can span gigabytes.
//
// Runnability is checked BEFORE the throttle is reserved. An extension whose
// runtime this run never prepared cannot collect anything, and reserving its
// interval anyway would burn the window on a no-op — so its cache would go
// uncollected for an hour every time some unrelated run happened to finish.
func MaybeOpportunisticCacheGC(
	ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription,
	interval time.Duration,
) []string {
	var started []string
	for _, ext := range ExtensionsDeclaringCacheCommand(extensions, extensionproto.CommandCacheGC) {
		if _, _, ok := resolveCacheGCCommand(ws, ext); !ok {
			continue
		}
		if !OpportunisticCacheGCDue(ws, ext, interval) {
			continue
		}
		if StartDetachedCacheGC(ws, ext) {
			started = append(started, ext.Name)
		}
	}
	return started
}

// OpportunisticCacheGCDue reports whether an opportunistic collection may start
// for this extension now, and RESERVES the interval by refreshing the stamp
// when it says yes.
//
// Reserving before the collection rather than after is deliberate: a collector
// that crashes must not re-trigger on every subsequent run, and two concurrent
// putnami processes must not both decide it is due.
func OpportunisticCacheGCDue(
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
	interval time.Duration,
) bool {
	if ws == nil || ext == nil || interval <= 0 {
		return false
	}
	root := extensionproto.MachineCacheRoot(ext.Name, ws.Root)
	if root == "" {
		return false
	}
	stamp := filepath.Join(root, OpportunisticGCStampFile)
	if info, err := os.Stat(stamp); err == nil && time.Since(info.ModTime()) < interval {
		return false
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false
	}
	// Keep the common, not-due path to one stat above. Once a process sees a
	// missing or expired stamp, take a cross-process lock and check again: a
	// sibling worktree may have reserved the interval just before this lock was
	// acquired. Holding the lock only for this check-and-write makes exactly one
	// caller return true for a window without blocking the detached collector.
	lock, err := flock.Acquire(filepath.Join(root, opportunisticGCStampLockFile), true, false)
	if err != nil {
		return false
	}
	defer func() { _ = lock.Release() }()
	if info, err := os.Stat(stamp); err == nil && time.Since(info.ModTime()) < interval {
		return false
	}
	if err := os.WriteFile(stamp, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		return false
	}
	return true
}

// cacheCommandEnv builds the subprocess environment for one cache command.
func cacheCommandEnv(
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
	def *extension.JobDefinition,
	command string,
	scratchRoot string,
	machineRoot string,
	tmplVars map[string]string,
) []string {
	env := os.Environ()
	env = append(env,
		"PUTNAMI_WORKSPACE_ROOT="+ws.Root,
		"PUTNAMI_EXTENSION_ROOT="+ext.Path,
		"PUTNAMI_EXTENSION_NAME="+ext.Name,
		"PUTNAMI_CACHE_ROOT="+scratchRoot,
		extensionproto.MachineCacheRootEnv+"="+machineRoot,
		CacheCommandEnv+"="+command,
	)
	for k, v := range def.Env {
		env = append(env, k+"="+extension.ExpandTemplateVars(v, tmplVars))
	}
	// A hosted run's cache command downloads nothing and holds no framework
	// credential.
	return runcredential.ChildEnv(env, false)
}
