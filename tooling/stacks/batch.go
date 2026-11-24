package stacks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// A delegated verb runs as one tool command (ADR 0019): the scheduler hands
// the leader of a batch every project of the verb in ctx.SelectedProjects, the
// command runs once per stack root over their components, and each project's
// result is read from the tool's report.

// BatchReport is what a stack's ReadBatch reads once a batch command ran.
type BatchReport struct {
	// Task is the task the command ran: lint, test or build.
	Task string
	// Root is the stack root's absolute path, where the command ran.
	Root string
	// Components are the components the command ran, in its order.
	Components []string
	// Command is the step that failed, as it ran: the targets it names.
	Command []string
	// Started is when the command started: a report the tool wrote before is
	// no report of this run.
	Started time.Time
	// Output is the command's output, its tail when it is long.
	Output []byte
}

// BatchOutcome is one component's result as the tool's report states it.
type BatchOutcome struct {
	// Failed is true for a component the tool reports failed.
	Failed bool
	// Message is the tool's message for a failed component.
	Message string
}

// batchOutputLimit bounds the output a batch keeps for the stack to read.
const batchOutputLimit = 8 << 20

// batchResultsKey is the data key of the batch contract's results.
const batchResultsKey = "batchResults"

// batchResult is one project's result under the batch contract.
type batchResult struct {
	ProjectID   string            `json:"projectId"`
	Status      string            `json:"status"`
	Data        map[string]any    `json:"data,omitempty"`
	Diagnostics []batchDiagnostic `json:"diagnostics,omitempty"`
	Summary     batchSummary      `json:"summary"`
}

type batchDiagnostic struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
}

type batchSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
}

// collector is the reporter of one project of a batch: it keeps the
// project's diagnostics for its result, and logs its notes under its name.
type collector struct {
	component   string
	info        func(string)
	diagnostics []batchDiagnostic
	summary     batchSummary
}

func (c *collector) DiagnosticWithCode(severity, message, file string, _, _ int, code string) {
	c.diagnostics = append(c.diagnostics, batchDiagnostic{Category: code, Severity: severity, Description: message, File: file})
	switch severity {
	case "error":
		c.summary.Errors++
	case "warning":
		c.summary.Warnings++
	default:
		c.summary.Infos++
	}
}

func (c *collector) Summary(message string) { c.Info(message) }

func (c *collector) Info(message string) {
	if c.info != nil {
		c.info(c.component + ": " + message)
	}
}

// batches reports whether the task runs over several components when the
// scheduler groups it.
func (s Stack) batches(task string) bool {
	t, ok := s.Profile.Tasks[task]
	return ok && t.Batch != nil
}

// batchMember is one selected project of a batch.
type batchMember struct {
	ref     pctx.ProjectRef
	in      runInput
	meta    Metadata
	profile Profile
	task    Task
	outDir  string
	report  *collector
	// result is set once the project's result is known.
	result *batchResult
}

// runBatch runs one task over the selected projects and answers their results
// under the batch contract. A project that fails before the command fails
// alone; the others still run.
func (s Stack) runBatch(ctx *pctx.Context, emit *jsonl.Emitter, task string) (string, map[string]any, error) {
	background := context.Background()
	members := s.batchMembers(ctx, emit, task)
	// byRoot holds the members of each command, by stack root and shape.
	byRoot := map[string][]*batchMember{}
	var roots []string
	for _, m := range members {
		if m.result != nil {
			continue
		}
		if m.task.Batch == nil {
			// A variant that states no batch runs its own steps.
			status, data, _ := s.run(background, m.in, m.report)
			m.finish(status, data)
			continue
		}
		// One command per stack root, toolchain and batch shape: the members' steps
		// may differ only in the arguments that name their component, such
		// as moon run api:test pricing:unit.
		// The toolchain too: a project may state its own check or version,
		// and one command runs one check.
		toolchain, _ := json.Marshal(m.profile.Toolchain)
		key := m.meta.Root + "\x00" + batchShape(m.task.Batch.Steps) + "\x00" + string(toolchain)
		if _, ok := byRoot[key]; !ok {
			roots = append(roots, key)
		}
		byRoot[key] = append(byRoot[key], m)
	}
	for _, root := range roots {
		s.runBatchRoot(background, task, byRoot[root], emit)
	}
	results := make([]batchResult, len(members))
	for i, m := range members {
		results[i] = *m.result
	}
	return statusOK, map[string]any{batchResultsKey: results}, nil
}

