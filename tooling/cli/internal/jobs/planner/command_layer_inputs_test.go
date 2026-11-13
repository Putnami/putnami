package planner

import (
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The TypeScript and Go `test` commands leave their command layers out of the
// steps that run build tasks, and only those: the steps that run and finalize
// the tests keep reading `options.test`.
func TestRealTestPipelinesLeaveTheCommandLayerOutOfTheirBuildSteps(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-step-may-read-no-command-option-layer",
		"the-typescript-and-go-test-steps-shared-with-build-read-no-command-layer")
	repoRoot := findJobsRepoRoot(t)
	cases := []struct {
		dir     string
		leaving []string
	}{
		{dir: "typescript/extension", leaving: []string{"generate", "tsc", "tsc-auto"}},
		{dir: "go/extension", leaving: []string{"config-merge", "describe", "generate"}},
	}
	for _, tc := range cases {
		ext := extension.LoadExtensionFromDir(filepath.Join(repoRoot, filepath.FromSlash(tc.dir)), "/"+tc.dir)
		if ext == nil {
			t.Fatalf("%s/putnami.extension.json did not load", tc.dir)
		}
		for name, job := range ext.Jobs {
			var leaving []string
			for _, step := range job.PipelineSteps {
				if !step.ReadsCommandLayer() {
					leaving = append(leaving, step.ID)
				}
			}
			slices.Sort(leaving)
			want := []string(nil)
			if name == "test" {
				want = tc.leaving
			}
			if !slices.Equal(leaving, want) {
				t.Errorf("%s %s: steps that read no command layer = %v, want %v", tc.dir, name, leaving, want)
			}
		}
	}
}
