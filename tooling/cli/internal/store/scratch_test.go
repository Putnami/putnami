//go:build unix

package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/flock"
)

func scratchFile(t *testing.T, ws, name string) string {
	t.Helper()
	file := filepath.Join(ResolveScratchRoot(ws), name)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("rebuildable"), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func expireScratch(t *testing.T, ws string) {
	t.Helper()
	old := time.Now().Add(-scratchGenerationLifetime - time.Hour)
	if err := os.Chtimes(filepath.Join(ws, putnamiDir, scratchGenerationName), old, old); err != nil {
		t.Fatal(err)
	}
}

func visitScratch(t *testing.T, ws string) {
	t.Helper()
	lease, err := AcquireScratch(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

// awaitScratchRelease returns once no holder retains the scratch lease. The
// exclusive acquire IS the observation: it is granted exactly when the last
// descriptor on the lease closes, which a reaped child's exit status does not
// state. The bounded wait is a failure guard for a lease that never comes
// back, never the synchronization.
func awaitScratchRelease(t *testing.T, ws string) {
	t.Helper()
	released := make(chan error, 1)
	go func() {
		lease, err := flock.Acquire(filepath.Join(ws, putnamiDir, scratchLockName), true, false)
		if err != nil {
			released <- err
			return
		}
		released <- lease.Release()
	}()
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("scratch lease leaked: no holder released it after the child exited")
	}
}

// failScratchProbe makes the idleness probe report err while the shared lease
// keeps working — the one combination a real filesystem cannot be asked for.
func failScratchProbe(t *testing.T, err error) {
	t.Helper()
	previous := acquireScratchLease
	acquireScratchLease = func(path string, exclusive, nonBlocking bool) (*flock.Lock, error) {
		if exclusive {
			return nil, err
		}
		return previous(path, exclusive, nonBlocking)
	}
	t.Cleanup(func() { acquireScratchLease = previous })
}

// captureScratchLogs redirects the default logger for the duration of the test.
func captureScratchLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestScratchGenerationReclaimsOrphansWithoutInventory(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "scratch-generation-retention", "expired-generations-reclaim-arbitrary-orphans")
	ws := t.TempDir()
	orphan := scratchFile(t, ws, "any-extension/renamed-project/binary")
	visitScratch(t, ws) // Legacy roots get a full initial retention window.
	visitScratch(t, ws)
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("fresh generation removed: %v", err)
	}
	expireScratch(t, ws)
	visitScratch(t, ws)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan survived: %v", err)
	}
	warm := scratchFile(t, ws, "other-extension/live-project/next")
	visitScratch(t, ws)
	if _, err := os.Stat(warm); err != nil {
		t.Fatalf("new generation was not retained: %v", err)
	}
}

func TestScratchActiveAndNestedConsumersPreventReaping(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "scratch-generation-retention", "active-and-nested-consumers-retain-the-generation")
	ws := t.TempDir()
	parent, err := AcquireScratch(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Close() }()
	file := scratchFile(t, ws, "active/context")
	expireScratch(t, ws)
	done := make(chan error, 1)
	go func() {
		child, err := AcquireScratch(ws)
		if err == nil {
			err = child.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested consumer waited for its parent's shared lease")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("active generation removed: %v", err)
	}
	_ = parent.Close()
	visitScratch(t, ws)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("idle expired generation survived: %v", err)
	}
}

func TestScratchChildLeaseSurvivesParentClose(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "scratch-generation-retention", "a-detached-child-retains-its-lease")
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "read finish")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	lease, err := AttachScratch(cmd, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	_ = lease.Close() // The detached process is now the only holder.
	file := scratchFile(t, ws, "child/still-reading")
	expireScratch(t, ws)
	visitScratch(t, ws)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("child lost its generation: %v", err)
	}
	if _, err := stdin.Write([]byte("finish\n")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	awaitScratchRelease(t, ws)
	visitScratch(t, ws) // The FIRST visit after the release reclaims.
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the released child lease was not reclaimed: %v", err)
	}
}

func TestScratchOnlyAnActiveConsumerSkipsReclamationSilently(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "scratch-generation-retention", "only-an-active-consumer-skips-reclamation-silently")
	for _, probe := range []struct {
		name     string
		err      error
		reported bool
	}{
		{name: "active-consumer", err: flock.ErrBusy},
		{name: "unusable-lease", err: errors.New("scratch lease is unusable"), reported: true},
	} {
		t.Run(probe.name, func(t *testing.T) {
			ws := t.TempDir()
			visitScratch(t, ws)
			file := scratchFile(t, ws, "unprobed/generation")
			expireScratch(t, ws)
			failScratchProbe(t, probe.err)
			logs := captureScratchLogs(t)
			lease, err := AcquireScratch(ws)
			if err != nil {
				t.Fatalf("a failed idleness probe refused the consumer: %v", err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("a skipped reclamation removed the generation: %v", err)
			}
			if got := strings.Contains(logs.String(), "scratch reclamation skipped"); got != probe.reported {
				t.Fatalf("reclamation skip reported = %t, want %t; logs: %s", got, probe.reported, logs.String())
			}
		})
	}
}

func TestScratchReapingDoesNotFollowSymlinks(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "scratch-generation-retention", "reaping-never-follows-symlinks")
	for _, name := range []string{"nested", "cache-root", "parent", "stamp"} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			visitScratch(t, ws)
			expireScratch(t, ws)
			outside := t.TempDir()
			keep := filepath.Join(outside, "keep")
			if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "nested":
				scratchFile(t, ws, "other/file")
				if err := os.Symlink(outside, filepath.Join(ResolveScratchRoot(ws), "link")); err != nil {
					t.Fatal(err)
				}
			case "cache-root":
				if err := os.Symlink(outside, ResolveScratchRoot(ws)); err != nil {
					t.Fatal(err)
				}
			case "parent":
				if err := os.Rename(filepath.Join(ws, putnamiDir), filepath.Join(ws, "saved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(ws, putnamiDir)); err != nil {
					t.Fatal(err)
				}
			case "stamp":
				stamp := filepath.Join(ws, putnamiDir, scratchGenerationName)
				if err := os.Remove(stamp); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(keep, stamp); err != nil {
					t.Fatal(err)
				}
			}
			visitScratch(t, ws)
			if got, err := os.ReadFile(keep); err != nil || string(got) != "keep" {
				t.Fatalf("symlink target changed: %q, %v", got, err)
			}
		})
	}
}

func TestScratchReapingFailureDoesNotAdvanceGeneration(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "scratch-generation-retention", "failed-reclamation-is-retried")
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	ws := t.TempDir()
	visitScratch(t, ws)
	file := scratchFile(t, ws, "blocked/file")
	expireScratch(t, ws)
	blocked := filepath.Dir(file)
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	visitScratch(t, ws) // Reclamation is advisory, but does not grant a new window.
	stamp, err := os.Stat(filepath.Join(ws, putnamiDir, scratchGenerationName))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(stamp.ModTime()) < scratchGenerationLifetime {
		t.Fatal("failed deletion advanced the generation")
	}
	if err := os.Chmod(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	visitScratch(t, ws)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("reclamation was not retried: %v", err)
	}
}

func TestScratchLeaseFailureRefusesConsumer(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, putnamiDir, scratchLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	if lease, err := AcquireScratch(ws); err == nil {
		_ = lease.Close()
		t.Fatal("consumer ran without a scratch lease")
	}
}
