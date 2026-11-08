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
	"go.putnami.dev/sdk/extension/cli"
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

// failWith reports a diagnostic and returns its error. The diagnostic carries
// the message; the error names its code only, so the run does not print the
// message twice.
func failWith(emit reporter, code, format string, args ...any) error {
	emit.DiagnosticWithCode("error", fmt.Sprintf(format, args...), "", 0, 0, code)
	return errors.New(code)
}

func (s Stack) component(in runInput, emit reporter) (Metadata, Profile, error) {
	var meta Metadata
	if len(in.metadata) == 0 || json.Unmarshal(in.metadata, &meta) != nil || (meta.Component == "" && meta.Root == "") {
		return Metadata{}, Profile{}, failWith(emit, CodeNotAComponent, "%s is no component %s found: run putnami projects sync, or remove %s from the project's extensions", in.projectAbs, s.Extension, s.Extension)
	}
	if meta.Component == "" {
		return meta, Profile{}, nil
	}
	profile, err := s.Resolved(in.projectAbs)
	if err != nil {
		return Metadata{}, Profile{}, failWith(emit, CodeProjectConfigInvalid, "%v", err)
	}
	return meta, profile, nil
}

// selectToolchain runs the toolchain check of the component and returns the
// toolchain it selects. A verdict that does not fail is a warning.
func selectToolchain(ctx context.Context, in runInput, meta Metadata, profile Profile, emit reporter) (Selection, error) {
	projectRel, err := filepath.Rel(in.workspaceRoot, in.projectAbs)
	if err != nil {
		return Selection{}, failWith(emit, CodeNotAComponent, "%v", err)
	}
	expected, err := profile.Toolchain.ExpectedVersion(in.workspaceRoot, filepath.ToSlash(projectRel), meta.Root)
	if err != nil {
		return Selection{}, failWith(emit, CodeProjectConfigInvalid, "the expected toolchain version cannot be read: %v", err)
	}
	selected, err := profile.Toolchain.Select(ctx, in.projectAbs, expected, os.Environ())
	if errors.Is(err, ErrToolchainMissing) {
		if profile.Toolchain.Provisioned != "" {
			return Selection{}, failWith(emit, CodeToolchainMissing, "%v: the %s stack needs it on PATH, or a release its extension pins: run putnami install", err, profile.ID)
		}
		return Selection{}, failWith(emit, CodeToolchainMissing, "%v: the %s stack needs it on PATH", err, profile.ID)
	}
	if err != nil {
		return Selection{}, failWith(emit, CodeToolchainMissing, "%v", err)
	}
	if v := selected.Verdict; v.Code != "" {
		if v.Fail {
			return Selection{}, failWith(emit, v.Code, "%s", v.Message)
		}
		emit.DiagnosticWithCode("warning", v.Message, "", 0, 0, v.Code)
	}
	return selected, nil
}

// lockStackRoot takes the lock of the component's stack root: the components
// of one root share its build directory and its reports, so one task runs at
// a time per root. The caller releases the returned lock.
func lockStackRoot(workspaceRoot string, meta Metadata, profile Profile) (*filelock.Lock, error) {
	lockDir := filepath.Join(workspaceRoot, ".putnami", "locks", "stacks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	lock, err := filelock.Acquire(filepath.Join(lockDir, lockName(profile.ID, meta.Root)), true, false)
	if err != nil {
		return nil, fmt.Errorf("lock the stack root %s: %w", meta.Root, err)
	}
	return lock, nil
}

// RootStep is a step a stack extension runs at a component's stack root, under
// the root's lock, with the toolchain the component's tasks select.
type RootStep struct {
	// Dir is the stack root's absolute path.
	Dir string
	// Env is the environment of the step.
	Env []string
	// Program returns the program to run for a command name: the pinned
	// toolchain for the toolchain command, and name otherwise.
	Program func(name string) string
}

// RootJob is a job that runs step at the component's stack root: the toolchain
// check, then step under the root's lock.
func (s Stack) RootJob(step func(context.Context, RootStep, *jsonl.Emitter) error) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		background := context.Background()
		in := runInput{workspaceRoot: ctx.WorkspaceRoot, projectAbs: ctx.Project.FullPath, metadata: ctx.Project.Metadata[s.Extension]}
		meta, profile, err := s.component(in, emit)
		if err != nil {
			return statusFailed, nil, err
		}
		if meta.Component == "" {
			return statusOK, nil, nil
		}
		selected, err := selectToolchain(background, in, meta, profile, emit)
		if err != nil {
			return statusFailed, nil, err
		}
		lock, err := lockStackRoot(in.workspaceRoot, meta, profile)
		if err != nil {
			return statusFailed, nil, err
		}
		defer func() { _ = lock.Release() }()
		root := RootStep{
			Dir:     filepath.Join(in.workspaceRoot, filepath.FromSlash(meta.Root)),
			Env:     selected.Env,
			Program: func(name string) string { return selected.Program(profile.Toolchain, name) },
		}
		if err := step(background, root, emit); err != nil {
			return statusFailed, nil, err
		}
		return statusOK, nil, nil
	}
}

