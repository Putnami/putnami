package scheduler

import (
	"slices"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/repotest"
)

// keysOnTheWorkspaceCut reports whether a job's key reads Git candidates
// anywhere in the workspace: it keys on the candidate cut
// (cachekey.KeysOnCandidateCut) through a workspace `git:` input, or through a
// project or closure `git:` input that no "!../**" keeps under the directory
// it is read from, since a `git:` pattern's "*" and "**" also match ".."
// segments (store.FileInputRegion). A job whose every `git:` input is so kept
// reads the cut of its own project and closure members only.
func keysOnTheWorkspaceCut(job *ScheduledJob) bool {
	if !cachekey.KeysOnCandidateCut(job) {
		return false
	}
	key := job.JobDef.TaskCachePolicy.Key
	readsGit := func(patterns []string) bool {
		return slices.ContainsFunc(patterns, func(pattern string) bool {
			_, git := wsproto.GitFilePattern(pattern)
			return git
		})
	}
	unbounded := func(patterns []string) bool {
		return readsGit(patterns) && !slices.Contains(patterns, "!../**")
	}
	return readsGit(key.WorkspaceFiles) || unbounded(key.Files) || unbounded(key.ClosureFiles)
}

// The classification the generate-asset control of
// TestTwoLanesOnOneTreeShareTheirCacheKeys relies on: a workspace `git:`
// input, or a project or closure one that reaches above its directory, reads
// the whole cut; project and closure inputs kept under their directories read
// their own; a job with no `git:` input reads none.
func TestKeysOnTheWorkspaceCutClassifiesEachGitInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  extproto.TaskCacheKey
		want bool
	}{
		{"a workspace git input", extproto.TaskCacheKey{WorkspaceFiles: []string{"git:**"}}, true},
		{"a project git input that reaches above the project", extproto.TaskCacheKey{Files: []string{"git:**/*"}}, true},
		{"a closure git input that reaches above each member", extproto.TaskCacheKey{
			Files:        []string{"git:**/*", "!../**"},
			ClosureFiles: []string{"git:**/*", "!node_modules/**"},
		}, true},
		{"project and closure git inputs kept under their directories", extproto.TaskCacheKey{
			Files:        []string{"git:**/*", "!../**"},
			ClosureFiles: []string{"git:**/*", "!../**"},
		}, false},
		{"no git input", extproto.TaskCacheKey{Files: []string{"**/*.ts"}, WorkspaceFiles: []string{"tsconfig*.json"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.key
			job := &ScheduledJob{JobDef: &extension.JobDefinition{TaskCachePolicy: &extension.TaskCachePolicy{Key: &key}}}
			if got := keysOnTheWorkspaceCut(job); got != tc.want {
				t.Errorf("keysOnTheWorkspaceCut(%+v) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// The shipped build-emit task keys on the cut of its project and closure
// members only, and the shipped lint-docs on the workspace's whole cut, so the
// control classifies each by what its key reads.
func TestTheShippedEmitAndDocsTasksReadTheirCuts(t *testing.T) {
	repotest.Require(t)
	exts := treeKeyedExtensions(t)
	tasks := map[string]bool{"build-emit": false, "lint-docs": true}
	for _, ext := range exts {
		if ext.Name != "@putnami/typescript" {
			continue
		}
		for name, want := range tasks {
			task, ok := ext.Tasks[name]
			if !ok {
				t.Fatalf("@putnami/typescript declares no %s task", name)
			}
			key := extension.DeriveTaskCacheKey(task.Inputs)
			job := &ScheduledJob{JobDef: &extension.JobDefinition{TaskCachePolicy: &extension.TaskCachePolicy{Key: &key}}}
			if got := keysOnTheWorkspaceCut(job); got != want {
				t.Errorf("%s keys on the workspace's cut = %v, want %v (key %s)", name, got, want, strings.Join(slices.Concat(key.Files, key.ClosureFiles, key.WorkspaceFiles), " "))
			}
		}
		return
	}
	t.Fatal("no @putnami/typescript extension loaded")
}
