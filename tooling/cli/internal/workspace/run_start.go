package workspace

import (
	"context"

	"go.putnami.dev/tooling/cli/internal/git"
)

type runStartKey struct{}

// CaptureRunStart records, on ctx, the paths of root no commit records before
// this process writes to the tree, so an --impacted selection can tell a
// change the operator made from a file the run's own first-use install or
// hooks wrote (ImpactedSelectionOptions.RunStart). The CLI adapter calls it
// before its bootstrap; the engine calls it again at its own boundary, and the
// first capture wins. It records nothing for a run that does not select
// --impacted, or when git cannot answer — every uncommitted path then counts.
func CaptureRunStart(ctx context.Context, root string, impacted bool) context.Context {
	if !impacted || root == "" || ctx.Value(runStartKey{}) != nil {
		return ctx
	}
	paths, err := git.UncommittedPaths(root)
	if err != nil {
		return ctx
	}
	if paths == nil {
		paths = []string{}
	}
	return context.WithValue(ctx, runStartKey{}, paths)
}

// RunStartFrom returns the paths CaptureRunStart recorded on ctx, or nil when
// none were.
func RunStartFrom(ctx context.Context) []string {
	paths, _ := ctx.Value(runStartKey{}).([]string)
	return paths
}
