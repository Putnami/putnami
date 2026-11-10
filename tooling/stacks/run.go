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
	"runtime"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
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
// directory, or the stack root when the task's dir is root, under the stack
// root's lock, then the reports, copied into the task's output directory and
// read. A delegated stack's result names the stack in delegatedTo.
func (s Stack) Run(task string) func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return s.RunWith(task, nil)
}

// RunWith is Run with the arguments extra answers appended to the task's last
// step, the one that does the task's work. It calls extra with the task's
// selected toolchain, its output directory and that step's expanded command,
// under the stack root's lock. A nil extra adds none.
func (s Stack) RunWith(task string, extra func(ctx *pctx.Context, root RootStep, lastStep []string) []string) func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return s.runWith(task, extra, false)
}

// RunFiltered is RunWith for a test filter: filter answers the arguments that
// pass the filter to the task's last step, or none without a filter. A filter
// also reaches the components the selected ones depend on, where it may select
// no test, and a test runner writes no report for a run that selects none: a
// filtered run whose steps pass and that finds no JUnit or TRX report reads
// zero tests and no coverage, where any other run fails with
// CodeReportMissing.
func (s Stack) RunFiltered(task string, filter func(ctx *pctx.Context, root RootStep, lastStep []string) []string) func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return s.runWith(task, filter, true)
}

