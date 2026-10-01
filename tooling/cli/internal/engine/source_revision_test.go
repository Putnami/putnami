package engine

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// unboundCommit is a well-formed commit id no fixture repository has.
const unboundCommit = "0123456789abcdef0123456789abcdef01234567"

// A run that PUTNAMI_SOURCE_REVISION binds to a commit whose version cannot be
// read fails before any hook or job runs. Tolerating it the way a workspace
// outside git is tolerated would leave every line without a version, and a
// publish would ship 0.0.0 under the bound revision. The tests set the
// environment, so none of them is parallel.
func TestRunFailsWhenTheBoundCommitCannotBeVersioned(t *testing.T) {
	repo := t.TempDir()
	initCLISelectionGitRepo(t, repo)
	commitPackageJSON(t, repo)
	outside := t.TempDir()
	for _, tc := range []struct {
		name, dir, revision, commitTime, names string
	}{
		{"unreachable commit without a time", repo, unboundCommit, "", git.SourceCommitTimeEnv},
		{"unreachable commit with a malformed time", repo, unboundCommit, "yesterday", git.SourceCommitTimeEnv},
		{"malformed revision", repo, strings.ToUpper(unboundCommit), "1767323045", git.SourceRevisionEnv},
		{"a workspace outside git", outside, unboundCommit, "1767323045", git.SourceRevisionEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(git.SourceRevisionEnv, tc.revision)
			t.Setenv(git.SourceCommitTimeEnv, tc.commitTime)
			result, err := New().Run(context.Background(), Request{WorkspaceRoot: tc.dir, Commands: []string{"publish"}}, nil)
			if err == nil || result.ExitCode != ExitError || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("Run = exit %d, %v; want exit %d and an error naming %s", result.ExitCode, err, ExitError, tc.names)
			}
			versions, err := BuildVersionInfo(snapshotWorkspace(t, tc.dir))
			if err == nil || versions != nil || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("BuildVersionInfo = %+v, %v; want no versions and an error naming %s", versions, err, tc.names)
			}
		})
	}
}

// Unset, a workspace outside git keeps today's tolerance. Bound to HEAD itself,
// the version keeps HEAD's base and commit time and carries the override's
// fixed 12-character prefix as its SHA.
func TestBoundVersionKeepsTheBaseAndTimeOfHEAD(t *testing.T) {
	outside := workspace.NewWorkspace(t.TempDir(), nil, []*workspace.Project{{ID: "/app", Name: "app", Path: "."}})
	t.Setenv(git.SourceRevisionEnv, "")
	t.Setenv(git.SourceCommitTimeEnv, "")
	if _, err := BuildVersionInfo(outside); err != nil {
		t.Fatalf("unset override outside git = %v, want the run tolerated", err)
	}

	dir := t.TempDir()
	initCLISelectionGitRepo(t, dir)
	commitPackageJSON(t, dir)
	ws := snapshotWorkspace(t, dir)
	unbound := mustBuildVersionInfo(t, ws)[""]
	head := strings.TrimSpace(runCLISelectionGit(t, dir, "rev-parse", "HEAD"))
	t.Setenv(git.SourceRevisionEnv, head)
	bound := mustBuildVersionInfo(t, ws)[""]
	if unbound == nil || bound == nil {
		t.Fatalf("versions = %+v / %+v, want a root line both ways", unbound, bound)
	}
	commitTime, _, _ := strings.Cut(unbound.Suffix, "-")
	if bound.SHA != head[:12] || bound.Suffix != commitTime+"-"+head[:12] {
		t.Fatalf("bound version SHA/Suffix = %q/%q, want %q at HEAD's time %s", bound.SHA, bound.Suffix, head[:12], commitTime)
	}
	if bound.Base != unbound.Base || bound.Full != bound.Base+"-"+bound.Suffix || bound.Branch != unbound.Branch || bound.IsDirty != unbound.IsDirty {
		t.Fatalf("bound version = %+v, want the unbound base, branch and tree state %+v", bound, unbound)
	}
}
