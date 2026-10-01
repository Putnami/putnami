// Package jobs provides shared helpers used across all Python extension jobs.
package jobs

import (
	"fmt"
	"os"

	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/python/extension/internal/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/hostenv"
	"go.putnami.dev/sdk/extension/jsonl"
)

// ResolvePackageName returns the Python package name from pyproject.toml,
// falling back to the project name from the context.
func ResolvePackageName(ctx *pctx.Context) string {
	name, err := workspace.ParsePyprojectName(ctx.Project.FullPath + "/pyproject.toml")
	if err == nil && name != "" {
		return name
	}
	return ctx.Project.Name
}

// MakeEnv creates an environment for subprocess execution with standard
// Putnami variables set. The subprocess inherits the extension's environment
// whole — including any host platform identity, which `run` and `serve` want:
// a locally served app is a deployment, and lying to it about where it runs
// would be the bug. Test subprocesses use MakeTestEnv instead.
func MakeEnv(workspaceRoot, workingDir string, extra map[string]string) []string {
	return decorateEnv(os.Environ(), workspaceRoot, workingDir, extra)
}

// MakeTestEnv creates the environment for a TEST subprocess: MakeEnv minus the
// host platform identity block (go.putnami.dev/sdk/extension/hostenv).
//
// A `putnami test` run on a managed runtime — the reported case is a CI worker
// that is itself a Cloud Run service — would otherwise hand pytest the HOST's
// K_SERVICE, which application code reads as "I am the deployed production
// workload" and uses to refuse test-only behavior. The signal describes
// the harness host, not the code under test.
//
// The scrub covers the INHERITED environment only, so PYTHONPATH, the
// PUTNAMI_* locators and anything passed in extra are applied afterwards and
// still win. It is deliberately confined to the test paths: `run`, `serve`,
// `lint` and the workspace/deps jobs keep MakeEnv's inherit-everything
// behavior.
func MakeTestEnv(workspaceRoot, workingDir string, extra map[string]string) []string {
	return decorateEnv(hostenv.ScrubPlatformIdentity(os.Environ()), workspaceRoot, workingDir, extra)
}

// decorateEnv appends the standard Putnami variables, uv's workspace-local
// directories (toolchain.UVDirEnv), and the caller's extras to a base
// environment, never aliasing it.
func decorateEnv(base []string, workspaceRoot, workingDir string, extra map[string]string) []string {
	uvDirs := toolchain.UVDirEnv(workspaceRoot, base)
	env := make([]string, 0, len(base)+3+len(uvDirs)+len(extra))
	env = append(env, base...)
	env = append(env,
		"FORCE_COLOR=1",
		"PUTNAMI_WORKSPACE="+workspaceRoot,
		"PUTNAMI_WORKING_DIR="+workingDir,
	)
	for k, v := range uvDirs {
		env = append(env, k+"="+v)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// FileExists returns true if the path exists.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// SyncWorkspace runs the UV workspace sync phase and resolves the UV binary.
// Returns false if the job should return early (on failure or missing UV).
func SyncWorkspace(wsRoot string, emit *jsonl.Emitter) (ok bool) {
	emit.PhaseStart("sync")
	updated, members, err := workspace.SyncUVWorkspace(wsRoot)
	if err != nil {
		emit.PhaseEnd("sync", "failed")
		emit.Diagnostic("error", fmt.Sprintf("Workspace sync failed: %v", err), "", 0)
		return false
	}
	if len(members) == 0 {
		emit.PhaseEnd("sync", "skipped")
	} else {
		if updated {
			emit.Log("info", fmt.Sprintf("Updated uv workspace members (%d)", len(members)))
		}
		emit.PhaseEnd("sync", "success")
	}

	if _, err := toolchain.ResolveUV(); err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return false
	}
	return true
}
