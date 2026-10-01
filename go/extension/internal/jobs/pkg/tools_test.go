package pkg

import (
	"archive/zip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/pkgmeta"

	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
)

// TestStagesPinnedTools_OnlyForTheProjectThatOwnsThePins pins the gate. Every
// Go project goes through this packager; only the one that ships
// tools/versions.json owns the pinned tools, and any other project paying four
// golangci-lint cross-compiles would be a pure regression.
func TestStagesPinnedTools_OnlyForTheProjectThatOwnsThePins(t *testing.T) {
	ordinary := t.TempDir()
	if stagesPinnedTools(ordinary) {
		t.Error("an ordinary project staged the pinned tools")
	}

	extension := t.TempDir()
	if err := os.MkdirAll(filepath.Join(extension, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "tools", "versions.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !stagesPinnedTools(extension) {
		t.Error("the project shipping tools/versions.json did not stage the pinned tools")
	}
}

// TestBuildPinnedTool_StagesAVerifiableBinaryPerPlatform covers the two facts
// the packager depends on and that `go` does not document in one place:
//
//   - a CROSS build of `go install pkg@version` lands in
//     $GOPATH/bin/<goos>_<goarch>, while a host build lands in $GOPATH/bin;
//   - the resulting binary carries the module version in its buildinfo, which
//     is what both the packager and the consumer compare against the pin.
//
// It builds a one-file probe module rather than golangci-lint so the case costs
// seconds instead of minutes while exercising the same code path.
func TestBuildPinnedTool_StagesAVerifiableBinaryPerPlatform(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available: %v", err)
	}
	probeCacheEnv(t, writeProbeProxy(t, "example.com/probe", "v1.2.3"), t.TempDir())

	for _, target := range []platform.Target{
		platform.HostTarget(),
		{GOOS: "linux", GOARCH: "arm64", Suffix: "linux-arm64"},
		// `go install` names a windows program <tool>.exe.
		{GOOS: "windows", GOARCH: "amd64", Suffix: "windows-x64"},
	} {
		t.Run(target.GOOS+"-"+target.GOARCH, func(t *testing.T) {
			gopath := t.TempDir()
			build, err := newPinnedToolBuild("go", gopath, 0)
			if err != nil {
				t.Fatalf("newPinnedToolBuild: %v", err)
			}
			built, err := buildPinnedTool(build, "go", target, "probe", "example.com/probe@v1.2.3")
			if err != nil {
				t.Fatalf("buildPinnedTool: %v", err)
			}
			if !strings.HasPrefix(built, gopath) {
				t.Errorf("built %q escaped the throwaway GOPATH %q", built, gopath)
			}
			if want := pkgmeta.ExecutableName(target.GOOS, "probe"); filepath.Base(built) != want {
				t.Errorf("built %q, want a file named %s", built, want)
			}
			if err := verifyPinnedToolBuild(built, "probe", "v1.2.3"); err != nil {
				t.Fatalf("verifyPinnedToolBuild: %v", err)
			}

			err = verifyPinnedToolBuild(built, "probe", "v9.9.9")
			if err == nil {
				t.Fatal("a binary that is not the pinned build was accepted for packaging")
			}
			if !strings.Contains(err.Error(), "v1.2.3") || !strings.Contains(err.Error(), "v9.9.9") {
				t.Errorf("refusal = %q, want both the embedded and the pinned version", err)
			}

			dest := filepath.Join(t.TempDir(), "probe")
			if err := copyExecutable(built, dest); err != nil {
				t.Fatalf("copyExecutable: %v", err)
			}
			info, err := os.Stat(dest)
			if err != nil {
				t.Fatal(err)
			}
			// Windows has no execute permission bit: a program runs by its
			// .exe name, which the build already asserted.
			if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
				t.Errorf("staged tool mode = %v, want it executable", info.Mode())
			}
		})
	}
}

