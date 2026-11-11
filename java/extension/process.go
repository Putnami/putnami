package java

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

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

// runnableKindOf reads how the component's build makes it runnable.
func runnableKindOf(workspaceRoot string, project string, meta stacks.Metadata) (runnableKind, error) {
	rel, err := filepath.Rel(workspaceRoot, project)
	if err != nil {
		return 0, err
	}
	rel = filepath.ToSlash(rel)
	if meta.Variant == VariantMaven {
		data, err := os.ReadFile(filepath.Join(project, "pom.xml")) //nolint:gosec // the module's own POM
		if err == nil && mavenBootPlugin(data) {
			return mavenBootJar, nil
		}
		return 0, fmt.Errorf("%s builds with no active spring-boot-maven-plugin: the extension runs a Maven module through its Spring Boot jar", rel)
	}
	_, build := buildFile(workspaceRoot, rel)
	plugins := gradlePlugins(build)
	if slices.ContainsFunc(plugins, isSpringBoot) {
		return bootJar, nil
	}
	if slices.Contains(plugins, "application") || slices.Contains(plugins, "org.gradle.application") {
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
func buildRunnable(ctx context.Context, root stacks.RootStep, meta stacks.Metadata, kind runnableKind, out io.Writer, info func(string)) (runnable, error) {
	var argv []string
	switch kind {
	case installDist:
		argv = []string{filepath.Join(root.Dir, "gradlew"), "--console=plain", meta.Component + "installDist"}
	case bootJar:
		argv = []string{filepath.Join(root.Dir, "gradlew"), "--console=plain", meta.Component + "bootJar"}
	case mavenBootJar:
		argv = []string{"mvn", "--batch-mode", "--file", filepath.Join(root.Dir, "pom.xml"), "--projects", meta.Component, "--also-make", "package", "-DskipTests"}
	}
	argv, err := Step(ctx, "", meta, root, argv, info)
	if err != nil {
		return runnable{}, err
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
// A build can leave the form of an earlier version beside the new one, such as
// app-1.0.jar beside app-1.1.jar: the build just wrote the newest.
func single(dir string, keep func(os.DirEntry) bool) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	newest, newestTime := "", time.Time{}
	for _, e := range entries {
		if !keep(e) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", err
		}
		if newest == "" || info.ModTime().After(newestTime) {
			newest, newestTime = filepath.Join(dir, e.Name()), info.ModTime()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("found no candidate in %s", dir)
	}
	return newest, nil
}

// application selects the toolchain of the job's component, reads how it
// runs, builds its runnable form under the stack root's lock, as every task of
// the build does, and copies it into a private directory before it releases
// the lock: a task that cleans the module later never touches what runs or
// what an image packages. The caller calls the returned cleanup.
func application(ctx *pctx.Context, stack stacks.Stack, emit *jsonl.Emitter) (stacks.RootStep, runnable, func(), error) {
	fail := func(code string, err error) error {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
		return fmt.Errorf("%s: %w", code, err)
	}
	none := func() {}
	var meta stacks.Metadata
	if err := json.Unmarshal(ctx.Project.Metadata[stack.Extension], &meta); err != nil || meta.Component == "" {
		return stacks.RootStep{}, runnable{}, none, fail(CodeNotRunnable, fmt.Errorf("%s is no component @putnami/java found: run putnami projects sync", ctx.Project.Path))
	}
	kind, err := runnableKindOf(ctx.WorkspaceRoot, ctx.Project.FullPath, meta)
	if err != nil {
		return stacks.RootStep{}, runnable{}, none, fail(CodeNotRunnable, err)
	}
	// Under the workspace's .putnami, not the system temp directory, which
	// may be mounted noexec and would not run a launcher.
	scratch := filepath.Join(ctx.WorkspaceRoot, ".putnami", "tmp")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return stacks.RootStep{}, runnable{}, none, fail(CodeRunFailed, err)
	}
	private, err := os.MkdirTemp(scratch, "java-app-")
	if err != nil {
		return stacks.RootStep{}, runnable{}, none, fail(CodeRunFailed, err)
	}
	cleanup := func() { _ = os.RemoveAll(private) }
	var root stacks.RootStep
	var app runnable
	err = stack.Locked(ctx, emit, func(run context.Context, r stacks.RootStep, emit *jsonl.Emitter) error {
		root = r
		built, err := buildRunnable(run, r, meta, kind, os.Stderr, emit.Info)
		if err != nil {
			return fail(CodeRunFailed, err)
		}
		if app, err = built.copyTo(private, r.Program("java")); err != nil {
			return fail(CodeRunFailed, err)
		}
		return nil
	})
	if err != nil {
		cleanup()
		return stacks.RootStep{}, runnable{}, none, err
	}
	return root, app, cleanup, nil
}

// copyTo copies the runnable form into dir and answers the copy.
func (r runnable) copyTo(dir, java string) (runnable, error) {
	target := filepath.Join(dir, filepath.Base(r.path))
	if r.kind != installDist {
		if err := copyFile(r.path, target); err != nil {
			return runnable{}, err
		}
		return runnable{kind: r.kind, path: target, command: []string{java, "-jar", target}}, nil
	}
	err := filepath.WalkDir(r.path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.path, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(target, rel), 0o755)
		}
		return copyFile(path, filepath.Join(target, rel))
	})
	if err != nil {
		return runnable{}, err
	}
	rel, err := filepath.Rel(r.path, r.command[0])
	if err != nil {
		return runnable{}, err
	}
	return runnable{kind: r.kind, path: target, command: []string{filepath.Join(target, rel)}}, nil
}

// copyFile copies a file and its executable bit. A symbolic link is an error:
// an image's cache key does not cover what a link points to.
func copyFile(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is no regular file, which a runnable form cannot hold", source)
	}
	data, err := os.ReadFile(source) //nolint:gosec // a file the build produced
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	return os.WriteFile(target, data, mode) //nolint:gosec // a private copy
}

// ServeJob runs an application until the user stops it, and announces it
// ready once it accepts connections on the port flag. Port 0 selects a free
// port.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, app, cleanup, err := application(ctx, stack, emit)
		defer cleanup()
		if err != nil {
			return "FAILED", nil, err
		}
		if port == 0 {
			if port, err = stacks.FreePort(); err != nil {
				emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
				return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
			}
		}
		emit.Info("serving " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port), "SERVER_PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go stacks.AnnounceReady(serving, emit, port)
		argv := append(slices.Clone(app.command), strings.Fields(ctx.Params.String("args"))...)
		code, stopped, err := stacks.RunForeground(root, argv[0], env, emit, argv[1:]...)
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
		root, app, cleanup, err := application(ctx, stack, emit)
		defer cleanup()
		if err != nil {
			return "FAILED", nil, err
		}
		argv := append(slices.Clone(app.command), strings.Fields(ctx.Params.String("args"))...)
		code, _, err := stacks.RunForeground(root, argv[0], root.Env, nil, argv[1:]...)
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