func (s Stack) runWith(task string, extra func(ctx *pctx.Context, root RootStep, lastStep []string) []string, filters bool) func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		output, err := outputFlag(args)
		if err != nil {
			return statusFailed, nil, err
		}
		var stepArgs func(RootStep, []string) []string
		if extra != nil {
			stepArgs = func(root RootStep, lastStep []string) []string { return extra(ctx, root, lastStep) }
		}
		return s.run(context.Background(), runInput{
			task:          task,
			stepArgs:      stepArgs,
			filters:       filters,
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
	task string
	// stepArgs answers the arguments appended to the task's last step, given
	// that step's expanded command.
	stepArgs func(RootStep, []string) []string
	// filters reads the arguments stepArgs answers as a test filter: a run
	// they select no test in reads zero tests.
	filters       bool
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
	// A stack root that holds no component runs no step, but a job at that
	// root, such as an install, selects its toolchain: its profile resolves
	// too.
	profile, err := s.Resolved(in.projectAbs, meta.Variant)
	if err != nil {
		return Metadata{}, Profile{}, failWith(emit, CodeProjectConfigInvalid, "%v", err)
	}
	return meta, profile, nil
}

// selectToolchain runs the toolchain check of the component and returns the
// toolchain it selects. A verdict that does not fail is a warning.
func (s Stack) selectToolchain(ctx context.Context, in runInput, meta Metadata, profile Profile, emit reporter) (Selection, error) {
	projectRel, err := filepath.Rel(in.workspaceRoot, in.projectAbs)
	if err != nil {
		return Selection{}, failWith(emit, CodeNotAComponent, "%v", err)
	}
	// A check that states no version warns only when no check does: the key
	// then carries no toolchain version at all.
	checks := profile.Toolchain.Checks()
	expectedAll := make([]Expected, len(checks))
	pinned := false
	for i, check := range checks {
		expectedAll[i], err = check.ExpectedVersion(in.workspaceRoot, filepath.ToSlash(projectRel), meta.Root)
		if err != nil {
			return Selection{}, failWith(emit, CodeProjectConfigInvalid, "the expected toolchain version cannot be read: %v", err)
		}
		pinned = pinned || expectedAll[i].Version != ""
	}
	expected := expectedAll[0]
	env := os.Environ()
	matches := versionMatches
	if s.MatchVersion != nil {
		matches = func(e, m string) bool {
			return s.MatchVersion(in.workspaceRoot, Expected{Version: e, Source: expected.Source}, m)
		}
	}
	// The further checks read the selected toolchain: a command beside it
	// runs that one.
	further := func(sel Selection) ([]Verdict, error) {
		var out []Verdict
		for i, check := range checks[1:] {
			c := check
			c.Command = append([]string{sel.ProgramIn(profile.Toolchain, check.Command[0], sel.Env)}, check.Command[1:]...)
			machine, err := c.MachineVersion(ctx, in.projectAbs, sel.Env)
			if err != nil {
				return nil, err
			}
			out = append(out, check.Compare(expectedAll[i+1], machine))
		}
		return out, nil
	}
	furtherFails := func(verdicts []Verdict, err error) bool {
		return err != nil || slices.ContainsFunc(verdicts, func(v Verdict) bool { return v.Fail })
	}
	selected, err := profile.Toolchain.SelectWith(ctx, in.projectAbs, expected, env, matches)
	var furtherVerdicts []Verdict
	var furtherErr error
	if err == nil && !selected.Verdict.Fail {
		furtherVerdicts, furtherErr = further(selected)
	}
	offline := os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
	provides := s.provides(profile)
	if provides && !offline && (errors.Is(err, ErrToolchainMissing) || err == nil && (selected.Verdict.Fail || furtherFails(furtherVerdicts, furtherErr))) {
		// A further check that fails, such as the Dart a Flutter SDK
		// bundles, asks for the installed toolchain even when the one on
		// PATH passes the first check.
		forcePinned := err == nil && !selected.Verdict.Fail
		withRoot := slices.Clone(expectedAll)
		for i := range withRoot {
			withRoot[i].Root = in.workspaceRoot
		}
		provided, provideErr := s.Provide(ctx, profile.Toolchain, withRoot, emit.Info)
		if provideErr != nil {
			return Selection{}, failWith(emit, CodeToolchainMissing, "install the %s toolchain: %v", profile.ID, provideErr)
		}
		env = append(append(env, provided.Env...), profile.Toolchain.Provisioned+"="+provided.Executable)
		if forcePinned {
			selected, err = profile.Toolchain.Pinned(ctx, in.projectAbs, expected, env)
		} else {
			selected, err = profile.Toolchain.SelectWith(ctx, in.projectAbs, expected, env, matches)
		}
		furtherVerdicts, furtherErr = nil, nil
		if err == nil && !selected.Verdict.Fail {
			furtherVerdicts, furtherErr = further(selected)
		}
	}
	if errors.Is(err, ErrToolchainMissing) {
		if provides {
			return Selection{}, failWith(emit, CodeToolchainMissing, "%v: the %s stack needs it on PATH, or a network to install it", err, profile.ID)
		}
		if profile.Toolchain.Provisioned != "" {
			return Selection{}, failWith(emit, CodeToolchainMissing, "%v: the %s stack needs it on PATH, or a release its extension pins: run putnami install", err, profile.ID)
		}
		return Selection{}, failWith(emit, CodeToolchainMissing, "%v: the %s stack needs it on PATH", err, profile.ID)
	}
	if err != nil {
		return Selection{}, failWith(emit, CodeToolchainMissing, "%v", err)
	}
	warnedUnpinned := false
	report := func(v Verdict, stated Expected) error {
		if v.Code == "" || (stated.Version == "" && pinned) || (v.Code == CodeToolchainUnpinned && warnedUnpinned) {
			return nil
		}
		if v.Fail {
			return failWith(emit, v.Code, "%s", v.Message)
		}
		warnedUnpinned = warnedUnpinned || v.Code == CodeToolchainUnpinned
		emit.DiagnosticWithCode("warning", v.Message, "", 0, 0, v.Code)
		return nil
	}
	if err := report(selected.Verdict, expected); err != nil {
		return Selection{}, err
	}
	if furtherErr != nil {
		return Selection{}, failWith(emit, CodeToolchainMissing, "%v: the %s stack needs it on PATH", furtherErr, profile.ID)
	}
	for i, v := range furtherVerdicts {
		if err := report(v, expectedAll[i+1]); err != nil {
			return Selection{}, err
		}
	}
	if s.Tools != nil && !offline {
		dirs, err := s.Tools(ctx, in.task, meta, selected.Env, emit.Info)
		if err != nil {
			return Selection{}, failWith(emit, CodeToolchainMissing, "install the tools of the %s task: %v", in.task, err)
		}
		for _, dir := range dirs {
			selected.Env = appendPath(selected.Env, dir)
		}
	}
	return selected, nil
}

// lockStackRoot takes the lock of the component's stack root: the components
// of one root share its build directory and its reports, so one task runs at
// a time per root. The caller releases the returned lock.
func lockStackRoot(workspaceRoot string, meta Metadata, profile Profile) (*filelock.Lock, error) {
	return LockRoot(workspaceRoot, profile.ID, meta.Root)
}

// LockRoot takes the lock the tasks of a stack hold on its root, for a tool
// call an extension makes outside a task. The caller releases it.
func LockRoot(workspaceRoot, stack, root string) (*filelock.Lock, error) {
	lockDir := filepath.Join(workspaceRoot, ".putnami", "locks", "stacks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	lock, err := filelock.Acquire(filepath.Join(lockDir, lockName(stack, root)), true, false)
	if err != nil {
		return nil, fmt.Errorf("lock the stack root %s: %w", root, err)
	}
	return lock, nil
}

// RootStep is a step a stack extension runs at a component's stack root, under
// the root's lock, with the toolchain the component's tasks select.
type RootStep struct {
	// Dir is the stack root's absolute path.
	Dir string
	// Project is the component's absolute path.
	Project string
	// Component is the name of the component the step runs for; empty at a
	// stack root that holds no component.
	Component string
	// Expected is the version the component's first toolchain check expects.
	Expected Expected
	// Env is the environment of the step.
	Env []string
	// Output is the task's output directory, emptied before its steps, for
	// the arguments RunWith and RunFiltered append to a task's last step;
	// empty for a step a root job runs.
	Output string
	// Program returns the program to run for a command name: the pinned
	// toolchain for the toolchain command, and name otherwise.
	Program func(name string) string
	// Manifests are the absolute manifest paths of the components the step
	// runs for, for a detector that names them: the job's component, or, in a
	// workspace job, every component of the workspace at this stack root.
	Manifests []string
}

// RootJob is a job that runs step at the component's stack root: the toolchain
// check, then step under the root's lock.
func (s Stack) RootJob(step func(context.Context, RootStep, *jsonl.Emitter) error) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		in := runInput{workspaceRoot: ctx.WorkspaceRoot, projectAbs: ctx.Project.FullPath, metadata: ctx.Project.Metadata[s.Extension]}
		if err := s.atRoot(context.Background(), in, step, emit); err != nil {
			return statusFailed, nil, err
		}
		return statusOK, nil, nil
	}
}

