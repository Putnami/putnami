package jobs

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.putnami.dev/python/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// runExitCodeKey is the result-data key carrying the workload's exit code. The
// CLI reads it to forward the child's exact exit code as the `putnami run`
// process exit code (see tooling/cli runForwardedExitCode). Kept in sync with
// the Go and TypeScript extensions.
const runExitCodeKey = "exit-code"

// Run executes a Python workload once for the host and forwards its exit code.
// It is the one-shot counterpart to Serve: it runs the entrypoint a single
// time (no watching, no restart) so one-shot Jobs can be validated locally.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	_ = args
	if ctx.Project.Name == "" {
		return "SKIP", nil, nil
	}

	entrypoint := ctx.Params.String("entrypoint")
	if entrypoint == "" {
		entrypoint = "src/main.py"
	}
	port := ctx.Params.Int("port", 0)

	wsRoot := ctx.WorkspaceRoot
	projectRoot := ctx.Project.FullPath

	if !SyncWorkspace(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	packageName := ResolvePackageName(ctx)
	cmdArgs := toolchain.UVRunArgs(packageName, wsRoot, entrypoint)

	extra := map[string]string{}
	if port > 0 {
		extra["PORT"] = fmt.Sprintf("%d", port)
	}
	env := MakeEnv(wsRoot, projectRoot, extra)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	emit.PhaseStart("run")
	emit.Log("debug", "Running "+ctx.Project.Name+" ("+entrypoint+") once...")
	exitCode := runServer(emit, cmdArgs, projectRoot, env, sigChan)

	data := map[string]any{runExitCodeKey: exitCode}
	if exitCode == 0 {
		emit.PhaseEnd("run", "success")
		return "OK", data, nil
	}
	emit.PhaseEnd("run", "failed")
	// FAILED with a nil error on purpose: the job ran correctly, the workload
	// itself exited non-zero. A non-nil error would make the SDK drop the result
	// data (and with it the exit code the CLI forwards).
	return "FAILED", data, nil
}
