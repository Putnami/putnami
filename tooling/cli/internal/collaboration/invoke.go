package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A provider serves a contract operation through a manifest-declared tool: a
// subprocess that reads one ToolCallRequest on stdin and writes one
// ToolCallResult on stdout. This file runs that subprocess.

const defaultExtensionToolTimeout = 30 * time.Second

type extensionToolCandidate struct {
	name string
	// ext is the whole resolved description, not only its root, because a tool
	// may name the extension's PREPARED RUNTIME ({extensionRuntime}) as its
	// command — and resolving that needs the extension's runtime declaration,
	// which lives on the description.
	ext *extension.ExtensionDescription
	def extension.ToolDefinition
}

func (c extensionToolCandidate) extensionRoot() string {
	if c.ext == nil {
		return ""
	}
	return c.ext.Path
}

// extensionToolWorkingDir resolves the tool's Cwd. A relative command is
// interpreted by exec.Command from cmd.Dir.
func extensionToolWorkingDir(workspaceRoot string, vars map[string]string, def extension.ToolDefinition) string {
	cwd := workspaceRoot
	if def.Cwd != "" {
		cwd = extension.ExpandTemplateVars(def.Cwd, vars)
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(workspaceRoot, cwd)
	}
	return cwd
}

// invocationStage is how far a failed tool invocation got. It is what decides
// whether a mutation can have happened: nothing ran before the process
// started, and anything may have run after.
type invocationStage int

const (
	// stagePrepare: the request or the runtime could not be prepared.
	stagePrepare invocationStage = iota
	// stageStart: the process could not start.
	stageStart
	// stageRun: the process ran and exited unsuccessfully.
	stageRun
	// stageTimeout: the tool's deadline killed the process.
	stageTimeout
	// stageCanceled: the caller canceled while the process ran.
	stageCanceled
	// stageOutput: the process exited successfully without one readable result.
	stageOutput
	// stageTimeoutBeforeStart: the tool's deadline passed before the process
	// started, so nothing ran.
	stageTimeoutBeforeStart
	// stageCanceledBeforeStart: the caller canceled before the process
	// started, so nothing ran.
	stageCanceledBeforeStart
)

// extensionToolError is a failed invocation and the stage it reached.
type extensionToolError struct {
	stage invocationStage
	err   error
	// withoutStderr is the same failure described without anything the
	// subprocess wrote to stderr, for a caller that must not publish it.
	withoutStderr string
}

func (e *extensionToolError) Error() string { return e.err.Error() }

func (e *extensionToolError) Unwrap() error { return e.err }

// summary describes the failure without the subprocess's stderr.
func (e *extensionToolError) summary() string {
	if e.withoutStderr != "" {
		return e.withoutStderr
	}
	return e.err.Error()
}

func invocationFailure(stage invocationStage, format string, args ...any) error {
	return &extensionToolError{stage: stage, err: fmt.Errorf(format, args...)}
}

// invokeExtensionTool runs one manifest-declared tool: it prepares the
// extension's runtime when the tool names it, writes request to the tool's
// stdin, and reads exactly one ToolCallResult of text content from its stdout.
// Every failure is an *extensionToolError naming the stage it reached. The
// subprocess's stderr is folded into a run failure's message; callers that
// must not publish it withhold the message.
func invokeExtensionTool(ctx context.Context, workspaceRoot string, candidate extensionToolCandidate, request proto.ToolCallRequest) (proto.ToolCallResult, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return proto.ToolCallResult{}, invocationFailure(stagePrepare, "encode extension tool request: %w", err)
	}

	timeout := defaultExtensionToolTimeout
	if candidate.def.TimeoutMs > 0 {
		timeout = time.Duration(candidate.def.TimeoutMs) * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vars, err := extensionToolTemplateVars(callCtx, workspaceRoot, candidate)
	if err != nil {
		return proto.ToolCallResult{}, &extensionToolError{stage: stagePrepare, err: err}
	}
	command := extension.ExpandTemplateVars(candidate.def.Command, vars)
	argsForCommand := make([]string, len(candidate.def.Args))
	for i, arg := range candidate.def.Args {
		argsForCommand[i] = extension.ExpandTemplateVars(arg, vars)
	}
	cwd := extensionToolWorkingDir(workspaceRoot, vars, candidate.def)

	// A tool runs in the workspace, like a job: it counts as repository code
	// (runcredential.MarkRepositoryCodeStarted).
	runcredential.MarkRepositoryCodeStarted("extension tool " + candidate.name)
	cmd := exec.CommandContext(callCtx, command, argsForCommand...)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = append(extensionToolBaseEnv(),
		"PUTNAMI_WORKSPACE_ROOT="+workspaceRoot,
		"PUTNAMI_EXTENSION_ROOT="+candidate.extensionRoot(),
	)
	for key, value := range candidate.def.Env {
		cmd.Env = append(cmd.Env, key+"="+extension.ExpandTemplateVars(value, vars))
	}
	stdout, err := cmd.Output()
	if err != nil {
		// Only an exit error proves the process started; every other error is
		// a start failure, after which nothing can have run.
		var exitErr *exec.ExitError
		started := errors.As(err, &exitErr)
		deadline := callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		switch {
		case !started && deadline:
			return proto.ToolCallResult{}, invocationFailure(stageTimeoutBeforeStart, "extension tool %s timed out after %s before it started", request.Name, timeout)
		case !started && ctx.Err() != nil:
			return proto.ToolCallResult{}, invocationFailure(stageCanceledBeforeStart, "extension tool %s was canceled before it started: %w", request.Name, ctx.Err())
		case !started:
			return proto.ToolCallResult{}, invocationFailure(stageStart, "extension tool %s failed: %w", request.Name, err)
		case deadline:
			return proto.ToolCallResult{}, invocationFailure(stageTimeout, "extension tool %s timed out after %s", request.Name, timeout)
		case ctx.Err() != nil:
			return proto.ToolCallResult{}, invocationFailure(stageCanceled, "extension tool %s was canceled: %w", request.Name, ctx.Err())
		}
		if msg := boundedToolStderr(stderr.String()); msg != "" {
			return proto.ToolCallResult{}, &extensionToolError{
				stage:         stageRun,
				err:           fmt.Errorf("extension tool %s failed: %s", request.Name, msg),
				withoutStderr: fmt.Sprintf("extension tool %s failed: %v", request.Name, err),
			}
		}
		return proto.ToolCallResult{}, invocationFailure(stageRun, "extension tool %s failed: %w", request.Name, err)
	}

	var result proto.ToolCallResult
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	if err := decoder.Decode(&result); err != nil {
		return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned invalid JSON: %w", request.Name, err)
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned invalid JSON: %w", request.Name, err)
	}
	if len(result.Content) == 0 {
		return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned no content", request.Name)
	}
	for _, item := range result.Content {
		if item.Type != "text" {
			return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned unsupported content type %q", request.Name, item.Type)
		}
	}
	return result, nil
}

