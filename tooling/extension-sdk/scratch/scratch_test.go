//go:build unix

package scratch

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// childPrefixEnv makes the test binary act as a scratch creator: it creates a
// directory with that prefix, prints its path and waits to be killed.
const childPrefixEnv = "PUTNAMI_SCRATCH_TEST_CHILD_PREFIX"

func TestMain(m *testing.M) {
	if prefix := os.Getenv(childPrefixEnv); prefix != "" {
		dir, err := New(prefix)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(dir.Path())
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		_ = dir.Remove()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// isolateTempDir points os.TempDir() at a private directory, for this test and
// for the creators it starts.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	// Background sweeps New started for this root finish before it goes.
	t.Cleanup(sweeping.Wait)
	return root
}

// startCreator runs a creator process and returns it with the directory it
// created. The process holds the directory's owner lock until it is killed.
func startCreator(t *testing.T, prefix string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childPrefixEnv+"="+prefix)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("creator printed no directory: %v", err)
	}
	return cmd, strings.TrimSpace(line)
}

// awaitLockRelease blocks until the owner lock of dir is free. A killed
// process's descriptors close asynchronously with respect to the wait that
// reaps it, so the lock itself, never the exit status, says the owner is gone.
func awaitLockRelease(t *testing.T, dir string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(dir, OwnerLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	acquired := make(chan error, 1)
	go func() { acquired <- lockFile(file, false) }()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the owner lock of a killed creator was never released")
	}
}

func TestNewRemovesTheScratchOfAKilledRun(t *testing.T) {
	isolateTempDir(t)
	const prefix = "putnami-scratch-test-"

	cmd, leaked := startCreator(t, prefix)
	if err := os.WriteFile(filepath.Join(leaked, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	awaitLockRelease(t, leaked)
	if _, err := os.Stat(leaked); err != nil {
		t.Fatalf("the killed run must have left its scratch behind: %v", err)
	}

	// The next creator is of another kind: reclaiming depends on the owner
	// lock, not on the killed creator's name.
	next, err := New("putnami-scratch-other-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Remove() })
	sweeping.Wait()

	if _, err := os.Stat(leaked); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the next creator must remove the killed run's scratch %s, stat: %v", leaked, err)
	}
	if _, err := os.Stat(filepath.Join(next.Path(), OwnerLockName)); err != nil {
		t.Fatalf("the next run's own scratch must exist with its owner lock: %v", err)
	}
}

func TestSweepKeepsACopyOfADeadCreatorsScratch(t *testing.T) {
	root := isolateTempDir(t)
	const prefix = "putnami-scratch-test-"

	cmd, leaked := startCreator(t, prefix)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	awaitLockRelease(t, leaked)
	lock, err := os.ReadFile(filepath.Join(leaked, OwnerLockName))
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(root, prefix+"copy")
	if err := os.Mkdir(copied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copied, OwnerLockName), lock, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Sweep(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaked); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dead creator's scratch must be removed, stat: %v", err)
	}
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("a copy carrying another directory's lock is not that directory: %v", err)
	}
}

func TestSweepRemovesAnOldDirectoryWithAForeignLock(t *testing.T) {
	root := isolateTempDir(t)
	const prefix = "putnami-scratch-test-"

	foreign := filepath.Join(root, prefix+"foreign")
	if err := os.MkdirAll(filepath.Join(foreign, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An owner lock of an earlier format (empty) or of another directory.
	if err := os.WriteFile(filepath.Join(foreign, OwnerLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-UnownedGrace - time.Hour)
	if err := os.Chtimes(foreign, past, past); err != nil {
		t.Fatal(err)
	}

	if err := Sweep(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an old directory whose lock names no directory is unowned and must be removed, stat: %v", err)
	}
}

func TestSweepKeepsTheScratchOfALiveCreator(t *testing.T) {
	isolateTempDir(t)
	const prefix = "putnami-scratch-test-"

	_, live := startCreator(t, prefix)
	if err := Sweep(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(live, OwnerLockName)); err != nil {
		t.Fatalf("a sweep must never remove a live creator's scratch: %v", err)
	}
}

func TestSweepKeepsTheScratchOfThisProcess(t *testing.T) {
	isolateTempDir(t)
	const prefix = "putnami-scratch-test-"

	mine, err := New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mine.Remove() })
	if err := Sweep(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine.Path()); err != nil {
		t.Fatalf("a sweep must keep a directory this process still owns: %v", err)
	}
}

func TestSweepRemovesUnownedScratchOnlyAfterTheGrace(t *testing.T) {
	root := isolateTempDir(t)
	const prefix = "putnami-scratch-test-"

	old := filepath.Join(root, prefix+"old")
	young := filepath.Join(root, prefix+"young")
	other := filepath.Join(root, "other-tool-old")
	sibling := filepath.Join(root, Namespace+"sibling-old")
	for _, dir := range []string{old, young, other, sibling} {
		if err := os.MkdirAll(filepath.Join(dir, "store"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-UnownedGrace - time.Hour)
	for _, dir := range []string{old, other, sibling} {
		if err := os.Chtimes(dir, past, past); err != nil {
			t.Fatal(err)
		}
	}

	if err := Sweep(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unowned directory past the grace must be removed, stat: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("an unowned directory within the grace may be a creator taking its lock: %v", err)
	}
	for _, dir := range []string{other, sibling} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("an unowned directory with another prefix is not this sweep's to judge by age: %v", err)
		}
	}
}

func TestSweepIgnoresASymlinkedOwnerLock(t *testing.T) {
	root := isolateTempDir(t)
	const prefix = "putnami-scratch-test-"

	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(root, prefix+"planted")
	if err := os.Mkdir(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(planted, OwnerLockName)); err != nil {
		t.Fatal(err)
	}

	if err := Sweep(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Fatalf("a directory whose owner lock is a symlink is not scratch to remove: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
		t.Fatalf("the symlink target must be untouched: %q, %v", data, err)
	}
}

func TestRemoveDeletesTheDirectoryOnce(t *testing.T) {
	isolateTempDir(t)

	dir, err := New("putnami-scratch-test-")
	if err != nil {
		t.Fatal(err)
	}
	path := dir.Path()
	if err := dir.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove must delete the directory, stat: %v", err)
	}
	if err := dir.Remove(); err != nil {
		t.Fatalf("a second Remove must be a no-op: %v", err)
	}
	var none *Dir
	if err := none.Remove(); err != nil {
		t.Fatalf("Remove on a nil Dir must be a no-op: %v", err)
	}
}

func TestNewRefusesAPatternPrefix(t *testing.T) {
	for _, prefix := range []string{"", "other-", "putnami-*", "putnami-a/b"} {
		if _, err := New(prefix); err == nil {
			t.Errorf("New(%q) must be refused", prefix)
		}
	}
}
