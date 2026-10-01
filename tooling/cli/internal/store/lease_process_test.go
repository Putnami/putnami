//go:build unix

package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const leaseHelperModeEnv = "PUTNAMI_TEST_STORE_LEASE_HELPER"

// TestLeaseHelperProcess is re-executed by the process-level tests below. A
// filesystem barrier makes every child contend on the same cold key after all
// processes have started.
func TestLeaseHelperProcess(t *testing.T) {
	mode := os.Getenv(leaseHelperModeEnv)
	if mode == "" {
		return
	}

	root := os.Getenv("PUTNAMI_TEST_STORE_LEASE_ROOT")
	startPath := os.Getenv("PUTNAMI_TEST_STORE_LEASE_START")
	deadline := time.Now().Add(10 * time.Second)
	for startPath != "" {
		if _, err := os.Stat(startPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for subprocess start barrier")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := NewLocalStore(root)
	if raw := os.Getenv("PUTNAMI_TEST_STORE_LEASE_TTL_MS"); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("parse helper TTL: %v", err)
		}
		configureFastLease(s, time.Duration(ms)*time.Millisecond)
	}

	switch mode {
	case "singleflight":
		winner, release := s.TryClaim(leaseTestKey, time.Second)
		if winner {
			counter := os.Getenv("PUTNAMI_TEST_STORE_LEASE_COUNTER")
			f, err := os.OpenFile(counter, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatalf("open compute counter: %v", err)
			}
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			time.Sleep(250 * time.Millisecond) // keep the cold-key contention visible
			if err := s.Put(leaseTestKey, leaseStatusEntry()); err != nil {
				release()
				t.Fatalf("winner publish: %v", err)
			}
			release()
			return
		}
		if err := s.WaitForPublish(leaseTestKey, 10*time.Second); err != nil {
			t.Fatalf("loser wait: %v", err)
		}
		entry, err := s.Get(leaseTestKey)
		if err != nil || entry == nil {
			t.Fatalf("published entry missing: entry=%v err=%v", entry, err)
		}

	case "crash":
		winner, _ := s.TryClaim(leaseTestKey, time.Second)
		if !winner {
			t.Fatal("crash helper did not win its cold key")
		}
		ready := os.Getenv("PUTNAMI_TEST_STORE_LEASE_READY")
		if err := os.WriteFile(ready, []byte("claimed"), 0o644); err != nil {
			t.Fatalf("write crash-ready marker: %v", err)
		}
		os.Exit(0) // simulate process death: no release, heartbeat stops

	default:
		t.Fatalf("unknown lease helper mode %q", mode)
	}
}

func TestLease_MultipleProcessesComputeColdKeyOnce(t *testing.T) {
	root := t.TempDir()
	startPath := filepath.Join(root, "start")
	counterPath := filepath.Join(root, "computes")

	const processes = 6
	type child struct {
		cmd *exec.Cmd
		out bytes.Buffer
	}
	children := make([]*child, 0, processes)
	for range processes {
		c := &child{cmd: exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$")}
		c.cmd.Env = append(os.Environ(),
			leaseHelperModeEnv+"=singleflight",
			"PUTNAMI_TEST_STORE_LEASE_ROOT="+root,
			"PUTNAMI_TEST_STORE_LEASE_START="+startPath,
			"PUTNAMI_TEST_STORE_LEASE_COUNTER="+counterPath,
			"PUTNAMI_TEST_STORE_LEASE_TTL_MS=5000",
		)
		c.cmd.Stdout = &c.out
		c.cmd.Stderr = &c.out
		if err := c.cmd.Start(); err != nil {
			t.Fatalf("start helper: %v", err)
		}
		children = append(children, c)
	}
	if err := os.WriteFile(startPath, []byte("go"), 0o644); err != nil {
		t.Fatalf("release helper barrier: %v", err)
	}

	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, c.out.String())
		}
	}
	data, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read compute counter: %v", err)
	}
	lines := strings.Fields(string(data))
	if len(lines) != 1 {
		t.Fatalf("cold key computed %d times, want exactly 1; owners=%q", len(lines), lines)
	}
}