// atRoot selects the component's toolchain and runs step at its stack root,
// under the root's lock.
func (s Stack) atRoot(ctx context.Context, in runInput, step func(context.Context, RootStep, *jsonl.Emitter) error, emit *jsonl.Emitter) error {
	root, meta, profile, err := s.rootStep(ctx, in, emit)
	if err != nil {
		return err
	}
	lock, err := lockStackRoot(in.workspaceRoot, meta, profile)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	return step(ctx, root, emit)
}

func (s Stack) rootStep(ctx context.Context, in runInput, emit *jsonl.Emitter) (RootStep, Metadata, Profile, error) {
	meta, profile, err := s.component(in, emit)
	if err != nil {
		return RootStep{}, Metadata{}, Profile{}, err
	}
	selected, err := s.selectToolchain(ctx, in, meta, profile, emit)
	if err != nil {
		return RootStep{}, Metadata{}, Profile{}, err
	}
	var manifests []string
	if meta.Manifest != "" {
		manifests = []string{filepath.Join(in.workspaceRoot, filepath.FromSlash(meta.Manifest))}
	}
	projectRel, err := filepath.Rel(in.workspaceRoot, in.projectAbs)
	if err != nil {
		return RootStep{}, Metadata{}, Profile{}, failWith(emit, CodeNotAComponent, "%v", err)
	}
	expected, err := profile.Toolchain.ExpectedVersion(in.workspaceRoot, filepath.ToSlash(projectRel), meta.Root)
	if err != nil {
		return RootStep{}, Metadata{}, Profile{}, failWith(emit, CodeProjectConfigInvalid, "the expected toolchain version cannot be read: %v", err)
	}
	return RootStep{
		Dir:       filepath.Join(in.workspaceRoot, filepath.FromSlash(meta.Root)),
		Project:   in.projectAbs,
		Component: meta.Component,
		Expected:  expected,
		Env:       selected.Env,
		Program:   func(name string) string { return selected.Program(profile.Toolchain, name) },
		Manifests: manifests,
	}, meta, profile, nil
}

