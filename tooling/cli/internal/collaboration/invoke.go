package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A provider serves a contract's operations through the subprocess its
// manifest declares under providers: the process reads one ProviderRequest on
// stdin and writes one Response on stdout. This file runs that subprocess.

const defaultProviderTimeout = 30 * time.Second

type providerCandidate struct {
	// ext is the whole resolved description, not only its root, because a
	// provider may name the extension's PREPARED RUNTIME ({extensionRuntime})
	// as its command — and resolving that needs the extension's runtime
	// declaration, which lives on the description.
	ext *extension.ExtensionDescription
	// contract names the declaration, for messages.
	contract    string
	declaration extension.ProviderDeclaration
}

func (c providerCandidate) extensionRoot() string {
	if c.ext == nil {
		return ""
	}
	return c.ext.Path
}

// extensionName names the extension that declares the provider, for messages.
func (c providerCandidate) extensionName() string {
	if c.ext == nil {
		return ""
	}
	return c.ext.Name
}

// providerWorkingDir resolves the provider's Cwd. A relative command is
// interpreted by exec.Command from cmd.Dir.
func providerWorkingDir(workspaceRoot string, vars map[string]string, declaration extension.ProviderDeclaration) string {
	cwd := workspaceRoot
	if declaration.Cwd != "" {
		cwd = extension.ExpandTemplateVars(declaration.Cwd, vars)
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(workspaceRoot, cwd)
	}
	return cwd
}

// invocationStage is how far a failed provider invocation got. It is what
// decides whether a mutation can have happened: nothing ran before the process
// started, and anything may have run after.
type invocationStage int

const (
	// stagePrepare: the request or the runtime could not be prepared.
	stagePrepare invocationStage = iota
	// stageStart: the process could not start.
	stageStart
	// stageRun: the process ran and exited unsuccessfully.
	stageRun
	// stageTimeout: the provider's deadline killed the process.
	stageTimeout
	// stageCanceled: the caller canceled while the process ran.
	stageCanceled
	// stageTimeoutBeforeStart: the provider's deadline passed before the
	// process started, so nothing ran.
	stageTimeoutBeforeStart
	// stageCanceledBeforeStart: the caller canceled before the process
	// started, so nothing ran.
	stageCanceledBeforeStart
)

// providerError is a failed invocation and the stage it reached.
type providerError struct {
	stage invocationStage
	err   error
	// withoutStderr is the same failure described without anything the
	// subprocess wrote to stderr, for a caller that must not publish it.
	withoutStderr string
}

func (e *providerError) Error() string { return e.err.Error() }

func (e *providerError) Unwrap() error { return e.err }

// summary describes the failure without the subprocess's stderr.
func (e *providerError) summary() string {
	if e.withoutStderr != "" {
		return e.withoutStderr
	}
	return e.err.Error()
}

func invocationFailure(stage invocationStage, format string, args ...any) error {
	return &providerError{stage: stage, err: fmt.Errorf(format, args...)}
}

