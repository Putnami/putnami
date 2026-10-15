package cachekey

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A project takes the version of ITS line, not one answer for the run.
func TestVersionInfoForProject_ResolvesTheProjectsLine(t *testing.T) {
	t.Parallel()
	versions := RunVersions{
		"typescript": &JobContextVersion{Base: "0.4.0", Full: "0.4.0-abc1234", Line: "typescript"},
		"go":         &JobContextVersion{Base: "1.2.0", Full: "1.2.0-abc1234", Line: "go"},
	}
	got := VersionInfoForProject(versions, &workspace.Project{Line: "go"})
	if got == nil || got.Full != "1.2.0-abc1234" {
		t.Fatalf("version = %+v, want the go line's", got)
	}
	if base := LineBaseVersion(versions, &workspace.Project{Line: "typescript"}); base != "0.4.0" {
		t.Errorf("LineBaseVersion = %q, want the typescript line's base", base)
	}
}

// A project outside every declared line falls back to the root line, which is
// the answer a workspace that declares no line gives every project.
func TestVersionInfoForProject_FallsBackToTheRootLine(t *testing.T) {
	t.Parallel()
	versions := RunVersions{"": &JobContextVersion{Base: "0.0.0", Full: "0.0.0-abc1234"}}
	got := VersionInfoForProject(versions, &workspace.Project{Line: "unknown"})
	if got == nil || got.Full != "0.0.0-abc1234" {
		t.Fatalf("version = %+v, want the root line's", got)
	}
	if base := LineBaseVersion(nil, &workspace.Project{}); base != "" {
		t.Errorf("LineBaseVersion without versions = %q, want none", base)
	}
	if info := VersionInfoForProject(nil, nil); info != nil {
		t.Errorf("VersionInfoForProject without versions = %+v, want nil", info)
	}
}

// Primary is the reporting answer for a whole run: the root line when there is
// one, else the first line in sorted order, so two runs over one workspace file
// their report the same way.
func TestRunVersionsPrimary(t *testing.T) {
	t.Parallel()
	root := &JobContextVersion{Base: "1.0.0"}
	if got := (RunVersions{"": root, "go": {Base: "2.0.0"}}).Primary(); got != root {
		t.Errorf("Primary = %+v, want the root line's", got)
	}
	first := &JobContextVersion{Base: "2.0.0", Line: "go"}
	if got := (RunVersions{"typescript": {Base: "3.0.0"}, "go": first}).Primary(); got != first {
		t.Errorf("Primary = %+v, want the first line in sorted order", got)
	}
	if got := (RunVersions{}).Primary(); got != nil {
		t.Errorf("Primary of nothing = %+v, want nil", got)
	}
}

// rootLineVersions is a run's versions for a workspace made of one root line,
// which is what every fixture in this package builds.
func rootLineVersions(info *JobContextVersion) RunVersions {
	if info == nil {
		return nil
	}
	return RunVersions{"": info}
}
