package jobs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestFirstPartyBunRuntimesResolveTheInstallUnderThePutnamiHome resolves the
// bun runtime of the shipped TypeScript and clientgen manifests on a machine
// whose PATH holds a bun of another release than the lock pins. That bun is
// never substituted. Once the pinned release exists where the TypeScript
// extension installs it, toolchains/bun/bun-<version>/bin/bun under the
// Putnami home, the runtime resolves it and BUN_INSTALL names its install
// directory, so the bun keeps its own files there.
func TestFirstPartyBunRuntimesResolveTheInstallUnderThePutnamiHome(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "exact-lock-resolution",
		"a-first-party-bun-runtime-resolves-the-install-under-the-putnami-home")
	const pinned, ambient = "1.9.9", "1.4.0"
	repoRoot := findJobsRepoRoot(t)
	cases := []struct {
		dir, name, alias string
		// required reports whether a job of the extension fails without the
		// runtime, or runs with empty bindings.
		required bool
	}{
		{dir: filepath.Join("typescript", "extension"), name: "@putnami/typescript", alias: "taskRuntime", required: true},
		{dir: filepath.Join("tooling", "clientgen-extension"), name: "@putnami/clientgen", alias: "typescriptEmitter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeRuntimeToolchainLock(t, root, "bun", pinned, "integrity-a")
			onPath := t.TempDir()
			writeRuntimeTool(t, filepath.Join(onPath, "bun"), ambient)
			home := t.TempDir()
			putnamiHome := filepath.Join(home, ".putnami")
			installDir := filepath.Join(putnamiHome, "toolchains", "bun", "bun-"+pinned)
			base := []string{"PATH=" + onPath, "HOME=" + home, "USERPROFILE=" + home, "PUTNAMI_HOME=" + putnamiHome}

			resolve := func() (model.RuntimeToolchainResolution, error) {
				t.Helper()
				ext := extension.LoadExtensionFromDir(filepath.Join(repoRoot, tc.dir), tc.name)
				if ext == nil || ext.Runtime == nil {
					t.Fatalf("%s declares no runtime", tc.dir)
				}
				if requirement, declared := ext.Runtime.Toolchains[tc.alias]; !declared || requirement.Lock != "bun" {
					t.Fatalf("%s: runtime toolchain %q is not locked on bun; this test guards nothing", tc.dir, tc.alias)
				}
				err := resolveRuntimeToolchainRefs(context.Background(), root, ext, []string{tc.alias}, base, resolveStrict)
				return ext.RuntimeToolchains[tc.alias], err
			}

			// Only the bun of another release exists: it is not the runtime.
			resolution, err := resolve()
			if tc.required {
				if want := `no candidate matched locked version "` + pinned + `"`; err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("resolution with only bun %s on PATH: error = %v, want it to contain %q", ambient, err, want)
				}
			} else if err != nil || resolution.Available {
				t.Fatalf("optional resolution with only bun %s on PATH = %+v, %v; want unavailable", ambient, resolution, err)
			}

			installed := writeRuntimeTool(t, filepath.Join(installDir, "bin", "bun"), pinned)
			resolution, err = resolve()
			if err != nil {
				t.Fatalf("resolution with bun %s installed under the Putnami home: %v", pinned, err)
			}
			if !resolution.Available || !sameResolvedPath(t, resolution.Executable, installed) {
				t.Fatalf("resolution = %+v, want the install %s", resolution, installed)
			}
			if got := resolution.Environment["BUN_INSTALL"]; !sameResolvedPath(t, got, installDir) {
				t.Fatalf("BUN_INSTALL = %q, want the install directory %s", got, installDir)
			}
		})
	}
}

// sameResolvedPath reports whether a and b name the same file once symbolic
// links are resolved, which a temporary directory holds on macOS.
func sameResolvedPath(t *testing.T, a, b string) bool {
	t.Helper()
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return resolvedA == resolvedB
}
