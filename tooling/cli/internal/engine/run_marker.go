package engine

import (
	"context"
	"os"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// This file isolates successful-run marker recording (local last-build state +
// remote run-marker publication) from the execution stage in execute.go.
// recordSuccessfulBuild is best-effort: every failure is swallowed (logged only
// under --debug) so it never affects the command's exit code.

func recordSuccessfulBuild(
	sessStore *workspace_state.SessionStore,
	ws *workspace.Workspace,
	wsRoot string,
	commands []string,
	params map[string]any,
	debug bool,
	remote *jobs.RemoteCache,
	observedSHA string,
) {
	if sessStore == nil || wsRoot == "" {
		return
	}
	branch, err := git.CurrentBranch(wsRoot)
	if err != nil {
		if debug {
			iox.Fprintf(os.Stderr, "[debug] last build state skipped: %v\n", err)
		}
		return
	}
	if strings.TrimSpace(observedSHA) == "" {
		previousSHA, err := sessStore.LastBuildSHA(branch, commands, params)
		if err == nil {
			observedSHA = previousSHA
		}
	}
	sha, err := git.HeadSHA(wsRoot)
	if err != nil {
		if debug {
			iox.Fprintf(os.Stderr, "[debug] last build state skipped: %v\n", err)
		}
		return
	}
	if err := sessStore.RecordSuccessfulBuild(branch, commands, params, sha); err != nil && debug {
		iox.Fprintf(os.Stderr, "[debug] last build state skipped: %v\n", err)
	}
	remote.PublishRunMarker(context.Background(), jobs.RunMarkerWorkspaceID(ws), branch, commands, workspace_state.LastBuildParamsHash(params), sha, observedSHA)
}

func observedRunMarkerSHA(req *Request) string {
	if req == nil {
		return ""
	}
	if req.Global.AutoSelected && req.Global.Impacted {
		return req.Global.Baseline
	}
	return ""
}

func shouldRecordSuccessfulBuild(req *Request) bool {
	if req == nil {
		return false
	}
	// A lifecycle run publishes NO successful-run marker (Request.WorkspaceLifecycle
	// note 4). Markers are keyed by (branch, commands, params) with no provenance,
	// so a marker written by `putnami install` would let a later `--impacted` run
	// treat everything up to HEAD as already built. The adapter's selection flags
	// (Projects="*", never All/AutoSelected) already fail the test below; this is
	// the explicit statement of the invariant, so a future flag change cannot turn
	// installs into build markers by accident.
	if req.WorkspaceLifecycle {
		return false
	}
	// A NARROWED plan must not publish a marker either. Markers are keyed
	// (branch, commands, params) with no extension dimension, so the marker a run
	// writes claims "everything this command covers is built at HEAD". That claim
	// is false when the run planned against one extension instead of every
	// discovered one — which is exactly what the extension-alias adapter does
	// (Request.PlanExtension, added for the extension-alias adapter, so `putnami cloud deploy`
	// does not schedule every extension's `deploy`).
	//
	// Without this guard: extensions A and B both declare the flat command
	// `deploy`; a bare `putnami a deploy` on main succeeds and records
	// (main, ["deploy"], params)=HEAD — locally and, with a remote cache, for
	// every other machine. A later bare `putnami deploy` reads that marker and
	// narrows to impacted-since-HEAD, silently skipping B's `deploy` on every
	// unchanged project. Found by the whole-epic integration review: extracting
	// the marker machinery relied on the terminal path's implicit invariant
	// that a run covers every provider of its commands, and planning extension
	// aliases against one extension broke that invariant later — neither change
	// was wrong on its own.
	if req.PlanExtension != nil || req.PlanExtensions != nil {
		return false
	}
	if hasProjectSelectionFilters(req.Global) {
		return false
	}
	return req.Global.All || req.Global.AutoSelected
}
