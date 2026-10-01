package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const offlineSignal = "PUTNAMI_OFFLINE_DEPENDENCIES=1"

// offlineGoldenEnv is a representative job environment: a declared module
// origin, a caller GOFLAGS and a caller GONOPROXY pattern, and a cache root.
func offlineGoldenEnv(cacheRoot string, extra ...string) []string {
	return append([]string{
		"PATH=/usr/bin",
		"PUTNAMI_GO_CACHE_DIR=" + cacheRoot,
		testOriginEnv,
		"GOFLAGS=-trimpath",
		"GONOPROXY=corp.example/*",
	}, extra...)
}

// TestGoCommandEnvIsUnchangedWithoutTheOfflineSignal pins the environment a
// go command gets when the engine does not run the job offline: exactly the
// one it got before the offline signal existed, entry for entry and in order.
// Only the value "1" is the signal; any other value, or none, changes nothing
// but the entry that carries it.
func TestGoCommandEnvIsUnchangedWithoutTheOfflineSignal(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"without-the-offline-signal-go-commands-are-unchanged")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	want := []string{
		"PATH=/usr/bin",
		"PUTNAMI_GO_CACHE_DIR=" + cacheRoot,
		testOriginEnv,
		"GOFLAGS=-trimpath",
		"GOCACHE=" + filepath.Join(cacheRoot, "build"),
		"GOMODCACHE=" + filepath.Join(cacheRoot, "mod"),
		"GOTOOLCHAIN=local",
		"GOPROXY=" + defaultGoProxy,
		"GONOPROXY=corp.example/*," + testModulePattern,
		"GONOSUMDB=" + testModulePattern,
	}
	if got := GoCommandEnv(offlineGoldenEnv(cacheRoot), ""); !slices.Equal(got, want) {
		t.Fatalf("GoCommandEnv without the offline signal =\n%q\nwant\n%q", got, want)
	}
	for _, value := range []string{"", "0", "true", "yes", " 1"} {
		signal := "PUTNAMI_OFFLINE_DEPENDENCIES=" + value
		got := GoCommandEnv(offlineGoldenEnv(cacheRoot, signal), "")
		wantWithSignal := slices.Insert(slices.Clone(want), 4, signal)
		if !slices.Equal(got, wantWithSignal) {
			t.Errorf("GoCommandEnv with %q =\n%q\nwant\n%q", signal, got, wantWithSignal)
		}
	}
}

// TestGoCommandEnvForbidsModuleDownloadsOffline pins the policy every go
// command of a hosted run gets: GOPROXY=off, GONOPROXY=none, and -mod=readonly
// added to GOFLAGS. Everything else is the environment the command gets
// without the signal.
func TestGoCommandEnvForbidsModuleDownloadsOffline(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"offline-go-commands-forbid-every-module-download")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	want := []string{
		"PATH=/usr/bin",
		"PUTNAMI_GO_CACHE_DIR=" + cacheRoot,
		testOriginEnv,
		offlineSignal,
		"GOCACHE=" + filepath.Join(cacheRoot, "build"),
		"GOMODCACHE=" + filepath.Join(cacheRoot, "mod"),
		"GOTOOLCHAIN=local",
		"GONOSUMDB=" + testModulePattern,
		"GOPROXY=off",
		"GONOPROXY=none",
		"GOFLAGS=-trimpath -mod=readonly",
	}
	got := GoCommandEnv(offlineGoldenEnv(cacheRoot, offlineSignal), "")
	if !slices.Equal(got, want) {
		t.Fatalf("GoCommandEnv offline =\n%q\nwant\n%q", got, want)
	}
	if !ModuleDownloadsDisabled(got) {
		t.Error("ModuleDownloadsDisabled(offline env) = false, want true: build-tidy would run a tidy that cannot resolve")
	}
}

