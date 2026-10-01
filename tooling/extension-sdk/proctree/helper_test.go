package proctree

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// The trees the tests start are this test binary, three levels deep: level 1
// is the tree's root, it spawns level 2, which spawns level 3. Each level
// records its pid once it can receive a stop request, and records that it
// received one before it exits.
const (
	helperLevelEnv     = "PROCTREE_HELPER_LEVEL"
	helperDirEnv       = "PROCTREE_HELPER_DIR"
	helperRootExitsEnv = "PROCTREE_HELPER_ROOT_EXITS"
	helperExitCodeEnv  = "PROCTREE_HELPER_EXIT_CODE"
	helperLevels       = 3
	// helperDetachEnv makes the root start level 2 with StartDetached.
	helperDetachEnv = "PROCTREE_HELPER_DETACH"
	// helperNoBreakawayEnv makes the root join a job that forbids breakaway
	// before it starts level 2 with StartDetached.
	helperNoBreakawayEnv = "PROCTREE_HELPER_NO_BREAKAWAY"
)

// helperCommand returns the command that runs level 1 of a helper tree whose
// levels record themselves under dir.
func helperCommand(dir string, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), helperLevelEnv+"=1", helperDirEnv+"="+dir)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func levelFile(dir string, level int) string {
	return filepath.Join(dir, "level-"+strconv.Itoa(level))
}

func terminatedFile(dir string, level int) string {
	return filepath.Join(dir, "terminated-"+strconv.Itoa(level))
}

// startedInsideFile records that StartDetached started level 2 inside the
// root's job, as a copy of the command, because the job forbids breakaway.
func startedInsideFile(dir string) string {
	return filepath.Join(dir, "started-inside")
}

// TestProcessTreeHelper is not a test: it is one level of a helper tree, and it
// returns at once unless a test started it as one.
func TestProcessTreeHelper(t *testing.T) {
	level, err := strconv.Atoi(os.Getenv(helperLevelEnv))
	if err != nil {
		return
	}
	if code := os.Getenv(helperExitCodeEnv); code != "" {
		exitCode, _ := strconv.Atoi(code)
		os.Exit(exitCode)
	}
	dir := os.Getenv(helperDirEnv)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	if level < helperLevels {
		child := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
		child.Env = append(os.Environ(), helperLevelEnv+"="+strconv.Itoa(level+1))
		if err := startLevel(level, dir, child); err != nil {
			os.Exit(2)
		}
	}
	if err := writeAtomically(levelFile(dir, level), strconv.Itoa(os.Getpid())); err != nil {
		os.Exit(2)
	}
	if level == 1 && os.Getenv(helperRootExitsEnv) == "1" {
		// The root leaves its descendants behind, still members of the tree.
		if !waitForFile(levelFile(dir, helperLevels), time.Minute) {
			os.Exit(2)
		}
		os.Exit(0)
	}
	select {
	case <-stop:
		_ = writeAtomically(terminatedFile(dir, level), "")
		os.Exit(0)
	case <-time.After(10 * time.Minute):
		os.Exit(3)
	}
}

// startLevel starts the level after level: with StartDetached from the root
// when the test asks for it, with cmd.Start otherwise.
func startLevel(level int, dir string, child *exec.Cmd) error {
	if level != 1 || os.Getenv(helperDetachEnv) != "1" {
		return child.Start()
	}
	if os.Getenv(helperNoBreakawayEnv) == "1" {
		if err := joinJobWithoutBreakaway(); err != nil {
			return err
		}
	}
	started, err := StartDetached(child)
	if err != nil {
		return err
	}
	if started != child {
		return writeAtomically(startedInsideFile(dir), "")
	}
	return nil
}

func writeAtomically(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readWhenReady reads a file that waitForFile found, retrying a failed read
// until timeout. On Windows a file that was just renamed into place can still
// be open elsewhere, by the writer's rename or by a scanner, and a read then
// fails with a sharing violation for a moment.
func readWhenReady(path string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil || !time.Now().Before(deadline) {
			return data, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForLevels waits until every level of the helper tree under dir recorded
// itself, and returns their pids, root first.
func waitForLevels(t *testing.T, dir string) []int {
	t.Helper()
	pids := make([]int, 0, helperLevels)
	for level := 1; level <= helperLevels; level++ {
		path := levelFile(dir, level)
		if !waitForFile(path, 30*time.Second) {
			t.Fatalf("level %d of the helper tree never started", level)
		}
		data, err := readWhenReady(path, 5*time.Second)
		if err != nil {
			t.Fatalf("read level %d: %v", level, err)
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			t.Fatalf("level %d recorded %q: %v", level, data, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// startedTree is a helper tree the test started, and the exit of its root.
type startedTree struct {
	tree   *Tree
	dir    string
	exited chan struct{}
	err    error
}

// startHelperTree starts a helper tree and ends it, whatever the test did, when
// the test finishes.
func startHelperTree(t *testing.T, env ...string) *startedTree {
	t.Helper()
	started := &startedTree{dir: t.TempDir(), exited: make(chan struct{})}
	cmd := helperCommand(started.dir, env...)
	started.tree = New(cmd)
	if err := started.tree.Start(); err != nil {
		t.Fatalf("start the helper tree: %v", err)
	}
	go func() {
		started.err = cmd.Wait()
		close(started.exited)
	}()
	id := started.tree.ID()
	t.Cleanup(func() {
		_ = started.tree.Kill()
		_ = started.tree.Close()
		_ = KillGroup(id)
		<-started.exited
	})
	return started
}

// exitTimeout bounds how long a test waits for the root of a helper tree to
// exit.
const exitTimeout = 30 * time.Second

// waitForExit waits for the root of a started tree to exit and be reaped.
func (s *startedTree) waitForExit(t *testing.T) {
	t.Helper()
	select {
	case <-s.exited:
	case <-time.After(exitTimeout):
		t.Fatalf("the root of the helper tree did not exit within %v", exitTimeout)
	}
}
