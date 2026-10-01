package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

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
	versions RunVersions,
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
func RunJobInteractiveWithStreams(
	ctx context.Context,
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams, configDefaults map[string]any,
	versions RunVersions,
	stdin io.Reader,
	stdout, stderr io.Writer,
) (*JobResult, error) {
	ctx = CaptureProcessCapabilities(ctx)
	start := time.Now()

	ctx, cancel, inv, err := prepareJobInvocation(ctx, ws, job, commandParams, configDefaults, versions, []string{"PUTNAMI_INTERACTIVE=1"})
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

	runErr := runInteractive(cmd, func(start func() error) error { return inv.startJob(job, start) })
	duration := time.Since(start)

	result := &JobResult{Status: "success", Duration: duration}
	if runErr != nil {
		result.Status = "failed"
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				// Same structural deadline marker the streaming runner sets.
				result.TimedOut = true
				result.Error = &JobError{Message: "job timed out"}
			} else {
				result.Status = "canceled"
			}
		} else {
			result.Error = &JobError{Message: runErr.Error()}
		}
	}
	return result, nil
}
