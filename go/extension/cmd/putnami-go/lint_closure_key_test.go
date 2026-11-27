package main

import (
	"slices"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

// golangci-lint and staticcheck type-check a project against its workspace
// dependencies, and staticcheck carries facts such as a deprecation across
// packages, so a dependency's API edit can change a dependent's findings. The
// lint command waits for no step of a dependency whose key would carry that
// edit, so each analyzer task keys on the non-test sources and module files of
// its dependency closure. Without the port, a deprecation added to a dependency
// served the dependent's cached green verdict.
func TestLintTasksKeyOnTheirDependencyClosure(t *testing.T) {
	m := loadExtensionManifest(t)
	want := []string{"!**/*_test.go", "!**/.gen/**", "**/*.go", "go.mod", "go.sum"}
	for _, taskName := range []string{"lint-golangci-fix", "lint-golangci-readonly", "lint-staticcheck"} {
		input, ok := m.Tasks[taskName].Inputs["dependencies"]
		if !ok || input.From != proto.TaskInputFromClosure {
			t.Errorf("%s dependencies = %+v, present=%t; want a closure input", taskName, input, ok)
			continue
		}
		// The closure port's `!**/*_test.go` also excludes the project's own
		// test files from `**/*.go`: only the go-embed:test selector, whose
		// files are added after exclusions, keeps them in the key.
		if sources := m.Tasks[taskName].Inputs["sources"]; !slices.Contains(sources.Files, "go-embed:test") {
			t.Errorf("%s sources = %v; want go-embed:test, which keeps the project's own test files keyed", taskName, sources.Files)
		}
		// DeriveTaskCacheKey sorts the patterns it folds.
		if key := proto.DeriveTaskCacheKey(m.Tasks[taskName].Inputs); !slices.Equal(key.ClosureFiles, want) {
			t.Errorf("%s closure cache files = %v, want %v", taskName, key.ClosureFiles, want)
		}
	}
}
