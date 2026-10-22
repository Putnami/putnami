package scheduler

import (
	"runtime"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// The fixtures below build the runtime toolchains that the jobs tests resolve
// through the toolchain package: the runner's invocation, the planned-job
// check, and credential custody.

func runtimeToolchainFixture(lock string) extensionproto.RuntimeToolchain {
	return extensionproto.RuntimeToolchain{
		Lock:       lock,
		Candidates: []extensionproto.RuntimeToolchainCandidate{{From: extensionproto.RuntimeToolchainCandidatePath, Path: "compiler"}},
		Probe:      extensionproto.RuntimeToolchainProbe{Args: []string{"--version"}, Expect: "{version}"},
		Environment: map[string]extensionproto.RuntimeToolchainEnvironment{
			"COMPILER_ROOT": {From: extensionproto.RuntimeToolchainEnvironmentAncestor, Levels: 2},
		},
		PrependPath: true,
	}
}

func runtimeToolchainExtension(requirement extensionproto.RuntimeToolchain) *model.ExtensionDescription {
	return &model.ExtensionDescription{
		Name: "example",
		Runtime: &extensionproto.RuntimeDefinition{
			Executable: "compiled/example",
			Toolchains: map[string]extensionproto.RuntimeToolchain{"compiler": requirement},
		},
	}
}

// provisioningToolchainExtension declares one required toolchain every task of
// the runtime runs with, and two workspace commands that reach it: the
// provisioning verb and build.
func provisioningToolchainExtension() *model.ExtensionDescription {
	ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
	ext.Runtime.RunToolchains = []string{"compiler"}
	ext.Tasks = map[string]model.TaskDefinition{"install": {}, "compile": {}}
	ext.Jobs = map[string]*model.JobDefinition{
		toolchain.ToolchainProvisioningCommand: {PipelineSteps: []extensionproto.PipelineStep{{ID: "install", Task: "install"}}},
		"build":                                {PipelineSteps: []extensionproto.PipelineStep{{ID: "compile", Task: "compile"}}},
	}
	return ext
}

func writeRuntimeToolchainLock(t *testing.T, root, name, version, integrity string) {
	t.Helper()
	// A toolchain pin without archives is lock format version 4.
	locked := lockfile.NewLockFile()
	locked.Version = lockfile.FormatVersionV4
	locked.SetToolchain(name, lockfile.LockEntry{
		Version:     version,
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): integrity},
	})
	if err := lockfile.WriteLockFile(root, locked); err != nil {
		t.Fatal(err)
	}
}

// writeRuntimeTool places, at path, a program that prints version, and
// returns its path (fixtureproc.Write).
func writeRuntimeTool(t *testing.T, path, version string) string {
	t.Helper()
	return fixtureproc.Write(t, path, fixtureproc.Program{Stdout: version + "\n"})
}
