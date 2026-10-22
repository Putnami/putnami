package toolchain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/putnamihome"
)

const (
	// bunToolchainName names Bun in the toolchains subtree of a Putnami home.
	bunToolchainName = "bun"
	// bunInstallEnv is the directory bun keeps its own files under: its
	// package cache is install/cache in it.
	bunInstallEnv = "BUN_INSTALL"
	// bunTranspilerCacheEnv is where bun keeps the cache of the sources it
	// transpiles. Bun does not derive it from BUN_INSTALL.
	bunTranspilerCacheEnv = "BUN_RUNTIME_TRANSPILER_CACHE_PATH"
	// workspaceRootEnv names the workspace root in the environment of a job.
	workspaceRootEnv = "PUTNAMI_WORKSPACE_ROOT"
)

// bunHost is what selecting a bun reads from the machine and writes to the
// environment of this process.
type bunHost struct {
	// platform is the host a bun is selected for.
	platform platform
	// lookup reads one environment variable.
	lookup func(string) string
	// setenv sets one environment variable of this process, which every
	// program it starts inherits.
	setenv func(key, value string) error
	// lookPath finds a program on PATH.
	lookPath func(file string) (string, error)
	// userHome returns the user's home directory.
	userHome func() (string, error)
}

// currentBunHost is the machine this process runs on.
func currentBunHost() bunHost {
	return bunHost{
		platform: hostPlatform(),
		lookup:   os.Getenv,
		setenv:   os.Setenv,
		lookPath: exec.LookPath,
		userHome: os.UserHomeDir,
	}
}

// ResolveBun finds the bun a job or a task runs with. It installs none.
//
// A bun the host holds comes first (see hostBuns): the CLI resolves the
// task runtime before it starts a job, provisioning the release the
// workspace lock pins under the Putnami home when the host holds none, and
// puts the bun it resolved first on PATH. Then comes the release under the
// Putnami home of the release the workspace pins or declares, or of the
// extension's default release (provisionedBun). The error names
// `putnami install`, which provisions it.
func ResolveBun() (string, error) {
	return resolveBun(currentBunHost())
}

func resolveBun(host bunHost) (string, error) {
	if found := host.hostBuns(); len(found) > 0 {
		return found[0], nil
	}
	version := DefaultBunVersion
	workspaceRoot := strings.TrimSpace(host.lookup(workspaceRootEnv))
	if workspaceRoot != "" {
		if wanted := wantedBunVersion(workspaceRoot); wanted != "" {
			version = wanted
		}
	}
	// Without a workspace root and without a home directory there is no
	// Putnami home to look under.
	if workspaceRoot != "" || putnamihome.Resolve(host.lookup) != "" {
		root := putnamihome.ToolchainRoot(host.lookup, workspaceRoot, bunToolchainName)
		if bun := provisionedBun(root, version, host.platform); bun != "" {
			if err := host.useManagedBun(managedBunDir(root, version), filepath.Dir(bun)); err != nil {
				return "", err
			}
			return bun, nil
		}
	}
	return "", fmt.Errorf("no Bun %s on this machine: run `putnami install`, which provisions it under the Putnami home", version)
}

// hostBuns lists the bun programs the host holds, in the order the task
// runtime of the extension manifest lists its candidates: bun on PATH, then
// bin/bun under $BUN_INSTALL, then bin/bun under .bun in the user's home
// directory, where bun's own installer writes it. The installer adds that
// directory to the user's PATH only for shells started after it ran. Each
// program is listed once.
func (h bunHost) hostBuns() []string {
	program := bunProgram(h.platform.goos)
	var found []string
	add := func(candidate string) {
		if candidate == "" || !isRegularFile(candidate) {
			return
		}
		for _, known := range found {
			if known == candidate {
				return
			}
		}
		found = append(found, candidate)
	}
	if onPath, err := h.lookPath("bun"); err == nil {
		add(onPath)
	}
	if root := strings.TrimSpace(h.lookup(bunInstallEnv)); root != "" {
		add(filepath.Join(root, "bin", program))
	}
	if home, err := h.userHome(); err == nil && strings.TrimSpace(home) != "" {
		add(filepath.Join(home, ".bun", "bin", program))
	}
	return found
}

// managedBunDir is the directory of a Bun release under root, the
// toolchains/bun directory of a Putnami home.
func managedBunDir(root, version string) string {
	return filepath.Join(root, "bun-"+version)
}

