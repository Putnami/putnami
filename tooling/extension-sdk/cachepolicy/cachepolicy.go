// Package cachepolicy is the neutral half of an extension-owned machine cache.
//
// An earlier migration moved the Go and Bun collectors out of the CLI and into
// the extensions that own those caches. What was left over after the move is
// this: the eviction arithmetic, the cross-process lock, and the JSONL summary
// wire format — none of which is language-specific, and all three of which were
// about to be copy-pasted into every extension that keeps a cache.
//
// The split is deliberate:
//
//   - The EXTENSION decides what a cache entry is. Only the Go extension knows
//     that a build-cache file is a 66-character hex name under a two-character
//     shard and that the module cache is immutable; only the TypeScript
//     extension knows that a Bun package directory is one with a package.json
//     in it. Discovery therefore stays in the extension and arrives here as a
//     slice of Entry.
//
//   - This package decides WHICH of those entries go. Sort by recency, evict to
//     a low watermark, never touch anything inside the grace window, and report
//     what happened. That policy is identical for every ecosystem, and getting
//     it subtly different per extension is exactly how one of them ends up
//     deleting a cache another process is mid-read of.
//
// GRACE IS NOT DECORATION. A concurrent build does not uniformly tolerate a
// vanished cache entry: a compiler already handed an entry's path fails outright
// when the file disappears underneath it, even though a lookup that never found
// it would have been an ordinary miss. The lock here only excludes other
// collectors — it cannot exclude the toolchain — so the grace window is the only
// thing standing between a background collection and a broken build.
package cachepolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one eviction candidate discovered by the extension.
type Entry struct {
	// Path is the absolute path of the file or directory to remove.
	Path string
	// Size is the bytes reclaimed by removing it (for a directory, the summed
	// size of the tree).
	Size int64
	// LastUsed is the recency the eviction order is taken over. Whether it is a
	// true access time or merely a download/extraction time is the extension's
	// business; the policy only requires that older means less valuable.
	LastUsed time.Time
	// Dir marks an entry that must be removed as a tree rather than a file.
	Dir bool
}

// Options configures one collection pass.
type Options struct {
	// MaxBytes is the byte budget for the WHOLE cache — including bytes the
	// extension accounted but declared no candidate for. Zero or negative
	// disables collection entirely rather than collecting everything: a budget
	// nobody set is not a budget of zero.
	MaxBytes int64
	// Grace spares any entry used within this window. Zero opts out of
	// protection, which is only ever correct for an explicit, user-initiated
	// clean.
	Grace time.Duration
	// FloorBytes keeps at least this many candidate bytes alive even when the
	// budget cannot be met. It exists for caches whose unevictable part (Go's
	// immutable module cache, say) can alone exceed the budget: without a floor
	// the loop deletes every evictable entry, produces a machine-wide cold
	// cache, and STILL does not reach the target — on every single pass.
	FloorBytes int64
	// Now overrides the clock recency is measured against. Zero means time.Now.
	Now time.Time
	// NonBlocking skips the pass rather than waiting when another collector
	// holds the lock.
	NonBlocking bool
}

// Result summarizes one collection pass.
type Result struct {
	// ScannedEntries is the number of eviction candidates considered.
	ScannedEntries int
	// TotalBytes is the whole cache's size as the extension accounted it.
	TotalBytes int64
	// EvictedEntries and FreedBytes are what this pass actually removed.
	EvictedEntries int
	FreedBytes     int64
	// RemainingBytes is TotalBytes less FreedBytes.
	RemainingBytes int64
	// Skipped reports that another collector held the lock and this pass did
	// nothing. It is not an error: the other collector is doing the same work.
	Skipped bool
}

// OverBudget reports whether the cache is still above its budget after the
// pass — the honest signal that grace-protected or unevictable bytes are what
// remain, rather than a silent "collected, all good".
func (r *Result) OverBudget(maxBytes int64) bool {
	return r != nil && maxBytes > 0 && r.RemainingBytes > maxBytes
}

// LockFileName is the advisory lock file every collector for a given root
// takes. It lives inside the root so two collectors of the same cache contend
// and collectors of different caches never do.
const LockFileName = ".putnami-cache-gc.lock"

// lowWatermarkDivisor sets the collection target at 80% of the budget. Evicting
// exactly to the budget would make the next byte written trigger another full
// pass; leaving headroom means a cache collects in bursts rather than
// continuously.
const lowWatermarkDivisor = 5

