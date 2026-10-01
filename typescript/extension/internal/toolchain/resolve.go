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

// platform is the host a binary is resolved for: its operating system and
// architecture in Go's spelling, and whether its C library is musl.
type platform struct {
	goos, goarch string
	musl         bool
}

// muslLoaders matches the dynamic loader a musl C library installs.
const muslLoaders = "/lib/ld-musl-*.so.1"

// hostPlatform is the platform this process runs on. A Linux host is a musl
// one when it holds musl's dynamic loader.
func hostPlatform() platform {
	p := platform{goos: runtime.GOOS, goarch: runtime.GOARCH}
	if p.goos == "linux" {
		loaders, _ := filepath.Glob(muslLoaders)
		p.musl = len(loaders) > 0
	}
	return p
}

// nativeBinary locates the native executable an npm package installs for the
// host as one of its optional platform packages.
type nativeBinary struct {
	// pkg is the package that depends on the platform packages, relative to a
	// node_modules directory.
	pkg string
	// program is the executable relative to a node_modules directory, with %s
	// standing for the platform in npm's spelling (see npmPlatform). On
	// Windows the file carries a .exe extension.
	program string
}

// nativeBinaries names, per binary, the native executable its npm package
// installs. The file a package manager writes in node_modules/.bin starts the
// package's JavaScript launcher, which starts this same file; starting it
// directly needs no JavaScript runtime and, on Windows, keeps cmd.exe and its
// parsing of the arguments out of the way.
var nativeBinaries = map[string]nativeBinary{
	"biome": {pkg: "@biomejs/biome", program: "@biomejs/cli-%s/biome"},
}

// nativeCandidates lists where the native executable of binary for p can be,
// seen from the node_modules directory nodeModules, in lookup order: in
// nodeModules itself, where a hoisted install puts every package, then in the
// node_modules directory that holds the real directory of the binary's
// package, where an isolated install links that package's dependencies.
func nativeCandidates(binary, nodeModules string, p platform) []string {
	native, ok := nativeBinaries[binary]
	if !ok {
		return nil
	}
	program := filepath.FromSlash(fmt.Sprintf(native.program, npmPlatform(p)))
	if p.goos == "windows" {
		program += ".exe"
	}
	candidates := []string{filepath.Join(nodeModules, program)}
	pkg := filepath.FromSlash(native.pkg)
	if realPkg, err := filepath.EvalSymlinks(filepath.Join(nodeModules, pkg)); err == nil {
		if dependencies := strings.TrimSuffix(realPkg, pkg); dependencies != realPkg {
			candidates = append(candidates, filepath.Join(dependencies, program))
		}
	}
	return candidates
}

// npmPlatform spells p the way the name of an npm platform package does:
// process.platform, process.arch and, for a musl host, a musl suffix.
func npmPlatform(p platform) string {
	name := p.goos
	if name == "windows" {
		name = "win32"
	}
	name += "-" + npmArch(p.goarch)
	if p.musl {
		name += "-musl"
	}
	return name
}

// npmArch spells a Go architecture the way npm's process.arch does.
func npmArch(goarch string) string {
	if goarch == "amd64" {
		return "x64"
	}
	return goarch
}

// resolveInNodeModulesFor looks for the file that starts binary on p in
// node_modules/, starting from startDir and walking up to stopDir. The nearest
// node_modules directory wins. Within one, the package's native executable
// comes first (see nativeCandidates), then the files of node_modules/.bin in
// the order of nodeBinShims: on Windows a .cmd shim is last, because Windows
// runs it through cmd.exe (see CheckShimArgs).
func resolveInNodeModulesFor(binary, startDir, stopDir string, p platform) string {
	shims := nodeBinShims(binary, p.goos)
	for dir := startDir; ; dir = filepath.Dir(dir) {
		nodeModules := filepath.Join(dir, "node_modules")
		candidates := nativeCandidates(binary, nodeModules, p)
		for _, shim := range shims {
			candidates = append(candidates, filepath.Join(nodeModules, ".bin", shim))
		}
		for _, candidate := range candidates {
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

// resolveBinary finds the file that starts name on p: in node_modules, from
// projectRoot up to workspaceRoot, then on PATH.
func resolveBinary(name, projectRoot, workspaceRoot string, p platform) string {
	if path := resolveInNodeModulesFor(name, projectRoot, workspaceRoot, p); path != "" {
		return path
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return ""
}
