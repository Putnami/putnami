package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/python/extension/internal/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// uvVersion returns the release the uv program at path reports. It is a seam
// so the job can be tested without a uv.
var uvVersion = toolchain.UVVersion

// uvCommandRun runs the uv program uv with args in dir and returns its
// combined output. It is a seam so the jobs that run uv lock can be tested
// without uv.
var uvCommandRun = func(uv string, args []string, dir string, env []string) ([]byte, error) {
	cmd := exec.Command(uv, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// WorkspaceInstall syncs the UV workspace, declares the uv release it runs in
// the root pyproject.toml when nothing does, and runs uv lock with it. The job
// installs no uv: it runs the one the CLI resolved for the uv runtime
// toolchain, which the CLI provisions under the Putnami home before the job
// starts. With none, the job fails naming the release and `putnami install`,
// and requests nothing.
func WorkspaceInstall(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)
	force := cli.FlagBool(flags, "force", ctx.Params.Bool("force", false))
	// A hosted run, whose dependencies the engine already downloaded
	// (extensionproto.OfflineDependenciesEnv), writes no file the repository
	// commits.
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

	uv, err := resolveUV(wsRoot)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	if !hosted {
		version, err := uvVersion(uv)
		if err != nil {
			emit.Diagnostic("error", fmt.Sprintf("Cannot read the release of %s: %v", uv, err), "", 0)
			return "FAILED", nil, nil
		}
		declared, err := workspace.EnsureUVRequiredVersion(wsRoot, version)
		if err != nil {
			emit.Diagnostic("error", fmt.Sprintf("Cannot declare uv %s in pyproject.toml: %v", version, err), "", 0)
			return "FAILED", nil, nil
		}
		if declared {
			emit.Log("info", fmt.Sprintf("Declared required-version = \"==%s\" in the [tool.uv] table of pyproject.toml", version))
		}
	}

	uvLock := filepath.Join(wsRoot, "uv.lock")
	if !force && !updated && FileExists(uvLock) {
		emit.Log("info", "uv.lock is up to date")
		return "OK", nil, nil
	}

	emit.PhaseStart("lock")
	output, err := uvCommandRun(uv, []string{"lock"}, wsRoot, MakeEnv(wsRoot, wsRoot, nil))

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
