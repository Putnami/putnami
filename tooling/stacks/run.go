package stacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Diagnostic codes of the task runner.
const (
	CodeNotAComponent = "stacks.not_a_component"
	// CodeCoverageUnavailable warns that the toolchain has no coverage support.
	CodeCoverageUnavailable = "stacks.coverage_unavailable"
	CodeStepFailed          = "stacks.step_failed"
)

// Job statuses.
const (
	statusOK     = "OK"
	statusFailed = "FAILED"
)

// Run is the job of one task: the toolchain check, the steps in the project
// directory under the stack root's lock, then the reports, copied into the
// task's output directory and read.
func (s Stack) Run(task string) func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		output, err := outputFlag(args)
		if err != nil {
			return statusFailed, nil, err
		}
		return s.run(context.Background(), runInput{
			task:          task,
			workspaceRoot: ctx.WorkspaceRoot,
			projectAbs:    ctx.Project.FullPath,
			metadata:      ctx.Project.Metadata[s.Extension],
			outputRoot:    output,
			stdout:        os.Stderr,
		}, emit)
	}
}

func outputFlag(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		if v, ok := strings.CutPrefix(args[i], "--report-dir="); ok {
			return v, nil
		}
		if args[i] == "--report-dir" && i+1 < len(args) {
			return args[i+1], nil
		}
	}
	return "", errors.New("the task names no --report-dir directory")
}

// runInput is what one task run reads.
type runInput struct {
	task          string
	workspaceRoot string
	projectAbs    string
	metadata      json.RawMessage
	// outputRoot is the command-output directory; the task writes under
	// outputRoot/<task>.
	outputRoot string
	// stdout receives the steps' output.
	stdout io.Writer
}

// reporter receives a run's diagnostics; jsonl.Emitter is one.
type reporter interface {
	DiagnosticWithCode(severity, message string, file string, line, column int, code string)
	Summary(message string)
	Info(message string)
}