// Locked runs step at the job's component's stack root with its toolchain
// selected, under the root's lock: a workload command that builds with the
// stack's build, then runs what it built, builds under the lock and releases
// it before it runs, so it never shares the build directory with a task.
func (s Stack) Locked(ctx *pctx.Context, emit *jsonl.Emitter, step func(context.Context, RootStep, *jsonl.Emitter) error) error {
	in := runInput{workspaceRoot: ctx.WorkspaceRoot, projectAbs: ctx.Project.FullPath, metadata: ctx.Project.Metadata[s.Extension]}
	return s.atRoot(context.Background(), in, step, emit)
}

// Selected answers the job's component with its toolchain selected and no lock
// of its stack root held: a process the user keeps running must not block the
// other tasks of the root.
func (s Stack) Selected(ctx *pctx.Context, emit *jsonl.Emitter) (RootStep, error) {
	in := runInput{workspaceRoot: ctx.WorkspaceRoot, projectAbs: ctx.Project.FullPath, metadata: ctx.Project.Metadata[s.Extension]}
	root, _, _, err := s.rootStep(context.Background(), in, emit)
	return root, err
}

// MissingToolchain is what a workspace job does at a stack root whose
// toolchain the machine does not have.
type MissingToolchain int

const (
	// FailOnMissingToolchain fails the job.
	FailOnMissingToolchain MissingToolchain = iota
	// WarnOnMissingToolchain reports a warning and moves on to the next root.
	WarnOnMissingToolchain
)

// WorkspaceRootJob is a workspace job: it runs step once at each stack root of
// the workspace's components, in path order. It leaves out the projects of
// another extension and the projects whose authored tags the workspace's
// disable.tags names.
func (s Stack) WorkspaceRootJob(missing MissingToolchain, step func(context.Context, RootStep, *jsonl.Emitter) error) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		background := context.Background()
		components, manifests, err := s.workspaceComponents(ctx)
		if err != nil {
			return statusFailed, nil, failWith(emit, CodeProjectConfigInvalid, "%v", err)
		}
		for _, c := range components {
			step := step
			if m := manifests[c.Path]; len(m) > 0 {
				inner := step
				step = func(ctx context.Context, root RootStep, emit *jsonl.Emitter) error {
					root.Manifests = m
					return inner(ctx, root, emit)
				}
			}
			in := runInput{workspaceRoot: ctx.WorkspaceRoot, projectAbs: filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(c.Path)), metadata: c.Metadata}
			if missing == WarnOnMissingToolchain && s.toolchainMissing(background, in) {
				emit.DiagnosticWithCode("warning", fmt.Sprintf("%s is left out: the %s toolchain is not on this machine", c.Path, s.Profile.ID), "", 0, 0, CodeToolchainMissing)
				continue
			}
			if err := s.atRoot(background, in, step, emit); err != nil {
				return statusFailed, nil, err
			}
		}
		return statusOK, nil, nil
	}
}

// provides reports whether the stack installs the toolchain of a resolved
// profile: it sets Provide, and the profile names toolchain.provisioned. A
// variant that names none, such as an Xcode project's, keeps its check only.
func (s Stack) provides(profile Profile) bool {
	return s.Provide != nil && profile.Toolchain.Provisioned != ""
}

// toolchainMissing reports whether the machine has no toolchain for the
// component. A profile that does not resolve counts as present: atRoot reports
// it; so does a toolchain the stack installs when the run is not offline.
func (s Stack) toolchainMissing(ctx context.Context, in runInput) bool {
	var meta Metadata
	_ = json.Unmarshal(in.metadata, &meta)
	profile, err := s.Resolved(in.projectAbs, meta.Variant)
	if err != nil {
		return false
	}
	if s.provides(profile) && os.Getenv(extensionproto.OfflineDependenciesEnv) != "1" {
		return false
	}
	_, err = profile.Toolchain.Select(ctx, in.projectAbs, Expected{}, os.Environ())
	return errors.Is(err, ErrToolchainMissing)
}