// invokeProvider runs one provider operation: it prepares the extension's
// runtime when the declaration names it, writes request to the process's
// stdin, and returns what the process wrote to stdout once it exits
// successfully. The caller parses that output as one Response. Every failure
// is a *providerError naming the stage it reached. The subprocess's stderr is
// folded into a run failure's message; callers that must not publish it
// withhold the message.
func invokeProvider(ctx context.Context, workspaceRoot string, candidate providerCandidate, request collab.ProviderRequest) ([]byte, error) {
	name := "provider " + candidate.contract
	if request.Provider != nil {
		name = "provider " + request.Provider.Contract + "." + request.Provider.Operation
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, invocationFailure(stagePrepare, "encode the %s request: %w", name, err)
	}

	timeout := defaultProviderTimeout
	if candidate.declaration.TimeoutMs > 0 {
		timeout = time.Duration(candidate.declaration.TimeoutMs) * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vars, err := providerTemplateVars(callCtx, workspaceRoot, candidate)
	if err != nil {
		return nil, &providerError{stage: stagePrepare, err: err}
	}
	command := extension.ExpandTemplateVars(candidate.declaration.Command, vars)
	argsForCommand := make([]string, len(candidate.declaration.Args))
	for i, arg := range candidate.declaration.Args {
		argsForCommand[i] = extension.ExpandTemplateVars(arg, vars)
	}
	cwd := providerWorkingDir(workspaceRoot, vars, candidate.declaration)

	// A provider runs in the workspace, like a job: it counts as repository
	// code (runcredential.MarkRepositoryCodeStarted).
	runcredential.MarkRepositoryCodeStarted(name + " of " + candidate.extensionName())
	cmd := exec.CommandContext(callCtx, command, argsForCommand...)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"PUTNAMI_WORKSPACE_ROOT="+workspaceRoot,
		"PUTNAMI_EXTENSION_ROOT="+candidate.extensionRoot(),
	)
	for key, value := range candidate.declaration.Env {
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
			return nil, invocationFailure(stageTimeoutBeforeStart, "%s timed out after %s before it started", name, timeout)
		case !started && ctx.Err() != nil:
			return nil, invocationFailure(stageCanceledBeforeStart, "%s was canceled before it started: %w", name, ctx.Err())
		case !started:
			return nil, invocationFailure(stageStart, "%s failed: %w", name, err)
		case deadline:
			return nil, invocationFailure(stageTimeout, "%s timed out after %s", name, timeout)
		case ctx.Err() != nil:
			return nil, invocationFailure(stageCanceled, "%s was canceled: %w", name, ctx.Err())
		}
		if msg := boundedProviderStderr(stderr.String()); msg != "" {
			return nil, &providerError{
				stage:         stageRun,
				err:           fmt.Errorf("%s failed: %s", name, msg),
				withoutStderr: fmt.Sprintf("%s failed: %v", name, err),
			}
		}
		return nil, invocationFailure(stageRun, "%s failed: %w", name, err)
	}
	return stdout, nil
}

func boundedProviderStderr(stderr string) string {
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

// providerTemplateVars resolves the variables one provider invocation
// expands, preparing the extension's runtime on FIRST USE when the declaration
// names it.
//
// An extension has no prepared-runtime resolver and must never grow one, so the
// orchestrator resolves which executable {extensionRuntime} names, exactly as
// it does for a job subprocess. A local extension's runtime is COMPILED, which
// is work nobody asked for until a call needs the provider. The call's own
// context carries the deadline, so a runtime that will not build fails the
// call it belongs to.
//
// The resolved path is cached on the description, so a second call pays
// nothing. Same shape as `projects sync`'s syncRuntimeResolver.
func providerTemplateVars(
	ctx context.Context, workspaceRoot string, candidate providerCandidate,
) (map[string]string, error) {
	vars := extension.BuildTemplateVars(
		workspaceRoot, workspaceRoot, candidate.extensionRoot(), "")
	if !referencesExtensionRuntime(candidate.declaration.Command) && !providerArgsReferenceRuntime(candidate.declaration) {
		return vars, nil
	}
	executable, err := providerRuntime(ctx, workspaceRoot, candidate)
	if err != nil {
		return nil, err
	}
	vars[proto.TemplateVarExtensionRuntime] = executable
	return vars, nil
}

// providerArgsReferenceRuntime reports whether the args, the cwd or an env
// value of the declaration name the prepared runtime; each is expanded with
// the same variables.
func providerArgsReferenceRuntime(declaration extension.ProviderDeclaration) bool {
	for _, arg := range declaration.Args {
		if referencesExtensionRuntime(arg) {
			return true
		}
	}
	for _, value := range declaration.Env {
		if referencesExtensionRuntime(value) {
			return true
		}
	}
	return referencesExtensionRuntime(declaration.Cwd)
}

func providerRuntime(
	ctx context.Context, workspaceRoot string, candidate providerCandidate,
) (string, error) {
	ext := candidate.ext
	if ext == nil || ext.Runtime == nil {
		return "", fmt.Errorf("the %s provider of extension %q names {%s}, but the extension declares no runtime",
			candidate.contract, candidate.extensionName(), proto.TemplateVarExtensionRuntime)
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
		return "", fmt.Errorf("prepare the %s runtime for its %s provider: %w", ext.Name, candidate.contract, err)
	}
	if ext.RuntimeExecutable == "" {
		return "", fmt.Errorf("the %s runtime prepared no executable for its %s provider", ext.Name, candidate.contract)
	}
	return ext.RuntimeExecutable, nil
}