// batchMembers reads each selected project's component and readies its output
// directory. A project the task does not run on gets its result here.
func (s Stack) batchMembers(ctx *pctx.Context, emit *jsonl.Emitter, task string) []*batchMember {
	metadata := s.selectedMetadata(ctx)
	members := make([]*batchMember, 0, len(ctx.SelectedProjects))
	for _, ref := range ctx.SelectedProjects {
		fullPath := ref.FullPath
		if fullPath == "" {
			fullPath = filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(ref.Path))
		}
		m := &batchMember{
			ref: ref,
			in: runInput{
				task:          task,
				workspaceRoot: ctx.WorkspaceRoot,
				projectAbs:    fullPath,
				metadata:      metadata[filepath.ToSlash(ref.Path)],
				options:       ctx.Workspace.Options[s.Extension],
				outputRoot:    ref.OutputPath,
				stdout:        os.Stderr,
			},
		}
		members = append(members, m)
		meta, profile, err := s.component(m.in, m.collector(emit))
		m.meta = meta
		if err != nil {
			m.finish(statusFailed, nil)
			continue
		}
		m.report.component = meta.Component
		if meta.Component == "" {
			m.report.Info(fmt.Sprintf("%s is a %s stack root with no component: its %s task runs no step", meta.Root, s.Profile.ID, task))
			m.finish(statusOK, nil)
			continue
		}
		t, ok := profile.Tasks[task]
		if !ok {
			m.finish(statusOK, nil)
			continue
		}
		projectRel, _ := filepath.Rel(ctx.WorkspaceRoot, fullPath)
		if tasks := s.componentTasks(ctx.WorkspaceRoot, filepath.ToSlash(projectRel), meta.Tasks); tasks != nil && !slices.Contains(tasks, task) {
			m.report.Info(fmt.Sprintf("runs no %s task", task))
			m.finish(statusOK, nil)
			continue
		}
		if ref.OutputPath == "" {
			_ = failWith(m.report, CodeStepFailed, "the batch names no output directory for %s", ref.Path)
			m.finish(statusFailed, nil)
			continue
		}
		m.profile, m.task = profile, t
		m.outDir = filepath.Join(ref.OutputPath, task)
		if err := resetDir(m.outDir); err != nil {
			_ = failWith(m.report, CodeStepFailed, "%v", err)
			m.finish(statusFailed, nil)
		}
	}
	return members
}

// selectedMetadata answers the stack's metadata of each selected project, by
// its slash-separated path: the job's own project carries it, the others are
// probed once.
func (s Stack) selectedMetadata(ctx *pctx.Context) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	var paths []string
	for _, ref := range ctx.SelectedProjects {
		path := filepath.ToSlash(ref.Path)
		if path == filepath.ToSlash(ctx.Project.Path) {
			if meta := ctx.Project.Metadata[s.Extension]; len(meta) > 0 {
				out[path] = meta
				continue
			}
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return out
	}
	result, err := s.Probe(ctx.WorkspaceRoot, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: paths})
	if err != nil {
		return out
	}
	for _, p := range result.Projects {
		out[filepath.ToSlash(p.Path)] = p.Metadata
	}
	return out
}

func (m *batchMember) collector(emit *jsonl.Emitter) *collector {
	if m.report == nil {
		m.report = &collector{component: m.ref.Path, info: emit.Info}
	}
	return m.report
}

func (m *batchMember) finish(status string, data map[string]any) {
	m.result = &batchResult{
		ProjectID:   m.ref.ID,
		Status:      status,
		Data:        data,
		Diagnostics: m.report.diagnostics,
		Summary:     m.report.summary,
	}
}

