package git

import (
	"slices"
	"testing"
)

func TestRepositoryEnvironmentKeepsTransportAndRemovesForeignRepository(t *testing.T) {
	input := []string{"GIT_DIR=/tmp/foreign.git", "GIT_WORK_TREE=/tmp/foreign", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/tmp/foreign", "GIT_NAMESPACE=other",
		"GIT_SSH_COMMAND=ssh -i /tmp/key", "GIT_ASKPASS=/tmp/askpass", "GIT_LITERAL_PATHSPECS=1", "PATH=/bin"}
	got := RepositoryEnvironment(input)
	for _, selector := range input[:6] {
		if slices.Contains(got, selector) {
			t.Fatalf("selector survived: %q", selector)
		}
	}
	for _, transport := range input[6:] {
		if !slices.Contains(got, transport) {
			t.Fatalf("transport or safe setting lost: %q", transport)
		}
	}
}