// workspaceComponents returns one component per stack root among the
// workspace's projects this extension owns, and, by the path of each, the
// absolute manifests of every component at its root.
func (s Stack) workspaceComponents(ctx *pctx.Context) ([]wsproto.ProbeProject, map[string][]string, error) {
	disabled, err := disabledTags(ctx.WorkspaceRoot)
	if err != nil {
		return nil, nil, err
	}
	paths := []string{}
	for _, ref := range ctx.WorkspaceProjects {
		if s.ownedBy(ctx.WorkspaceRoot, ref.Extensions) && !carriesAny(ref.Config, disabled) {
			paths = append(paths, ref.Path)
		}
	}
	slices.Sort(paths)
	result, err := s.Probe(ctx.WorkspaceRoot, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: paths})
	if err != nil {
		return nil, nil, err
	}
	// A component stands for its stack root before the input-only project of
	// that root: its toolchain is the one the root's tasks select.
	chosen := map[string]int{}
	components := make([]wsproto.ProbeProject, 0, len(result.Projects))
	inputOnly := map[int]bool{}
	rootManifests := map[string][]string{}
	for _, p := range result.Projects {
		var meta Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			return nil, nil, err
		}
		if meta.Manifest != "" {
			rootManifests[meta.Root] = append(rootManifests[meta.Root], filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(meta.Manifest)))
		}
		i, seen := chosen[meta.Root]
		switch {
		case !seen:
			chosen[meta.Root] = len(components)
			inputOnly[len(components)] = meta.Component == ""
			components = append(components, p)
		case inputOnly[i] && meta.Component != "":
			components[i], inputOnly[i] = p, false
		}
	}
	manifests := map[string][]string{}
	for _, c := range components {
		var meta Metadata
		_ = json.Unmarshal(c.Metadata, &meta)
		manifests[c.Path] = rootManifests[meta.Root]
	}
	return components, manifests, nil
}

// ownedBy reports whether one of the extension references is this stack's: its
// name, or a workspace path whose manifest has that name.
func (s Stack) ownedBy(workspaceRoot string, references []string) bool {
	for _, reference := range references {
		if reference == s.Extension {
			return true
		}
		if strings.HasPrefix(reference, "/") {
			data, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(reference), "putnami.extension.json")) //nolint:gosec // a path of the workspace
			var manifest struct {
				Name string `json:"name"`
			}
			if err == nil && json.Unmarshal(data, &manifest) == nil && manifest.Name == s.Extension {
				return true
			}
		}
	}
	return false
}