func (s Stack) run(ctx context.Context, in runInput, emit reporter) (string, map[string]any, error) {
	meta, profile, err := s.component(in, emit)
	if err != nil {
		return statusFailed, nil, err
	}
	if meta.Component == "" {
		emit.Info(fmt.Sprintf("%s is a %s stack root with no component: its %s task reads the root's files and runs no step", meta.Root, s.Profile.ID, in.task))
		return statusOK, nil, nil
	}
	t, ok := profile.Tasks[in.task]
	if !ok {
		emit.Info(fmt.Sprintf("the %s profile declares no %s task", profile.ID, in.task))
		return statusOK, nil, nil
	}
	rootAbs := filepath.Join(in.workspaceRoot, filepath.FromSlash(meta.Root))
	outDir := filepath.Join(in.outputRoot, in.task)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return statusFailed, nil, err
	}

	selected, err := selectToolchain(ctx, in, meta, profile, emit)
	if err != nil {
		return statusFailed, nil, err
	}
	env := selected.Env

	lock, err := lockStackRoot(in.workspaceRoot, meta, profile)
	if err != nil {
		return statusFailed, nil, err
	}
	defer func() { _ = lock.Release() }()

	// A report left by another component of the root, or by an earlier
	// run, must not pass for this run's: the reports outside the task's own
	// output are removed before the steps, under the lock.
	if err := removeReports(profile, in.task, in.projectAbs, rootAbs); err != nil {
		return statusFailed, nil, err
	}

	if w := t.WithoutCoverage; w != nil && t.Reports != nil && len(t.Reports.Coverage) > 0 && !hasCoverageSupport(ctx, selected.Program(profile.Toolchain, w.Probe[0]), w.Probe[1:], in.projectAbs, env) {
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
			stepErr = failWith(emit, CodeStepFailed, "%s: %v", strings.Join(argv, " "), err)
			break
		}
	}
	if stepErr != nil && t.Reports == nil {
		return statusFailed, nil, stepErr
	}

	if err := copyArtifacts(t.Artifacts, meta.Component, in.projectAbs, rootAbs, outDir); err != nil {
		return statusFailed, nil, err
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
	summary, diags := ReadReports(t.Reports, resolve, outDir, s.attribution(in.workspaceRoot, in.projectAbs, meta.Component))
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
	if summary.OtherCases > 0 || summary.OtherFiles > 0 {
		emit.Info(fmt.Sprintf("left to their own components: %d test cases and %d coverage files", summary.OtherCases, summary.OtherFiles))
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

// copyArtifacts copies the files a task's artifact patterns match into
// outDir/artifacts, keeping their mode. A pattern that matches nothing copies
// nothing.
func copyArtifacts(patterns []string, component, projectAbs, rootAbs, outDir string) error {
	for _, pattern := range patterns {
		pattern = strings.ReplaceAll(pattern, "{component}", component)
		base, rest := projectAbs, pattern
		if r, ok := strings.CutPrefix(pattern, "{root}/"); ok {
			base, rest = rootAbs, r
		}
		matches, err := globFiles(base, rest)
		if err != nil {
			return err
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(m) //nolint:gosec // an artifact the profile names
			if err != nil {
				return err
			}
			dst := filepath.Join(outDir, "artifacts", filepath.Base(m))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
				return fmt.Errorf("copy the artifact %s: %w", m, err)
			}
		}
	}
	return nil
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
