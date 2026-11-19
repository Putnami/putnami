package main

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestServeAndRunStepsReadGen pins that the serve and run steps read the
// project's generated tree, which build-generate writes: the scheduler then
// holds the project reader lock while their program loads, so another
// session's generate never empties .gen under it.
func TestServeAndRunStepsReadGen(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-and-run-read-the-generated-tree", "the-serve-and-run-steps-read-gen")
	m := readTscManifest(t)
	if !slices.Contains(m.Tasks["build-generate"].Writes, any("gen")) {
		t.Fatalf("build-generate writes = %v, want gen", m.Tasks["build-generate"].Writes)
	}
	for command, step := range map[string]string{"serve": "serve", "run": "run"} {
		task := steps(m, command)[step].Task
		if task == "" {
			t.Fatalf("%s has no %s step", command, step)
		}
		if reads := m.Tasks[task].Reads; !slices.Contains(reads, any("gen")) {
			t.Errorf("%s step %s (task %s) reads = %v, want gen", command, step, task, reads)
		}
	}
}
