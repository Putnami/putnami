package cache

import "strings"

// Remote-cache eligibility policy. These helpers operate purely on the wire
// contract's own fields so the build-system client and the cloud server make
// the SAME decision about which keys participate in remote caching. They are
// stricter than local caching: a result that is safe to cache locally may
// still be ineligible for the shared remote cache.

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

// BreakEvenParams tunes the transfer-vs-rebuild trade-off used to decide
// whether storing an artifact remotely is worthwhile.
type BreakEvenParams struct {
	// BandwidthBytesPerSec is the assumed effective remote throughput used to
	// predict transfer time from artifact size. Non-positive disables the
	// size check (build duration alone decides).
	BandwidthBytesPerSec int64
	// MinDurationMs is the build-time floor below which an artifact is treated
	// as too cheap to rebuild to be worth remote caching.
	MinDurationMs int64
}

// DefaultBreakEven assumes ~50 MB/s effective throughput and a 200ms
// build-time floor. Measured cache restores run ~70 MB/s on an ordinary dev
// link (batched fetches, warm connections), so 50 keeps headroom while not
// pricing out mid-size artifacts: the previous 10 MB/s assumption permanently
// excluded cross-compile binaries and generated-site outputs whose rebuild
// costs seconds on every cold-store run. The asymmetry favors admitting more —
// an upload the guard wrongly admits costs one bounded async transfer off the
// critical path, while an entry it wrongly excludes re-executes forever.
var DefaultBreakEven = BreakEvenParams{
	BandwidthBytesPerSec: 50 * 1024 * 1024,
	MinDurationMs:        200,
}

// WorthRemoteCaching reports whether an entry of sizeBytes produced by a build
// of durationMs is worth storing remotely.
//
// A files-less entry (sizeBytes <= 0) always is: there is nothing to transfer
// — the status result rides the batched negotiate/exchange for free — while
// excluding it turns its key into a permanent remote miss whose task
// re-executes on every cold-store run. The break-even guard exists to price
// artifact transfer, so it only applies when there are bytes to move: the
// build must then clear the minimum-duration floor, and the predicted transfer
// time must not exceed the build time it would save. An unknown duration
// (<= 0) for a bytes-carrying entry is treated as too cheap and returns false.
func WorthRemoteCaching(durationMs, sizeBytes int64, p BreakEvenParams) bool {
	if sizeBytes <= 0 {
		return true
	}
	if durationMs < p.MinDurationMs {
		return false
	}
	if p.BandwidthBytesPerSec <= 0 {
		return true
	}
	transferMs := sizeBytes * 1000 / p.BandwidthBytesPerSec
	return transferMs <= durationMs
}

// EligibleForRemote reports whether key k should participate in remote caching:
// its task must not be side-effecting and it must clear the break-even guard
// given its recorded DurationMs and SizeBytes.
func EligibleForRemote(k KeyRequest, p BreakEvenParams) bool {
	if SideEffectingTask(k.Task) {
		return false
	}
	return WorthRemoteCaching(k.DurationMs, k.SizeBytes, p)
}