// runBatchRoot runs the batch steps once at a stack root over its members'
// components, then reads each member's result.
func (s Stack) runBatchRoot(ctx context.Context, task string, members []*batchMember, emit *jsonl.Emitter) {
	first := members[0]
	rootAbs := filepath.Join(first.in.workspaceRoot, filepath.FromSlash(first.meta.Root))
	failAll := func(format string, args ...any) {
		for _, m := range members {
			_ = failWith(m.report, CodeStepFailed, format, args...)
			m.finish(statusFailed, delegated(first.profile))
		}
	}
	// One toolchain check for the root: the components of one root select
	// the same toolchain.
	check := &collector{component: first.meta.Root, info: emit.Info}
	selected, err := s.selectToolchain(ctx, first.in, first.meta, first.profile, check)
	if err != nil {
		for _, m := range members {
			m.report.diagnostics = append(m.report.diagnostics, check.diagnostics...)
			m.report.summary.Errors += check.summary.Errors
			m.finish(statusFailed, nil)
		}
		return
	}
	env := selected.Env
	lock, err := lockStackRoot(first.in.workspaceRoot, first.meta, first.profile)
	if err != nil {
		failAll("%v", err)
		return
	}
	defer func() { _ = lock.Release() }()

	components := make([]string, len(members))
	for i, m := range members {
		components[i] = m.meta.Component
		if err := removeReports(m.profile, task, m.in.projectAbs, rootAbs); err != nil {
			failAll("%v", err)
			return
		}
	}
	stepRoot := RootStep{Dir: rootAbs, Project: first.in.projectAbs, Component: first.meta.Component, Env: env, Program: func(name string) string { return selected.Program(first.profile.Toolchain, name) }}
	output := &tailBuffer{limit: batchOutputLimit}
	started := time.Now()
	var stepErr error
	var failedArgv []string
	for i := range first.task.Batch.Steps {
		perMember := make([][]string, len(members))
		for k, m := range members {
			perMember[k] = m.task.Batch.Steps[i]
		}
		argv := expandBatchStep(perMember, rootAbs, components)
		if s.Step != nil {
			adapted, err := s.Step(ctx, task, first.meta, stepRoot, argv, emit.Info)
			if err != nil {
				failAll("%s: %v", strings.Join(argv, " "), err)
				return
			}
			argv = adapted
		}
		emit.Info("$ " + strings.Join(argv, " "))
		cmd := exec.CommandContext(ctx, selected.ProgramIn(first.profile.Toolchain, argv[0], env), argv[1:]...) //nolint:gosec // the profile's own step
		cmd.Dir = rootAbs
		cmd.Env = env
		writer := io.MultiWriter(os.Stderr, output)
		cmd.Stdout = writer
		cmd.Stderr = writer
		if err := cmd.Run(); err != nil {
			stepErr, failedArgv = err, argv
			break
		}
	}
	outcomes := map[string]BatchOutcome{}
	if stepErr != nil && s.ReadBatch != nil {
		outcomes = s.ReadBatch(BatchReport{Task: task, Root: rootAbs, Components: components, Command: failedArgv, Started: started, Output: output.Bytes()})
	}
	named := false
	var failed []string
	for _, c := range components {
		if o, ok := outcomes[c]; ok {
			named = true
			if o.Failed {
				failed = append(failed, c)
			}
		}
	}
	// A command that failed while its report names no failure fails every
	// project: the report cannot say which one, and passing one would turn a
	// killed or crashed tool green.
	if len(failed) == 0 {
		named = false
	}
	command := strings.Join(failedArgv, " ")
	for _, m := range members {
		var memberErr error
		o, ok := outcomes[m.meta.Component]
		switch {
		case stepErr == nil:
		case !named:
			memberErr = failWith(m.report, CodeStepFailed, "%s: %v", command, stepErr)
		case !ok:
			memberErr = failWith(m.report, CodeStepFailed, "%s did not run %s: it failed on %s", command, m.meta.Component, strings.Join(failed, ", "))
		case o.Failed:
			message := o.Message
			if message == "" {
				message = fmt.Sprintf("%s failed", m.meta.Component)
			}
			memberErr = failWith(m.report, CodeStepFailed, "%s", message)
		}
		status, data, _ := s.readTaskReports(m.in, m.meta, m.profile, m.task, rootAbs, m.outDir, memberErr, false, m.report)
		m.finish(status, data)
	}
}

// delegated is the data of a delegated profile's failed result.
func delegated(profile Profile) map[string]any {
	if !profile.Delegated {
		return nil
	}
	return map[string]any{"delegatedTo": profile.ID}
}

// batchShape is a batch's steps with each argument that names {component}
// standing for every component: two members of one shape run one command.
func batchShape(steps [][]string) string {
	shape := make([][]string, len(steps))
	for i, step := range steps {
		shape[i] = make([]string, len(step))
		for j, arg := range step {
			if strings.Contains(arg, "{component}") {
				arg = "{component}"
			}
			shape[i][j] = arg
		}
	}
	data, _ := json.Marshal(shape)
	return string(data)
}

// expandBatchStep answers one batch step over the components, from each
// member's own version of the step (one shape): an argument that names
// {component} once per component, from that component's own argument;
// {components} the components joined with commas; {root} the stack root.
func expandBatchStep(perMember [][]string, rootAbs string, components []string) []string {
	var argv []string
	for j, arg := range perMember[0] {
		if !strings.Contains(arg, "{component}") {
			arg = strings.ReplaceAll(arg, "{root}", rootAbs)
			argv = append(argv, strings.ReplaceAll(arg, BatchPlaceholder, strings.Join(components, ",")))
			continue
		}
		for k, c := range components {
			own := strings.ReplaceAll(perMember[k][j], "{root}", rootAbs)
			argv = append(argv, strings.ReplaceAll(own, "{component}", c))
		}
	}
	return argv
}

// resetDir empties a directory, creating it.
func resetDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if over := len(b.data) - b.limit; over > 0 {
		b.data = append(b.data[:0:0], b.data[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.data)
}
