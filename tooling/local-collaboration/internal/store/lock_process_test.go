package store

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// holdLockVariable makes the test binary a lock holder instead of a test
// run: it takes the store lock at the path the variable names, reports
// "held" on its standard output, and releases the lock when its standard
// input closes.
const holdLockVariable = "PUTNAMI_LOCAL_STORE_HOLD_LOCK"

func TestMain(m *testing.M) {
	if path := os.Getenv(holdLockVariable); path != "" {
		os.Exit(holdLock(path))
	}
	os.Exit(m.Run())
}

func holdLock(path string) int {
	lock, err := acquire(path, time.Minute)
	if err != nil {
		return 2
	}
	defer lock.release()
	if _, err := os.Stdout.WriteString("held\n"); err != nil {
		return 2
	}
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	return 0
}

func TestAnotherProcessHoldingTheLockMakesAWriteBusy(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	if _, err := s.Update(func(state *State) error { state.Next.Task = 1; return nil }); err != nil {
		t.Fatal(err)
	}

	holder := exec.Command(os.Args[0], "-test.run=^$")
	holder.Env = append(os.Environ(), holdLockVariable+"="+filepath.Join(root, lockFile))
	release, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	reports, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	// The holder releases the lock after 3 s at the latest, so a write that
	// waits for it without a bound ends too, and fails the test.
	timer := time.AfterFunc(3*time.Second, func() { _ = release.Close() })
	defer timer.Stop()
	if line, err := bufio.NewReader(reports).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("the lock holder answered %q (%v)", line, err)
	}

	s.LockTimeout = 100 * time.Millisecond
	ran := false
	started := time.Now()
	_, err = s.Update(func(state *State) error { ran = true; state.Next.Task = 99; return nil })
	if !errors.Is(err, ErrBusy) || ran || time.Since(started) > 2*time.Second {
		t.Fatalf("a write under another process's lock: %v after %s (change ran: %v)", err, time.Since(started), ran)
	}

	_ = release.Close()
	if err := holder.Wait(); err != nil {
		t.Fatalf("the lock holder: %v", err)
	}
	written, err := s.Update(func(state *State) error { state.Next.Task++; return nil })
	if err != nil || written.Next.Task != 2 {
		t.Fatalf("a write once the other process released the lock: %v %+v", err, written)
	}
}