// TestLease_CrashedOwnerExpiresAndWaiterTakesOver pins both halves of crash
// recovery: a dead owner's key stays owned until its recorded deadline passes,
// and once it passes exactly one waiter takes over.
//
// The helper claims with the production default TTL rather than a short one on
// purpose. "Not expired yet" cannot be asserted against a short wall-clock
// budget here, because everything between the child's claim and the parent's
// first observation — child teardown, wait4, and the parent's own scheduling —
// is unbounded, and a race-instrumented child alone can cost ~1s to spawn and
// reap. Instead of sleeping out a deadline the test then fast-forwards the
// recorded deadline into the past, which is what elapsed time does to it.
func TestLease_CrashedOwnerExpiresAndWaiterTakesOver(t *testing.T) {
	root := t.TempDir()
	readyPath := filepath.Join(root, "claimed")

	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$")
	cmd.Env = append(os.Environ(),
		leaseHelperModeEnv+"=crash",
		"PUTNAMI_TEST_STORE_LEASE_ROOT="+root,
		"PUTNAMI_TEST_STORE_LEASE_READY="+readyPath,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start crash helper: %v", err)
	}
	waitForTestFile(t, readyPath, 5*time.Second)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("crash helper: %v\n%s", err, output.String())
	}

	waiter := NewLocalStore(root)
	configureFastLease(waiter, time.Second)

	// The owner is gone and its heartbeat died with it, but its record still
	// stands, so neither entry point may hand the key out: claiming loses, and
	// waiting reports a live owner rather than an expired one.
	if winner, release := waiter.TryClaim(leaseTestKey, time.Second); winner {
		release()
		t.Fatal("waiter took over before crashed owner's TTL elapsed")
	}
	if err := waiter.WaitForPublish(leaseTestKey, 50*time.Millisecond); !errors.Is(err, ErrLeaseWaitTimeout) {
		t.Fatalf("wait while the crashed owner's lease is live = %v, want ErrLeaseWaitTimeout", err)
	}

	// Fast-forwarding the deadline is the only step that would otherwise be a
	// sleep. The record must still name the dead child: the lost claim above must
	// not have taken ownership, or the takeover below would prove nothing.
	crashedOwner := expireLeaseDeadline(t, waiter, leaseTestKey)
	if pid := strconv.Itoa(cmd.Process.Pid); !strings.HasPrefix(crashedOwner, pid+"-") {
		t.Fatalf("lease owner = %q, want the crashed helper's pid %s", crashedOwner, pid)
	}

	started := time.Now()
	err := waiter.WaitForPublish(leaseTestKey, 3*time.Second)
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("wait after crash = %v, want ErrLeaseExpired", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("crashed-owner fallback took %s", elapsed)
	}

	winner, release := waiter.TryClaim(leaseTestKey, time.Second)
	if !winner {
		t.Fatal("waiter did not take over the expired lease")
	}
	release()
}

// expireLeaseDeadline moves key's recorded deadline into the past under the same
// lock discipline expireLease uses, and returns the owner it belonged to.
// Production compares ExpiresAtNano against time.Now() and nothing else, so this
// is exactly equivalent to the TTL elapsing while leaving every code path under
// test — claim, wait, takeover — the real one.
func expireLeaseDeadline(t *testing.T, s *LocalStore, key string) string {
	t.Helper()
	path := s.leasePath(key)

	releaseStore := s.lockShared()
	defer releaseStore()
	releaseLease, err := acquireLeaseFileLock(path)
	if err != nil {
		t.Fatalf("lock lease record: %v", err)
	}
	defer releaseLease()

	rec, ok := readLeaseRecord(path)
	if !ok {
		t.Fatal("crashed owner left no lease record to expire")
	}
	rec.ExpiresAtNano = time.Now().Add(-time.Millisecond).UnixNano()
	if err := writeLeaseRecord(path, rec); err != nil {
		t.Fatalf("fast-forward lease deadline: %v", err)
	}
	return rec.Owner
}

func waitForTestFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
