package machineproc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const syncWait = 5 * time.Second

// watchedTree makes a worktree with a/b, c and .putnami, and watches it.
func watchedTree(t *testing.T, budget int) (*Watcher, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"a/b", "c", ".putnami", "node_modules/pkg"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "a", "b", "f.txt"), "f")
	write(t, filepath.Join(root, "c", "g.txt"), "g")
	w, err := NewWatcher(budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := w.Watch(root); err != nil {
		t.Fatal(err)
	}
	return w, root
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func syncPoint(t *testing.T, w *Watcher, root string) {
	t.Helper()
	if err := w.Sync(root, syncWait); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func TestWatcherAWriteIsInTheJournalOnceTheSyncPointIsReached(t *testing.T) {
	w, root := watchedTree(t, 0)
	seq := w.Since(root, 0).Seq
	write(t, filepath.Join(root, "a", "b", "f.txt"), "changed")
	syncPoint(t, w, root)
	j := w.Since(root, seq)
	if j.Holds(filepath.Join(root, "a", "b", "f.txt")) {
		t.Errorf("a written file holds: %+v", j)
	}
	if !j.Holds(filepath.Join(root, "c", "g.txt")) {
		t.Errorf("an untouched file does not hold: %+v", j)
	}
	if len(j.Broken) != 0 {
		t.Errorf("a plain write broke %v", j.Broken)
	}
	if again := w.Since(root, j.Seq); len(again.Changed) != 0 || len(again.Broken) != 0 {
		t.Errorf("a read from the last sequence is not empty: %+v", again)
	}
}

func TestWatcherANewDirectoryBreaksItsSubtreeOnceAndIsThenWatched(t *testing.T) {
	w, root := watchedTree(t, 0)
	seq := w.Since(root, 0).Seq
	if err := os.MkdirAll(filepath.Join(root, "x", "y"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "x", "y", "early.txt"), "early")
	syncPoint(t, w, root)
	j := w.Since(root, seq)
	if j.Holds(filepath.Join(root, "x", "y", "early.txt")) {
		t.Fatalf("a file written in a new directory holds: %+v", j)
	}
	write(t, filepath.Join(root, "x", "y", "late.txt"), "late")
	syncPoint(t, w, root)
	later := w.Since(root, j.Seq)
	if len(later.Broken) != 0 {
		t.Errorf("the new directory is still broken: %+v", later)
	}
	if later.Holds(filepath.Join(root, "x", "y", "late.txt")) {
		t.Errorf("a write in the watched new directory holds: %+v", later)
	}
}

func TestWatcherAMovedDirectoryBreaksItsOldPathAndWatchesItsNewOne(t *testing.T) {
	w, root := watchedTree(t, 0)
	seq := w.Since(root, 0).Seq
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	syncPoint(t, w, root)
	j := w.Since(root, seq)
	for _, path := range []string{"a/b/f.txt", "moved/b/f.txt"} {
		if j.Holds(filepath.Join(root, path)) {
			t.Errorf("%s holds after the move: %+v", path, j)
		}
	}
	write(t, filepath.Join(root, "moved", "b", "f.txt"), "after")
	syncPoint(t, w, root)
	if later := w.Since(root, j.Seq); later.Holds(filepath.Join(root, "moved", "b", "f.txt")) {
		t.Errorf("a write below the moved directory holds: %+v", later)
	}
	// The old path stays broken: nothing watches it any more.
	if later := w.Since(root, w.Since(root, 0).Seq); later.Holds(filepath.Join(root, "a", "b", "f.txt")) {
		t.Errorf("the old path holds after the move: %+v", later)
	}
}

func TestWatcherARemovedDirectoryStaysBroken(t *testing.T) {
	w, root := watchedTree(t, 0)
	if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	syncPoint(t, w, root)
	if j := w.Since(root, w.Since(root, 0).Seq); j.Holds(filepath.Join(root, "a", "b", "f.txt")) {
		t.Errorf("a path below a removed directory holds: %+v", j)
	}
}

func TestWatcherAQueueOverflowBreaksTheWholeWorktree(t *testing.T) {
	data, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		t.Skip("max_queued_events cannot be read")
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || limit > 100000 {
		t.Skipf("max_queued_events is %s", strings.TrimSpace(string(data)))
	}
	w, root := watchedTree(t, 0)
	seq := w.Since(root, 0).Seq
	// The reader stops at its gate, woken by one write, while the queue
	// fills past its limit.
	gate := make(chan struct{})
	w.gate.Store(&gate)
	write(t, filepath.Join(root, "c", "wake"), "")
	time.Sleep(200 * time.Millisecond)
	for i := 0; i <= limit; i++ {
		write(t, filepath.Join(root, "c", fmt.Sprintf("f%d", i)), "")
	}
	w.gate.Store(nil)
	close(gate)
	// The sync point's own event may be lost to the full queue: that breaks
	// the worktree too.
	_ = w.Sync(root, syncWait)
	deadline := time.Now().Add(syncWait)
	for w.Since(root, seq).Holds(filepath.Join(root, "a", "b", "f.txt")) {
		if time.Now().After(deadline) {
			t.Fatalf("an untouched file holds after an overflow: %+v", w.Since(root, seq).Broken)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatcherADirectoryPastTheBudgetIsBrokenInEveryRead(t *testing.T) {
	w, root := watchedTree(t, 2)
	if got := w.Watches(); got != 2 {
		t.Fatalf("watches = %d, want the budget 2", got)
	}
	j := w.Since(root, w.Since(root, 0).Seq)
	unwatched := 0
	for _, path := range []string{"a/b/f.txt", "c/g.txt"} {
		if !j.Holds(filepath.Join(root, path)) {
			unwatched++
		}
	}
	if unwatched == 0 {
		t.Errorf("every path holds although the budget left directories unwatched: %+v", j)
	}
}

func TestWatcherASyncPointThatCannotBeReachedBreaksTheWorktree(t *testing.T) {
	w, root := watchedTree(t, 0)
	seq := w.Since(root, 0).Seq
	if err := os.RemoveAll(filepath.Join(root, ".putnami")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(root, 200*time.Millisecond); err == nil {
		t.Fatal("a sync without its cookie directory succeeded")
	}
	if j := w.Since(root, seq); j.Holds(filepath.Join(root, "c", "g.txt")) {
		t.Errorf("a path holds after a failed sync point: %+v", j)
	}
}

func TestWatcherASelfCheckThatFindsAMissedChangeTurnsTheWatcherOff(t *testing.T) {
	w, root := watchedTree(t, 0)
	// A file below a skipped directory is not watched, so the journal names
	// none of its changes: the self-check stands for a missed event.
	missed := filepath.Join(root, "node_modules", "pkg", "index.js")
	write(t, missed, "one")
	seen := filepath.Join(root, "c", "g.txt")
	recorded := map[string]FileSig{}
	for _, path := range []string{missed, seen} {
		sig, ok := SigOf(path)
		if !ok {
			t.Fatal("stat")
		}
		recorded[path] = sig
	}
	seq := w.Since(root, 0).Seq
	if !w.SelfCheck(root, seq, recorded) {
		t.Fatal("the self-check failed on an unchanged tree")
	}
	write(t, missed, "two, longer")
	syncPoint(t, w, root)
	if w.SelfCheck(root, seq, recorded) {
		t.Fatal("the self-check missed a change the journal did not name")
	}
	if !w.Disabled() {
		t.Error("the watcher is still on after a failed self-check")
	}
	if j := w.Since(root, w.Since(root, 0).Seq); j.Holds(seen) {
		t.Errorf("a path holds on a watcher turned off: %+v", j)
	}
}

func TestWatcherAReaderBehindTheJournalsFloorReadsTheWorktreeBroken(t *testing.T) {
	j := newJournal("/w")
	for i := 0; i < maxJournal+10; i++ {
		j.record(fmt.Sprintf("/w/f%d", i), false)
	}
	if got := j.since(1); got.Holds("/w/untouched") {
		t.Errorf("a reader behind the floor reads a path that holds: %+v", got.Broken)
	}
	if got := j.since(j.current() - 1); !got.Holds("/w/untouched") {
		t.Errorf("a reader within the journal reads the worktree broken")
	}
}
