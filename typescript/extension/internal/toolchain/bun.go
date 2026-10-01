package toolchain

import (
	"fmt"
	"net/http"
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
	// client downloads a release. Nil selects the client of pinnedarchive.
	client *http.Client
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

// ResolveBun finds the bun a task runs with.
//
// A bun the host holds comes first (see hostBuns): the CLI puts the bun it
// resolved for the task first on PATH. Then comes the install under the
// Putnami home of the release the workspace pins or declares, or of the
// extension's default release. ResolveBun installs nothing: `putnami install`
// does, and the error names it.
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
		if want, err := wantedBun(workspaceRoot); err == nil && want.version != "" {
			version = want.version
		}
	}
	// Without a workspace root and without a home directory there is no
	// Putnami home to look under.
	root := ""
	if workspaceRoot != "" || putnamihome.Resolve(host.lookup) != "" {
		root = putnamihome.ToolchainRoot(host.lookup, workspaceRoot, bunToolchainName)
	}
	if root != "" && IsPlainBunRelease(version) {
		dir := managedBunDir(root, version)
		if bun := managedBunPath(dir, host.platform.goos); isRegularFile(bun) {
			if err := host.useManagedBun(dir); err != nil {
				return "", err
			}
			return bun, nil
		}
	}
	return "", fmt.Errorf("no Bun %s on this machine: run `putnami install`, which installs it under the Putnami home", version)
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

// managedBunDir is the directory of the Bun version Putnami installs under
// root, the toolchains/bun directory of a Putnami home.
func managedBunDir(root, version string) string {
	return filepath.Join(root, "bun-"+version)
}

// managedBunPath is the bun program of the install at dir.
func managedBunPath(dir, goos string) string {
	return filepath.Join(dir, "bin", bunProgram(goos))
}

// managedBunDirOf returns the install directory when bun is the program of a
// Bun that Putnami installed, and "" otherwise. Such an install is
// toolchains/bun/bun-<version> under a Putnami home, and its program is
// bin/bun in it.
func managedBunDirOf(bun string) string {
	dir := filepath.Dir(filepath.Dir(bun))
	if !isManagedBunDir(dir) {
		return ""
	}
	return dir
}

// isManagedBunDir reports whether dir has the place and the name of a Bun
// that Putnami installed: toolchains/bun/bun-<version>.
func isManagedBunDir(dir string) bool {
	return putnamihome.IsToolchainInstall(dir, bunToolchainName)
}

// useManagedBun makes this process, and every program it starts, run the Bun
// that Putnami installed at dir, and keeps what that Bun writes under dir:
// BUN_INSTALL names dir, so its package cache is install/cache there, the
// transpiler cache is the @t@ directory of that cache, and bin is first on
// PATH. An explicit transpiler cache setting is left alone.
//
// A Bun the host holds gets none of this: it keeps its own locations.
func (h bunHost) useManagedBun(dir string) error {
	if err := h.setenv(bunInstallEnv, dir); err != nil {
		return fmt.Errorf("set %s: %w", bunInstallEnv, err)
	}
	if strings.TrimSpace(h.lookup(bunTranspilerCacheEnv)) == "" {
		if err := h.setenv(bunTranspilerCacheEnv, filepath.Join(dir, "install", "cache", "@t@")); err != nil {
			return fmt.Errorf("set %s: %w", bunTranspilerCacheEnv, err)
		}
	}
	bin := filepath.Join(dir, "bin")
	path := h.lookup("PATH")
	if first, _, _ := strings.Cut(path, string(os.PathListSeparator)); first == bin {
		return nil
	}
	if path != "" {
		bin += string(os.PathListSeparator) + path
	}
	if err := h.setenv("PATH", bin); err != nil {
		return fmt.Errorf("set PATH: %w", err)
	}
	return nil
}

// ApplyManagedBunEnv keeps the files of a Bun that Putnami installed under
// its install directory, for this process and everything it starts.
//
// The CLI names the install the task runs with in BUN_INSTALL. When that is a
// Bun that Putnami installed, bun's transpiler cache moves under it too (see
// useManagedBun), so that such a Bun writes nothing to .bun in the user's home
// directory. Called once, at process start.
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
	_ = h.useManagedBun(dir)
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
