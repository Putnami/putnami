package toolchain

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// The toolchains create checks before project-setup are those the job
// resolves, resolved as the job resolves them: a required one with no pin or
// no candidate that runs the pinned release is named by its lock, an optional
// one never is, and the description create discovered records no resolution.
func TestUnresolvedRuntimeToolchainLocksResolveAsTheSetupJob(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "project-setup-runs-in-the-project-extension",
		"create-reads-the-toolchains-the-setup-job-resolves")
	cases := []struct {
		name, pin, release string
		want               []string
	}{
		{"a pinned toolchain that runs", "compiler", "1.2.3", nil},
		{"no pin", "other", "1.2.3", []string{"compiler"}},
		{"no candidate runs the pinned release", "compiler", "0.9.0", []string{"compiler"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeRuntimeToolchainLock(t, root, tc.pin, "1.2.3", "integrity-a")
			bin := t.TempDir()
			writeRuntimeTool(t, filepath.Join(bin, "compiler"), tc.release)

			ext := provisioningToolchainExtension()
			helper := runtimeToolchainFixture("helper")
			helper.Optional = true
			ext.Runtime.Toolchains["helper"] = helper
			ext.Tasks["setup"] = model.TaskDefinition{Toolchains: []string{"helper"}}
			ext.Jobs[ProjectSetupCommand] = &model.JobDefinition{PipelineSteps: []extensionproto.PipelineStep{{ID: "setup", Task: "setup"}}}

			got, err := UnresolvedRuntimeToolchainLocks(context.Background(), root, ext, ProjectSetupCommand, []string{"PATH=" + bin})
			if err != nil {
				t.Fatalf("UnresolvedRuntimeToolchainLocks: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("unresolved = %v, want %v", got, tc.want)
			}
			if ext.RuntimeToolchains != nil {
				t.Fatalf("the check recorded resolutions on the description: %+v", ext.RuntimeToolchains)
			}
		})
	}
}
