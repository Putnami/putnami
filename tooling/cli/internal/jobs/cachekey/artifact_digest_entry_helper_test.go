package cachekey

import "go.putnami.dev/tooling/cli/internal/store"

// ArtifactDigestFromEntry derives the digest of the declared output id from
// what a task-owned entry recorded, as a hit publishes it for a port that
// excludes nothing.
func ArtifactDigestFromEntry(entry *store.TaskEntry, id string) (string, bool) {
	files, ok := ArtifactFilesFromEntry(entry, id)
	if !ok {
		return "", false
	}
	recorded, _ := entry.Output(id)
	return ArtifactDigest(recorded.Kind, files), true
}
