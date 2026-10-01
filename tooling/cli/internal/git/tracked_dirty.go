package git

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
)

// deletedTrackedDigest marks a tracked file the working tree no longer has.
const deletedTrackedDigest = "deleted"

// TrackedDirtyDigests maps every tracked file that differs from HEAD — in the
// index or the working tree — to the SHA-256 of its working-tree content, or
// deletedTrackedDigest when it is gone. Untracked files are not tracked and
// are not listed. It returns nil, nil outside a git repository, so a caller
// with nothing to compare against sees nothing to report.
//
// Two snapshots, one before and one after a step, tell which committed files
// that step rewrote (RewrittenTrackedFiles): the question `putnami install`
// asks of itself, because a committed file it rewrites is a diff `--impacted`
// then plans from on this machine alone.
func TrackedDirtyDigests(repoRoot string) (map[string]string, error) {
	if _, err := run(repoRoot, "rev-parse", "--git-dir"); err != nil {
		return nil, nil
	}
	dirty, err := run(repoRoot, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	digests := make(map[string]string)
	for _, path := range splitNUL(dirty) {
		content, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if err != nil {
			if os.IsNotExist(err) {
				digests[path] = deletedTrackedDigest
				continue
			}
			return nil, err
		}
		sum := sha256.Sum256(content)
		digests[path] = hex.EncodeToString(sum[:])
	}
	return digests, nil
}

// RewrittenTrackedFiles returns, sorted, the tracked files whose working-tree
// content the step between the two snapshots changed: dirty after and not
// before, or dirty in both with different content. A file that was already
// dirty and untouched is not the step's doing and is not listed.
func RewrittenTrackedFiles(before, after map[string]string) []string {
	var rewritten []string
	for path, digest := range after {
		if previous, wasDirty := before[path]; !wasDirty || previous != digest {
			rewritten = append(rewritten, path)
		}
	}
	sort.Strings(rewritten)
	return rewritten
}