func (s Stack) run(ctx context.Context, in runInput, emit reporter) (string, map[string]any, error) {
	// The diagnostic carries the message; the error names its code only, so
	// the run does not print the message twice.
	failWith := func(code, format string, args ...any) error {
		emit.DiagnosticWithCode("error", fmt.Sprintf(format, args...), "", 0, 0, code)
		return errors.New(code)
	}
	var meta Metadata
	if len(in.metadata) == 0 || json.Unmarshal(in.metadata, &meta) != nil || meta.Component == "" {
		return statusFailed, nil, failWith(CodeNotAComponent, "%s is no component %s found: run putnami projects sync, or remove %s from the project's extensions", in.projectAbs, s.Extension, s.Extension)
	}
	profile, err := s.Resolved(in.projectAbs)
	if err != nil {
		return statusFailed, nil, failWith(CodeProjectConfigInvalid, "%v", err)
	}
	t, ok := profile.Tasks[in.task]
	if !ok {
		emit.Info(fmt.Sprintf("the %s profile declares no %s task", profile.ID, in.task))
		return statusOK, nil, nil
	}
	rootAbs := filepath.Join(in.workspaceRoot, filepath.FromSlash(meta.Root))
	projectRel, err := filepath.Rel(in.workspaceRoot, in.projectAbs)
	if err != nil {
		return statusFailed, nil, failWith(CodeNotAComponent, "%v", err)
	}
	outDir := filepath.Join(in.outputRoot, in.task)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return statusFailed, nil, err
	}

	env := os.Environ()
	expected, err := profile.Toolchain.ExpectedVersion(in.workspaceRoot, filepath.ToSlash(projectRel), meta.Root)
	if err != nil {
		return statusFailed, nil, failWith(CodeProjectConfigInvalid, "the expected toolchain version cannot be read: %v", err)
	}
	selected, err := profile.Toolchain.Select(ctx, in.projectAbs, expected, env)
	if errors.Is(err, ErrToolchainMissing) {
		return statusFailed, nil, failWith(CodeToolchainMissing, "%v: the %s stack needs it on PATH, or a release its extension pins: run putnami install", err, profile.ID)
	}
	if err != nil {
		return statusFailed, nil, failWith(CodeToolchainMissing, "%v", err)
	}
	env = selected.Env
	if v := selected.Verdict; v.Code != "" {
		if v.Fail {
			return statusFailed, nil, failWith(v.Code, "%s", v.Message)
		}
		emit.DiagnosticWithCode("warning", v.Message, "", 0, 0, v.Code)
	}

	// The components of one stack root share its build directory and its
	// reports: one task at a time per root, and the reports leave it before
	// the next one starts.
	lockDir := filepath.Join(in.workspaceRoot, ".putnami", "locks", "stacks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return statusFailed, nil, err
	}
	lock, err := filelock.Acquire(filepath.Join(lockDir, lockName(profile.ID, meta.Root)), true, false)
	if err != nil {
		return statusFailed, nil, fmt.Errorf("lock the stack root %s: %w", meta.Root, err)
	}
	defer func() { _ = lock.Release() }()

	// A report left by another component of the root, or by an earlier
	// run, must not pass for this run's: the reports outside the task's own
	// output are removed before the steps, under the lock.
	if err := removeReports(profile, in.task, in.projectAbs, rootAbs); err != nil {
		return statusFailed, nil, err
	}

	if w := t.WithoutCoverage; w != nil && !hasCoverageSupport(ctx, selected.Program(profile.Toolchain, w.Probe[0]), w.Probe[1:], in.projectAbs, env) {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("the %s toolchain has no coverage support: %s runs with no coverage report", profile.ID, in.task), "", 0, 0, CodeCoverageUnavailable)
		reports := *t.Reports
		reports.Coverage = nil
		t.Steps, t.Reports = w.Steps, &reports
	}

	vars := map[string]string{"{root}": rootAbs, "{component}": meta.Component, "{project}": in.projectAbs, "{output}": outDir}
	// A test runner exits non-zero when a case fails: the steps stop there,
	// and the reports it wrote are still read, so the failed cases name the
	// failure.
	var stepErr error
	for _, step := range t.Steps {
		argv := make([]string, len(step))
		for i, a := range step {
			argv[i] = expand(a, vars)
		}
		emit.Info("$ " + strings.Join(argv, " "))
		cmd := exec.CommandContext(ctx, selected.Program(profile.Toolchain, argv[0]), argv[1:]...) //nolint:gosec // the profile's own step
		cmd.Dir = in.projectAbs
		cmd.Env = env
		cmd.Stdout = in.stdout
		cmd.Stderr = in.stdout
		if err := cmd.Run(); err != nil {
			stepErr = failWith(CodeStepFailed, "%s: %v", strings.Join(argv, " "), err)
			break
		}
	}
	if stepErr != nil && t.Reports == nil {
		return statusFailed, nil, stepErr
	}

	copied := 0
	resolve := func(pattern string) ([]string, error) {
		base, rest := in.projectAbs, pattern
		if r, ok := strings.CutPrefix(pattern, "{output}/"); ok {
			base, rest = outDir, r
		} else if r, ok := strings.CutPrefix(pattern, "{root}/"); ok {
			base, rest = rootAbs, r
		}
		matches, err := globFiles(base, rest)
		if err != nil || base == outDir {
			return matches, err
		}
		var out []string
		for _, m := range matches {
			copied++
			dst := filepath.Join(outDir, "reports", fmt.Sprintf("%02d-%s", copied, filepath.Base(m)))
			if err := copyFile(m, dst); err != nil {
				return nil, err
			}
			out = append(out, dst)
		}
		return out, nil
	}
	summary, diags := ReadReports(t.Reports, resolve, outDir)
	failed := stepErr != nil
	for _, d := range diags {
		// A step that failed may have written no report: the step's own
		// diagnostic says why, not a missing report.
		if stepErr != nil && d.Code == CodeReportMissing {
			continue
		}
		emit.DiagnosticWithCode(severity(d), d.Message, d.Field, 0, 0, d.Code)
		failed = failed || d.Severity == diag.Error
	}
	data := map[string]any{}
	if t.Reports != nil && len(t.Reports.JUnit) > 0 {
		data["testSummary"] = map[string]any{
			"total": summary.Tests.Total, "passed": summary.Tests.Passed,
			"failed": summary.Tests.Failed, "skipped": summary.Tests.Skipped,
		}
		emit.Summary(fmt.Sprintf("%d tests: %d passed, %d failed, %d skipped", summary.Tests.Total, summary.Tests.Passed, summary.Tests.Failed, summary.Tests.Skipped))
	}
	if summary.Coverage != nil {
		pct := 0.0
		if summary.Coverage.Total > 0 {
			pct = float64(summary.Coverage.Covered) * 100 / float64(summary.Coverage.Total)
		}
		data["coverageSummary"] = map[string]any{
			"percentage": pct, "granularity": "lines",
			"covered": summary.Coverage.Covered, "total": summary.Coverage.Total,
		}
	}
	if len(summary.Reports) > 0 {
		data["reports"] = summary.Reports
	}
	if stepErr != nil && summary.Tests.Failed == 0 {
		return statusFailed, data, stepErr
	}
	if failed {
		return statusFailed, data, fmt.Errorf("the %s reports of %s failed", in.task, meta.Component)
	}
	return statusOK, data, nil
}

