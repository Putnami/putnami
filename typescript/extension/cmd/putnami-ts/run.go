package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/project"
	"go.putnami.dev/typescript/extension/internal/serve"
)

// runExitCodeKey is the result-data key carrying the workload's exit code. The
// CLI reads it to forward the child's exact exit code as the `putnami run`
// process exit code (see tooling/cli runForwardedExitCode). Kept in sync with
// the Go and Python extensions.
const runExitCodeKey = "exit-code"

// runBunOnceFunc is the function runRun uses to execute the workload. It is a
// package var so tests can drive runRun's dispatch/exit-code mapping without
// spawning a real process, mirroring the existing resolveBunBin/execRunFunc
// seams elsewhere in the extension.
var runBunOnceFunc = runBunOnce

// runRun builds for the host (Bun transpiles on the fly) and runs a TypeScript
// workload once, forwarding its exit code. It is the one-shot counterpart to
// serve: no watching, no resident process.
func runRun(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	_ = args
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)

	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("resolve-entrypoint")
	entrypoint, err := resolveRunEntrypoint(projectPath, ctx.Params.String("entrypoint"))
	if err != nil {
		emit.PhaseEnd("resolve-entrypoint", "failed")
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, codeOr(err, errs.CodeNoRunEntrypoint).String())
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("resolve-entrypoint", "success")

	var extraArgs []string
	if a := ctx.Params.String("args"); a != "" {
		extraArgs = strings.Fields(a)
	}
	port := ctx.Params.Int("port", 0)

	emit.PhaseStart("run")
	emit.Log("debug", "Running "+ctx.Project.Name+" ("+entrypoint+") once...")
	exitCode := runBunOnceFunc(emit, bunBin, projectPath, entrypoint, port, extraArgs)

	data := map[string]any{runExitCodeKey: exitCode}
	if exitCode == 0 {
		emit.PhaseEnd("run", "success")
		return "OK", data, nil
	}
	emit.PhaseEnd("run", "failed")
	// FAILED with a nil error on purpose: the job itself succeeded, the workload
	// exited non-zero. A non-nil error would make the SDK drop the result data
	// (and with it the exit code the CLI forwards).
	return "FAILED", data, nil
}

// resolveRunEntrypoint picks the file Bun should run. Priority: explicit flag >
// package.json "./run" export > package.json "main" > src/main.ts.
func resolveRunEntrypoint(projectPath, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if pkg := project.ReadPackageJSONSafe(filepath.Join(projectPath, "package.json")); pkg != nil {
		if ep := project.ResolveExportPath(pkg.Exports, "./run"); ep != "" {
			return ep, nil
		}
		if pkg.Main != "" {
			return pkg.Main, nil
		}
	}
	if project.FileExists(filepath.Join(projectPath, "src", "main.ts")) {
		return "src/main.ts", nil
	}
	return "", errs.New(errs.CodeNoRunEntrypoint, "no run entrypoint found; set package.json \"main\", add a \"./run\" export, or pass --entrypoint").WithCategory(errs.CategoryUser)
}

// runBunOnce runs `bun run <entrypoint>` a single time, streaming its output as
// JSONL events and forwarding termination signals to the whole process group.
// It returns the child's exit code (130 for a clean SIGINT-driven shutdown).
func runBunOnce(emit *jsonl.Emitter, bunBin, projectPath, entrypoint string, port int, extraArgs []string) int {
	bunArgs := make([]string, 0, 2+len(extraArgs))
	bunArgs = append(bunArgs, "run", entrypoint)
	bunArgs = append(bunArgs, extraArgs...)

	cmd := exec.Command(bunBin, bunArgs...) //nolint:gosec // args are constructed programmatically, not from user input
	cmd.Dir = projectPath
	cmd.Stdin = os.Stdin

	env := os.Environ()
	if os.Getenv("NODE_ENV") == "" {
		env = append(env, "NODE_ENV=development")
	}
	if port > 0 {
		env = append(env, "PORT="+strconv.Itoa(port))
	}
	resolvedProjectPath, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		resolvedProjectPath = projectPath
	}
	env = append(env, "PUTNAMI_PROJECT_ROOT="+resolvedProjectPath)
	cmd.Env = env

	// Root of its own process tree so a signal reaches the workload and anything
	// it spawned.
	tree := proctree.New(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return 1
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return 1
	}

	if err := tree.Start(); err != nil {
		emit.Diagnostic("error", "failed to start workload: "+err.Error(), "", 0)
		return 1
	}
	// Ends what the workload left running once it exits (on Windows, the
	// descendants in its job).
	defer func() { _ = tree.Close() }()

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)

	doneCh := make(chan error, 1)
	go func() {
		wg.Wait()
		doneCh <- cmd.Wait()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Grace period before a process group that ignores SIGTERM is force-killed,
	// matching serve.go's shutdown standard.
	const terminateGrace = 5 * time.Second

	return serve.SuperviseRun(cmd.Process, sigCh, doneCh, terminateGrace)
}
