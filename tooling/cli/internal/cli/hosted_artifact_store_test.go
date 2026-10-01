package cli

import (
	"errors"
	"testing"
)

// A hosted run whose artifact store would fall back to the workspace trusts
// what the repository committed there, so it is refused; a run without the
// credential keeps the workspace fallback.
func TestAHostedRunRefusesAnArtifactStoreInsideTheWorkspace(t *testing.T) {
	t.Parallel()
	inWorkspace := func() (string, bool) { return "", false }
	outside := func() (string, bool) { return "/home/runner/.putnami/artifacts", true }

	if err := requireHostedArtifactStore(false, inWorkspace); err != nil {
		t.Fatalf("without the credential: %v", err)
	}
	if err := requireHostedArtifactStore(true, inWorkspace); !errors.Is(err, ErrHostedArtifactStore) {
		t.Fatalf("hosted, store in the workspace: want ErrHostedArtifactStore, got %v", err)
	}
	if err := requireHostedArtifactStore(true, outside); err != nil {
		t.Fatalf("hosted, store outside the workspace: %v", err)
	}
}
