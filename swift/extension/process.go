package swift

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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

// Process job names.
const (
	ServeJobName = "serve"
	RunJobName   = "run"
)

// Diagnostic codes of serve and run.
const (
	// CodeNoExecutable is the diagnostic of a component with no executable
	// product to start.
	CodeNoExecutable = "swift.no_executable"
	// CodeRunFailed is the diagnostic of a process the job could not start.
	CodeRunFailed = "swift.run_failed"
)

const (
	defaultServePort = 3000
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
	// stopGrace is how long a stopped process has to exit before it is killed.
	stopGrace = 3 * time.Second
	// readyPollInterval is how often serve checks that the server accepts
	// connections.
	readyPollInterval = 50 * time.Millisecond
)

// ServeJob runs the executable product of a package with swift run, PORT set,
// until the user stops it, and announces it ready once the port accepts a
// connection. Port 0 selects a free port.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, product, err := executableOf(stack, ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if port == 0 {
			if port, err = freePort(); err != nil {
				return "FAILED", nil, fail(emit, CodeRunFailed, err)
			}
		}
		emit.Info("serving " + product + " of " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go announceReady(serving, emit, port)
		code, stopped, err := runForeground(root, env, emit, "run", "--force-resolved-versions", product)
		if err != nil {
			return "FAILED", nil, fail(emit, CodeRunFailed, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

// RunJob runs the executable product once with the args parameter and
// answers its exit code.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		root, product, err := executableOf(stack, ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		argv := []string{"run", "--force-resolved-versions", product}
		if extra := strings.Fields(ctx.Params.String("args")); len(extra) > 0 {
			argv = append(append(argv, "--"), extra...)
		}
		code, _, err := runForeground(root, root.Env, nil, argv...)
		if err != nil {
			return "FAILED", nil, fail(emit, CodeRunFailed, err)
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

// executableOf selects the job's component and the executable product it
// runs: the entrypoint parameter, else the only executable of the package,
// else the one named after the package.
func executableOf(stack stacks.Stack, ctx *pctx.Context, emit *jsonl.Emitter) (stacks.RootStep, string, error) {
	root, err := stack.Selected(ctx, emit)
	if err != nil {
		return stacks.RootStep{}, "", err
	}
	if !isPackage(root.Project) {
		return root, "", fail(emit, CodeNoExecutable, fmt.Errorf("%s is an Xcode project: serve and run start a package's executable product", root.Project))
	}
	if named := ctx.Params.String("entrypoint"); named != "" {
		return root, named, nil
	}
	manifest, _ := os.ReadFile(filepath.Join(root.Project, "Package.swift")) //nolint:gosec // the package's manifest
	executables := ExecutableProducts(manifest)
	switch {
	case len(executables) == 1:
		return root, executables[0], nil
	case len(executables) == 0:
		return root, "", fail(emit, CodeNoExecutable, fmt.Errorf("%s declares no executable product or target", root.Project))
	}
	if slices.Contains(executables, ctx.Project.Name) {
		return root, ctx.Project.Name, nil
	}
	return root, "", fail(emit, CodeNoExecutable, fmt.Errorf("%s declares several executables (%s): name one with --entrypoint", root.Project, strings.Join(executables, ", ")))
}

var (
	executableProduct = regexp.MustCompile(`\.executable\s*\(\s*name\s*:\s*"([^"]+)"`)
	executableTarget  = regexp.MustCompile(`\.executableTarget\s*\(\s*name\s*:\s*"([^"]+)"`)
	lineComment       = regexp.MustCompile(`//[^\n]*`)
	blockComment      = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// ExecutableProducts reads the executable products a Package.swift declares,
// sorted; with none, the executable targets SwiftPM makes a product of. Its
// comments are left out.
func ExecutableProducts(manifest []byte) []string {
	source := lineComment.ReplaceAllString(blockComment.ReplaceAllString(string(manifest), " "), "")
	for _, re := range []*regexp.Regexp{executableProduct, executableTarget} {
		var out []string
		for _, m := range re.FindAllStringSubmatch(source, -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
		if len(out) > 0 {
			slices.Sort(out)
			return out
		}
	}
	return nil
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

// runForeground runs swift in the project directory until it exits or the
// user stops the job. The job's stdout carries its protocol: forward turns the
// program's output into log events, and a nil forward sends it to stderr. It
// answers the exit code and whether the user stopped it; an error says the
// program did not start.
func runForeground(root stacks.RootStep, env []string, forward *jsonl.Emitter, args ...string) (int, bool, error) {
	stopping, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(stopping, root.Program("swift"), args...) //nolint:gosec // the selected Swift
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
