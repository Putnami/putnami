package dotnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

const (
	// ServeJobName is the job of the serve command.
	ServeJobName = "serve"
	// RunJobName is the job of the run command.
	RunJobName = "run"
)

const (
	// CodeRunFailed is the diagnostic of a process the job could not start.
	CodeRunFailed = "dotnet.run_failed"
	// CodeNoManifest is the diagnostic of a project the probe named no
	// project file for.
	CodeNoManifest = "dotnet.no_project_file"
)

const (
	defaultServePort = 3000
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
)

// ServeJob runs an application with dotnet run until the user stops it, on
// the port flag, and announces it ready once the port accepts connections.
// Port 0 selects a free port.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		manifest, err := projectFile(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		root, err := built(ctx, emit, stack, manifest)
		if err != nil {
			return "FAILED", nil, err
		}
		if port == 0 {
			if port, err = stacks.FreePort(); err != nil {
				return "FAILED", nil, runFailed(emit, err)
			}
		}
		emit.Info("serving " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env),
			"PORT="+strconv.Itoa(port),
			"ASPNETCORE_URLS=http://"+net.JoinHostPort("0.0.0.0", strconv.Itoa(port)),
		)
		serving, done := context.WithCancel(context.Background())
		defer done()
		go stacks.AnnounceReady(serving, emit, port)
		argv := []string{"run", "--no-build", "--project", manifest, "--no-launch-profile"}
		if words := strings.Fields(ctx.Params.String("args")); len(words) > 0 {
			argv = append(append(argv, "--"), words...)
		}
		code, stopped, err := stacks.RunForeground(root, root.Program("dotnet"), env, emit, argv...)
		if err != nil {
			return "FAILED", nil, runFailed(emit, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

// RunJob runs an application once with dotnet run and the args parameter,
// and answers its exit code.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		manifest, err := projectFile(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		root, err := built(ctx, emit, stack, manifest)
		if err != nil {
			return "FAILED", nil, err
		}
		argv := []string{"run", "--no-build", "--project", manifest, "--no-launch-profile"}
		if words := strings.Fields(ctx.Params.String("args")); len(words) > 0 {
			argv = append(append(argv, "--"), words...)
		}
		code, _, err := stacks.RunForeground(root, root.Program("dotnet"), root.Env, nil, argv...)
		if err != nil {
			return "FAILED", nil, runFailed(emit, err)
		}
		data := map[string]any{exitCodeKey: code}
		if code != 0 {
			// A nil error: the job ran, and the SDK keeps the result data that
			// carries the exit code only when there is none.
			return "FAILED", data, nil
		}
		return "OK", data, nil
	}
}

// built builds the application under the stack root's lock, since a build
// writes the outputs of the projects it references, and answers the selected
// SDK to run it with, outside the lock.
func built(ctx *pctx.Context, emit *jsonl.Emitter, stack stacks.Stack, manifest string) (stacks.RootStep, error) {
	status, _, err := stack.RootJob(func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
		cmd := exec.CommandContext(run, root.Program("dotnet"), "build", manifest) //nolint:gosec // the selected SDK
		cmd.Dir = root.Project
		cmd.Env = root.Env
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		emit.Info("$ dotnet build " + manifest)
		if err := cmd.Run(); err != nil {
			return runFailed(emit, fmt.Errorf("dotnet build %s: %w", manifest, err))
		}
		return nil
	})(ctx, emit, nil)
	if err != nil || status != "OK" {
		if err == nil {
			err = errors.New(CodeRunFailed)
		}
		return stacks.RootStep{}, err
	}
	return stack.Selected(ctx, emit)
}

// projectFile is the absolute path of the project file the probe recorded.
func projectFile(ctx *pctx.Context, emit *jsonl.Emitter) (string, error) {
	var meta stacks.Metadata
	if raw := ctx.Project.Metadata[ExtensionName]; len(raw) == 0 || json.Unmarshal(raw, &meta) != nil || meta.Manifest == "" {
		msg := fmt.Sprintf("%s has no project file %s found: run putnami projects sync", ctx.Project.FullPath, ExtensionName)
		emit.DiagnosticWithCode("error", msg, "", 0, 0, CodeNoManifest)
		return "", errors.New(CodeNoManifest)
	}
	return filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(meta.Manifest)), nil
}

func runFailed(emit *jsonl.Emitter, err error) error {
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
	return fmt.Errorf("%s: %w", CodeRunFailed, err)
}
