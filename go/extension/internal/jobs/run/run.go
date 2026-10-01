// Package run executes a Go workload once for the host and forwards its exit
// code. Unlike serve (a resident dev server with hot-reload), run is one-shot:
// it builds a host binary, runs the program a single time with environment
// passthrough, streams its output, and reports the child's exit code so local
// validation of one-shot Jobs (migrations, batch workloads) stays inside the
// CLI.
//
// The workload is compiled with `go build` and then executed directly rather
// than via `go run`: `go run` collapses every non-zero program exit to 1, which
// would defeat the whole point of forwarding the child's exact exit code.
package run

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/proctree"
)

// stopGrace is how long the workload is given to exit after it was asked to
// stop before it is killed. It ends before the CLI job runner's own deadline,
// five seconds from the SIGTERM it sends the job's process group to the
// SIGKILL that follows (processGroupKillDelay in
// tooling/cli/internal/jobs/runner.go), so the extension reaps the workload
// itself and reports how the run ended.
const stopGrace = 3 * time.Second

// exitCodeKey is the result-data key carrying the workload's exit code. The CLI
// reads it to forward the child's exact exit code as the `putnami run` process
// exit code (see tooling/cli runForwardedExitCode). Kept in sync with the
// TypeScript and Python extensions.
const exitCodeKey = "exit-code"

// Run executes the run job: build a host binary and run the workload once,
// forwarding its exit code.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	_ = args
	entrypoint := ctx.Params.String("entrypoint")
	race := ctx.Params.Bool("race", false)
	port := ctx.Params.String("port")
	extraArgs := ctx.Params.String("args")

	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}

	// PORT passthrough: only override when explicitly requested so generic
	// workloads (jobs, CLIs) keep the ambient environment untouched.
	if port != "" {
		os.Setenv("PORT", port)
	}

	emit.PhaseStart("build")
	ep, err := platform.ResolveServeEntrypoint(ctx.Project.FullPath, ctx.Project.Name, entrypoint)
	if err != nil {
		emit.PhaseEnd("build", "failed")
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	binDir, err := os.MkdirTemp("", "putnami-run-")
	if err != nil {
		emit.PhaseEnd("build", "failed")
		return "FAILED", nil, err
	}
	defer os.RemoveAll(binDir)
	binPath := filepath.Join(binDir, platform.HostBinaryName(runtime.GOOS, ep, ctx.Project.Name))

	if !buildHostBinary(goBinary, binPath, ep, race, ctx.Project.FullPath, emit) {
		emit.PhaseEnd("build", "failed")
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("build", "success")

	cmdArgs := []string{binPath}
	if extraArgs != "" {
		cmdArgs = append(cmdArgs, strings.Fields(extraArgs)...)
	}

	emit.PhaseStart("run")
	emit.Log("debug", "Running "+ctx.Project.Name+" ("+ep+") once...")
	exitCode := runOnce(cmdArgs, ctx.Project.FullPath, emit)

	data := map[string]any{exitCodeKey: exitCode}
	if exitCode == 0 {
		emit.PhaseEnd("run", "success")
		return "OK", data, nil
	}
	emit.PhaseEnd("run", "failed")
	// FAILED is returned with a nil error on purpose: the job ran correctly, the
	// workload itself exited non-zero. Returning a non-nil error would make the
	// SDK drop the result data (and with it the exit code the CLI forwards).
	return "FAILED", data, nil
}

// buildHostBinary compiles the entrypoint to binPath for the host platform.
// Build diagnostics are streamed; it returns false on failure.
func buildHostBinary(goBinary, binPath, entrypoint string, race bool, dir string, emit *jsonl.Emitter) bool {
	buildArgs := []string{"build", "-o", binPath}
	if race {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, entrypoint)

	cmd := exec.Command(goBinary, buildArgs...)
	cmd.Dir = dir
	// `go build` resolves modules: re-point GOWORK at the governing go.work so an
	// inherited GOWORK=off (leaked from a standalone CLI build) can't disable
	// workspace resolution and send framework v0.0.0 placeholders to the proxy
	// as doomed 404s.
	cmd.Env = toolchain.WorkspaceBuildEnv(os.Environ(), dir, goBinary)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		emit.Diagnostic("error", "failed to start build: "+err.Error(), "", 0)
		return false
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "error", &wg)
	wg.Wait()

	return cmd.Wait() == nil
}

// runOnce runs the command a single time, streaming stdout/stderr as JSONL log
// events and forwarding termination signals to the child. It returns the
// child's exit code (130 for a clean SIGINT-driven shutdown).
func runOnce(cmdArgs []string, dir string, emit *jsonl.Emitter) int {
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return 1
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return 1
	}

	if err := cmd.Start(); err != nil {
		emit.Diagnostic("error", "failed to start workload: "+err.Error(), "", 0)
		return 1
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)

	doneChan := make(chan error, 1)
	go func() {
		wg.Wait()
		doneChan <- cmd.Wait()
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	select {
	case <-sigChan:
		_ = stopWorkload(cmd.Process, doneChan, stopGrace, proctree.Relay)
		return 130
	case waitErr := <-doneChan:
		if waitErr == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
}

// stopWorkload asks process to exit through ask and returns its wait error
// from done once it has exited. A workload still running grace after the
// request is killed, and so is one that could not be asked: waiting on a
// request that was never delivered would be bounded by nothing.
func stopWorkload(process *os.Process, done <-chan error, grace time.Duration, ask func(*os.Process) error) error {
	return proctree.Stop(done, grace, func() error { return ask(process) }, process.Kill)
}
