package gitread

import (
	"fmt"
	"strings"

	sdkgit "go.putnami.dev/sdk/extension/gitread"
)

// ResolveBaseline chooses the ref the architecture ratchet compares against
// when the caller passed none. An explicit ref is returned unchanged so a typo
// or an intentionally unusual ref surfaces from the later read rather than from
// a silent substitution.
//
// Order: explicit → trunk (origin/HEAD, origin/main, local main/master) →
// upstream tracking ref (skipped when it is this branch's own remote
// counterpart) → error.
//
// It has no epic-branch tier and reports no tier label: the architecture engine
// names no epic branch and reads only the ref. The error names every source it
// tried and what to pass instead, in the words the CLI uses for the same
// failure.
func ResolveBaseline(repoRoot, explicit string) (string, error) {
	if ref := strings.TrimSpace(explicit); ref != "" {
		return ref, nil
	}
	if trunk := sdkgit.TrunkRef(repoRoot); trunk != "" {
		return trunk, nil
	}
	currentBranch := sdkgit.CurrentBranchName(repoRoot)
	if ref := sdkgit.UpstreamRef(repoRoot); ref != "" && !sdkgit.UpstreamIsSelfCounterpart(repoRoot, ref, currentBranch) {
		return ref, nil
	}
	return "", fmt.Errorf("resolve git baseline: no explicit baseline, origin/HEAD, origin/main, local main/master, or usable upstream tracking ref was found (a branch is never measured against itself) — pass --baseline or set the workspace config baseline")
}
