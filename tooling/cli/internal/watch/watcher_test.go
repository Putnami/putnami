package watch

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// TestWatcher_DropsGitIgnoredChanges is issue #233: a log a serve run writes
// into an ignored directory restarted the server on every line. A change Git
// ignores is dropped, including a `log/*` rule that ignores the files but not
// the directory. A dotenv file still counts, such as conf/.env.local.yaml that
// config-merge reads, and so does a tracked file that matches an ignore rule.
func TestWatcher_DropsGitIgnoredChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("init", "-q")
	write(".gitignore", ".context/\n.playwright-mcp/\nlog/*\n!log/.keep\n*.log\n.env\n.env.local.yaml\n")
	write("app/main.go", "package main")
	write("log/.keep", "")
	write("kept.log", "tracked despite the rule")
	runGit("add", ".gitignore", "app/main.go", "log/.keep")
	runGit("add", "-f", "kept.log")

	w := NewWatcher(WatcherConfig{Roots: []string{dir}, Ignored: gitIgnored(dir)})
	if err := w.TakeSnapshot(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	write(".context/serve.log", "listening")
	write(".playwright-mcp/page.yml", "snapshot")
	write("log/development.log", "line")
	write("app/server.log", "line")
	write("app/.env", "PORT=3001")
	write("app/conf/.env.local.yaml", "port: 3001")
	write("app/main.go", "package main\n// edited")
	write("kept.log", "edited")

	changed := w.dropIgnored(w.detectChanges())
	for i := range changed {
		changed[i] = filepath.ToSlash(changed[i])
	}
	slices.Sort(changed)
	want := []string{"app/.env", "app/conf/.env.local.yaml", "app/main.go", "kept.log"}
	if !slices.Equal(changed, want) {
		t.Errorf("changes = %v, want %v", changed, want)
	}
}

// TestGitIgnored_OutsideARepositoryIgnoresNothing keeps the watcher working
// where Git cannot answer: every change counts, as before.
func TestGitIgnored_OutsideARepositoryIgnoresNothing(t *testing.T) {
	if ignored := gitIgnored(t.TempDir())([]string{"serve.log"}); len(ignored) != 0 {
		t.Errorf("ignored = %v, want none", ignored)
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