func TestAppendEnvValues_ReplacesRatherThanDuplicates(t *testing.T) {
	got := appendEnvValues([]string{"GOOS=darwin", "PATH=/bin"}, "GOOS=linux", "GOARCH=arm64", "bogus")
	want := []string{"GOOS=linux", "PATH=/bin", "GOARCH=arm64"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("appendEnvValues = %v, want %v", got, want)
	}
}

// TestNewPinnedToolBuild_ReplacesTheInheritedGOMAXPROCS pins the CPU bound of
// parallel packaging. The job inherits GOMAXPROCS equal to the task's whole
// grant, and several platforms build at the same time, so each `go install`
// must carry its own share exactly once. A second GOMAXPROCS entry would leave
// the winner to the order of the environment.
func TestNewPinnedToolBuild_ReplacesTheInheritedGOMAXPROCS(t *testing.T) {
	probeCacheEnv(t, t.TempDir(), t.TempDir())
	t.Setenv("GOMAXPROCS", "10")

	for _, tc := range []struct {
		goMaxProcs int
		want       []string
	}{
		{goMaxProcs: 3, want: []string{"GOMAXPROCS=3"}},
		{goMaxProcs: 1, want: []string{"GOMAXPROCS=1"}},
		// No share: the build keeps what the job inherited.
		{goMaxProcs: 0, want: []string{"GOMAXPROCS=10"}},
	} {
		build, err := newPinnedToolBuild("go", t.TempDir(), tc.goMaxProcs)
		if err != nil {
			t.Fatalf("newPinnedToolBuild(%d): %v", tc.goMaxProcs, err)
		}
		var got []string
		for _, entry := range build.env {
			if strings.HasPrefix(entry, "GOMAXPROCS=") {
				got = append(got, entry)
			}
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("newPinnedToolBuild(%d) GOMAXPROCS entries = %v, want %v", tc.goMaxProcs, got, tc.want)
		}
	}
}

// TestBuildPinnedTool_CrossCompilesOfflineFromTheWarmedModuleCache is the
// regression case for the failure that stopped framework releases: the runner
// runs the whole task graph with GOPROXY=off, and `package~archives` compiles
// the pinned tools inside it.
//
// The case is the CI sequence in miniature — a cold cache, the warm-up
// `putnami install` performs with the proxy reachable, then the packaging build
// with the network taken away — and it cross-compiles for a platform that is
// NOT the host, because the release matrix does.
//
// It also pins WHY the warm-up cannot be a plain module download: `go install
// pkg@version` asks for <module>@latest to report a deprecation before it
// compiles anything, and no amount of module cache answers that query with
// GOPROXY=off. warmProbeModule uses `go install -n`, which makes the same two
// queries and skips only the compile.
func TestBuildPinnedTool_CrossCompilesOfflineFromTheWarmedModuleCache(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available: %v", err)
	}
	const install = "example.com/probe@v1.2.3"
	proxy := writeProbeProxy(t, "example.com/probe", "v1.2.3")
	cacheRoot := t.TempDir()
	probeCacheEnv(t, proxy, cacheRoot)

	warmProbeModule(t, install)

	// The runner's contract, verbatim: no proxy, no registry, no fallback.
	t.Setenv("GOPROXY", "off")

	target := foreignTarget()
	gopath := t.TempDir()
	build, err := newPinnedToolBuild("go", gopath, 0)
	if err != nil {
		t.Fatalf("newPinnedToolBuild: %v", err)
	}
	if !build.offline {
		t.Fatal("GOPROXY=off was not recognized as an offline build")
	}
	built, err := buildPinnedTool(build, "go", target, "probe", install)
	if err != nil {
		t.Fatalf("offline cross build for %s/%s: %v", target.GOOS, target.GOARCH, err)
	}
	if err := verifyPinnedToolBuild(built, "probe", "v1.2.3"); err != nil {
		t.Fatalf("verifyPinnedToolBuild: %v", err)
	}
}

