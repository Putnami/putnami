package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/python/extension/internal/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// cliUserAgentEnv names the User-Agent the CLI gives the downloads its
// extensions make.
const cliUserAgentEnv = "PUTNAMI_CLI_USER_AGENT"

// provisionUV selects the uv workspace-install runs, and installs the release
// the workspace pins when the machine holds none and mode allows it
// (toolchain.ProvisionUV). workspace-install runs before the lock may pin a uv
// and before any is installed, so it selects one itself where every other job
// takes the one the CLI resolved. It is a seam so the job can be tested without
// a download.
var provisionUV = func(wsRoot string, emit *jsonl.Emitter, mode toolchain.UVMode) (toolchain.UV, error) {
	// An interrupt ends a download, and the staging files it leaves are
	// removed by the next install of the same release.
	stopped, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return toolchain.ProvisionUV(stopped, toolchain.UVRequest{
		WorkspaceRoot: wsRoot,
		Mode:          mode,
		UserAgent:     strings.TrimSpace(os.Getenv(cliUserAgentEnv)),
		Log:           emit.Log,
	})
}

// uvCommandRun runs the uv program uv with args in dir and returns its
// combined output. It is a seam so the jobs that run uv lock can be tested
// without uv.
var uvCommandRun = func(uv string, args []string, dir string, env []string) ([]byte, error) {
	cmd := exec.Command(uv, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// WorkspaceInstall syncs the UV workspace, provisions the uv release the
// workspace pins, declares it in the root pyproject.toml when nothing does,
// and runs uv lock with it.
func WorkspaceInstall(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)
	force := cli.FlagBool(flags, "force", ctx.Params.Bool("force", false))
	// A hosted run, whose dependencies the engine already downloaded
	// (extensionproto.OfflineDependenciesEnv), downloads no toolchain and
	// writes no file the repository commits: it runs the uv the runner holds.
	hosted := os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"

	wsRoot := ctx.WorkspaceRoot

	emit.PhaseStart("sync")
	updated, members, err := workspace.SyncUVWorkspace(wsRoot)
	if err != nil {
		emit.PhaseEnd("sync", "failed")
		emit.Diagnostic("error", fmt.Sprintf("Workspace sync failed: %v", err), "", 0)
		return "FAILED", nil, nil
	}
	if len(members) == 0 {
		emit.PhaseEnd("sync", "skipped")
		emit.Log("info", "No Python workspace members found")
		return "SKIP", nil, nil
	}
	if updated {
		emit.Log("info", fmt.Sprintf("Updated pyproject.toml with %d Python project(s)", len(members)))
	}
	emit.PhaseEnd("sync", "success")

	mode := toolchain.UVInstall
	if hosted {
		mode = toolchain.UVFind
	}
	uv, err := provisionUV(wsRoot, emit, mode)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	if !hosted {
		declared, err := workspace.EnsureUVRequiredVersion(wsRoot, uv.Version)
		if err != nil {
			emit.Diagnostic("error", fmt.Sprintf("Declaring uv %s in pyproject.toml failed: %v", uv.Version, err), "", 0)
			return "FAILED", nil, nil
		}
		if declared {
			emit.Log("info", fmt.Sprintf("Declared required-version = \"==%s\" in the [tool.uv] table of pyproject.toml", uv.Version))
		}
	}

	uvLock := filepath.Join(wsRoot, "uv.lock")
	if !force && !updated && FileExists(uvLock) {
		emit.Log("info", "uv.lock is up to date")
		return "OK", nil, nil
	}

	emit.PhaseStart("lock")
	output, err := uvCommandRun(uv.Path, []string{"lock"}, wsRoot, MakeEnv(wsRoot, wsRoot, nil))

	if len(output) > 0 {
		_, _ = os.Stderr.Write(output)
	}

	if err != nil {
		emit.PhaseEnd("lock", "failed")
		emit.Diagnostic("error", fmt.Sprintf("uv lock failed:\n%s", toolchain.TailText(string(output), "", 10)), "", 0)
		return "FAILED", nil, nil
	}

	emit.PhaseEnd("lock", "success")
	return "OK", nil, nil
}