// hasCoverageSupport runs a task's coverage probe: an exit other than zero
// means the toolchain has no coverage support.
func hasCoverageSupport(ctx context.Context, program string, args []string, dir string, env []string) bool {
	cmd := exec.CommandContext(ctx, program, args...) //nolint:gosec // the profile's own probe
	cmd.Dir = dir
	cmd.Env = env
	return cmd.Run() == nil
}

func severity(d diag.Diagnostic) string {
	if d.Severity == diag.Error {
		return "error"
	}
	return "warning"
}

func expand(arg string, vars map[string]string) string {
	for k, v := range vars {
		arg = strings.ReplaceAll(arg, k, v)
	}
	return arg
}

// lockName is a file name for the lock of one stack root.
func lockName(stack, root string) string {
	r := strings.NewReplacer("/", "_", ".", "_", ":", "_").Replace(root)
	return stack + "-" + r + ".lock"
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src) //nolint:gosec // a report the profile names
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644) //nolint:gosec // a copied report
}

// removeReports deletes the files the report patterns of a task match
// outside its output directory, under the project or the stack root. A file
// the task's own inputs select is a source, never a stale report: it is kept.
func removeReports(profile Profile, task, projectAbs, rootAbs string) error {
	r := profile.Tasks[task].Reports
	if r == nil {
		return nil
	}
	projectInputs, rootInputs := profile.TaskPatterns(task)
	isInput := func(m string) bool {
		for _, in := range []struct {
			base     string
			patterns []string
		}{{projectAbs, projectInputs}, {rootAbs, rootInputs}} {
			rel, err := filepath.Rel(in.base, m)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			for _, p := range in.patterns {
				if MatchPattern(p, filepath.ToSlash(rel)) {
					return true
				}
			}
		}
		return false
	}
	patterns := slices.Clone(r.JUnit)
	for _, c := range r.Coverage {
		patterns = append(patterns, c.Paths...)
	}
	for _, p := range patterns {
		if strings.HasPrefix(p, "{output}/") {
			continue
		}
		base, rest := projectAbs, p
		if r, ok := strings.CutPrefix(p, "{root}/"); ok {
			base, rest = rootAbs, r
		}
		matches, err := globFiles(base, rest)
		if err != nil {
			return err
		}
		for _, m := range matches {
			if isInput(m) {
				continue
			}
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove the stale report %s: %w", m, err)
			}
		}
	}
	return nil
}
