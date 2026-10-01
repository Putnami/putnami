package cachecmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CacheVerifyEngineResult is the engine-owned evidence returned through the
// CLI adapter. commands never constructs a scheduler or calls jobs.RunPlan.
type CacheVerifyEngineResult struct {
	ExitCode int
	Plan     []*jobs.ScheduledJob
	Projects []*workspace.Project
	Results  map[string]*jobs.JobResult
}

// CacheVerifyRunner routes one execution through the canonical engine.
// storeRoot is an isolated verification namespace.
type CacheVerifyRunner func(
	ctx context.Context,
	workspaceRoot string,
	storeRoot string,
) (CacheVerifyEngineResult, error)

// CacheVerifyCommand runs two live executions in independent worktrees/stores,
// then restores the first run into a third pristine worktree and evaluates the
// three cache-correctness invariants.
func CacheVerifyCommand(
	ctx context.Context,
	wsRoot string,
	commands []string,
	outputFormat string,
	runner CacheVerifyRunner,
) error {
	if runner == nil {
		return fmt.Errorf("cache verify engine adapter is not configured")
	}
	clean, err := git.WorktreeClean(wsRoot)
	if err != nil {
		return fmt.Errorf("cache verify cannot inspect the worktree: %w", err)
	}
	if !clean {
		return cmderr.InvalidConfigf("cache verify requires a clean worktree")
	}
	head, err := git.HeadSHA(wsRoot)
	if err != nil {
		return fmt.Errorf("cache verify cannot resolve HEAD: %w", err)
	}
	staging, err := os.MkdirTemp("", "putnami-cache-verify-")
	if err != nil {
		return fmt.Errorf("create cache verification staging: %w", err)
	}
	defer os.RemoveAll(staging)

	worktrees := []string{
		filepath.Join(staging, "live-a"),
		filepath.Join(staging, "live-b"),
		filepath.Join(staging, "hit"),
	}
	for _, worktree := range worktrees {
		if err := addDetachedWorktree(ctx, wsRoot, worktree, head); err != nil {
			removeDetachedWorktrees(ctx, wsRoot, worktrees)
			return err
		}
	}
	defer removeDetachedWorktrees(context.WithoutCancel(ctx), wsRoot, worktrees)

	storeA, storeB := filepath.Join(staging, "store-a"), filepath.Join(staging, "store-b")
	first, err := executeCacheVerificationRun(ctx, runner, "first live run", worktrees[0], storeA)
	if err != nil {
		return err
	}
	if len(first.Plan) == 0 {
		return fmt.Errorf("cache verify could not produce a non-empty plan")
	}
	second, err := executeCacheVerificationRun(ctx, runner, "second live run", worktrees[1], storeB)
	if err != nil {
		return err
	}
	hit, err := executeCacheVerificationRun(ctx, runner, "cache-hit run", worktrees[2], storeA)
	if err != nil {
		return err
	}

	report := Analyze(commands, first, second, hit)
	runErr := error(nil)
	if report.Status != "passed" {
		runErr = shared.WithResultData(cmderr.InvalidConfigf(
			"cache verification found %d blocking determinism issue(s)", report.Summary.Blocking), report)
	}
	return renderCacheVerify(outputFormat, report, runErr)
}

func executeCacheVerificationRun(
	ctx context.Context,
	runner CacheVerifyRunner,
	phase string,
	root string,
	storeRoot string,
) (Run, error) {
	result, err := runner(ctx, root, storeRoot)
	if err != nil {
		return Run{}, fmt.Errorf("cache verification %s: %w", phase, err)
	}
	if result.ExitCode != 0 {
		// Name the phase and the tasks that failed. An audit that aborts with a
		// bare exit code sends the reader looking for a determinism bug when the
		// cause is an ordinary build failure in a throwaway worktree.
		return Run{}, fmt.Errorf("cache verification %s failed with exit %d%s",
			phase, result.ExitCode, failedTaskSuffix(result.Results))
	}
	ws, err := workspace.Load(root)
	if err != nil {
		return Run{}, fmt.Errorf("load staged workspace: %w", err)
	}
	trees, err := SnapshotProjects(root, result.Projects)
	if err != nil {
		return Run{}, err
	}
	return Run{
		Workspace: ws,
		Plan:      result.Plan,
		Results:   result.Results,
		Trees:     trees,
		StoreRoot: storeRoot,
	}, nil
}

// failedTaskSuffix names the tasks that failed in an aborted verification run,
// with their first error message, so the reader is not left with a bare exit
// code. Empty when nothing failed (the run's exit came from somewhere else).
// Bounded: an audit runs the whole plan, and a broken toolchain fails all of it.
func failedTaskSuffix(results map[string]*jobs.JobResult) string {
	const shown = 3
	keys := make([]string, 0, len(results))
	for key, result := range results {
		if result != nil && result.Status == "failed" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	extra := 0
	if len(keys) > shown {
		extra, keys = len(keys)-shown, keys[:shown]
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if err := results[key].Error; err != nil && strings.TrimSpace(err.Message) != "" {
			parts = append(parts, key+" ("+strings.TrimSpace(err.Message)+")")
			continue
		}
		parts = append(parts, key)
	}
	suffix := ": " + strings.Join(parts, ", ")
	if extra > 0 {
		suffix += fmt.Sprintf(" (+%d more failed task(s))", extra)
	}
	return suffix
}

// addDetachedWorktree checks head out into target. The checkout runs the
// repository's post-checkout hook and smudge filters, so it counts as
// repository code (runcredential.MarkRepositoryCodeStarted).
func addDetachedWorktree(ctx context.Context, repoRoot, target, head string) error {
	runcredential.MarkRepositoryCodeStarted("git worktree add")
	command := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", "--detach", target, head)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("create detached cache-verification worktree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func removeDetachedWorktrees(ctx context.Context, repoRoot string, targets []string) {
	for _, target := range targets {
		command := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "remove", "--force", target)
		_ = command.Run()
	}
}

func renderCacheVerify(outputFormat string, report Report, runErr error) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		if runErr != nil {
			return runErr
		}
		_, err := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
			protocolcli.NewResultV2("cache verify", report, nil))
		return err
	}
	// "N with declared ambient inputs" rather than "N ambient": the counter is
	// a count of DECLARATIONS, not of comparisons that were let off. Those
	// tasks' keys are compared like every other task's; the declaration only
	// downgrades a key difference to informational if one actually occurs.
	iox.Fprintf(os.Stdout, "\n  Cache verify: %s (%d task(s), %d blocking finding(s), %d with declared ambient inputs)\n\n",
		report.Status, report.Summary.Tasks, report.Summary.Blocking, report.Summary.Ambient)
	for _, finding := range report.Findings {
		scope := strings.TrimSpace(strings.Join([]string{finding.Project, finding.Task, finding.Path}, " "))
		iox.Fprintf(os.Stdout, "  [%s] %s %s: %s\n", finding.Severity, finding.Check, scope, finding.Message)
	}
	return runErr
}