// TestGoCommandEnvOfflineOverridesEveryDownloadRoute covers the inherited
// settings that would otherwise reach the network: a third-party proxy, a
// caller GONOPROXY, and GOPRIVATE, which Go falls back to whenever GONOPROXY
// is empty and which sends matching modules straight to their origin.
func TestGoCommandEnvOfflineOverridesEveryDownloadRoute(t *testing.T) {
	for _, inherited := range [][]string{
		{"GOPROXY=https://corp-proxy.example.test"},
		{"GOPROXY=https://corp-proxy.example.test,direct", "GONOPROXY=corp.example/*"},
		{"GOPRIVATE=corp.example/*"},
		{testOriginEnv, "GOPROXY=direct", "GONOPROXY="},
		{"GOPROXY=off"},
	} {
		env := GoCommandEnv(append(slices.Clone(inherited), offlineSignal), "")
		if got := envValue(env, "GOPROXY"); got != "off" {
			t.Errorf("%v: GOPROXY = %q, want off", inherited, got)
		}
		if got := envValue(env, "GONOPROXY"); got != "none" {
			t.Errorf("%v: GONOPROXY = %q, want none: a non-empty pattern, or an empty one with GOPRIVATE set, fetches direct", inherited, got)
		}
		for _, key := range []string{"GOPROXY", "GONOPROXY", "GOFLAGS"} {
			if n := envKeyCount(env, key); n != 1 {
				t.Errorf("%v: %d %s entries, want one: %q", inherited, n, key, env)
			}
		}
	}
}

