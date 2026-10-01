package jobs

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// toolVersionProbeTimeout bounds the ambient `<tool> --version` subprocess. The
// probe gates every cache key that folds an ambient tool in, so an unbounded one
// wedges the whole scheduler before any task starts.
//
// The value is deliberately far above what a healthy `--version` costs
// (milliseconds) because tripping the deadline changes the resolved cache
// IDENTITY: a probe that times out on a slow-but-working tool would key that run
// differently from every other run on the same machine. Five seconds absorbs a
// cold page-in of a ~100MB binary on a loaded CI box while still bounding a
// wedged one to a single short stall per process.
const toolVersionProbeTimeout = 5 * time.Second

// toolVersionProbeWaitDelay bounds the drain AFTER the deadline kills the child.
// Killing a process does not close pipes a grandchild inherited, so without this
// os/exec's Wait can outlive the deadline it was given.
const toolVersionProbeWaitDelay = time.Second

// toolVersionProbeOutputLimit caps what the probe keeps from the child, so a
// binary that writes forever costs a fixed number of bytes instead of the
// scheduler's memory. It is orders of magnitude above any real version line;
// exceeding it is evidence the executable is not answering the question, not
// evidence of a long version.
const toolVersionProbeOutputLimit = 4096

// toolVersionMaxLen rejects a "version" too long to be one. It keeps a degraded
// identity legible in a key dump and in `--debug` output.
const toolVersionMaxLen = 64

// Reserved cache identities for a tool that could not be identified. Each
// failure mode gets a DISTINCT token rather than collapsing onto one:
//
//   - "unavailable" and "unknown" are unchanged from the earlier scheme, so no
//     existing key moves. "unavailable" means the tool is not on PATH (an
//     ordinary environment for a workspace that does not use it, where its tasks
//     cannot run anyway) or that it failed to start / exited non-zero; "unknown"
//     means it succeeded and printed nothing.
//   - "timeout" and "invalid" are new, and are separate from "unavailable"
//     because they describe a materially different machine: a tool that IS on
//     PATH and CAN run tasks, but whose version this process could not learn.
//     Folding them into "unavailable" would let a host whose tool hangs share one
//     cache identity with a host that does not have the tool at all, and would
//     hide the degradation from the key, from `--debug`, and from the warning
//     resolveAmbientToolVersion emits.
//
// The cost of distinct tokens is accepted deliberately: a transient timeout
// yields a different key than both a healthy probe and a missing tool, so that
// run misses the cache and writes under the degraded identity. That is the safe
// direction — a probe that never learned the compiler's version must not serve,
// or be served by, artifacts a known-good compiler produced. The failure mode is
// an extra miss, never a wrong hit.
const (
	toolVersionUnavailable = "unavailable"
	toolVersionUnknown     = "unknown"
	toolVersionTimeout     = "timeout"
	toolVersionInvalid     = "invalid"
)

// resolveBunVersionForCache is cached for one CLI invocation: every TypeScript
// task needs the same ambient Bun identity, and resolving it once avoids a
// subprocess per task without letting a process observe two toolchains.
//
// A degraded result is memoized like any other. Re-probing on every task would
// multiply a wedged binary's stall by the task count, and — worse — a process
// that probed twice could hand two different identities to two tasks in the same
// run. One probe, one identity, whatever it says.
//
// The deadline is the probe's OWN, not a run context threaded down from the
// caller. Neither call site (computeJobCacheHash, Scheduler.readyBatchKey) holds
// one, and threading one into a process-memoized value would be actively wrong:
// the MCP server runs many `run_jobs` requests, each with its own context, in a
// single process (internal/mcp/adapter.go -> engine.Run), so one canceled
// request would poison every later request's cache identity. Resolving it during
// preflight instead was rejected for the laziness the memo already buys — a run
// with no task of that ecosystem must not pay a subprocess at all.
// resolveAmbientToolVersionWith keeps process lookup and warning delivery
// explicit for focused callers. Tests can provide local dependencies without
// mutating package globals, so independent tool probes remain safe to run
// concurrently.
func resolveAmbientToolVersionWith(
	tool string,
	lookup func(string) (string, error),
	logger *slog.Logger,
) string {
	path, err := lookup(tool)
	if err != nil {
		return toolVersionUnavailable
	}
	version := probeToolVersion(path, toolVersionProbeTimeout)
	if isDegradedToolVersion(version) {
		// A degraded identity is not a failure — the run proceeds — but it does
		// mean this run's cache entries for that ecosystem are not pinned to a
		// tool version, which is worth saying out loud exactly once.
		logger.Warn("cache key: ambient tool version probe degraded; cache identity is not pinned to a tool version",
			"tool", tool, "path", path, "identity", version, "timeout", toolVersionProbeTimeout)
	}
	return version
}

