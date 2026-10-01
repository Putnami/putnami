// Package watch implements the --watch flag's trigger and REPLAN POLICY: file
// watching, serve process lifecycle, and the loop that decides when to re-run and
// over which projects.
//
// It runs nothing itself. Each iteration goes back through the engine via
// the injected SessionConfig.RunIteration seam, and the
// change→project mapping is the shared workspace.ProjectsForChangedFiles that
// --impacted uses; the private planner, scheduler, session writer and impact
// classifier this package used to carry are gone.
package watch

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultDebounce is the debounce window for coalescing rapid changes.
const DefaultDebounce = 150 * time.Millisecond

// DefaultPollInterval is the interval between file system polls. 500ms
// keeps idle CPU low on large monorepos — at 100ms the watcher
// re-walked the full project tree ~10×/second while idle, dominating
// CPU on otherwise-quiet watch sessions. Tests can still set
// PollInterval explicitly for fast feedback.
const DefaultPollInterval = 500 * time.Millisecond

// DefaultIgnorePatterns are directory names excluded from watching.
var DefaultIgnorePatterns = []string{
	".git",
	"node_modules",
	".putnami",
	"dist",
	"build",
	"__pycache__",
	".venv",
}

// FileEvent represents a detected file change.
type FileEvent struct {
	Path    string // relative to watched root
	ModTime time.Time
}

// WatcherConfig configures the file watcher.
type WatcherConfig struct {
	// Roots are the directories to watch recursively.
	Roots []string
	// IgnorePatterns are directory names to skip.
	IgnorePatterns []string
	// Debounce is the window to coalesce changes.
	Debounce time.Duration
	// PollInterval is how often to check for changes.
	PollInterval time.Duration
}

// Watcher polls the file system for changes and reports batched events
// after a debounce window. It uses polling (no external dependencies)
// for maximum portability.
type Watcher struct {
	cfg      WatcherConfig
	snapshot map[string]time.Time // path → modTime
	mu       sync.Mutex
}

// NewWatcher creates a file watcher with the given config.
// If IgnorePatterns is empty, DefaultIgnorePatterns is used.
func NewWatcher(cfg WatcherConfig) *Watcher {
	if len(cfg.IgnorePatterns) == 0 {
		cfg.IgnorePatterns = DefaultIgnorePatterns
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	return &Watcher{
		cfg:      cfg,
		snapshot: make(map[string]time.Time),
	}
}

// TakeSnapshot captures the current file system state for all watched roots.
// Call this before Watch to establish the baseline.
func (w *Watcher) TakeSnapshot() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.snapshot = make(map[string]time.Time)
	for _, root := range w.cfg.Roots {
		if err := w.walkRoot(root); err != nil {
			return err
		}
	}
	return nil
}

// Watch polls for changes and sends batches of changed file paths to the
// returned channel. Each batch is debounced: rapid changes within the
// debounce window are coalesced into a single batch.
// The channel is closed when the done channel is closed or receives a value.
func (w *Watcher) Watch(done <-chan struct{}) <-chan []string {
	ch := make(chan []string)

	go func() {
		defer close(ch)

		ticker := time.NewTicker(w.cfg.PollInterval)
		defer ticker.Stop()

		var pending []string
		var debounceTimer *time.Timer
		var debounceCh <-chan time.Time

		for {
			select {
			case <-done:
				return

			case <-ticker.C:
				changed := w.detectChanges()
				if len(changed) > 0 {
					pending = mergeChanges(pending, changed)

					// Reset debounce timer
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					debounceTimer = time.NewTimer(w.cfg.Debounce)
					debounceCh = debounceTimer.C
				}

			case <-debounceCh:
				if len(pending) > 0 {
					// Send batch
					batch := pending
					pending = nil
					debounceCh = nil
					debounceTimer = nil

					select {
					case ch <- batch:
					case <-done:
						return
					}
				}
			}
		}
	}()

	return ch
}

// detectChanges compares the current file system state against the snapshot
// and returns a list of changed file paths (relative to their root).
func (w *Watcher) detectChanges() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	current := make(map[string]time.Time)
	for _, root := range w.cfg.Roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				if w.shouldIgnore(info.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			current[path] = info.ModTime()
			return nil
		})
	}

	var changed []string

	// Check for modified or new files
	for path, modTime := range current {
		oldTime, existed := w.snapshot[path]
		if !existed || !modTime.Equal(oldTime) {
			// Make path relative to its root
			relPath := w.relativePath(path)
			changed = append(changed, relPath)
		}
	}

	// Check for deleted files
	for path := range w.snapshot {
		if _, exists := current[path]; !exists {
			relPath := w.relativePath(path)
			changed = append(changed, relPath)
		}
	}

	// Update snapshot
	w.snapshot = current

	return changed
}

// walkRoot populates the snapshot with files under the given root.
func (w *Watcher) walkRoot(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		if info.IsDir() {
			if w.shouldIgnore(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		w.snapshot[path] = info.ModTime()
		return nil
	})
}

// shouldIgnore returns true if the directory name matches an ignore pattern.
func (w *Watcher) shouldIgnore(name string) bool {
	for _, pattern := range w.cfg.IgnorePatterns {
		if name == pattern {
			return true
		}
		// Support glob patterns
		if strings.ContainsAny(pattern, "*?[") {
			if matched, _ := filepath.Match(pattern, name); matched {
				return true
			}
		}
	}
	return false
}

// relativePath makes a path relative to the closest watched root.
func (w *Watcher) relativePath(path string) string {
	for _, root := range w.cfg.Roots {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}

// mergeChanges deduplicates changed files.
func mergeChanges(existing, new []string) []string {
	seen := make(map[string]bool, len(existing)+len(new))
	result := make([]string, 0, len(existing)+len(new))
	for _, p := range existing {
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	for _, p := range new {
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result
}
