package toolchain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveBun finds the bun binary.
// Resolution order: PATH (bun is typically globally installed), then, on
// Windows, the place bun's own installer puts it (see installedBun).
func ResolveBun() (string, error) {
	if p, err := exec.LookPath("bun"); err == nil {
		return p, nil
	}
	if p := installedBun(runtime.GOOS, os.Getenv, os.UserHomeDir); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("bun not found in PATH: install from https://bun.sh")
}

// installedBun returns the bun that bun's Windows installer wrote, or "" when
// there is none. The installer puts bun.exe in $BUN_INSTALL\bin, by default
// %USERPROFILE%\.bun\bin, and adds that directory to the user's Path only for
// shells started after it ran. On any other goos it returns "": PATH is the
// only place looked at there.
func installedBun(goos string, getenv func(string) string, home func() (string, error)) string {
	if goos != "windows" {
		return ""
	}
	root := strings.TrimSpace(getenv("BUN_INSTALL"))
	if root == "" {
		dir, err := home()
		if err != nil || dir == "" {
			return ""
		}
		root = filepath.Join(dir, ".bun")
	}
	candidate := filepath.Join(root, "bin", "bun.exe")
	if !fileExists(candidate) {
		return ""
	}
	return candidate
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
