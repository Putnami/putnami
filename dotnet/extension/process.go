package dotnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.putnami.dev/protocol/runtime"
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
	// stopGrace is how long a stopped process has to exit before it is killed.
	// It is shorter than the CLI's own kill delay, so this job reaps the process.
	stopGrace = 3 * time.Second
	// readyPollInterval is how often serve checks that the application accepts
	// connections.
	readyPollInterval = 50 * time.Millisecond
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
			if port, err = freePort(); err != nil {
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
		go announceReady(serving, emit, port)
		code, stopped, err := runForeground(root, env, emit, "run", "--no-build", "--project", manifest, "--no-launch-profile")
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
		code, _, err := runForeground(root, root.Env, nil, argv...)
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

// freePort answers a TCP port no process listens on.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("the listener bound %s, which has no TCP port", listener.Addr())
	}
	return address.Port, nil
}

// announceReady emits the ready events of the server on port and of the
// workload once the server accepts a connection, or nothing when serving ends
// first.
func announceReady(serving context.Context, emit *jsonl.Emitter, port int) {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()
	for {
		if connection, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			_ = connection.Close()
			emit.Ready(runtime.ReadyData{
				Target:    runtime.ReadyTargetServer,
				Endpoints: []runtime.ReadyEndpoint{{Scheme: runtime.ReadySchemeHTTP, Host: "localhost", Port: port}},
			})
			emit.Ready(runtime.ReadyData{Target: runtime.ReadyTargetWorkload})
			return
		}
		select {
		case <-serving.Done():
			return
		case <-ticker.C:
		}
	}
}

// runForeground runs dotnet in the project directory until it exits or the
// user stops the job. The job's stdout carries its protocol: forward turns the
// program's output into log events, and a nil forward sends it to stderr. It
// answers the exit code and whether the user stopped it; an error says the
// program did not start.
func runForeground(root stacks.RootStep, env []string, forward *jsonl.Emitter, args ...string) (int, bool, error) {
	stopping, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(stopping, root.Program("dotnet"), args...) //nolint:gosec // the selected SDK
	cmd.Dir = root.Project
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace
	err := runAttached(cmd, forward)
	stopped := stopping.Err() != nil
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stopped, nil
	}
	if err != nil && stopped {
		return 0, true, nil
	}
	return 0, stopped, err
}

// runAttached runs cmd with its output sent as runForeground describes.
func runAttached(cmd *exec.Cmd, forward *jsonl.Emitter) error {
	if forward == nil {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go jsonl.ForwardPipe(forward, stdout, "info", &readers)
	go jsonl.ForwardPipe(forward, stderr, "info", &readers)
	readers.Wait()
	return cmd.Wait()
}
