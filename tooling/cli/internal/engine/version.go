package engine

import (
	"fmt"
	"os"
	"slices"

	internalextension "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// BuildVersionInfo computes the run's version per line from git metadata. It is
// exported for the extension-command path, which stamps a single interactive
// job without going through the run lifecycle and therefore has no captured
// snapshot to layer. It fails where Engine.Run fails (captureVersionSnapshot).
func BuildVersionInfo(ws *workspace.Workspace) (jobs.RunVersions, error) {
	snapshot, err := captureVersionSnapshot(ws.Root)
	if err != nil {
		return nil, err
	}
	// A line git cannot read degrades rather than failing the command: an
	// extension command is not a release (jobs.BuildRunVersions).
	versions, _ := jobs.BuildRunVersions(ws, snapshot)
	return versions, nil
}

// captureVersionSnapshot reads the git-derived TREE state (SHA, branch, dirty
// flag, suffix) from the working tree. Engine.Run calls this at the start of a
// run, BEFORE any in-run tree mutation (before-hooks, codegen jobs), so the
// stamped version reflects the tree as the user left it rather than putnami's
// own transient edits. A line's base version depends on tags and
// history rather than on the tree, so it is resolved later, by runVersions.
//
// Without PUTNAMI_SOURCE_REVISION it returns nil and no error when git cannot
// answer, as for a workspace outside git. With it, the run promised a bound
// commit, so the TreeState error is returned and fails the run: a malformed
// override, a bound commit whose time cannot be established, or a checkout git
// cannot read would otherwise leave every line unversioned and publish 0.0.0
// under the bound revision.
func captureVersionSnapshot(wsRoot string) (*git.VersionInfo, error) {
	snapshot, err := git.TreeState(wsRoot)
	if err == nil {
		return snapshot, nil
	}
	if _, set, _ := git.SourceRevisionOverride(); set {
		return nil, fmt.Errorf("read the version of the commit %s binds this run to: %w", git.SourceRevisionEnv, err)
	}
	return nil, nil
}

// gitHistoryCommands are the commands that ship a commit: each refuses a
// workspace root Git does not manage.
var gitHistoryCommands = []string{"publish", "deploy"}

// requireRepository returns the one-line refusal when the run includes a
// command that ships a commit and Git does not manage the workspace root.
//
// It asks git only when the tree-state capture found no commit and the run
// includes such a command, so every other run starts no extra process. A run
// that only previews its plan ships nothing and is not refused. The executing
// side of a frozen portable request is exempt: the submitter resolved its
// versions, and the snapshot it runs in carries no repository. A version 2
// request runs on a checkout, which is asked like any other root.
func requireRepository(req *Request) error {
	if req.VersionSnapshot != nil || req.Portable.frozen() || req.Global.Plan || req.previewsOnly() {
		return nil
	}
	for _, command := range gitHistoryCommands {
		if !slices.Contains(req.Commands, command) {
			continue
		}
		if unmanaged := git.Unmanaged(req.WorkspaceRoot); unmanaged != nil {
			return fmt.Errorf("%s needs git history: %w", command, unmanaged)
		}
		return nil
	}
	return nil
}

// reportRepositoryRefusal prints why a run requireRepository refused stops, and
// returns the error the run ends with.
//
// A selected command that no loaded extension declares, while an extension the
// workspace declares is not installed, cannot run in any directory: installing
// that extension is the first thing to do, and the repository matters only
// once the command can run. That run reports the missing extension with the
// missing-extension guard's own message (buildPlan) and no refusal line. Every
// other run prints the one-line refusal. Either way it stops before any hook,
// job or remote call. A command a loaded extension declares but no selected
// project activates gets the refusal: telling it apart needs the plan, which
// this run never builds.
func reportRepositoryRefusal(req *Request, refusal error) error {
	if discovered := discoverDeclaredExtensions(req); discovered != nil {
		missing := missingRegistryExtensions(req.Config, discovered.Extensions)
		if len(missing) > 0 && len(selectedCommandsWithoutJobs(req.Commands, nil, discovered.Extensions)) > 0 {
			reportMissingExtensions(missing, discovered.Skipped, "")
			reportUnservedSDDCommands(req.Commands, discovered.Extensions)
			return nil
		}
	}
	iox.Fprintf(os.Stderr, "putnami: %v\n", refusal)
	return refusal
}

// discoverDeclaredExtensions resolves the extensions of a workspace that
// declares a registry extension, the only kind that can be missing. It returns
// nil for any other workspace, and when the workspace or its extensions cannot
// be read: the refusal then stands.
func discoverDeclaredExtensions(req *Request) *internalextension.DiscoveryResult {
	if len(missingRegistryExtensions(req.Config, nil)) == 0 {
		return nil
	}
	ws, err := workspace.Load(req.WorkspaceRoot)
	if err != nil {
		return nil
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, project := range ws.Projects {
		projectPaths[i] = project.Path
	}
	discovered, err := internalextension.DiscoverExtensionsDetailed(req.WorkspaceRoot, req.Config, projectPaths)
	if err != nil {
		return nil
	}
	return discovered
}

// newRunCacheManager builds the run's one cache manager, honoring a
// cache-verification store override.
//
// A tree state this run captured is git answering inside the workspace root,
// so the manager starts out knowing Git manages the root: no execution key and
// no version stamp of the run asks Git again. A frozen portable request
// carries the tree state its submitter captured, which proves nothing about
// the root it runs in, so that root is asked like any other.
func newRunCacheManager(req *Request, ws *workspace.Workspace) *store.CacheManager {
	var storeRootOverride string
	if req.CacheVerification != nil {
		storeRootOverride = req.CacheVerification.StoreRoot
	}
	cache := jobs.NewRunCacheManager(req.WorkspaceRoot, storeRootOverride)
	if req.VersionSnapshot != nil && !req.Portable.frozen() {
		cache.RecordManagedRoot(ws.Root)
	}
	return cache
}

// runVersions resolves the per-line versions once, before the plan can key or
// execute work. Every later consumer must use that same snapshot: re-reading
// tags for evidence recovery can both change its cache keys and repeat the
// workspace's Git history traversal. Engine.Run clears this memo on each run.
// A nil snapshot still resolves any lines git can answer with their own tree
// fields, as for callers outside the engine lifecycle.
func (req *Request) runVersions(ws *workspace.Workspace) jobs.RunVersions {
	if req.versions == nil {
		if req.Portable.frozen() {
			// The executing side stamps exactly the lines the submitter resolved:
			// the snapshot carries no tags or history to derive them from.
			req.versions = portableRunVersions(req.Portable.Request.Source.Versions)
		} else {
			req.versions, req.versionsErr = jobs.BuildRunVersions(ws, req.VersionSnapshot)
			if req.Global.Debug {
				for _, reason := range jobs.DegradedVersionLines(req.versionsErr) {
					iox.Fprintf(os.Stderr, "[debug] version line degraded: %s\n", reason)
				}
			}
		}
	}
	return req.versions
}
