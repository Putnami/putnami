package jobs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// A command that starts a pinned tool outside the task graph, as `projects
// create` starts the compiler of a new project, gets the resolution a task of
// an ordinary command gets: the locked executable first on PATH with its
// bindings, a refusal when the lock exists without a pin, and the ambient
// environment while no lock exists.
func TestRunToolchainEnvironmentResolvesLikeAnOrdinaryTask(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "create-installs-a-missing-go",
		"a-lifecycle-command-resolves-run-toolchains-like-a-task")
	ambient := t.TempDir()
	writeRuntimeTool(t, filepath.Join(ambient, "compiler"), "1.2.2")

	t.Run("a pinned toolchain installed inside the workspace", func(t *testing.T) {
		workspaceRoot := t.TempDir()
		binary := writeRuntimeTool(t, filepath.Join(workspaceRoot, ".tools", "compiler-1.2.3", "bin", "compiler"), "1.2.3")
		binary, err := filepath.EvalSymlinks(binary)
		if err != nil {
			t.Fatal(err)
		}
		writeRuntimeToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "integrity-a")
		requirement := runtimeToolchainFixture("compiler")
		requirement.Candidates = append(requirement.Candidates, extensionproto.RuntimeToolchainCandidate{
			From:        extensionproto.RuntimeToolchainCandidateEnvironment,
			Environment: runtimeToolchainWorkspaceRootEnv,
			Path:        ".tools/compiler-{version}/bin/compiler",
		})
		ext := runtimeToolchainExtension(requirement)
		ext.Runtime.RunToolchains = []string{"compiler"}
		base := []string{"PATH=" + ambient, "KEEP=1"}

		env, err := RunToolchainEnvironment(context.Background(), workspaceRoot, ext, base)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Split(envLastValue(env, "PATH"), string(os.PathListSeparator)); len(got) != 2 || got[0] != filepath.Dir(binary) || got[1] != ambient {
			t.Fatalf("PATH = %q, want the pinned directory before %q", got, ambient)
		}
		if got, want := envLastValue(env, "COMPILER_ROOT"), filepath.Dir(filepath.Dir(binary)); got != want {
			t.Fatalf("COMPILER_ROOT = %q, want %q", got, want)
		}
		if envLastValue(env, "KEEP") != "1" {
			t.Fatalf("environment = %v, want the base kept", env)
		}
		if path, err := ProviderExecutable("compiler", env); err != nil || path != binary {
			t.Fatalf("ProviderExecutable = %q, %v, want the pinned %q", path, err, binary)
		}
		if base[0] != "PATH="+ambient {
			t.Fatalf("base = %v, want it unchanged", base)
		}
	})

	t.Run("a lock without the pin", func(t *testing.T) {
		workspaceRoot := t.TempDir()
		writeRuntimeToolchainLock(t, workspaceRoot, "other", "1.2.3", "integrity-a")
		ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
		ext.Runtime.RunToolchains = []string{"compiler"}
		_, err := RunToolchainEnvironment(context.Background(), workspaceRoot, ext, []string{"PATH=" + ambient})
		if err == nil || !strings.Contains(err.Error(), `workspace lock has no exact "compiler" pin`) {
			t.Fatalf("error = %v, want the missing pin named, as an ordinary task gets it", err)
		}
	})

	t.Run("no lock yet", func(t *testing.T) {
		ext := runtimeToolchainExtension(runtimeToolchainFixture("compiler"))
		ext.Runtime.RunToolchains = []string{"compiler"}
		base := []string{"PATH=" + ambient}
		env, err := RunToolchainEnvironment(context.Background(), t.TempDir(), ext, base)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(env, base) {
			t.Fatalf("environment = %v, want the ambient %v", env, base)
		}
		if resolution := ext.RuntimeToolchains["compiler"]; resolution.Available || resolution.Identity == "" {
			t.Fatalf("resolution = %+v, want the unavailable bootstrap identity", resolution)
		}
	})
}

// Without an extension, or with one whose tasks name no run toolchain, the
// environment stays as it was, and the result never aliases the caller's.
func TestRunToolchainEnvironmentWithoutRunToolchainsKeepsTheBase(t *testing.T) {
	cases := map[string]*model.ExtensionDescription{
		"no extension":      nil,
		"no run toolchains": runtimeToolchainExtension(runtimeToolchainFixture("compiler")),
	}
	for name, ext := range cases {
		t.Run(name, func(t *testing.T) {
			base := []string{"PATH=/usr/bin", "KEEP=1"}
			env, err := RunToolchainEnvironment(context.Background(), t.TempDir(), ext, base)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(env, base) {
				t.Fatalf("environment = %v, want %v", env, base)
			}
			env[0] = "PATH=/changed"
			if base[0] != "PATH=/usr/bin" {
				t.Fatal("the returned environment aliases the base")
			}
		})
	}
}