// probeToolVersion runs `<path> --version` under a deadline and a byte cap, and
// returns the version string a healthy tool printed or one of the reserved
// degraded identities.
func probeToolVersion(path string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out := &boundedBuffer{limit: toolVersionProbeOutputLimit}
	cmd := exec.CommandContext(ctx, path, "--version")
	// One writer for both streams reproduces CombinedOutput byte for byte:
	// os/exec gives an identical Stdout and Stderr a single pipe and a single
	// copier, so the happy-path value is exactly what it was before this
	// bound was added, and no synchronization is needed around the buffer.
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = toolVersionProbeWaitDelay

	if err := cmd.Run(); err != nil {
		// The deadline is read before the error, not instead of it: killing a
		// child reports a generic signal failure, and calling THAT "unavailable"
		// would hide the hang this exists to bound. Reading it only when the run
		// failed keeps a probe that answered a nanosecond before its deadline from
		// being thrown away.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return toolVersionTimeout
		}
		return toolVersionUnavailable
	}
	if out.truncated {
		return toolVersionInvalid
	}

	version := strings.TrimSpace(string(out.buf))
	switch {
	case version == "":
		return toolVersionUnknown
	case len(version) > toolVersionMaxLen, strings.ContainsAny(version, "\r\n"):
		return toolVersionInvalid
	case isDegradedToolVersion(version):
		// An executable that printed a reserved token as its version would
		// otherwise collide with a degraded identity. Mapping it onto a degraded
		// token keeps the invariant one-directional: a reserved value always
		// means "not identified", never "identified as this".
		return toolVersionInvalid
	}
	return version
}

func isDegradedToolVersion(version string) bool {
	switch version {
	case toolVersionUnavailable, toolVersionUnknown, toolVersionTimeout, toolVersionInvalid:
		return true
	}
	return false
}

// boundedBuffer keeps the first limit bytes written to it and discards the rest,
// recording that it did.
//
// It keeps draining after the cap instead of erroring: an io.Writer that fails
// makes os/exec abandon the pipe copy, which blocks the child on a full pipe and
// leaves Wait stuck until the deadline. Accepting and dropping bytes lets a
// merely chatty binary exit on its own while a truly endless one still costs a
// constant amount of memory.
type boundedBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
			return len(p), nil
		}
		b.buf = append(b.buf, p[:room]...)
	}
	if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

// cacheToolchainVersion returns the CLI runtime identity used by every task.
// TypeScript tasks additionally include the ambient Bun version because Bun
// produces their transpilation, type-check, test, and compile outputs. The
// static extension version and declared lockfile inputs do not change when a
// user upgrades Bun in place, so omitting this signal would restore artifacts
// produced by a different compiler.
func cacheToolchainVersion(job *ScheduledJob) string {
	base := runtime.Version()
	if job == nil || job.Extension == nil || job.JobDef == nil {
		return base
	}
	if identity := runtimeToolchainIdentityForRefs(job.Extension, job.JobDef.Toolchains); identity != "" {
		return base + ";runtime-tools=" + identity
	}
	return base
}
