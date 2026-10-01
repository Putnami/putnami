package sdd

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// This file replaces what the CLI tests got for free.
//
// `tooling/cli/internal/commands/sdd`'s tests wrote a fixture tree and then let
// `workspace.Load` discover its membership. An extension has no loader — the
// orchestrator resolves membership and puts it on the job-context wire — so a
// test here writes the same tree and STATES the membership the loader would
// have found. Everything a test then asserts is the engine's answer over that
// membership, which is the point of the split.

// writeFixtureFile writes one fixture file, creating its parents. Same body as
// `sharedtest.WriteFeatureFixture`, which lives in the CLI's test-only package.
func writeFixtureFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureWorkspace states a fixture tree's membership: the projects a loaded
// workspace would have carried, indexed the same way.
func fixtureWorkspace(name, root string, projects ...*workspace.Project) *workspace.Workspace {
	return workspace.NewWorkspace(root, &wsproto.Config{Name: name}, projects)
}

// appProject is the common fixture member: an application at <path> named
// <name>, with the id `workspace.Load` would derive from its path.
func appProject(name, path string) *workspace.Project {
	return &workspace.Project{ID: "/" + path, Name: name, SourceName: name, Type: "application", Path: path}
}
