// Package toolchain resolves external tool binaries (bun, tsc, biome).
package toolchain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// fileExists checks if a path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// nodeBinShims lists the file names a package manager gives binary in
// node_modules/.bin/ on goos, in lookup order. On Windows bun writes a
// binary.exe shim and npm a binary.cmd one; the extensionless file npm also
// writes there is a shell script that Windows cannot start, so it is never a
// candidate.
func nodeBinShims(binary, goos string) []string {
	if goos == "windows" {
		return []string{binary + ".exe", binary + ".cmd"}
	}
	return []string{binary}
}

// windowsNativeBinaries names, per binary, the native Windows executable its
// npm package installs next to it in node_modules, with %s standing for the
// architecture in npm's spelling (x64, arm64). The .cmd shim npm writes starts
// the package's JavaScript launcher, which starts this same file; starting it
// directly keeps cmd.exe, and its parsing of the arguments, out of the way.
var windowsNativeBinaries = map[string]string{
	"biome": "@biomejs/cli-win32-%s/biome.exe",
}

// nodeModulesEntries lists the files, relative to a node_modules directory,
// that start binary on goos/goarch, in lookup order. On Windows a .exe shim
// comes first, then the package's native executable, and a .cmd shim last:
// Windows runs a .cmd through cmd.exe (see CheckShimArgs).
func nodeModulesEntries(binary, goos, goarch string) []string {
	shims := nodeBinShims(binary, goos)
	entries := make([]string, 0, len(shims)+1)
	for _, shim := range shims {
		if goos == "windows" && strings.HasSuffix(shim, ".cmd") {
			if native, ok := windowsNativeBinaries[binary]; ok {
				entries = append(entries, filepath.FromSlash(fmt.Sprintf(native, npmArch(goarch))))
			}
		}
		entries = append(entries, filepath.Join(".bin", shim))
	}
	return entries
}

// npmArch spells a Go architecture the way npm's process.arch does.
func npmArch(goarch string) string {
	if goarch == "amd64" {
		return "x64"
	}
	return goarch
}

// resolveInNodeModules looks for a binary in node_modules/ starting from
// startDir and walking up to stopDir.
func resolveInNodeModules(binary, startDir, stopDir string) string {
	return resolveInNodeModulesFor(binary, startDir, stopDir, runtime.GOOS, runtime.GOARCH)
}

// resolveInNodeModulesFor is resolveInNodeModules for goos/goarch. The nearest
// node_modules directory wins; within one, nodeModulesEntries decides.
func resolveInNodeModulesFor(binary, startDir, stopDir, goos, goarch string) string {
	entries := nodeModulesEntries(binary, goos, goarch)
	for dir := startDir; ; dir = filepath.Dir(dir) {
		for _, entry := range entries {
			candidate := filepath.Join(dir, "node_modules", entry)
			if fileExists(candidate) {
				return candidate
			}
		}
		if dir == stopDir || dir == filepath.Dir(dir) {
			break
		}
	}
	return ""
}

// ResolveConfig walks up from startDir to stopDir looking for filename.
// Returns fallback if not found.
func ResolveConfig(filename, startDir, stopDir, fallback string) string {
	for dir := startDir; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, filename)
		if fileExists(candidate) {
			return candidate
		}
		if dir == stopDir || dir == filepath.Dir(dir) {
			break
		}
	}
	return fallback
}

// resolveBinary tries node_modules/ then PATH.
func resolveBinary(name, projectRoot, workspaceRoot string) string {
	// Try node_modules/.bin/ walk
	if p := resolveInNodeModules(name, projectRoot, workspaceRoot); p != "" {
		return p
	}
	// Try PATH
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}
