package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/python/extension/internal/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// WorkspaceInstall syncs the UV workspace and runs uv lock.
func WorkspaceInstall(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)
	force := cli.FlagBool(flags, "force", ctx.Params.Bool("force", false))

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

	if _, err := toolchain.ResolveUV(); err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	uvLock := filepath.Join(wsRoot, "uv.lock")
	if !force && !updated && FileExists(uvLock) {
		emit.Log("info", "uv.lock is up to date")
		return "OK", nil, nil
	}

	emit.PhaseStart("lock")
	cmd := exec.Command("uv", "lock")
	cmd.Dir = wsRoot
	cmd.Env = MakeEnv(wsRoot, wsRoot, nil)
	output, err := cmd.CombinedOutput()

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
