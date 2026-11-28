package main

import (
	"slices"
	"strings"
	"testing"
)

// build-emit's key reads the files of each closure member, not the rest of the
// workspace: a `git:` pattern's "**" also matches ".." segments, so each of its
// project and closure inputs that holds one also holds "!../**". Otherwise
// every edit anywhere in the workspace moves the key, and the key of each test
// run that folds it before the manifest is known.
func TestBuildEmitKeysOnlyItsClosureMembersFiles(t *testing.T) {
	m := loadExtensionManifest(t)
	task, ok := m.Tasks["build-emit"]
	if !ok {
		t.Fatal("the manifest declares no build-emit task")
	}
	bounded := 0
	for name, input := range task.Inputs {
		if input.From != "project" && input.From != "closure" {
			continue
		}
		if !slices.ContainsFunc(input.Files, func(pattern string) bool { return strings.HasPrefix(pattern, "git:") }) {
			continue
		}
		if !slices.Contains(input.Files, "!../**") {
			t.Errorf("build-emit input %q (%s) reads Git candidates above its directory: %v", name, input.From, input.Files)
		}
		bounded++
	}
	if bounded == 0 {
		t.Fatal("build-emit reads no Git candidate of its project or closure; the check proves nothing")
	}
}
