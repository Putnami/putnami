//go:build unix

package devworkload

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// orphanTeardownBound is how long the composition of a killed putnami may take
// to notice the loss and tear down: its teardown has 75 seconds.
const orphanTeardownBound = 90 * time.Second

// TestComposeTearsDownOnceTheCLIIsKilled kills the real CLI outright while
// the @putnami/dev executable holds a running composition: the CLI signals
// nothing, so the executable must notice that its parent is gone and run the
// teardown a terminal signal runs. Every member's serve run and serve step
// stops, the lease is removed, the executable exits, and its exit document,
// written on the stdout it shared with the CLI, reports a clean cleanup.
func TestComposeTearsDownOnceTheCLIIsKilled(t *testing.T) {
	t.Parallel()
	requireFixtureTools(t)
	cli := fixtureproc.CLI(t)
	runtimePath := devRuntime(t)
	limitConcurrency(t)
	root := newFixture(t, runtimePath)
	starts := t.TempDir()

	cmd := exec.Command(cli, "compose", "app", "--port", "0", "--no-watch", "--output=jsonl")
	cmd.Dir = root
	cmd.Env = append(runEnv(t), fixtureStartsEnv+"="+starts)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// The executable keeps the stdout and stderr it shares with the CLI after
	// the CLI is killed: Wait returns once it exits, or after this delay.
	cmd.WaitDelay = orphanTeardownBound
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	killed := false
	t.Cleanup(func() {
		if killed {
			return
		}
		// A check failed before the kill. SIGTERM lets the composition stop
		// its members; SIGKILL would orphan them.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	awaitOutput(t, stdout, regexp.MustCompile(`^\{.*"command":"compose".*\}\n`), done, stderr)
	var start struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(stdout.String(), "\n", 2)[0]), &start); err != nil || start.Data.ID == "" {
		t.Fatalf("the start document names no composition (%v):\n%s", err, stdout.String())
	}
	leaseDir := filepath.Join(root, ".putnami", "compose", start.Data.ID)
	var lease struct {
		PID   int   `json:"pid"`
		PGIDs []int `json:"pgids"`
	}
	data, err := os.ReadFile(filepath.Join(leaseDir, "lease.json"))
	if err == nil {
		err = json.Unmarshal(data, &lease)
	}
	members := memberPIDs(t, starts)
	if len(members) != 2 {
		t.Fatalf("the composition started %d serve steps, want /provider and /app", len(members))
	}
	if err != nil || lease.PID <= 0 || len(lease.PGIDs) < len(members) {
		t.Fatalf("lease of the running composition = %+v (%v), want the executable's pid and the members' groups", lease, err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		// What survived the failed check would outlive the test.
		for _, pgid := range lease.PGIDs {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		for _, pid := range append(members, lease.PID) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	killed = true
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL the CLI: %v", err)
	}
	select {
	case <-done:
	case <-time.After(orphanTeardownBound + 30*time.Second):
		t.Fatalf("the CLI's stdout stayed open %s after the CLI was killed\nstdout:\n%s\nstderr:\n%s", orphanTeardownBound+30*time.Second, stdout.String(), stderr.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range append([]int{lease.PID}, members...) {
		awaitGone(t, deadline, "process", pid, pid, stderr)
	}
	for _, pgid := range lease.PGIDs {
		awaitGone(t, deadline, "process group", pgid, -pgid, stderr)
	}
	if _, err := os.Stat(leaseDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lease of the composition is still on disk (%v)", err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout holds %d documents, want the start and the exit document:\n%s\nstderr:\n%s", len(lines), stdout.String(), stderr.String())
	}
	exit := parseComposeDocument(t, lines[1])
	if exit.ExitCode != 0 || exit.Data.Cleanup == nil || exit.Data.Cleanup.State != "clean" {
		t.Fatalf("the exit document is not a clean exit:\n%s\nstderr:\n%s", lines[1], stderr.String())
	}
}

// memberPIDs returns the pids of the serve steps that recorded their start in
// starts.
func memberPIDs(t *testing.T, starts string) []int {
	t.Helper()
	entries, err := os.ReadDir(starts)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(starts, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var record fixtureStart
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, record.PID)
	}
	return pids
}

// awaitGone fails the test when what, which kill reaches through target (a
// pid, or a negated process group id), still exists at deadline.
func awaitGone(t *testing.T, deadline time.Time, what string, id, target int, stderr *lockedBuffer) {
	t.Helper()
	for syscall.Kill(target, 0) == nil {
		if time.Now().After(deadline) {
			t.Errorf("%s %d outlived the killed CLI's composition\nstderr:\n%s", what, id, stderr.String())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