// TestOfflineGoFlags pins how -mod=readonly joins the caller's GOFLAGS. A
// -mod the caller already set is kept: Go applies the last -mod it reads, so
// appending would silently replace a -mod=vendor build with one that ignores
// the vendor directory, and no -mod value downloads a module once GOPROXY is
// off. Fields are read the way Go reads GOFLAGS: split on spaces, with a
// quote honored only at the start of a field.
func TestOfflineGoFlags(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"offline-goflags-keep-an-explicit-mod")
	for _, tt := range []struct{ in, want string }{
		{"", "-mod=readonly"},
		{"   ", "-mod=readonly"},
		{"-trimpath", "-trimpath -mod=readonly"},
		{" -trimpath  -race ", "-trimpath  -race -mod=readonly"},
		{"-mod=readonly", "-mod=readonly"},
		{"-mod=vendor", "-mod=vendor"},
		{"-trimpath -mod=mod", "-trimpath -mod=mod"},
		{"--mod=vendor", "--mod=vendor"},
		{"-modcacherw", "-modcacherw -mod=readonly"},
		{"-modfile=alt.mod", "-modfile=alt.mod -mod=readonly"},
		{"'-ldflags=-X main.v=1 -mod=vendor'", "'-ldflags=-X main.v=1 -mod=vendor' -mod=readonly"},
		{"\"-gcflags=all=-N -l\" -trimpath", "\"-gcflags=all=-N -l\" -trimpath -mod=readonly"},
		{"---mod=vendor", "---mod=vendor -mod=readonly"},
	} {
		if got := OfflineGoFlags(tt.in); got != tt.want {
			t.Errorf("OfflineGoFlags(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestOfflineDependenciesReadsOnlyTheExactSignal pins the predicate: "1" and
// nothing else, as registrycred.OfflineDependencies reads it.
func TestOfflineDependenciesReadsOnlyTheExactSignal(t *testing.T) {
	if !OfflineDependencies([]string{offlineSignal}) {
		t.Error("OfflineDependencies(=1) = false, want true")
	}
	for _, env := range [][]string{nil, {"PUTNAMI_OFFLINE_DEPENDENCIES="}, {"PUTNAMI_OFFLINE_DEPENDENCIES=true"}, {"PUTNAMI_OFFLINE_DEPENDENCIES= 1"}} {
		if OfflineDependencies(env) {
			t.Errorf("OfflineDependencies(%q) = true, want false", env)
		}
	}
}

// TestWorkspaceBuildEnvForbidsModuleDownloadsOffline pins the offline policy
// through the environment build, test and tidy run with.
func TestWorkspaceBuildEnvForbidsModuleDownloadsOffline(t *testing.T) {
	root := t.TempDir()
	writeGoWork(t, root)
	env := WorkspaceBuildEnv([]string{"PATH=/usr/bin", "GOFLAGS=-trimpath", offlineSignal}, root, "")
	for key, want := range map[string]string{
		"GOPROXY":   "off",
		"GONOPROXY": "none",
		"GOFLAGS":   "-trimpath -mod=readonly",
	} {
		if got := envValue(env, key); got != want {
			t.Errorf("WorkspaceBuildEnv offline %s = %q, want %q", key, got, want)
		}
	}
}

// offlineGoEnv is the host environment a real go command runs with in these
// tests, isolated from the user's go env file and GOFLAGS.
func offlineGoEnv(t *testing.T, extra ...string) (string, []string) {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go binary unavailable: %v", err)
	}
	env := setEnvValue(os.Environ(), "GOENV", "off")
	env = setEnvValue(env, "GOFLAGS", "")
	for _, key := range []string{"GOPROXY", "GONOPROXY", "GOPRIVATE", "GOWORK", goRegistryURLEnvVar} {
		env = removeEnv(env, key)
	}
	return goBinary, append(env, extra...)
}

// TestOfflinePolicyRunsTheWorkspaceGoCommands proves, with the real go
// command, that -mod=readonly from GOFLAGS is accepted in workspace mode by
// every command a task runs: build, vet, list and test.
func TestOfflinePolicyRunsTheWorkspaceGoCommands(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"offline-policy-runs-workspace-go-commands")
	goBinary, inherited := offlineGoEnv(t, offlineSignal)
	root := t.TempDir()
	for path, content := range map[string]string{
		"go.work":         "go 1.24\n\nuse (\n\t./lib\n\t./app\n)\n",
		"lib/go.mod":      "module example.test/lib\n\ngo 1.24\n",
		"lib/lib.go":      "package lib\n\nfunc Answer() int { return 42 }\n",
		"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(Answer())\n\t}\n}\n",
		"app/go.mod":      "module example.test/app\n\ngo 1.24\n",
		"app/main.go":     "package main\n\nimport \"example.test/lib\"\n\nfunc main() { println(lib.Answer()) }\n",
	} {
		file := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := WorkspaceBuildEnv(inherited, filepath.Join(root, "app"), goBinary)
	if got := envValue(env, "GOFLAGS"); got != "-mod=readonly" {
		t.Fatalf("GOFLAGS = %q, want -mod=readonly", got)
	}
	for _, args := range [][]string{
		{"build", "-o", os.DevNull, "."},
		{"vet", "./..."},
		{"list", "-deps", "./..."},
		{"test", "-count=1", "example.test/lib"},
	} {
		cmd := exec.Command(goBinary, args...)
		cmd.Dir = filepath.Join(root, "app")
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("go %s offline in workspace mode: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
}

// TestOfflinePolicyClosesTheDirectOriginRoute proves, with the real go
// command, that a GOPRIVATE module is not fetched from its origin offline:
// GOPROXY=off alone leaves that route open, because GOPRIVATE feeds an empty
// GONOPROXY. The module path is under the reserved .test domain, so a
// regression fails on name resolution rather than reaching a real host.
func TestOfflinePolicyClosesTheDirectOriginRoute(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"offline-policy-closes-the-direct-origin-route")
	goBinary, inherited := offlineGoEnv(t, offlineSignal, "GOPRIVATE=private.example.test/*",
		"PUTNAMI_GO_CACHE_DIR="+filepath.Join(t.TempDir(), "cache"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/consumer\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := setEnvValue(GoCommandEnv(inherited, goBinary), "GOWORK", "off")
	cmd := exec.Command(goBinary, "mod", "download", "-x", "private.example.test/module@v1.0.0")
	cmd.Dir = dir
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("go mod download of a private module succeeded offline:\n%s", output)
	}
	if !strings.Contains(string(output), "module lookup disabled by GOPROXY=off") {
		t.Errorf("go mod download output = %q, want Go's GOPROXY=off refusal and no direct fetch", output)
	}
	for _, direct := range []string{"git ", "https://private.example.test"} {
		if strings.Contains(string(output), direct) {
			t.Errorf("go mod download tried the origin directly (%q):\n%s", direct, output)
		}
	}
}
