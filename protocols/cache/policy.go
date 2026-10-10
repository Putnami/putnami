package cache

import "strings"

// Remote-cache eligibility policy. These helpers operate purely on the wire
// contract's own fields so the build-system client and the cloud server make
// the SAME decision about which keys participate in remote caching. A key is
// remote-eligible unless its task is side-effecting: no duration or size rule
// leaves a result out of the shared cache.

// sideEffectingCommands are task commands whose execution has external side
// effects. Their results must never be served from or written to a shared
// cache — doing so would skip the side effect itself (e.g. an artifact that is
// reported as published but never actually pushed).
var sideEffectingCommands = map[string]bool{
	"publish": true,
}

// commandOf returns the root command of a task name, i.e. the segment before
// the first "~" ("publish~npm" → "publish"); a task with no "~" is its own
// command.
func commandOf(task string) string {
	if i := strings.IndexByte(task, '~'); i >= 0 {
		return task[:i]
	}
	return task
}

// SideEffectingTask reports whether task (e.g. "publish~npm") belongs to a
// side-effecting command and must never be remote-cached.
func SideEffectingTask(task string) bool {
	return sideEffectingCommands[commandOf(task)]
}

// BreakEvenParams is the parameter type EligibleForRemote and
// WorthRemoteCaching accept and ignore.
//
// Deprecated: remote eligibility reads only the task (see EligibleForRemote).
// No function of this package reads these parameters.
type BreakEvenParams struct {
	// BandwidthBytesPerSec is ignored.
	BandwidthBytesPerSec int64
	// MinDurationMs is ignored.
	MinDurationMs int64
}

// DefaultBreakEven is a BreakEvenParams value for callers that still pass one.
//
// Deprecated: no function of this package reads it; see BreakEvenParams.
var DefaultBreakEven = BreakEvenParams{
	BandwidthBytesPerSec: 50 * 1024 * 1024,
	MinDurationMs:        200,
}

// WorthRemoteCaching reports true for every entry, whatever its build duration
// or size: a result is shared unless its task is side-effecting.
//
// Deprecated: remote eligibility is !SideEffectingTask(task). The parameters
// are ignored.
func WorthRemoteCaching(_, _ int64, _ BreakEvenParams) bool {
	return true
}

// EligibleForRemote reports whether key k participates in remote caching: it
// does unless its task is side-effecting. The key's DurationMs and SizeBytes
// do not take part in the decision.
//
// Deprecated: use !SideEffectingTask(k.Task). The BreakEvenParams argument is
// ignored.
func EligibleForRemote(k KeyRequest, _ BreakEvenParams) bool {
	return !SideEffectingTask(k.Task)
}