// disabledTags reads the disable.tags of the workspace's configuration.
func disabledTags(workspaceRoot string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "putnami.workspace.json")) //nolint:gosec // the workspace's own file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config struct {
		Disable struct {
			Tags []string `json:"tags"`
		} `json:"disable"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("putnami.workspace.json: %w", err)
	}
	return config.Disable.Tags, nil
}

// carriesAny reports whether the authored project configuration has one of the
// tags.
func carriesAny(config json.RawMessage, tags []string) bool {
	if len(config) == 0 || len(tags) == 0 {
		return false
	}
	var project struct {
		Tags []string `json:"tags"`
	}
	if json.Unmarshal(config, &project) != nil {
		return false
	}
	for _, tag := range project.Tags {
		if slices.Contains(tags, tag) {
			return true
		}
	}
	return false
}

// expandStep answers step with its placeholders replaced.
func expandStep(step []string, vars map[string]string) []string {
	argv := make([]string, len(step))
	for i, a := range step {
		argv[i] = expand(a, vars)
	}
	return argv
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
	projectRel, err := filepath.Rel(in.workspaceRoot, in.projectAbs)
	if err != nil {
		return statusFailed, nil, failWith(emit, CodeNotAComponent, "%v", err)
	}
	if tasks := s.componentTasks(in.workspaceRoot, filepath.ToSlash(projectRel), meta.Tasks); tasks != nil && !slices.Contains(tasks, in.task) {
		emit.Info(fmt.Sprintf("%s runs no %s task", meta.Component, in.task))
		return statusOK, nil, nil
	}
	rootAbs := filepath.Join(in.workspaceRoot, filepath.FromSlash(meta.Root))
	// The output directory starts empty.
	outDir := filepath.Join(in.outputRoot, in.task)
	if err := os.RemoveAll(outDir); err != nil {
		return statusFailed, nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return statusFailed, nil, err
	}

	selected, err := s.selectToolchain(ctx, in, meta, profile, emit)
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
	// An artifact an earlier build left is no product of this one: the file
	// each literal artifact pattern names is removed before the steps, so
	// only what the steps write is copied. A component name that is no single
	// path segment names no artifact.
	artifacts := t.Artifacts
	if len(artifacts) > 0 && !singleSegment(meta.Component) {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("the component name %q is no single path segment: the %s task keeps no artifact", meta.Component, in.task), "", 0, 0, CodeProjectConfigInvalid)
		artifacts = nil
	}
	var literal []string
	for _, a := range artifactPatterns(artifacts, meta.Component) {
		if !strings.ContainsAny(a, "*?[") {
			literal = append(literal, a)
		}
	}
	if err := removeOutputs(literal, profile, in.projectAbs, rootAbs); err != nil {
		return statusFailed, nil, err
	}

	if w := t.WithoutCoverage; w != nil && t.Reports != nil && len(t.Reports.Coverage) > 0 && !hasCoverageSupport(ctx, selected.ProgramIn(profile.Toolchain, w.Probe[0], env), w.Probe[1:], in.projectAbs, env) {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("the %s toolchain has no coverage support: %s runs with no coverage report", profile.ID, in.task), "", 0, 0, CodeCoverageUnavailable)
		reports := *t.Reports
		reports.Coverage = nil
		t.Steps, t.Reports = w.Steps, &reports
	}

	vars := map[string]string{"{root}": rootAbs, "{component}": meta.Component, "{project}": in.projectAbs, "{output}": outDir}
	if meta.Manifest != "" {
		vars["{manifest}"] = filepath.Join(in.workspaceRoot, filepath.FromSlash(meta.Manifest))
	}
	for _, step := range t.Steps {
		for _, a := range step {
			if strings.Contains(a, "{manifest}") && meta.Manifest == "" {
				return statusFailed, nil, failWith(emit, CodeProjectConfigInvalid, "%s: the step names {manifest}, and the %s detector names no manifest for %s", strings.Join(step, " "), profile.ID, meta.Component)
			}
		}
	}
	// A step that names {target} runs once per target the task lists in the
	// project directory, and not at all when there is none.
	targets, err := t.ListTargets(in.projectAbs, runtime.GOOS)
	if err != nil {
		return statusFailed, nil, failWith(emit, CodeProjectConfigInvalid, "list the %s targets: %v", in.task, err)
	}
	var steps [][]string
	for _, step := range t.Steps {
		if !namesTarget(step) {
			steps = append(steps, step)
			continue
		}
		if len(targets) == 0 {
			emit.Info(fmt.Sprintf("no %s target in %s: %s does not run", in.task, meta.Component, strings.Join(step, " ")))
		}
		for _, target := range targets {
			steps = append(steps, expandStep(step, map[string]string{TargetPlaceholder: target}))
		}
	}
	t.Steps = steps
	// {runtime} is resolved only for a task that names it, in a step or a
	// report step, so a stack with no runtime command never depends on
	// locating its own executable.
	if slices.ContainsFunc(slices.Concat(t.Steps, t.ReportSteps), func(step []string) bool {
		return slices.ContainsFunc(step, func(a string) bool { return strings.Contains(a, "{runtime}") })
	}) {
		runtime, err := s.runtimePath()
		if err != nil {
			return statusFailed, nil, failWith(emit, CodeStepFailed, "%v", err)
		}
		vars["{runtime}"] = runtime
	}
	// A test runner exits non-zero when a case fails: the steps stop there,
	// and the reports it wrote are still read, so the failed cases name the
	// failure.
	var stepErr error
	var stepArgs []string
	if in.stepArgs != nil && len(t.Steps) > 0 {
		root := RootStep{Dir: rootAbs, Project: in.projectAbs, Env: env, Output: outDir, Program: func(name string) string { return selected.Program(profile.Toolchain, name) }}
		stepArgs = in.stepArgs(root, expandStep(t.Steps[len(t.Steps)-1], vars))
	}
	filtered := in.filters && len(stepArgs) > 0
	for n, step := range t.Steps {
		argv := expandStep(step, vars)
		if n == len(t.Steps)-1 {
			argv = append(argv, stepArgs...)
		}
		emit.Info("$ " + strings.Join(argv, " "))
		cmd := exec.CommandContext(ctx, selected.ProgramIn(profile.Toolchain, argv[0], env), argv[1:]...) //nolint:gosec // the profile's own step
		cmd.Dir = in.projectAbs
		if t.Dir == DirRoot {
			cmd.Dir = rootAbs
		}
		cmd.Env = env
		cmd.Stdout = in.stdout
		cmd.Stderr = in.stdout
		if err := cmd.Run(); err != nil {
			stepErr = failWith(emit, CodeStepFailed, "%s: %v", strings.Join(argv, " "), err)
			break
		}
	}
	// The report steps write the reports a failing runner leaves to a
	// converter: they run whatever the steps' outcome, so the failed cases
	// name the failure.
	for _, step := range t.ReportSteps {
		argv := expandStep(step, vars)
		emit.Info("$ " + strings.Join(argv, " "))
		cmd := exec.CommandContext(ctx, selected.ProgramIn(profile.Toolchain, argv[0], env), argv[1:]...) //nolint:gosec // the profile's own step
		cmd.Dir = in.projectAbs
		cmd.Env = env
		cmd.Stdout = in.stdout
		cmd.Stderr = in.stdout
		if err := cmd.Run(); err != nil {
			if stepErr == nil {
				stepErr = failWith(emit, CodeStepFailed, "%s: %v", strings.Join(argv, " "), err)
			} else {
				emit.Info(fmt.Sprintf("%s: %v", strings.Join(argv, " "), err))
			}
		}
	}
	if stepErr != nil && t.Reports == nil {
		var failure map[string]any
		if profile.Delegated {
			failure = map[string]any{"delegatedTo": profile.ID}
		}
		return statusFailed, failure, stepErr
	}

	if err := copyArtifacts(artifacts, meta.Component, in.projectAbs, rootAbs, outDir); err != nil {
		return statusFailed, nil, err
	}

	copied := 0
	// locate answers the directory a report pattern is relative to, and the
	// pattern below it.
	locate := func(pattern string) (string, string) {
		if r, ok := strings.CutPrefix(pattern, "{output}/"); ok {
			return outDir, r
		}
		if r, ok := strings.CutPrefix(pattern, "{root}/"); ok {
			return rootAbs, r
		}
		return in.projectAbs, pattern
	}
	resolve := func(pattern string) ([]string, error) {
		base, rest := locate(pattern)
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
	reports := t.Reports
	if reports != nil && len(reports.When) > 0 && !anyFile(in.projectAbs, reports.When) {
		emit.Info(fmt.Sprintf("no file matches %s: the %s task of %s expects no report", strings.Join(reports.When, ", "), in.task, meta.Component))
		reports = nil
	}
	// A filter that selects no test of the component leaves no test report:
	// the run reads zero tests, and no coverage of them.
	noTests := false
	if filtered && stepErr == nil && reports.HasTests() && !slices.ContainsFunc(slices.Concat(reports.JUnit, reports.TRX), func(pattern string) bool {
		matches, err := globFiles(locate(pattern))
		return err != nil || len(matches) > 0
	}) {
		emit.Info(fmt.Sprintf("the test filter selects no test of %s: its %s task reads zero tests", meta.Component, in.task))
		reports, noTests = nil, true
	}
	reportBase := in.projectAbs
	if t.Dir == DirRoot {
		reportBase = rootAbs
	}
	summary, diags := ReadReports(reports, resolve, outDir, s.attribution(in.workspaceRoot, in.projectAbs, reportBase, meta.Component))
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
	if profile.Delegated {
		data["delegatedTo"] = profile.ID
		emit.Info(fmt.Sprintf("%s ran %s on %s; its own cache decides what reran", profile.ID, in.task, meta.Component))
	}
	if reports.HasTests() || noTests {
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

// runtimePath is what {runtime} expands to: Stack.Runtime, else the running
// executable.
func (s Stack) runtimePath() (string, error) {
	if s.Runtime != "" {
		return s.Runtime, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the extension runtime for {runtime}: %w", err)
	}
	return exe, nil
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
	for _, pattern := range artifactPatterns(patterns, component) {
		base, rest := projectAbs, pattern
		if r, ok := strings.CutPrefix(pattern, "{root}/"); ok {
			base, rest = rootAbs, r
		}
		matches, err := globFiles(base, rest)
		if err != nil {
			return err
		}
		for _, m := range matches {
			if err := copyMode(m, filepath.Join(outDir, "artifacts", filepath.Base(m))); err != nil {
				return fmt.Errorf("copy the artifact %s: %w", m, err)
			}
		}
	}
	return nil
}

// copyMode streams src to dst and gives dst the mode of src.
func copyMode(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // an artifact the profile names
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()) //nolint:gosec // the task's own output
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
}

// singleSegment reports whether name is one clean path segment, with no
// glob character.
func singleSegment(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\*?[")
}

// artifactPatterns are a task's artifact patterns with {component}
// expanded.
func artifactPatterns(patterns []string, component string) []string {
	out := make([]string, len(patterns))
	for i, p := range patterns {
		out[i] = strings.ReplaceAll(p, "{component}", component)
	}
	return out
}

// removeReports deletes the files the report patterns of a task match
// outside its output directory, under the project or the stack root. A file
// the task's own inputs select is a source, never a stale report: it is kept.
func removeReports(profile Profile, task, projectAbs, rootAbs string) error {
	r := profile.Tasks[task].Reports
	if r == nil {
		return nil
	}
	patterns := slices.Concat(r.JUnit, r.TRX)
	for _, c := range r.Coverage {
		patterns = append(patterns, c.Paths...)
	}
	return removeOutputs(patterns, profile, projectAbs, rootAbs)
}

// removeOutputs deletes the files the patterns match under the project or
// the stack root ("{root}/…"); a pattern under "{output}/" is skipped. A
// match outside its base, or that the inputs of any task of the profile
// select, is kept.
func removeOutputs(patterns []string, profile Profile, projectAbs, rootAbs string) error {
	kept := profile.KeptInputs()
	// Each task's own exclusions decide what that task reads: a file one
	// task reads stays a source though another task's set leaves it out.
	selects := func(task, m string) bool {
		project, root := profile.TaskPatterns(task)
		if rel, err := filepath.Rel(projectAbs, m); err == nil && filepath.IsLocal(rel) {
			rel = filepath.ToSlash(rel)
			if !Excluded(profile.TaskExcludes(task), rel) && slices.ContainsFunc(project, func(p string) bool { return MatchPattern(p, rel) }) {
				return true
			}
		}
		if rel, err := filepath.Rel(rootAbs, m); err == nil && filepath.IsLocal(rel) {
			rel = filepath.ToSlash(rel)
			var includes, excludes []string
			for _, p := range root {
				if e, ok := strings.CutPrefix(p, "!"); ok {
					excludes = append(excludes, e)
				} else {
					includes = append(includes, p)
				}
			}
			return !Excluded(excludes, rel) && slices.ContainsFunc(includes, func(p string) bool { return MatchPattern(p, rel) })
		}
		return false
	}
	isInput := func(m string) bool {
		if rel, err := filepath.Rel(projectAbs, m); err == nil && filepath.IsLocal(rel) && slices.Contains(kept, filepath.ToSlash(rel)) {
			return true
		}
		return slices.ContainsFunc(TaskNames, func(task string) bool { return selects(task, m) })
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
			if rel, err := filepath.Rel(base, m); err != nil || !filepath.IsLocal(rel) || isInput(m) {
				continue
			}
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove the stale output %s: %w", m, err)
			}
		}
	}
	return nil
}

// anyFile reports whether one of the patterns matches a file under dir.
func anyFile(dir string, patterns []string) bool {
	for _, p := range patterns {
		if matches, err := globFiles(dir, p); err == nil && len(matches) > 0 {
			return true
		}
	}
	return false
}

// componentTasks are the tasks the component at the workspace-relative
// projectRel runs: the detector's answer now, or, when it no longer names the
// component, the probe's. nil runs every task.
func (s Stack) componentTasks(root, projectRel string, probed []string) []string {
	if s.Detect == nil {
		return probed
	}
	components, _ := s.Detect(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{projectRel}})
	for _, c := range components {
		if c.Path == projectRel {
			return c.Tasks
		}
	}
	return probed
}
