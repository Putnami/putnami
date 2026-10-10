package jobs

import (
	"runtime"
	"time"
)

// toolVersionProbeTimeout bounds a runtime toolchain's version probe. The
// toolchain's identity keys every task that uses it, so an unbounded probe
// wedges the whole scheduler before any task starts.
//
// The value is deliberately far above what a healthy `--version` costs
// (milliseconds), because a slow but working tool that trips the deadline
// fails its probe. Five seconds absorbs a cold page-in of a ~100MB binary on a
// loaded CI box while still bounding a tool that wedges while it runs to a
// single short stall per process.
//
// The deadline counts from the probe's first instruction (hostProcessDeadline).
// The time the host holds a new process before that, such as darwin's check of
// a freshly written executable, is not charged to it: hostAdmissionBound
// bounds that hold. A tool that blocks before its first sample, such as a
// wrapper waiting on a hung child, reads as held and stalls up to that bound.
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

// toolVersionMaxLen rejects a "version" too long to be one, so a probe answer
// stays legible in an error message.
const toolVersionMaxLen = 64

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
// A task that uses runtime toolchains additionally includes their lock
// identities: a TypeScript task's Bun produces its transpilation, type-check,
// test, and compile outputs, and neither the static extension version nor the
// declared lockfile inputs name that Bun, so omitting this signal would
// restore artifacts produced by a different compiler.
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