// provisionedBun returns the bun program of Bun version under root, or "".
// The CLI provisions a release there as its archive unpacks, the program at
// bun-<target>/bun in the release directory, which is the executable the
// toolchain-resolve answer names (bunPinOf). A release directory that holds its
// program at bin/bun instead is found too.
func provisionedBun(root, version string, host platform) string {
	if !IsPlainBunRelease(version) {
		return ""
	}
	dir := managedBunDir(root, version)
	program := bunProgram(host.goos)
	var candidates []string
	if archive, ok := bunArchiveFor(BunRelease{Version: version}, host.goos, host.goarch); ok {
		candidates = append(candidates, filepath.Join(dir, archive.dir, program))
	}
	for _, candidate := range append(candidates, filepath.Join(dir, "bin", program)) {
		if isRegularFile(candidate) {
			return candidate
		}
	}
	return ""
}

// isManagedBunDir reports whether dir has the place and the name of a Bun
// release under a Putnami home: toolchains/bun/bun-<version>.
func isManagedBunDir(dir string) bool {
	return putnamihome.IsToolchainInstall(dir, bunToolchainName)
}

// keepBunFilesUnder makes the Bun release at dir keep what it writes under
// dir, for this process and every program it starts: BUN_INSTALL names dir,
// so its package cache is install/cache there, and the transpiler cache is
// the @t@ directory of that cache. An explicit transpiler cache setting is
// left alone.
func (h bunHost) keepBunFilesUnder(dir string) error {
	if err := h.setenv(bunInstallEnv, dir); err != nil {
		return fmt.Errorf("set %s: %w", bunInstallEnv, err)
	}
	if strings.TrimSpace(h.lookup(bunTranspilerCacheEnv)) == "" {
		if err := h.setenv(bunTranspilerCacheEnv, filepath.Join(dir, "install", "cache", "@t@")); err != nil {
			return fmt.Errorf("set %s: %w", bunTranspilerCacheEnv, err)
		}
	}
	return nil
}

// useManagedBun makes this process, and every program it starts, run the Bun
// release at dir whose program is in programDir: its files stay under dir
// (keepBunFilesUnder), and programDir is first on PATH.
//
// A Bun the host holds gets none of this: it keeps its own locations.
func (h bunHost) useManagedBun(dir, programDir string) error {
	if err := h.keepBunFilesUnder(dir); err != nil {
		return err
	}
	path := h.lookup("PATH")
	if first, _, _ := strings.Cut(path, string(os.PathListSeparator)); first == programDir {
		return nil
	}
	if path != "" {
		programDir += string(os.PathListSeparator) + path
	}
	if err := h.setenv("PATH", programDir); err != nil {
		return fmt.Errorf("set PATH: %w", err)
	}
	return nil
}

// ApplyManagedBunEnv keeps the files of a Bun release under a Putnami home in
// that release's directory, for this process and everything it starts.
//
// The CLI names the release the task runtime resolved in BUN_INSTALL, two
// levels above its program, and puts the program first on PATH. A provisioned
// program at bun-<target>/bun and a program at bin/bun in the same folder
// both give the release directory, toolchains/bun/bun-<version>. For such a
// Bun, bun's transpiler cache moves under that directory too
// (keepBunFilesUnder), so it writes nothing to .bun in the user's home
// directory. PATH is left as the CLI set it. Called once, at process start.
func ApplyManagedBunEnv() {
	currentBunHost().applyManagedBunEnv()
}

func (h bunHost) applyManagedBunEnv() {
	dir := strings.TrimSpace(h.lookup(bunInstallEnv))
	if dir == "" || !isManagedBunDir(dir) {
		return
	}
	// Setting a variable fails only for a name or a value the system
	// refuses, and these come from the environment itself.
	_ = h.keepBunFilesUnder(dir)
}

// isRegularFile reports whether path names a regular file, through symbolic
// links.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Cache-root variables. PUTNAMI_BUN_CACHE_DIR is Putnami's override;
// BUN_INSTALL_CACHE_DIR is the one bun itself reads.
const (
	putnamiBunCacheDirEnv = "PUTNAMI_BUN_CACHE_DIR"
	bunInstallCacheDirEnv = "BUN_INSTALL_CACHE_DIR"
)

// ApplyBunCacheEnv translates Putnami's Bun cache override into the variable bun
// itself reads, for this process and everything it spawns.
//
// Until an earlier migration the CLI did this translation, for every job of every
// extension. Core stopped exporting language cache variables — so without this,
// PUTNAMI_BUN_CACHE_DIR would become a setting nothing honors and every install
// would silently land in bun's default location instead.
//
// An explicit BUN_INSTALL_CACHE_DIR is left alone: it is bun's own contract and
// the more specific of the two. Called once, at process start, so every bun this
// binary spawns inherits it without each call site remembering to.
func ApplyBunCacheEnv() {
	if strings.TrimSpace(os.Getenv(bunInstallCacheDirEnv)) != "" {
		return
	}
	override := strings.TrimSpace(os.Getenv(putnamiBunCacheDirEnv))
	if override == "" {
		return
	}
	_ = os.Setenv(bunInstallCacheDirEnv, override)
}
