package git

import "go.putnami.dev/sdk/extension/treefingerprint"

// TreeFingerprint identifies one worktree's exact state by content; see
// treefingerprint.Tree.
type TreeFingerprint = treefingerprint.Tree

// FingerprintTree computes the fingerprint of the worktree containing dir with
// the one implementation, treefingerprint.Compute, which extensions link too.
func FingerprintTree(dir string) (TreeFingerprint, error) {
	return treefingerprint.Compute(dir)
}
