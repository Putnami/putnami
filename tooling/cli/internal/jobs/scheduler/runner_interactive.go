package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// interactiveStopBudget is how long a canceled interactive job has to exit on
// its own before the runner kills it (abort.InteractiveStopBudget). The exec
// WaitDelay that enforces it also bounds the wait for the I/O pipes of a stream
// that is no file once the child exited; the terminal streams of a dispatch are
// files and need none.
var interactiveStopBudget = abort.InteractiveStopBudget

// RunJobInteractive runs a job as an interactive subprocess, inheriting the
// parent's stdin/stdout/stderr. Unlike RunJob, no JSONL parsing happens — the
// extension is expected to render its own human-readable UI directly to the
// terminal. The PUTNAMI_INTERACTIVE=1 environment variable is set so the
// extension can detect this mode and avoid emitting JSONL events that would
// otherwise leak into the user's terminal.
//
// The context file is still written and passed via --putnamiContext so the
// extension has access to workspace, project, params, and version metadata.
func RunJobInteractive(
	ctx context.Context,
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams, configDefaults map[string]any,
	versions cachekey.RunVersions,
) (*JobResult, error) {
	return RunJobInteractiveWithStreams(
		ctx, ws, job, commandParams, configDefaults, versions,
		os.Stdin, os.Stdout, os.Stderr,
	)
}

// RunJobInteractiveWithStreams is RunJobInteractive with explicit process
// streams. Production callers pass the terminal streams through
// RunJobInteractive; in-process adapters and tests can keep independent output
// without swapping the process-wide os.Stdout/os.Stderr variables.
//
// processEnv states facts about this spawn that the process receives on top
// of its environment, such as the host RPC channel the caller opened for it
// (go.putnami.dev/protocol/extension/hostrpc). They describe no work, so no
// cache key reads them, and the job's own Env cannot restate the channel.
func RunJobInteractiveWithStreams(
	ctx context.Context,
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams, configDefaults map[string]any,
	versions cachekey.RunVersions,
	stdin io.Reader,
	stdout, stderr io.Writer,
	processEnv ...string,
) (*JobResult, error) {
	ctx = CaptureProcessCapabilities(ctx)
	start := time.Now()

	extraEnv := append([]string{"PUTNAMI_INTERACTIVE=1"}, processEnv...)
	ctx, cancel, inv, err := prepareJobInvocation(ctx, ws, job, commandParams, configDefaults, versions, extraEnv)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer func() { _ = os.Remove(inv.contextFile) }()

	if err := os.MkdirAll(inv.jobCtx.OutputPath, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	cmd, err := inv.jobCommand(ctx, job, scopeProcessCapabilities(ctx, applyTaskDeadline(inv.env, job), job))
	if err != nil {
		return nil, err
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	forced, runErr := runInteractive(cmd, func(start func() error) error { return inv.startJob(job, start) })
	duration := time.Since(start)

	result := &JobResult{Status: "success", Duration: duration}
	if runErr == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// Same structural deadline marker the streaming runner sets.
		result.Status = "failed"
		result.TimedOut = true
		result.Error = &JobError{Message: "job timed out"}
	case ctx.Err() != nil || endedByInterrupt(runErr):
		// A terminal signal stopped the run: this process's cancel, or a
		// terminal Ctrl+C that reached the child first. The child owns its stop,
		// so a child that exited by itself keeps its own exit code, 0 included.
		// The run is canceled only when the child died by the signal or this
		// process killed it.
		switch {
		case !exitedOnItsOwn(cmd.ProcessState, forced):
			result.Status = "canceled"
		case result.ExitCode != 0:
			result.Status = "failed"
		}
	default:
		result.Status = "failed"
		result.Error = &JobError{Message: runErr.Error()}
	}
	return result, nil
}