// extensionToolBaseEnv is the process environment without the agent identity
// variables, so a tool never reads an identity it inherited from its caller.
func extensionToolBaseEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case proto.AgentHarnessEnv, proto.AgentModelEnv, proto.AgentUserAgentEnv:
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

func boundedToolStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	const max = 4 << 10
	if len(stderr) > max {
		return stderr[:max] + "…"
	}
	return stderr
}

// referencesExtensionRuntime reports whether a manifest string names the
// extension's prepared runtime.
func referencesExtensionRuntime(value string) bool {
	return strings.Contains(value, "{"+proto.TemplateVarExtensionRuntime+"}")
}

// extensionToolTemplateVars resolves the variables one tool invocation expands,
// preparing the extension's runtime on FIRST USE when the tool names it.
//
// An extension has no prepared-runtime resolver and must never grow one, so the
// orchestrator resolves which executable {extensionRuntime} names, exactly as
// it does for a job subprocess. A local extension's runtime is COMPILED, which
// is work nobody asked for until a call needs the tool. The call's own context
// carries the deadline, so a runtime that will not build fails the call it
// belongs to.
//
// The resolved path is cached on the description, so a second call pays
// nothing. Same shape as `projects sync`'s syncRuntimeResolver.
func extensionToolTemplateVars(
	ctx context.Context, workspaceRoot string, candidate extensionToolCandidate,
) (map[string]string, error) {
	vars := extension.BuildTemplateVars(
		workspaceRoot, workspaceRoot, candidate.extensionRoot(), "")
	if !referencesExtensionRuntime(candidate.def.Command) && !toolArgsReferenceRuntime(candidate.def) {
		return vars, nil
	}
	executable, err := extensionToolRuntime(ctx, workspaceRoot, candidate)
	if err != nil {
		return nil, err
	}
	vars[proto.TemplateVarExtensionRuntime] = executable
	return vars, nil
}

// toolArgsReferenceRuntime reports whether the args, the cwd or an env value of
// def name the prepared runtime; each is expanded with the same variables.
func toolArgsReferenceRuntime(def extension.ToolDefinition) bool {
	for _, arg := range def.Args {
		if referencesExtensionRuntime(arg) {
			return true
		}
	}
	for _, value := range def.Env {
		if referencesExtensionRuntime(value) {
			return true
		}
	}
	return referencesExtensionRuntime(def.Cwd)
}

func extensionToolRuntime(
	ctx context.Context, workspaceRoot string, candidate extensionToolCandidate,
) (string, error) {
	ext := candidate.ext
	if ext == nil || ext.Runtime == nil {
		return "", fmt.Errorf("extension tool %s names {%s}, but extension %q declares no runtime",
			candidate.name, proto.TemplateVarExtensionRuntime, candidate.name)
	}
	if ext.RuntimeExecutable != "" {
		return ext.RuntimeExecutable, nil
	}
	ws, err := workspace.Load(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("load workspace: %w", err)
	}
	if err := scheduler.SynchronizeExtensionRuntimes(
		ctx, ws, []*extension.ExtensionDescription{ext}, nil); err != nil {
		return "", fmt.Errorf("prepare the %s runtime for tool %s: %w", ext.Name, candidate.name, err)
	}
	if ext.RuntimeExecutable == "" {
		return "", fmt.Errorf("the %s runtime prepared no executable for tool %s", ext.Name, candidate.name)
	}
	return ext.RuntimeExecutable, nil
}