// TestBuildPinnedTool_OfflineWithoutTheWarmUpNamesTheCauseAndTheFix covers the
// diagnostic. An offline build with nothing in the cache is a real failure and
// must stay one — the value here is that the error carries go's own sentence
// and the address of the step that should have prevented it, instead of the
// bare "exit status 1" that hid this defect for a day.
func TestBuildPinnedTool_OfflineWithoutTheWarmUpNamesTheCauseAndTheFix(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available: %v", err)
	}
	proxy := writeProbeProxy(t, "example.com/probe", "v1.2.3")
	cacheRoot := t.TempDir()
	probeCacheEnv(t, proxy, cacheRoot)
	t.Setenv("GOPROXY", "off")

	build, err := newPinnedToolBuild("go", t.TempDir(), 0)
	if err != nil {
		t.Fatalf("newPinnedToolBuild: %v", err)
	}
	_, err = buildPinnedTool(build, "go", foreignTarget(), "probe", "example.com/probe@v1.2.3")
	if err == nil {
		t.Fatal("an offline build with a cold module cache reported success")
	}
	for _, want := range []string{"example.com/probe", "putnami install", "workspace-install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("offline failure = %q, want it to mention %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "go:") {
		t.Errorf("offline failure = %q, want go's own stderr in it", err)
	}
}

// probeCacheEnv points every go command of a case at the fixture proxy and one
// cache root, and nothing else. The fixture proxy is the only source, so
// nothing may route around it: GOENV=off keeps a persisted `go env -w` from
// rerouting the fetch behind the environment, and the explicit empty
// GOPRIVATE/GONOPROXY keep a developer machine's private-module setup from
// sending example.com to a real host.
func probeCacheEnv(t *testing.T, proxy, cacheRoot string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.Walk(cacheRoot, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(p, info.Mode()|0o200)
			}
			return nil
		})
	})
	t.Setenv("PUTNAMI_GO_CACHE_DIR", cacheRoot)
	// The warm-up is a bare `go` command, so it needs the caches named the way
	// the job's shell exports them; GoCommandEnv derives the same two paths
	// from PUTNAMI_GO_CACHE_DIR for the build side.
	t.Setenv("GOMODCACHE", filepath.Join(cacheRoot, "mod"))
	t.Setenv("GOCACHE", filepath.Join(cacheRoot, "build"))
	t.Setenv("GOPATH", filepath.Join(cacheRoot, "gopath"))
	t.Setenv("GOPROXY", jobtest.FileURL(proxy))
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GOENV", "off")
	t.Setenv("GOWORK", "off")
}

// warmProbeModule is the warm-up `putnami install` runs (workspace-install,
// phase `tools`): the whole of `go install pkg@version` except the compile.
func warmProbeModule(t *testing.T, install string) {
	t.Helper()
	cmd := exec.Command("go", "install", "-n", "-trimpath", install)
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("warm %s: %v\n%s", install, err, out)
	}
}

// foreignTarget is a release platform this machine cannot be, so a case that
// claims to cross-compile does.
func foreignTarget() platform.Target {
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		return platform.Target{GOOS: "linux", GOARCH: "amd64", Suffix: "linux-x64"}
	}
	return platform.Target{GOOS: "linux", GOARCH: "arm64", Suffix: "linux-arm64"}
}

// writeProbeProxy serves one trivial main module from a file:// module proxy.
func writeProbeProxy(t *testing.T, modulePath, version string) string {
	t.Helper()
	proxy := t.TempDir()
	versionDir := filepath.Join(proxy, filepath.FromSlash(modulePath), "@v")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	goMod := "module " + modulePath + "\n\ngo 1.22\n"
	for name, content := range map[string]string{
		"list":            version + "\n",
		version + ".info": `{"Version":"` + version + `","Time":"2026-09-11T00:00:00Z"}`,
		version + ".mod":  goMod,
	} {
		if err := os.WriteFile(filepath.Join(versionDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	zipFile, err := os.Create(filepath.Join(versionDir, version+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(zipFile)
	root := modulePath + "@" + version
	for _, entry := range []struct{ name, content string }{
		{path.Join(root, "go.mod"), goMod},
		{path.Join(root, "main.go"), "package main\n\nfunc main() {}\n"},
	} {
		file, err := archive.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipFile.Close(); err != nil {
		t.Fatal(err)
	}
	return proxy
}
