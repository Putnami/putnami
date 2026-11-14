package gitread

import (
	"strings"
	"testing"
)

// TestResolveBaselineReturnsAnExplicitRefUnchanged pins the first tier: a ref
// the caller named is never substituted, so a typo surfaces from the later read
// rather than from a silent fallback.
func TestResolveBaselineReturnsAnExplicitRefUnchanged(t *testing.T) {
	repo := initGitRepo(t)
	for _, explicit := range []string{"deadbeef", "  origin/whatever  ", "not-a-ref"} {
		got, err := ResolveBaseline(repo, explicit)
		if err != nil {
			t.Fatalf("ResolveBaseline(%q): %v", explicit, err)
		}
		if got != strings.TrimSpace(explicit) {
			t.Fatalf("ResolveBaseline(%q) = %q, want the trimmed input", explicit, got)
		}
	}
}

// TestResolveBaselineFallsBackToTheLocalTrunk pins the trunk tier. The fixture
// has no remote, so origin/HEAD, origin/main and origin/master are absent and the local `main`
// is the answer.
func TestResolveBaselineFallsBackToTheLocalTrunk(t *testing.T) {
	repo := initGitRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "feature")

	got, err := ResolveBaseline(repo, "")
	if err != nil {
		t.Fatalf("ResolveBaseline: %v", err)
	}
	if got != "main" {
		t.Fatalf("ResolveBaseline = %q, want the local trunk", got)
	}
}

// TestResolveBaselineFailsLoudlyWithNoTrunkAndNoUpstream pins the last tier: a
// repository with nothing to measure against says so, because an unresolved
// baseline that quietly became HEAD would compare a branch with itself and
// report "nothing changed" forever.
func TestResolveBaselineFailsLoudlyWithNoTrunkAndNoUpstream(t *testing.T) {
	repo := initGitRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "solo")
	runGit(t, repo, "branch", "-q", "-D", "main")

	_, err := ResolveBaseline(repo, "")
	if err == nil {
		t.Fatal("ResolveBaseline succeeded with no trunk and no upstream")
	}
	if !strings.Contains(err.Error(), "a branch is never measured against itself") {
		t.Fatalf("ResolveBaseline error = %v, want the operator-facing explanation", err)
	}
}
