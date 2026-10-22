package toolchain

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/putnamihome"
)

func TestResolveBun_Found(t *testing.T) {
	// Skip if bun is not installed on this machine.
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not found on PATH, skipping")
	}

	got, err := ResolveBun()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty path for bun")
	}
}

// fakeBunHost is a machine a test owns: its environment is a map, its PATH is
// searched in that map, and its home directory is a temporary directory.
type fakeBunHost struct {
	bunHost
	env  map[string]string
	home string
}

// newFakeBunHost returns a goos/goarch machine with an empty PATH, a home
// directory that holds nothing, and no Putnami home but the default one,
// .putnami under that home directory.
func newFakeBunHost(t *testing.T, goos, goarch string) *fakeBunHost {
	t.Helper()
	fake := &fakeBunHost{env: map[string]string{}, home: t.TempDir()}
	fake.env["HOME"], fake.env["USERPROFILE"] = fake.home, fake.home
	fake.bunHost = bunHost{
		platform: platform{goos: goos, goarch: goarch},
		lookup:   func(key string) string { return fake.env[key] },
		setenv: func(key, value string) error {
			fake.env[key] = value
			return nil
		},
		lookPath: func(file string) (string, error) {
			for _, dir := range filepath.SplitList(fake.env["PATH"]) {
				for _, name := range []string{file, file + ".exe"} {
					if candidate := filepath.Join(dir, name); isRegularFile(candidate) {
						return candidate, nil
					}
				}
			}
			return "", exec.ErrNotFound
		},
		userHome: func() (string, error) { return fake.home, nil },
	}
	return fake
}

// toolchainRoot is where this machine keeps the Bun releases Putnami installs.
func (f *fakeBunHost) toolchainRoot() string {
	return filepath.Join(f.home, ".putnami", "toolchains", "bun")
}

// writeFakeBun writes a bun program that reports version to dir/<program>.
// The tests read the version from the file instead of running it.
func writeFakeBun(t *testing.T, dir, program, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, program)
	if err := os.WriteFile(path, []byte(version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The buns of the host are listed in the order of the task runtime candidates
// of the extension manifest: PATH, $BUN_INSTALL/bin, then .bun/bin under the
// user's home directory, each once.
func TestHostBuns_FollowTheTaskRuntimeCandidates(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			program := bunProgram(goos)
			host := newFakeBunHost(t, goos, "amd64")
			if got := host.hostBuns(); len(got) != 0 {
				t.Fatalf("a host without bun lists %v", got)
			}

			inHome := writeFakeBun(t, filepath.Join(host.home, ".bun", "bin"), program, "1.0.0")
			if got := host.hostBuns(); !slices.Equal(got, []string{inHome}) {
				t.Fatalf("hostBuns = %v, want the bun of the user's home directory", got)
			}

			custom := t.TempDir()
			inBunInstall := writeFakeBun(t, filepath.Join(custom, "bin"), program, "1.0.0")
			host.env["BUN_INSTALL"] = custom
			if got := host.hostBuns(); !slices.Equal(got, []string{inBunInstall, inHome}) {
				t.Fatalf("hostBuns = %v, want $BUN_INSTALL before the home directory", got)
			}

			onPath := writeFakeBun(t, t.TempDir(), program, "1.0.0")
			host.env["PATH"] = filepath.Dir(onPath)
			if got := host.hostBuns(); !slices.Equal(got, []string{onPath, inBunInstall, inHome}) {
				t.Fatalf("hostBuns = %v, want PATH first", got)
			}

			// A program two candidates name is listed once.
			host.env["PATH"] = filepath.Join(custom, "bin")
			if got := host.hostBuns(); !slices.Equal(got, []string{inBunInstall, inHome}) {
				t.Fatalf("hostBuns = %v, want each program once", got)
			}
		})
	}
}

