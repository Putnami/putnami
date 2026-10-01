package shared

import (
	"bytes"
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// auxKillDelay bounds how long a canceled auxiliary subprocess may keep
// running after SIGTERM before it is force-killed. It mirrors the job
// runner's process-group kill delay (internal/jobs/runner.go).
const auxKillDelay = 5 * time.Second

// configureGroupCancellation makes cmd the root of its own process tree and,
// on context cancellation, terminates the whole tree (parent + children) with
// SIGTERM, escalating to SIGKILL after auxKillDelay. Start cmd with the
// returned tree. The returned function must be called once the command
// finishes (it stops the escalation goroutine from firing on an already-exited
// tree).
//
// This mirrors the job runner's cancellation contract so long-running
// auxiliary spawns (`go mod tidy`, a nested `putnami build,test`, …) cannot
// outlive a canceled root context or leave orphaned grandchildren behind.
func configureGroupCancellation(cmd *osexec.Cmd) (tree *proctree.Tree, done func()) {
	tree = proctree.New(cmd)
	finished := make(chan struct{})
	cmd.Cancel = func() error {
		err := tree.Terminate()
		go forceKillAuxGroupAfter(finished, tree, auxKillDelay)
		return err
	}
	cmd.WaitDelay = auxKillDelay
	var once bool
	return tree, func() {
		if once {
			return
		}
		once = true
		close(finished)
	}
}

// markRepositoryCode records, before the spawn, that the auxiliary command
// starts repository code: `go` in a workspace module runs its toolchain
// directive, and a nested CLI runs the workspace's hooks and jobs. From then
// on no process of this one receives the run credential
// (runcredential.StartHolder).
func markRepositoryCode(name string, args []string) {
	runcredential.MarkRepositoryCodeStarted(strings.TrimSpace(filepath.Base(name) + " " + strings.Join(args, " ")))
}

// RunGroupCombined runs name+args as the root of its own process tree (see
// configureGroupCancellation) and returns the combined stdout+stderr. It is the
// cancellation-aware analog of cmd.CombinedOutput for long-running auxiliary
// spawns. The command counts as repository code (markRepositoryCode).
func RunGroupCombined(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	markRepositoryCode(name, args)
	cmd := osexec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	// One writer for both streams, as cmd.CombinedOutput uses: os/exec then
	// writes both through a single pipe, in the order the child wrote them.
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	tree, done := configureGroupCancellation(cmd)
	err := tree.Run()
	done()
	return out.Bytes(), err
}

// RunGroupStreaming runs name+args as the root of its own process tree (see
// configureGroupCancellation) with stdout/stderr wired to the process's own
// streams, mirroring a direct cmd.Run() but with cancellation reaching the
// whole tree. The command counts as repository code (markRepositoryCode).
func RunGroupStreaming(ctx context.Context, dir string, env []string, name string, args ...string) error {
	markRepositoryCode(name, args)
	cmd := osexec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	tree, done := configureGroupCancellation(cmd)
	err := tree.Run()
	done()
	return err
}

// forceKillAuxGroupAfter kills the process tree once delay elapses, unless the
// tree has already finished (signaled via done). Mirrors
// internal/jobs/runner.go's forceKillProcessGroupAfter.
func forceKillAuxGroupAfter(done <-chan struct{}, tree *proctree.Tree, delay time.Duration) {
	select {
	case <-done:
	case <-time.After(delay):
		_ = tree.Kill()
	}
}
