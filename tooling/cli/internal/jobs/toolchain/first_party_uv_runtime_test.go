package toolchain

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// firstPartyUVTasks are the tasks of the shipped Python extension that run uv.
// Each one must require the uv runtime toolchain, so the CLI resolves the uv
// the lock pins for it and its cache key names that release.
var firstPartyUVTasks = []string{
	"workspace-install-exec",
	"deps-upgrade-exec",
	"lint-format-fix",
	"lint-format-readonly",
	"lint-check-fix",
	"lint-check-readonly",
	"test-run",
	"serve-run",
	"run-exec",
}

// TestFirstPartyUVRuntimeResolvesTheInstallUnderThePutnamiHome resolves the uv
// runtime toolchain of the shipped Python extension manifest on a machine whose
// PATH holds a uv of another release than the lock pins. That uv is never
// substituted. Once the pinned release exists under the Putnami home where the
// lock's archive puts it, toolchains/uv/uv-<version>/uv, the runtime resolves
// it. Every task that runs uv requires the toolchain, so its
// cache key changes with the pinned release.
func TestFirstPartyUVRuntimeResolvesTheInstallUnderThePutnamiHome(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "exact-lock-resolution",
		"a-first-party-uv-runtime-resolves-the-install-under-the-putnami-home")
	const pinned, next, ambient = "0.11.32", "0.12.0", "0.5.18"
	repoRoot := findJobsRepoRoot(t)
	onPath := t.TempDir()
	writeRuntimeTool(t, filepath.Join(onPath, "uv"), ambient)
	home := t.TempDir()
	putnamiHome := filepath.Join(home, ".putnami")
	base := []string{"PATH=" + onPath, "HOME=" + home, "USERPROFILE=" + home, "PUTNAMI_HOME=" + putnamiHome}

	resolve := func(version string) (*extension.ExtensionDescription, model.RuntimeToolchainResolution, error) {
		t.Helper()
		root := t.TempDir()
		writeArchivedToolchainLock(t, root, "uv", version, WithExecutableSuffix(runtime.GOOS, "uv"))
		ext := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "python", "extension"), "@putnami/python")
		if ext == nil || ext.Runtime == nil {
			t.Fatal("python/extension declares no runtime")
		}
		if requirement, declared := ext.Runtime.Toolchains["uv"]; !declared || requirement.Lock != "uv" || requirement.Optional {
			t.Fatalf("python/extension: runtime toolchain \"uv\" is not a required toolchain locked on uv: %+v", requirement)
		}
		err := resolveRuntimeToolchainRefs(context.Background(), root, ext, []string{"uv"}, base, ResolveStrict)
		return ext, ext.RuntimeToolchains["uv"], err
	}

	// Only the uv of another release exists: it is not the runtime.
	if _, _, err := resolve(pinned); err == nil || !strings.Contains(err.Error(), `no candidate matched locked version "`+pinned+`"`) {
		t.Fatalf("resolution with only uv %s on PATH: error = %v, want no matching candidate", ambient, err)
	}

	installed := writeRuntimeTool(t, filepath.Join(putnamiHome, "toolchains", "uv", "uv-"+pinned, "uv"), pinned)
	ext, resolution, err := resolve(pinned)
	if err != nil {
		t.Fatalf("resolution with uv %s installed under the Putnami home: %v", pinned, err)
	}
	if !resolution.Available || !sameResolvedPath(t, resolution.Executable, installed) {
		t.Fatalf("resolution = %+v, want the install %s", resolution, installed)
	}

	writeRuntimeTool(t, filepath.Join(putnamiHome, "toolchains", "uv", "uv-"+next, "uv"), next)
	nextExt, nextResolution, err := resolve(next)
	if err != nil || !nextResolution.Available {
		t.Fatalf("resolution of uv %s = %+v, %v", next, nextResolution, err)
	}
	for _, name := range firstPartyUVTasks {
		task, declared := ext.Tasks[name]
		if !declared {
			t.Fatalf("python/extension declares no task %q", name)
		}
		if !slices.Contains(task.Toolchains, "uv") {
			t.Errorf("task %q runs uv but does not require the uv toolchain: %v", name, task.Toolchains)
			continue
		}
		key := CacheToolchainVersion(&jobs.ScheduledJob{Extension: ext, JobDef: &extension.JobDefinition{Toolchains: task.Toolchains}})
		nextKey := CacheToolchainVersion(&jobs.ScheduledJob{Extension: nextExt, JobDef: &extension.JobDefinition{Toolchains: task.Toolchains}})
		if key == runtime.Version() || key == nextKey {
			t.Errorf("task %q cache key %q does not follow the pinned uv (uv %s keys it %q)", name, key, next, nextKey)
		}
	}
}