// The task runtime of the extension manifest lists the buns of the host in the
// order hostBuns reads them, then the provisioned release, which the CLI
// unpacks in toolchains/bun/bun-<version> under the Putnami home and runs at
// the executable the toolchain-resolve answer names. No candidate names a
// release the extension installs. BUN_INSTALL, derived from that executable,
// is the release directory, the one ResolveBun finds the release in.
func TestTaskRuntime_ListsTheProvisionedBunAsACandidate(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-under-the-putnami-home",
		"the-provisioned-bun-is-a-task-runtime-candidate")

	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	type candidate struct {
		From        string `json:"from"`
		Environment string `json:"environment,omitempty"`
		Path        string `json:"path,omitempty"`
	}
	var manifest struct {
		Runtime struct {
			Toolchains map[string]struct {
				Lock        string      `json:"lock"`
				Candidates  []candidate `json:"candidates"`
				Environment map[string]struct {
					From   string `json:"from"`
					Levels int    `json:"levels"`
				} `json:"environment"`
			} `json:"toolchains"`
			RunToolchains []string `json:"runToolchains"`
			Resolves      []string `json:"resolves"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	runtime, declared := manifest.Runtime.Toolchains["taskRuntime"]
	if !declared || runtime.Lock != bunToolchainName {
		t.Fatalf("the manifest declares no task runtime locked on %q: %+v", bunToolchainName, runtime)
	}
	if !slices.Contains(manifest.Runtime.RunToolchains, "taskRuntime") {
		t.Fatalf("runToolchains = %v, want every job to resolve the task runtime", manifest.Runtime.RunToolchains)
	}
	if !slices.Contains(manifest.Runtime.Resolves, bunToolchainName) {
		t.Fatalf("runtime.resolves = %v, want the %s lock the provisioned candidate needs", manifest.Runtime.Resolves, bunToolchainName)
	}
	want := []candidate{
		{From: "path", Path: "bun"},
		{From: "environment", Environment: bunInstallEnv, Path: "bin/bun"},
		{From: "home", Path: ".bun/bin/bun"},
		{From: "provisioned"},
	}
	if !slices.Equal(runtime.Candidates, want) {
		t.Fatalf("task runtime candidates = %+v, want %+v", runtime.Candidates, want)
	}
	for _, candidate := range runtime.Candidates {
		if candidate.From == "putnami-home" {
			t.Fatalf("a putnami-home candidate names %s: the extension installs no Bun", candidate.Path)
		}
	}

	binding, bound := runtime.Environment[bunInstallEnv]
	if !bound || binding.From != "ancestor" {
		t.Fatalf("the task runtime derives no %s from its program: %+v", bunInstallEnv, runtime.Environment)
	}
	home := filepath.Join(t.TempDir(), ".putnami")
	lookup := func(key string) string {
		if key == putnamihome.Env {
			return home
		}
		return ""
	}
	root := putnamihome.ToolchainRoot(lookup, "", bunToolchainName)
	pin := bunPinOf(DefaultBunRelease())
	for target, archive := range pin.Archives {
		goos, goarch, _ := strings.Cut(target, "/")
		dir := filepath.Join(home, "toolchains", runtime.Lock, runtime.Lock+"-"+pin.Version)
		provisioned := writeFakeBun(t, filepath.Dir(filepath.Join(dir, filepath.FromSlash(archive.Executable))),
			filepath.Base(archive.Executable), pin.Version)
		if got := provisionedBun(root, pin.Version, platform{goos: goos, goarch: goarch}); got != provisioned {
			t.Errorf("%s: the CLI provisions %s, ResolveBun finds %q", target, provisioned, got)
		}
		derived := provisioned
		for range binding.Levels {
			derived = filepath.Dir(derived)
		}
		if derived != managedBunDir(root, pin.Version) || !isManagedBunDir(derived) {
			t.Errorf("%s: %s derived from %s = %s, want the release directory %s",
				target, bunInstallEnv, provisioned, derived, managedBunDir(root, pin.Version))
		}
	}
}

func TestHostBuns_WithoutAHomeDirectory(t *testing.T) {
	host := newFakeBunHost(t, "linux", "amd64")
	writeFakeBun(t, filepath.Join(host.home, ".bun", "bin"), "bun", "1.0.0")
	host.userHome = func() (string, error) { return "", errors.New("no home") }
	if got := host.hostBuns(); len(got) != 0 {
		t.Fatalf("hostBuns without a home directory = %v, want none", got)
	}
}

// A task takes the bun of the host first: the CLI puts the bun it resolved
// first on PATH. Without one it takes the release of the version the
// workspace pins under the Putnami home, at the path the CLI provisions it,
// else at bin/bun in the release directory, and points the
// environment at it. It installs none.
func TestResolveBun_FallsBackToTheReleaseUnderThePutnamiHome(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-under-the-putnami-home",
		"a-job-without-a-bun-on-path-runs-the-release-under-the-putnami-home")
	host := newFakeBunHost(t, "linux", "arm64")
	workspace := t.TempDir()
	writeBunLock(t, workspace, BunRelease{Version: "1.9.9"})
	declareBun(t, workspace, "bun@1.8.0")
	host.env[workspaceRootEnv] = workspace
	host.env["PATH"] = "/usr/bin"

	_, err := resolveBun(host.bunHost)
	if err == nil || !strings.Contains(err.Error(), "Bun 1.9.9") || !strings.Contains(err.Error(), "`putnami install`") {
		t.Fatalf("resolveBun without any bun: err = %v, want one that names Bun 1.9.9 and `putnami install`", err)
	}
	if strings.Contains(err.Error(), "bun.sh") {
		t.Fatalf("the error asks the user to install Bun: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(host.home, ".putnami")); len(entries) != 0 {
		t.Fatalf("resolveBun wrote under the Putnami home: %v", entries)
	}

	dir := filepath.Join(host.toolchainRoot(), "bun-1.9.9")
	atBin := writeFakeBun(t, filepath.Join(dir, "bin"), "bun", "1.9.9")
	got, err := resolveBun(host.bunHost)
	if err != nil || got != atBin {
		t.Fatalf("resolveBun = (%q, %v), want the program at bin/bun %q", got, err, atBin)
	}

	// The program at bun-<target>/bun comes before the one at bin/bun. The environment
	// is the job's again, which names neither.
	provisioned := writeFakeBun(t, filepath.Join(dir, "bun-linux-aarch64"), "bun", "1.9.9")
	host.env["PATH"] = "/usr/bin"
	delete(host.env, "BUN_INSTALL")
	delete(host.env, "BUN_RUNTIME_TRANSPILER_CACHE_PATH")
	got, err = resolveBun(host.bunHost)
	if err != nil || got != provisioned {
		t.Fatalf("resolveBun = (%q, %v), want the provisioned %q", got, err, provisioned)
	}
	if host.env["BUN_INSTALL"] != dir {
		t.Fatalf("BUN_INSTALL = %q, want %q", host.env["BUN_INSTALL"], dir)
	}
	if got, want := host.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"], filepath.Join(dir, "install", "cache", "@t@"); got != want {
		t.Fatalf("BUN_RUNTIME_TRANSPILER_CACHE_PATH = %q, want %q", got, want)
	}
	if got, want := host.env["PATH"], filepath.Dir(provisioned)+string(os.PathListSeparator)+"/usr/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}

	onPath := writeFakeBun(t, t.TempDir(), "bun", "1.0.0")
	host.env["PATH"] = filepath.Dir(onPath)
	if got, err := resolveBun(host.bunHost); err != nil || got != onPath {
		t.Fatalf("resolveBun with a bun on PATH = (%q, %v), want %q", got, err, onPath)
	}
}

// Without a lock pin the release is the one package.json declares, and
// without a workspace it is the extension's default one.
func TestResolveBun_NamesTheDeclaredOrTheDefaultRelease(t *testing.T) {
	declared := newFakeBunHost(t, "linux", "arm64")
	workspace := t.TempDir()
	declareBun(t, workspace, "bun@1.8")
	declared.env[workspaceRootEnv] = workspace
	if _, err := resolveBun(declared.bunHost); err == nil || !strings.Contains(err.Error(), "Bun 1.8.0") {
		t.Fatalf("resolveBun: err = %v, want one that names the declared Bun 1.8.0", err)
	}

	host := newFakeBunHost(t, "linux", "arm64")
	_, err := resolveBun(host.bunHost)
	if err == nil || !strings.Contains(err.Error(), "Bun "+DefaultBunVersion) {
		t.Fatalf("resolveBun: err = %v, want one that names Bun %s", err, DefaultBunVersion)
	}

	installed := writeFakeBun(t, filepath.Join(host.toolchainRoot(), "bun-"+DefaultBunVersion, "bun-linux-aarch64"), "bun", DefaultBunVersion)
	if got, err := resolveBun(host.bunHost); err != nil || got != installed {
		t.Fatalf("resolveBun = (%q, %v), want %q", got, err, installed)
	}

	// No workspace and no home directory: there is no Putnami home to look
	// under, and nothing is looked for in the working directory.
	homeless := newFakeBunHost(t, "linux", "arm64")
	delete(homeless.env, "HOME")
	delete(homeless.env, "USERPROFILE")
	if got, err := resolveBun(homeless.bunHost); err == nil {
		t.Fatalf("resolveBun without a Putnami home = %q, want an error", got)
	}
}

// A Bun under toolchains/bun/bun-<version> in a Putnami home keeps its files
// in that folder. The CLI names the folder in BUN_INSTALL, two levels above
// the program it resolved, whether the program is the provisioned
// bun-<target>/bun or a bin/bun in the same folder, and puts the
// program first on PATH. The transpiler cache then moves under the package
// cache, install/cache in that folder, and PATH stays as the CLI set it. A
// Bun of the host keeps its own locations.
func TestManagedBunEnv_KeepsTheCachesOfABunUnderThePutnamiHomeInItsFolder(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-under-the-putnami-home",
		"a-bun-under-the-putnami-home-keeps-its-caches-in-its-folder")
	for _, layout := range []string{"bun-linux-aarch64", "bin"} {
		t.Run(layout, func(t *testing.T) {
			host := newFakeBunHost(t, "linux", "arm64")
			dir := filepath.Join(host.toolchainRoot(), "bun-1.9.9")
			program := filepath.Join(dir, layout, "bun")
			path := filepath.Dir(program) + string(os.PathListSeparator) + "/usr/bin"
			host.env["PATH"] = path
			host.env["BUN_INSTALL"] = filepath.Dir(filepath.Dir(program))

			host.applyManagedBunEnv()
			if got := host.env["BUN_INSTALL"]; got != dir {
				t.Fatalf("BUN_INSTALL = %q, want %q", got, dir)
			}
			if got, want := host.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"], filepath.Join(dir, "install", "cache", "@t@"); got != want {
				t.Fatalf("BUN_RUNTIME_TRANSPILER_CACHE_PATH = %q, want %q", got, want)
			}
			if got := host.env["PATH"]; got != path {
				t.Fatalf("PATH = %q, want the CLI's %q", got, path)
			}
			for _, value := range []string{host.env["BUN_INSTALL"], host.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"]} {
				if strings.HasPrefix(value, filepath.Join(host.home, ".bun")) {
					t.Fatalf("%q is under .bun in the user's home directory", value)
				}
			}

			// An explicit transpiler cache setting is kept.
			host.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"] = "/explicit"
			host.applyManagedBunEnv()
			if got := host.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"]; got != "/explicit" {
				t.Fatalf("an explicit transpiler cache was replaced by %q", got)
			}
		})
	}

	// The host's own Bun: nothing is set.
	own := newFakeBunHost(t, "linux", "arm64")
	own.env["BUN_INSTALL"] = filepath.Join(own.home, ".bun")
	own.env["PATH"] = "/usr/bin"
	own.applyManagedBunEnv()
	if _, set := own.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"]; set || own.env["PATH"] != "/usr/bin" {
		t.Fatalf("a Bun of the host got Putnami's locations: %v", own.env)
	}
}

func TestIsManagedBunDir(t *testing.T) {
	for dir, want := range map[string]bool{
		filepath.Join("/home/dev/.putnami", "toolchains", "bun", "bun-1.4.0"):  true,
		filepath.Join("/relocated", "toolchains", "bun", "bun-1.4.0") + "/":    true,
		filepath.Join("/home/dev", ".bun"):                                     false,
		filepath.Join("/home/dev/.putnami", "toolchains", "go", "go-1.26.1"):   false,
		filepath.Join("/home/dev/.putnami", "toolchains", "bun"):               false,
		filepath.Join("/home/dev/.putnami", "toolchains", "bun", "other-1.4"):  false,
		filepath.Join("/opt", "bun", "bun-1.4.0"):                              false,
		filepath.Join("/home/dev/.putnami", "toolchains", "bun", "bun-1", "x"): false,
	} {
		if got := isManagedBunDir(dir); got != want {
			t.Errorf("isManagedBunDir(%q) = %v, want %v", dir, got, want)
		}
	}
}