// Collect evicts the oldest candidates until the cache fits its budget.
//
// totalBytes is the whole cache as the extension measured it, which is NOT
// necessarily the sum of candidates: bytes an extension refuses to evict still
// count against the budget, because the budget is about disk use rather than
// about what is convenient to delete.
//
// A missing root, an empty candidate set, a cache already within budget and a
// contended lock are all ordinary "nothing to do" outcomes. Only a genuinely
// broken filesystem is an error.
func Collect(root string, totalBytes int64, candidates []Entry, opts Options) (*Result, error) {
	res := &Result{ScannedEntries: len(candidates), TotalBytes: totalBytes, RemainingBytes: totalBytes}
	if root == "" {
		return res, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return nil, err
	}
	if opts.MaxBytes <= 0 || totalBytes <= opts.MaxBytes || len(candidates) == 0 {
		return res, nil
	}

	lock, err := Acquire(root, opts.NonBlocking)
	if err != nil {
		if opts.NonBlocking && errors.Is(err, ErrBusy) {
			res.Skipped = true
			return res, nil
		}
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	for _, victim := range SelectVictims(candidates, totalBytes, opts) {
		var removeErr error
		if victim.Dir {
			removeErr = os.RemoveAll(victim.Path)
		} else {
			removeErr = os.Remove(victim.Path)
		}
		if removeErr != nil {
			// A candidate that vanished (or that another process is holding) is
			// not a failed collection: the next pass reconsiders it.
			continue
		}
		res.EvictedEntries++
		res.FreedBytes += victim.Size
		res.RemainingBytes -= victim.Size
	}
	return res, nil
}

// SelectVictims returns the candidates to evict, oldest first, stopping as soon
// as the low watermark is reachable. It is deterministic: equal recencies break
// by path, so two runs over the same cache choose the same victims.
//
// The target is expressed on the CANDIDATE side. Measuring progress against the
// whole cache instead makes the target unreachable the moment the unevictable
// part alone exceeds the watermark — and an unreachable target means the loop
// runs to the end of the list every time.
func SelectVictims(candidates []Entry, totalBytes int64, opts Options) []Entry {
	if opts.MaxBytes <= 0 || totalBytes <= opts.MaxBytes {
		return nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	grace := opts.Grace
	if grace < 0 {
		grace = 0
	}

	var candidateBytes int64
	ordered := make([]Entry, len(candidates))
	copy(ordered, candidates)
	for _, entry := range ordered {
		candidateBytes += entry.Size
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].LastUsed.Equal(ordered[j].LastUsed) {
			return ordered[i].Path < ordered[j].Path
		}
		return ordered[i].LastUsed.Before(ordered[j].LastUsed)
	})

	lowWatermark := opts.MaxBytes - opts.MaxBytes/lowWatermarkDivisor
	unevictable := totalBytes - candidateBytes
	target := lowWatermark - unevictable
	if opts.FloorBytes > 0 && target < opts.FloorBytes {
		target = opts.FloorBytes
	}

	remaining := candidateBytes
	var victims []Entry
	for _, entry := range ordered {
		if remaining <= target {
			break
		}
		if grace > 0 && now.Sub(entry.LastUsed) < grace {
			continue
		}
		victims = append(victims, entry)
		remaining -= entry.Size
	}
	return victims
}

// RemoveTree deletes a whole cache subtree and returns the bytes reclaimed.
// It is the `clean` primitive: no grace, no budget, no lock — an explicit clean
// accepts racing an in-flight build, which is why a COLLECTOR must never use it.
func RemoveTree(root string) int64 {
	size := TreeSize(root)
	if size == 0 {
		return 0
	}
	if err := os.RemoveAll(root); err != nil {
		return 0
	}
	return size
}

// TreeSize sums every regular file under root. A missing tree is 0, not an
// error: "how big is the cache I have not created yet" has an obvious answer.
func TreeSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// PruneEmptyDirs removes directories that emptied out under root, deepest
// first, leaving root itself in place.
func PruneEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
	}
}

// WriteSummary emits the one event core reads back from a cache command: the
// bytes this extension reclaimed. The shape is the runtime protocol's summary
// event, so a cache command's stdout is an ordinary JSONL stream rather than a
// private format.
func WriteSummary(out io.Writer, freedBytes int64) error {
	event := map[string]any{
		"v":    1,
		"type": "summary",
		"data": map[string]any{"freedBytes": freedBytes},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(encoded))
	return err
}

// EnvInt64 reads a positive int64 from an environment variable, falling back on
// absence or on any unusable value. A malformed override is treated as absent
// rather than as zero: "0" from a typo would otherwise disable the budget.
func EnvInt64(getenv func(string) string, name string, fallback int64) int64 {
	if getenv == nil {
		return fallback
	}
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// EnvDuration reads a non-negative duration from an environment variable. Zero
// IS accepted here, because "no grace" is a meaningful, deliberate choice.
func EnvDuration(getenv func(string) string, name string, fallback time.Duration) time.Duration {
	if getenv == nil {
		return fallback
	}
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}
