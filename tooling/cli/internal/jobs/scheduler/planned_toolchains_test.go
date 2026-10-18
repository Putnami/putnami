package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestRuntimeToolchainUnsatisfiedByTheLockFailsOnlyAPlannedJob pins that a
// run's synchronization stage leaves a required toolchain the lock does not
// satisfy unresolved, and the run fails only when its plan holds a job that
// requires it, with the error strict resolution returns.
func TestRuntimeToolchainUnsatisfiedByTheLockFailsOnlyAPlannedJob(t *testing.T) {
	bin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(bin, "compiler"), "0.9.0")
	t.Setenv("PATH", bin)
	cases := []struct {
		name, want string
		lock       func(t *testing.T, root string)
	}{
		{
			name: "no pin",
			lock: func(t *testing.T, root string) { writeRuntimeToolchainLock(t, root, "other", "1.2.3", "integrity-a") },
			want: `resolve extension runtime toolchains: resolve extension "example" runtime toolchain "compiler": ` +
				"workspace lock has no exact \"compiler\" pin: add a project that declares it, or run `putnami install` to record the declared one",
		},
		{
			name: "no matching candidate",
			lock: func(t *testing.T, root string) {
				writeRuntimeToolchainLock(t, root, "compiler", "1.2.3", "integrity-a")
			},
			want: `no candidate matched locked version "1.2.3"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.lock(t, root)
			project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
			ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
			job := func(ext *model.ExtensionDescription, name string) *ScheduledJob {
				return &ScheduledJob{Project: project, Extension: ext, JobDef: &model.JobDefinition{Name: name, Toolchains: []string{"compiler"}}}
			}
			synchronized := func() *model.ExtensionDescription {
				t.Helper()
				ext := provisioningToolchainExtension()
				active := map[string]bool{"build": true, toolchain.ToolchainProvisioningCommand: true}
				if err := toolchain.ResolveRuntimeToolchainsForCommandsMode(context.Background(), root, []*model.ExtensionDescription{ext},
					active, os.Environ(), toolchain.ResolveDeferred); err != nil {
					t.Fatalf("synchronization failed on a toolchain no planned job requires yet: %v", err)
				}
				if resolved, ok := ext.RuntimeToolchains["compiler"]; ok {
					t.Fatalf("synchronization recorded %+v, want the alias left unresolved", resolved)
				}
				return ext
			}

			synchronized()
			other := &model.ExtensionDescription{Name: "other"}
			if err := RequirePlannedRuntimeToolchains(ws, []*ScheduledJob{nil, {Project: project, Extension: other, JobDef: &model.JobDefinition{Name: "build~compile"}}}); err != nil {
				t.Fatalf("a plan with no job of the extension: %v", err)
			}

			// The build job is checked first, so the alias it shares with the
			// provisioning job resolves strictly.
			ext := synchronized()
			err := RequirePlannedRuntimeToolchains(ws, []*ScheduledJob{
				job(ext, toolchain.ToolchainProvisioningCommand+"~install"), job(ext, "build~compile"),
			})
			var runtimeErr *extensionRuntimeError
			if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimePrepareFailed ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("a planned build job: error = %v, want %s containing %q", err, extensionproto.FailureRuntimePrepareFailed, tc.want)
			}

			ext = synchronized()
			if err := RequirePlannedRuntimeToolchains(ws, []*ScheduledJob{job(ext, toolchain.ToolchainProvisioningCommand+"~install")}); err != nil {
				t.Fatalf("a plan of only %s: %v", toolchain.ToolchainProvisioningCommand, err)
			}
			if resolved, ok := ext.RuntimeToolchains["compiler"]; !ok || resolved.Available {
				t.Fatalf("%s resolution = %+v (present %v), want the unavailable identity", toolchain.ToolchainProvisioningCommand, resolved, ok)
			}
		})
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "unplanned-toolchains-do-not-stop-the-run",
		"only-a-planned-job-fails-on-an-unsatisfied-toolchain")
}
