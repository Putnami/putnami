package engine

import (
	"os"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// loadWorkspaceAndExtensions loads the workspace and resolves extensions, once
// per run. It returns the full discovery result so later stages can chain skip
// records into gate and guard errors.
//
// This is the run's ONLY extension resolution. The CLI shell still runs a
// cheap pre-parse discovery (App.Run) to decide whether argv[0] names an
// extension-owned command group, and that pass was deliberately NOT folded
// into this one: two things run between them that can install extensions —
// first-use workspace bootstrap and the lifecycle before-hooks — so reusing
// the pre-bootstrap snapshot here would make a fresh checkout plan against
// the extensions it had BEFORE `putnami install` materialized them.
//
// DO NOT FOLD THEM. An earlier note said this became safe once bootstrap
// moved inside the lifecycle. That change landed and did NOT do that: it
// routed bootstrap's install THROUGH the engine adapter, but the bootstrap
// call itself still fires in App.Run (internal/cli/app.go), between the two
// discoveries. The second pass is therefore still load-bearing, and the
// precondition that note described has never been met. Folding on the
// strength of the old wording would make zero-init worktrees plan against a
// pre-install extension set — the failure this comment exists to prevent.
// Corrected during a later integration review.
func loadWorkspaceAndExtensions(req *Request) (*workspace.Workspace, *extension.DiscoveryResult, int) {
	ws, err := workspace.Load(req.WorkspaceRoot)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: load workspace: %v\n", err)
		return nil, nil, ExitError
	}
	ApplyEnvOverrides(&req.Global, req.Config)

	// Surface workspace warnings (e.g., name divergence, malformed
	// package.json/go.mod). These are always shown — silently swallowed
	// parse errors lead to "my project isn't listed" with zero diagnostic.
	// Quiet mode still hides them so scripts can stay clean.
	if len(ws.Warnings) > 0 && !req.Global.Quiet {
		for _, w := range ws.Warnings {
			iox.Fprintf(os.Stderr, "putnami: warning: %s\n", w)
		}
	}

	projectPaths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		projectPaths[i] = p.Path
	}
	discovered, err := extension.DiscoverExtensionsDetailed(req.WorkspaceRoot, req.Config, projectPaths)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: discover extensions: %v\n", err)
		return nil, nil, ExitError
	}
	// Resolve placement before runtime preparation, selection, or snapshot
	// capture. Absence follows this same local lifecycle exactly once. The
	// EXECUTING side of a portable request never resolves placement: it is the
	// remote, and re-resolving would recurse into another provider.
	if req.Portable == nil {
		provider, err := resolvePlacement(req.Global.Where, discovered.Extensions)
		if err != nil {
			iox.Fprintf(os.Stderr, "putnami: %v\n", err)
			return nil, nil, ExitUsage
		}
		req.runnerProvider = provider
	}
	if req.Global.Debug {
		iox.Fprintf(os.Stderr, "[debug] workspace: %s (%s)\n", ws.Name, req.WorkspaceRoot)
		iox.Fprintf(os.Stderr, "[debug] projects: %d\n", len(ws.Projects))
		iox.Fprintf(os.Stderr, "[debug] extensions: %d\n", len(discovered.Extensions))
		for _, ext := range discovered.Extensions {
			iox.Fprintf(os.Stderr, "[debug]   ext: %s (%d jobs)\n", ext.Name, len(ext.Jobs))
			for name := range ext.Jobs {
				iox.Fprintf(os.Stderr, "[debug]     job: %s\n", name)
			}
		}
		for _, skip := range discovered.Skipped {
			iox.Fprintf(os.Stderr, "[debug]   skipped ext: %s (%v)\n", skip.Name, skip.Reason)
		}
	}
	return ws, discovered, ExitSuccess
}
