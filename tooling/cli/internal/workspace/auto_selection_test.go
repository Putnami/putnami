package workspace

import (
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
