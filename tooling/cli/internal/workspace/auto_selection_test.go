package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

func TestResolveAutoSelection_FeatureBranchUsesTrunk(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "safe-baseline", "a-feature-branch-compares-with-the-trunk")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)
	runGitForImpact(t, tmp, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGitForImpact(t, tmp, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGitForImpact(t, tmp, "checkout", "-b", "feature")

	ws := &Workspace{Root: tmp, Config: &wsproto.Config{}}
	got, err := ResolveAutoSelection(ws, []string{"build"}, nil)
	if err != nil {
		t.Fatalf("ResolveAutoSelection: %v", err)
	}
	if got.Mode != AutoSelectionImpacted {
		t.Errorf("Mode = %q, want impacted", got.Mode)
	}
	if got.Baseline != "origin/main" {
		t.Errorf("Baseline = %q, want origin/main", got.Baseline)
	}
	if got.Reason != AutoSelectionReasonTrunk {
		t.Errorf("Reason = %q, want trunk", got.Reason)
	}
}

func TestResolveAutoSelection_MainFreshBuildUsesAll(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "safe-baseline", "trunk-builds-use-the-last-build-or-all")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	ws := &Workspace{Root: tmp, Config: &wsproto.Config{}}
	got, err := ResolveAutoSelection(ws, []string{"build"}, func(branch string, commands []string) (string, error) {
		if branch != "main" {
			t.Errorf("branch = %q, want main", branch)
		}
		if len(commands) != 1 || commands[0] != "build" {
			t.Errorf("commands = %v, want [build]", commands)
		}
		return "", nil
	})
	if err != nil {
		t.Fatalf("ResolveAutoSelection: %v", err)
	}
	if got.Mode != AutoSelectionAll {
		t.Errorf("Mode = %q, want all", got.Mode)
	}
	if got.Baseline != "" {
		t.Errorf("Baseline = %q, want empty", got.Baseline)
	}
	if got.Reason != AutoSelectionReasonFirstBuild {
		t.Errorf("Reason = %q, want first-build", got.Reason)
	}
}

func TestResolveAutoSelection_UnbornMainUsesAll(t *testing.T) {
	tmp := t.TempDir()
	runGitForImpact(t, tmp, "init")
	runGitForImpact(t, tmp, "symbolic-ref", "HEAD", "refs/heads/main")

	ws := &Workspace{Root: tmp, Config: &wsproto.Config{}}
	got, err := ResolveAutoSelection(ws, []string{"build"}, nil)
	if err != nil {
		t.Fatalf("ResolveAutoSelection: %v", err)
	}
	if got.Mode != AutoSelectionAll {
		t.Errorf("Mode = %q, want all", got.Mode)
	}
	if got.Reason != AutoSelectionReasonFirstBuild {
		t.Errorf("Reason = %q, want first-build", got.Reason)
	}
}

func TestResolveAutoSelection_MainPreviousBuildUsesLastSHA(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "safe-baseline", "trunk-builds-use-the-last-build-or-all")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	ws := &Workspace{Root: tmp, Config: &wsproto.Config{}}
	got, err := ResolveAutoSelection(ws, []string{"build"}, func(branch string, commands []string) (string, error) {
		if branch != "main" {
			t.Errorf("branch = %q, want main", branch)
		}
		if len(commands) != 1 || commands[0] != "build" {
			t.Errorf("commands = %v, want [build]", commands)
		}
		return "abc123", nil
	})
	if err != nil {
		t.Fatalf("ResolveAutoSelection: %v", err)
	}
	if got.Mode != AutoSelectionImpacted {
		t.Errorf("Mode = %q, want impacted", got.Mode)
	}
	if got.Baseline != "abc123" {
		t.Errorf("Baseline = %q, want abc123", got.Baseline)
	}
	if got.Reason != AutoSelectionReasonLastBuild {
		t.Errorf("Reason = %q, want last-build", got.Reason)
	}
}

func TestResolveAutoSelection_OutsideARepositoryUsesAll(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "bare-run-covers-every-project",
		"a-bare-run-outside-a-repository-selects-every-project")
	tmp := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(tmp))

	ws := &Workspace{Root: tmp, Config: &wsproto.Config{}}
	got, err := ResolveAutoSelection(ws, []string{"build"}, func(string, []string) (string, error) {
		t.Error("the last build was looked up for a root with no branch")
		return "", nil
	})
	if err != nil {
		t.Fatalf("ResolveAutoSelection: %v", err)
	}
	want := AutoSelection{Mode: AutoSelectionAll, Reason: AutoSelectionReasonNoRepository}
	if got != want {
		t.Errorf("selection = %+v, want %+v", got, want)
	}

	// The same answer when no git program is on PATH.
	t.Setenv("PATH", t.TempDir())
	if got, err := ResolveAutoSelection(ws, []string{"build"}, nil); err != nil || got != want {
		t.Errorf("selection without a git program = %+v, %v, want %+v", got, err, want)
	}
}

// A git failure that is not a missing repository stays an error: the selection
// fails with git's own detail instead of widening to every project.
func TestResolveAutoSelection_AnotherGitFailureStaysAnError(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "bare-run-covers-every-project",
		"another-git-failure-still-fails-the-selection")
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in git is a POSIX shell script")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"fatal: detected dubious ownership in repository at '/work'\" >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")

	ws := &Workspace{Root: t.TempDir(), Config: &wsproto.Config{}}
	got, err := ResolveAutoSelection(ws, []string{"build"}, nil)
	if err == nil {
		t.Fatalf("ResolveAutoSelection = %+v, want the git failure", got)
	}
	for _, want := range []string{"resolve current branch", "dubious ownership"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to keep %q", err, want)
		}
	}
}
