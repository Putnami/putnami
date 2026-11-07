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
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Diagnostic codes of the task runner.
const (
	CodeNotAComponent = "stacks.not_a_component"
	CodeStepFailed    = "stacks.step_failed"
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
		if v, ok := strings.CutPrefix(args[i], "--output="); ok {
			return v, nil
		}
		if args[i] == "--output" && i+1 < len(args) {
			return args[i+1], nil
		}
	}
	return "", errors.New("the task names no --output directory")
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
	failWith := func(code, format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		emit.DiagnosticWithCode("error", msg, "", 0, 0, code)
		return errors.New(msg)
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
		return statusFailed, nil, failWith(CodeToolchainMismatch, "%v", err)
	}
	machine, err := profile.Toolchain.MachineVersion(ctx, in.projectAbs, env)
	if errors.Is(err, ErrToolchainMissing) {
		return statusFailed, nil, failWith(CodeToolchainMissing, "%v: the %s stack needs it on PATH; Putnami installs no toolchain", err, profile.ID)
	}
	if err != nil {
		return statusFailed, nil, failWith(CodeToolchainMissing, "%v", err)
	}
	if v := profile.Toolchain.Compare(expected, machine); v.Code != "" {
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

	vars := map[string]string{"{root}": rootAbs, "{component}": meta.Component, "{project}": in.projectAbs, "{output}": outDir}
	for _, step := range t.Steps {
		argv := make([]string, len(step))
		for i, a := range step {
			argv[i] = expand(a, vars)
		}
		emit.Info("$ " + strings.Join(argv, " "))
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the profile's own step
		cmd.Dir = in.projectAbs
		cmd.Env = env
		cmd.Stdout = in.stdout
		cmd.Stderr = in.stdout
		if err := cmd.Run(); err != nil {
			return statusFailed, nil, failWith(CodeStepFailed, "%s: %v", strings.Join(argv, " "), err)
		}
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
	failed := false
	for _, d := range diags {
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
	if failed {
		return statusFailed, data, fmt.Errorf("the %s reports of %s failed", in.task, meta.Component)
	}
	return statusOK, data, nil
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
