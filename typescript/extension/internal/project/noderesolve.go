package project

import (
	"path/filepath"
	"sort"
	"strings"
)

// ResolvePackageDir finds the installed directory of the package name the way
// Node resolves a bare import from a module in fromDir: fromDir/node_modules/
// <name>, then the same under each ancestor directory. It returns the real
// path of the first directory that holds a package.json, and false when none
// does.
func ResolvePackageDir(fromDir, name string) (string, bool) {
	dir := filepath.Clean(fromDir)
	for {
		if filepath.Base(dir) != "node_modules" {
			candidate := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
			if FileExists(filepath.Join(candidate, "package.json")) {
				if real, err := filepath.EvalSymlinks(candidate); err == nil {
					return real, true
				}
				return candidate, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// InstalledFromRegistry reports whether a package directory, a real path,
// lies inside a node_modules directory: the package manager installed it, and
// it is no workspace project linked into node_modules.
func InstalledFromRegistry(packageDir string) bool {
	for _, part := range strings.Split(filepath.ToSlash(packageDir), "/") {
		if part == "node_modules" {
			return true
		}
	}
	return false
}

// RuntimeDependencyNames returns the sorted names of the dependencies,
// optionalDependencies and peerDependencies of pkg: the packages its code may
// import at run time.
func RuntimeDependencyNames(pkg *PackageJSON) []string {
	if pkg == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.OptionalDependencies, pkg.PeerDependencies} {
		for name := range deps {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NodeExternals returns the packages a Node bundle of the project leaves as
// imports: each runtime dependency of its package.json, and of every workspace
// project it reaches through a dependency linked into node_modules, except
// those links themselves, which the bundle includes. A dependency that is not
// installed stays an import, so the bundle never inlines a package the
// install did not resolve.
func NodeExternals(projectPath string) []string {
	external := map[string]bool{}
	for _, dir := range WorkspaceLinkDirs(projectPath) {
		pkg := ReadPackageJSONSafe(filepath.Join(dir, "package.json"))
		for _, name := range RuntimeDependencyNames(pkg) {
			if linked, ok := ResolvePackageDir(dir, name); ok && !InstalledFromRegistry(linked) {
				continue
			}
			external[name] = true
		}
	}
	names := make([]string, 0, len(external))
	for name := range external {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// WorkspaceLinkDirs returns projectPath, then the real directory of each
// workspace project it reaches through runtime dependencies linked into
// node_modules, transitively, in the order it finds them. These are the
// packages a Node bundle includes.
func WorkspaceLinkDirs(projectPath string) []string {
	dirs := []string{projectPath}
	seen := map[string]bool{projectPath: true}
	if real, err := filepath.EvalSymlinks(projectPath); err == nil {
		seen[real] = true
	}
	for i := 0; i < len(dirs); i++ {
		pkg := ReadPackageJSONSafe(filepath.Join(dirs[i], "package.json"))
		for _, name := range RuntimeDependencyNames(pkg) {
			linked, ok := ResolvePackageDir(dirs[i], name)
			if !ok || InstalledFromRegistry(linked) || seen[linked] {
				continue
			}
			seen[linked] = true
			dirs = append(dirs, linked)
		}
	}
	return dirs
}
