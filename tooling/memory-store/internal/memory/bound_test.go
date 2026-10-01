package memory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

// maximalCheckpoint is the shared maximal checkpoint with every identity
// member at its bound too: a token of the widest printable rune.
func maximalCheckpoint() map[string]any {
	token := providertest.MaximalToken("")
	return providertest.MaximalCheckpoint(token, strings.Repeat("k", 128),
		collab.MemoryIdentity{Workspace: token, Repository: token, Scope: token})
}

// TestAMaximalCheckpointIsReadBackOnEveryBackend: a checkpoint the contract
// accepts is stored in a document larger than 1 MiB, and every backend
// writes it and reads it back unchanged.
func TestAMaximalCheckpointIsReadBackOnEveryBackend(t *testing.T) {
	spectest.Proves(t, feature, "readable-records", "a-maximal-checkpoint-is-read-back")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		request := maximalCheckpoint()
		written := h.save(request).Record
		if strings.HasSuffix(t.Name(), "/file") {
			path := filepath.Join(h.workspace, DefaultFilePath, store.RecordsDir, store.RecordFileName(written.Ref.ID))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() <= 1<<20 {
				t.Fatalf("the maximal record is stored in %d bytes; it no longer exceeds 1 MiB", info.Size())
			}
			t.Logf("the maximal record is stored in %d bytes", info.Size())
		}
		identity := request["identity"].(map[string]any)
		read := h.missionIn(request["mission"].(string), identity)
		if read.Revision != written.Revision || read.Content != request["content"] || read.Title != request["title"] ||
			len(read.Sources) != collab.MaxListMembers || len(read.Evidence) != collab.MaxListMembers ||
			!slices.Equal(read.Sources, written.Sources) || !slices.Equal(read.Evidence, written.Evidence) {
			t.Fatalf("the maximal mission read back at %s differs from the one written at %s", read.Revision, written.Revision)
		}
	})
}
