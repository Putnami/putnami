package gitread

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// git's environment loses every variable that selects another repository or
// injects configuration, and keeps transport credentials, tracing and the
// remaining settings.
func TestRepositoryEnvironmentKeepsTransportAndRemovesForeignRepository(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "tree-fingerprint",
		"the-environment-cannot-redirect-the-repository")
	selectors := []string{"GIT_DIR=/tmp/foreign.git", "GIT_WORK_TREE=/tmp/foreign", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/tmp/foreign", "GIT_CONFIG_GLOBAL=/tmp/gitconfig",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_INDEX_FILE=/tmp/foreign.index", "GIT_COMMON_DIR=/tmp/foreign.git",
		"GIT_OBJECT_DIRECTORY=/tmp/objects", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/alternates", "GIT_PREFIX=sub/",
		"GIT_CEILING_DIRECTORIES=/tmp", "GIT_DISCOVERY_ACROSS_FILESYSTEM=1", "GIT_NAMESPACE=other"}
	kept := []string{"GIT_SSH_COMMAND=ssh -i /tmp/key", "GIT_ASKPASS=/tmp/askpass", "GIT_LITERAL_PATHSPECS=1", "PATH=/bin"}
	got := RepositoryEnvironment(append(slices.Clone(selectors), kept...))
	for _, selector := range selectors {
		if slices.Contains(got, selector) {
			t.Errorf("selector survived: %q", selector)
		}
	}
	if !slices.Equal(got, kept) {
		t.Errorf("RepositoryEnvironment kept %q, want %q", got, kept)
	}
}

// A failed call names the verb, the directory and what git wrote on standard
// error.
func TestRunNamesTheVerbTheDirectoryAndGitsMessage(t *testing.T) {
	repo := initGitRepo(t)
	_, err := Run(repo, "rev-parse", "--verify", "--quiet", "missing^{commit}")
	if err == nil {
		t.Fatal("Run(rev-parse missing) succeeded, want an error")
	}
	if !strings.HasPrefix(err.Error(), "git rev-parse (in "+repo+"): ") {
		t.Fatalf("error = %q, want it to name the verb and the directory", err)
	}
}
