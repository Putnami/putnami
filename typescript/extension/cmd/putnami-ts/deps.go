package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// resolveBunBin resolves the bun binary path. Can be replaced in tests.
var resolveBunBin = toolchain.ResolveBun

// provisionBunBin selects the bun a provisioning job runs: workspace-install
// and workspace-fetch run before the lock pins a bun and before any is
// installed, so they select one themselves (toolchain.ProvisionBun) where
// every other job takes the one the CLI resolved. mode says whether the job
// may install the release, and refuse, when set, says why it must not run a
// given bun. Can be replaced in tests.
var provisionBunBin = func(ctx *pctx.Context, emit *jsonl.Emitter, mode toolchain.BunMode, refuse func(path string) string) (string, error) {
	// An interrupt ends a download, and the staging files it leaves are
	// removed by the next install of the same release.
	stopped, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bun, err := toolchain.ProvisionBun(stopped, toolchain.BunRequest{
		WorkspaceRoot: ctx.WorkspaceRoot,
		Mode:          mode,
		Version:       func(path string) (string, error) { return bunVersion(path, ctx.WorkspaceRoot) },
		Refuse:        refuse,
		UserAgent:     strings.TrimSpace(os.Getenv(cliUserAgentEnv)),
		Log:           emit.Log,
	})
	return bun.Path, err
}

// bunVersion returns the version the bun at bunBin reports with --version,
// run in dir.
func bunVersion(bunBin, dir string) (string, error) {
	result, err := runBunWithTimeout("bun --version", bunBin, []string{"--version"}, dir, bunVersionTimeout)
	if err != nil {
		return "", err
	}
	if !result.Success {
		return "", fmt.Errorf("bun --version exited with code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return strings.TrimSpace(result.Stdout), nil
}

// resolveBiomeBinFn resolves the biome binary path. Can be replaced in tests.
var resolveBiomeBinFn = toolchain.ResolveBiome

// resolveBiomeConfigFn resolves the biome config path. Can be replaced in tests.
var resolveBiomeConfigFn = toolchain.ResolveBiomeConfig

// wsExecRunFunc is the exec.Run function used by workspace_install. Can be replaced in tests.
var wsExecRunFunc = exec.Run

// bunNetworkTimeout bounds bun install/update subprocesses. These fetch from
// the package registry and can hang indefinitely on a stalled connection; the
// orchestrator timeout only covers the normal path, so an in-process backstop
// is required.
const bunNetworkTimeout = 10 * time.Minute

// runBunWithTimeout invokes wsExecRunFunc with a wall-clock timeout, returning a
// clear "timed out after N" error when the deadline is exceeded. The label
// names the operation (e.g. "bun install") for the error message.
func runBunWithTimeout(label, bunBin string, args []string, dir string, timeout time.Duration) (*exec.Result, error) {
	return runBunWithin(context.Background(), label, bunBin, args, dir, timeout)
}

// runBunWithin is runBunWithTimeout under a parent context: bun is killed when
// parent ends or the timeout passes, whichever comes first. opts are applied
// after the directory and the context.
func runBunWithin(parent context.Context, label, bunBin string, args []string, dir string, timeout time.Duration, opts ...exec.Option) (*exec.Result, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	options := append([]exec.Option{exec.Dir(dir), exec.WithContext(ctx)}, opts...)
	result, err := wsExecRunFunc(bunBin, args, options...)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", label, timeout)
	}
	if parentErr := parent.Err(); parentErr != nil {
		return nil, fmt.Errorf("%s stopped: %w", label, parentErr)
	}
	return result, err
}
