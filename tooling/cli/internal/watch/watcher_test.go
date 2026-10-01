package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewWatcher_Defaults(t *testing.T) {
	w := NewWatcher(WatcherConfig{Roots: []string{"/tmp"}})

	if w.cfg.Debounce != DefaultDebounce {
		t.Errorf("Debounce = %v, want %v", w.cfg.Debounce, DefaultDebounce)
	}
	if w.cfg.PollInterval != DefaultPollInterval {
		t.Errorf("PollInterval = %v, want %v", w.cfg.PollInterval, DefaultPollInterval)
	}
	if len(w.cfg.IgnorePatterns) == 0 {
		t.Error("expected default ignore patterns")
	}
}

func TestWatcher_ShouldIgnore(t *testing.T) {
	w := NewWatcher(WatcherConfig{
		Roots:          []string{"/tmp"},
		IgnorePatterns: []string{".git", "node_modules", "*.tmp"},
	})

	tests := []struct {
		name   string
		ignore bool
	}{
		{".git", true},
		{"node_modules", true},
		{"src", false},
		{"main.go", false},
	}

	for _, tt := range tests {
		if got := w.shouldIgnore(tt.name); got != tt.ignore {
			t.Errorf("shouldIgnore(%q) = %v, want %v", tt.name, got, tt.ignore)
		}
	}
}

func TestWatcher_TakeSnapshot(t *testing.T) {
	dir := t.TempDir()

	// Create some test files
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "lib.go"), []byte("package pkg"), 0o644)

	// Create an ignored directory
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644)

	w := NewWatcher(WatcherConfig{Roots: []string{dir}})

	if err := w.TakeSnapshot(); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	// Should have main.go and pkg/lib.go but not .git/HEAD
	if len(w.snapshot) != 2 {
		t.Errorf("expected 2 files in snapshot, got %d", len(w.snapshot))
		for k := range w.snapshot {
			t.Logf("  %s", k)
		}
	}
}

func TestWatcher_DetectChanges(t *testing.T) {
	dir := t.TempDir()

	// Create initial file
	mainFile := filepath.Join(dir, "main.go")
	os.WriteFile(mainFile, []byte("package main"), 0o644)

	w := NewWatcher(WatcherConfig{Roots: []string{dir}})
	if err := w.TakeSnapshot(); err != nil {
		t.Fatal(err)
	}

	// No changes initially
	changed := w.detectChanges()
	if len(changed) != 0 {
		t.Errorf("expected 0 changes, got %d: %v", len(changed), changed)
	}

	// Modify a file (need to ensure mtime changes)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(mainFile, []byte("package main\n// modified"), 0o644)

	changed = w.detectChanges()
	if len(changed) != 1 {
		t.Errorf("expected 1 change, got %d: %v", len(changed), changed)
	}

	// Add a new file
	os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main"), 0o644)

	changed = w.detectChanges()
	if len(changed) != 1 {
		t.Errorf("expected 1 new file, got %d: %v", len(changed), changed)
	}

	// Delete a file
	os.Remove(filepath.Join(dir, "new.go"))

	changed = w.detectChanges()
	if len(changed) != 1 {
		t.Errorf("expected 1 deletion, got %d: %v", len(changed), changed)
	}
}

func TestWatcher_Watch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)

	w := NewWatcher(WatcherConfig{
		Roots:        []string{dir},
		Debounce:     50 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	})
	if err := w.TakeSnapshot(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	ch := w.Watch(done)

	// Modify a file after a short delay
	time.Sleep(30 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n// updated"), 0o644)

	// Wait for the change batch
	select {
	case batch := <-ch:
		if len(batch) == 0 {
			t.Error("expected non-empty batch")
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("timed out waiting for change event")
	}

	close(done)
}

func TestMergeChanges(t *testing.T) {
	existing := []string{"a.go", "b.go"}
	newFiles := []string{"b.go", "c.go"}

	merged := mergeChanges(existing, newFiles)
	if len(merged) != 3 {
		t.Errorf("expected 3 merged files, got %d: %v", len(merged), merged)
	}
}

func TestWatcher_RelativePath(t *testing.T) {
	w := NewWatcher(WatcherConfig{Roots: []string{"/home/user/project"}})

	// The relative path keeps this platform's separators; the change-to-project
	// mapping cleans it the same way it cleans the project paths it indexes.
	rel := w.relativePath("/home/user/project/src/main.go")
	if want := filepath.FromSlash("src/main.go"); rel != want {
		t.Errorf("relativePath = %q, want %q", rel, want)
	}

	// Path outside any root
	outside := w.relativePath("/other/file.go")
	if outside != "/other/file.go" {
		t.Errorf("relativePath = %q, want %q", outside, "/other/file.go")
	}
}
