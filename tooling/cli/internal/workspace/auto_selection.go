package workspace

import (
	"fmt"
	"strings"

	"go.putnami.dev/tooling/cli/internal/git"
)

// ResolveAutoSelection chooses the default project selection for a bare
// putnami <jobs> invocation. Explicit --all, --impacted, targets, and "." are
// handled by the CLI before this resolver is called.
//
// Where Git does not manage the workspace root there is no branch and no
// baseline, so the selection is every project. Any other git failure is an
// error: a repository that cannot name its branch must not widen silently.
func ResolveAutoSelection(ws *Workspace, commands []string, lastBuild LastBuildLookup) (AutoSelection, error) {
	if ws == nil {
		return AutoSelection{}, fmt.Errorf("workspace is nil")
	}
	branch, err := git.CurrentBranch(ws.Root)
	if err != nil {
		if git.Unmanaged(ws.Root) != nil {
			return AutoSelection{
				Mode:   AutoSelectionAll,
				Reason: AutoSelectionReasonNoRepository,
			}, nil
		}
		return AutoSelection{}, fmt.Errorf("resolve current branch: %w", err)
	}

	if branch == "main" || branch == "master" {
		var lastSHA string
		if lastBuild != nil {
			lastSHA, err = lastBuild(branch, commands)
			if err != nil {
				return AutoSelection{}, fmt.Errorf("resolve last build for %q: %w", branch, err)
			}
		}
		lastSHA = strings.TrimSpace(lastSHA)
		if lastSHA == "" {
			return AutoSelection{
				Mode:   AutoSelectionAll,
				Branch: branch,
				Reason: AutoSelectionReasonFirstBuild,
			}, nil
		}
		return AutoSelection{
			Mode:     AutoSelectionImpacted,
			Baseline: lastSHA,
			Branch:   branch,
			Reason:   AutoSelectionReasonLastBuild,
		}, nil
	}

	trunk, err := git.ResolveTrunk(ws.Root)
	if err != nil {
		return AutoSelection{}, err
	}
	return AutoSelection{
		Mode:     AutoSelectionImpacted,
		Baseline: trunk,
		Branch:   branch,
		Reason:   AutoSelectionReasonTrunk,
	}, nil
}
