package java

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const (
	// ServeJobName is the job of the serve command.
	ServeJobName = "serve"
	// RunJobName is the job of the run command.
	RunJobName = "run"
)

const (
	// CodeNotRunnable is the diagnostic of a component the extension cannot
	// run: no application plugin and no Spring Boot.
	CodeNotRunnable = "java.not_runnable"
	// CodeRunFailed is the diagnostic of a process the job could not start or
	// a runnable form the build did not produce.
	CodeRunFailed = "java.run_failed"
)

const (
	defaultServePort = 3000
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
	// stopGrace is how long a stopped process has to exit before it is killed.
	stopGrace = 5 * time.Second
	// readyPollInterval is how often serve checks that the server accepts
	// connections.
	readyPollInterval = 100 * time.Millisecond
)

// runnableKind is the form a build gives a runnable application.
type runnableKind int

const (
	// installDist is the directory of the Gradle application plugin: a
	// launcher script under bin/ and the jars under lib/.
	installDist runnableKind = iota + 1
	// bootJar is the executable jar of Spring Boot, built by Gradle.
	bootJar
	// mavenBootJar is the executable jar of Spring Boot, built by Maven.
	mavenBootJar
)

var (
	springBoot             = regexp.MustCompile(`org\.springframework\.boot|plugins\.spring[.\-_]?boot`)
	gradleApplicationOnly  = regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_$./-])(?:org\.gradle\.)?application(?:[^A-Za-z0-9_$/.-]|$)`)
	mavenBootPluginPattern = mavenApplication
)

// runnableKindOf reads how the component's build makes it runnable.
func runnableKindOf(workspaceRoot string, project string, meta stacks.Metadata) (runnableKind, error) {
	rel, err := filepath.Rel(workspaceRoot, project)
	if err != nil {
		return 0, err
	}
	rel = filepath.ToSlash(rel)
	if meta.Variant == VariantMaven {
		data, err := os.ReadFile(filepath.Join(project, "pom.xml")) //nolint:gosec // the module's own POM
		if err == nil && mavenBootPluginPattern.Match(data) {
			return mavenBootJar, nil
		}
		return 0, fmt.Errorf("%s declares no spring-boot-maven-plugin: the extension runs a Maven module through its Spring Boot jar", rel)
	}
	_, build := buildFile(workspaceRoot, rel)
	switch {
	case springBoot.MatchString(build):
		return bootJar, nil
	case gradleApplicationOnly.MatchString(build):
		return installDist, nil
	}
	return 0, fmt.Errorf("%s applies neither the application plugin nor Spring Boot", rel)
}

// runnable is an application the build produced.
type runnable struct {
	kind runnableKind
	// path is the installDist directory, or the jar.
	path string
	// command starts the application, without its arguments.
	command []string
}

// buildRunnable builds the component's runnable form with its own build and
// locates it. Its output goes to out.
func buildRunnable(ctx context.Context, root stacks.RootStep, meta stacks.Metadata, kind runnableKind, out io.Writer) (runnable, error) {
	var argv []string
	switch kind {
	case installDist:
		argv = []string{filepath.Join(root.Dir, "gradlew"), "--console=plain", meta.Component + "installDist"}
	case bootJar:
		argv = []string{filepath.Join(root.Dir, "gradlew"), "--console=plain", meta.Component + "bootJar"}
	case mavenBootJar:
		argv = []string{root.Program("mvn"), "--batch-mode", "--file", filepath.Join(root.Dir, "pom.xml"), "--projects", meta.Component, "--also-make", "package", "-DskipTests"}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the component's own build
	cmd.Dir = root.Project
	cmd.Env = root.Env
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return runnable{}, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	java := root.Program("java")
	switch kind {
	case installDist:
		dir, err := single(filepath.Join(root.Project, "build", "install"), func(e os.DirEntry) bool { return e.IsDir() })
		if err != nil {
			return runnable{}, fmt.Errorf("installDist wrote no single directory under build/install: %w", err)
		}
		script, err := single(filepath.Join(dir, "bin"), func(e os.DirEntry) bool { return !e.IsDir() && !strings.HasSuffix(e.Name(), ".bat") })
		if err != nil {
			return runnable{}, fmt.Errorf("installDist wrote no launcher under %s/bin: %w", dir, err)
		}
		return runnable{kind: kind, path: dir, command: []string{script}}, nil
	case bootJar:
		jar, err := single(filepath.Join(root.Project, "build", "libs"), func(e os.DirEntry) bool {
			return strings.HasSuffix(e.Name(), ".jar") && !strings.HasSuffix(e.Name(), "-plain.jar")
		})
		if err != nil {
			return runnable{}, fmt.Errorf("bootJar wrote no single jar under build/libs: %w", err)
		}
		return runnable{kind: kind, path: jar, command: []string{java, "-jar", jar}}, nil
	default:
		jar, err := single(filepath.Join(root.Project, "target"), func(e os.DirEntry) bool {
			n := e.Name()
			return strings.HasSuffix(n, ".jar") && !strings.HasSuffix(n, "-sources.jar") && !strings.HasSuffix(n, "-javadoc.jar") && !strings.HasSuffix(n, "-tests.jar")
		})
		if err != nil {
			return runnable{}, fmt.Errorf("package wrote no single jar under target: %w", err)
		}
		return runnable{kind: kind, path: jar, command: []string{java, "-jar", jar}}, nil
	}
}

// single answers the one entry of dir that keep accepts.
func single(dir string, keep func(os.DirEntry) bool) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		if keep(e) {
			found = append(found, filepath.Join(dir, e.Name()))
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("found %d candidates in %s", len(found), dir)
	}
	return found[0], nil
}

// application selects the toolchain of the job's component, reads how it
// runs, and builds its runnable form.
func application(ctx *pctx.Context, stack stacks.Stack, emit *jsonl.Emitter) (stacks.RootStep, runnable, error) {
	fail := func(code string, err error) error {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
		return fmt.Errorf("%s: %w", code, err)
	}
	var meta stacks.Metadata
	if err := json.Unmarshal(ctx.Project.Metadata[stack.Extension], &meta); err != nil || meta.Component == "" {
		return stacks.RootStep{}, runnable{}, fail(CodeNotRunnable, fmt.Errorf("%s is no component @putnami/java found: run putnami projects sync", ctx.Project.Path))
	}
	root, err := stack.Selected(ctx, emit)
	if err != nil {
		return stacks.RootStep{}, runnable{}, err
	}
	kind, err := runnableKindOf(ctx.WorkspaceRoot, root.Project, meta)
	if err != nil {
		return stacks.RootStep{}, runnable{}, fail(CodeNotRunnable, err)
	}
	app, err := buildRunnable(context.Background(), root, meta, kind, os.Stderr)
	if err != nil {
		return stacks.RootStep{}, runnable{}, fail(CodeRunFailed, err)
	}
	return root, app, nil
}

// ServeJob runs an application until the user stops it, and announces it
// ready once it accepts connections on the port flag. Port 0 selects a free
// port.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, app, err := application(ctx, stack, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if port == 0 {
			if port, err = freePort(); err != nil {
				emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
				return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
			}
		}
		emit.Info("serving " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port), "SERVER_PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go announceReady(serving, emit, port)
		code, stopped, err := runForeground(root.Project, app.command, env, emit)
		if err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

// RunJob runs an application once with the args parameter and answers its
// exit code.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		root, app, err := application(ctx, stack, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		argv := append(slices.Clone(app.command), strings.Fields(ctx.Params.String("args"))...)
		code, _, err := runForeground(root.Project, argv, root.Env, nil)
		if err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
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

// runForeground runs argv in dir until it exits or the user stops the job. The
// job's stdout carries its protocol: forward turns the program's output into
// log events, and a nil forward sends it to stderr. It answers the exit code
// and whether the user stopped it; an error says the program did not start.
func runForeground(dir string, argv, env []string, forward *jsonl.Emitter) (int, bool, error) {
	stopping, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(stopping, argv[0], argv[1:]...) //nolint:gosec // the application the build produced
	cmd.Dir = dir
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
