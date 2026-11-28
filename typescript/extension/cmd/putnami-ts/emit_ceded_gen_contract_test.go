package main

import (
	"slices"
	"testing"

	"go.putnami.dev/typescript/extension/internal/emit"
)

// The emit manifest leaves out exactly the .gen paths that build-generate's
// gen output cedes to other tasks: a path it ceded but the manifest held would
// move the test run's key with the order other tasks run in, and a path the
// manifest left out but generate wrote would keep the key while generated code
// changed.
func TestTheEmitManifestLeavesOutWhatGenerateCedes(t *testing.T) {
	m := loadExtensionManifest(t)
	task, ok := m.Tasks["build-generate"]
	if !ok || task.Declares == nil {
		t.Fatal("the manifest declares no build-generate outputs")
	}
	gen, ok := task.Declares.Outputs["gen"]
	if !ok {
		t.Fatal("build-generate declares no gen output")
	}
	ceded := slices.Clone(gen.Excludes)
	slices.Sort(ceded)
	left := slices.Clone(emit.CededGenPaths)
	slices.Sort(left)
	if !slices.Equal(left, ceded) {
		t.Fatalf("the emit manifest leaves out %v, and build-generate's gen output cedes %v", left, ceded)
	}
}
