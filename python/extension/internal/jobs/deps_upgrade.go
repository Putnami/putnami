package jobs

import (
	"fmt"
	"os/exec"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// DepsUpgrade upgrades dependencies in explicitly configured Python projects.
func DepsUpgrade(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	wsRoot := ctx.WorkspaceRoot

	if ctx.Params.Bool("dry-run", false, "dryRun") {
		emit.Log("info", "Would sync Python workspace and run uv lock --upgrade")
		return "OK", nil, nil
	}

	if !SyncWorkspace(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	emit.PhaseStart("upgrade")

	// Use uv lock --upgrade to resolve latest versions of all dependencies.
	cmd := exec.Command("uv", "lock", "--upgrade")
	cmd.Dir = wsRoot
	cmd.Env = MakeEnv(wsRoot, wsRoot, nil)
	output, err := cmd.CombinedOutput()

	if err != nil {
		emit.PhaseEnd("upgrade", "failed")
		emit.Diagnostic("error", fmt.Sprintf("uv lock --upgrade failed:\n%s", string(output)), "", 0)
		return "FAILED", nil, nil
	}

	emit.PhaseEnd("upgrade", "success")
	emit.Log("info", "Dependencies for explicitly configured Python projects upgraded")
	return "OK", nil, nil
}
