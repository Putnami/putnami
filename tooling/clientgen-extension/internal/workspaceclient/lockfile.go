package workspaceclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The lockfiles of the package managers @putnami/typescript installs with. The
// cache key of client-generate-ts folds every one of them
// (client-generate-ts.cache.key.workspaceFiles), so the version a lockfile
// pins is part of the key whichever manager wrote it.
const (
	bunTextLock      = "bun.lock"
	bunBinaryLock    = "bun.lockb"
	npmLock          = "package-lock.json"
	npmShrinkwrap    = "npm-shrinkwrap.json"
	pnpmLock         = "pnpm-lock.yaml"
	pnpmWorkspace    = "pnpm-workspace.yaml"
	yarnLock         = "yarn.lock"
	rushConfig       = "rush.json"
	rushConfigFolder = "common/config/rush"
)

// managerLockfiles are the lockfiles each package manager reads, the one it
// honors first when it finds several.
var managerLockfiles = map[string][]string{
	"bun":  {bunTextLock, bunBinaryLock},
	"npm":  {npmShrinkwrap, npmLock},
	"pnpm": {pnpmLock},
	"yarn": {yarnLock},
}

// rootLockfiles are the root files that name the workspace's package manager
// when its root package.json declares none, in the order @putnami/typescript
// looks for them. pnpm-workspace.yaml names pnpm without being a lockfile.
var rootLockfiles = []string{bunTextLock, bunBinaryLock, pnpmLock, pnpmWorkspace, yarnLock, npmLock, npmShrinkwrap}

// rootFileManagers names the package manager each root file of rootLockfiles
// names.
var rootFileManagers = map[string]string{
	bunTextLock:   "bun",
	bunBinaryLock: "bun",
	pnpmLock:      "pnpm",
	pnpmWorkspace: "pnpm",
	yarnLock:      "yarn",
	npmLock:       "npm",
	npmShrinkwrap: "npm",
}

// rushLockfiles are the lockfiles a Rush repository commits, one per package
// manager Rush installs with.
var rushLockfiles = []string{
	rushConfigFolder + "/" + pnpmLock,
	rushConfigFolder + "/" + yarnLock,
	rushConfigFolder + "/" + npmShrinkwrap,
}

// workspaceLockfile returns the workspace-relative path of the lockfile the
// workspace's package manager writes, or "" when it has none yet. The manager
// is the one @putnami/typescript installs with: the packageManager field of the
// root package.json, else Rush for a root rush.json, else the manager the first
// root file of rootLockfiles names. The lockfile is the one that manager honors
// first among those it reads (managerLockfiles). A manager @putnami/typescript
// does not install with is an error.
func workspaceLockfile(workspaceRoot string) (string, error) {
	declared, err := declaredPackageManager(workspaceRoot)
	if err != nil {
		return "", err
	}
	if declared != "" {
		name, _, _ := strings.Cut(declared, "@")
		candidates, known := managerLockfiles[strings.TrimSpace(name)]
		if !known {
			return "", fmt.Errorf("package.json#packageManager declares %q, which @putnami/typescript does not install with: "+
				"the install check cannot read its lockfile, so the cache key would not describe the installed %s; "+
				"declare bun, npm, pnpm or yarn", declared, clientPackage)
		}
		return firstRegularFile(workspaceRoot, candidates), nil
	}
	if isRegularFile(filepath.Join(workspaceRoot, rushConfig)) {
		return firstRegularFile(workspaceRoot, rushLockfiles), nil
	}
	found := firstRegularFile(workspaceRoot, rootLockfiles)
	if found == "" {
		return "", nil
	}
	return firstRegularFile(workspaceRoot, managerLockfiles[rootFileManagers[found]]), nil
}

// declaredPackageManager returns the packageManager field of the root
// package.json, "" when the file or the field is missing.
func declaredPackageManager(workspaceRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "package.json")) //nolint:gosec // G304: the workspace's own manifest
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("read the workspace's package.json: %w", err)
	}
	var document struct {
		PackageManager *string `json:"packageManager"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("parse the workspace's package.json: %w", err)
	}
	if document.PackageManager == nil {
		return "", nil
	}
	return strings.TrimSpace(*document.PackageManager), nil
}

func firstRegularFile(root string, candidates []string) string {
	for _, candidate := range candidates {
		if isRegularFile(filepath.Join(root, filepath.FromSlash(candidate))) {
			return candidate
		}
	}
	return ""
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// lockedClientVersions returns the registry versions of @putnami/client the
// lockfile at path pins, or none when it pins no registry version. The lockfile
// is named by its workspace-relative path; bunKeys are the bun.lock package
// keys the install the emitter belongs to is recorded under, in the order they
// are looked up. A lockfile this reader cannot read is an error.
func lockedClientVersions(lockfile string, data []byte, bunKeys []string) ([]string, error) {
	switch filepath.Base(filepath.FromSlash(lockfile)) {
	case bunTextLock:
		if version := lockedVersion(data, bunKeys); version != "" {
			return []string{version}, nil
		}
		return nil, nil
	case npmLock, npmShrinkwrap:
		versions, err := npmLockedVersions(data)
		if err != nil {
			return nil, fmt.Errorf("parse the workspace's %s: %w", lockfile, err)
		}
		return versions, nil
	case pnpmLock:
		if !hasLineWithPrefix(data, "lockfileVersion:") {
			return nil, fmt.Errorf("parse the workspace's %s: it declares no lockfileVersion", lockfile)
		}
		return pnpmLockedVersions(data), nil
	case yarnLock:
		if !hasLineWithPrefix(data, "# yarn lockfile v1") && !hasLineWithPrefix(data, "__metadata:") {
			return nil, fmt.Errorf("parse the workspace's %s: it is neither a Yarn classic nor a Yarn Berry lockfile", lockfile)
		}
		return yarnLockedVersions(data), nil
	}
	return nil, fmt.Errorf("the workspace locks its packages in %s, which the install check cannot read: the cache key "+
		"would not show which %s the install holds; migrate the workspace once to the text bun.lock, which "+
		"`bun install --save-text-lockfile --lockfile-only` writes from it, then generate again", lockfile, clientPackage)
}

// hasLineWithPrefix reports whether a line of data starts with prefix, at its
// first column.
func hasLineWithPrefix(data []byte, prefix string) bool {
	for line := range strings.Lines(string(data)) {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// npmLockedVersions reads a package-lock.json or npm-shrinkwrap.json: each
// entry of the packages map of lockfile versions 2 and 3 installed at a
// node_modules/@putnami/client path, else each @putnami/client of the nested
// dependencies of version 1. A link is no version.
func npmLockedVersions(data []byte) ([]string, error) {
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Link    bool   `json:"link"`
		} `json:"packages"`
		Dependencies map[string]npmV1Dependency `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, err
	}
	var versions []string
	if lock.Packages != nil {
		for key, entry := range lock.Packages {
			if !entry.Link && (key == "node_modules/"+clientPackage || strings.HasSuffix(key, "/node_modules/"+clientPackage)) {
				versions = appendRegistryVersion(versions, entry.Version)
			}
		}
		return sortedVersions(versions), nil
	}
	var walk func(map[string]npmV1Dependency)
	walk = func(dependencies map[string]npmV1Dependency) {
		for name, dependency := range dependencies {
			if name == clientPackage {
				versions = appendRegistryVersion(versions, dependency.Version)
			}
			walk(dependency.Dependencies)
		}
	}
	walk(lock.Dependencies)
	return sortedVersions(versions), nil
}

type npmV1Dependency struct {
	Version      string                     `json:"version"`
	Dependencies map[string]npmV1Dependency `json:"dependencies"`
}

// pnpmLockedVersions reads the keys of the top-level packages section of a
// pnpm-lock.yaml: '@putnami/client@<version>' from lockfile version 9, with a
// leading slash in versions 6 to 8, and /@putnami/client/<version> before
// them. A peer suffix, in parentheses or after an underscore, is dropped.
func pnpmLockedVersions(data []byte) []string {
	var versions []string
	inPackages := false
	for line := range strings.Lines(string(data)) {
		line = strings.TrimRight(line, " \r\n")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			inPackages = line == "packages:"
			continue
		}
		if !inPackages || strings.HasPrefix(line, "   ") {
			continue
		}
		key := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(line), " {}"), ":")
		key = strings.TrimPrefix(strings.Trim(key, `'"`), "/")
		if cut := strings.IndexByte(key, '('); cut > 0 {
			key = key[:cut]
		}
		version, found := strings.CutPrefix(key, clientPackage+"@")
		if !found {
			if version, found = strings.CutPrefix(key, clientPackage+"/"); found {
				version, _, _ = strings.Cut(version, "_")
			}
		}
		if found {
			versions = appendRegistryVersion(versions, version)
		}
	}
	return sortedVersions(versions)
}

// yarnLockedVersions reads a yarn.lock, classic or Berry: each unindented
// entry lists the descriptors it resolves, and its indented version line the
// version they resolve to. An entry that resolves a workspace, a link, a portal
// or a file is no version.
func yarnLockedVersions(data []byte) []string {
	var versions []string
	matches := false
	for line := range strings.Lines(string(data)) {
		line = strings.TrimRight(line, " \r\n")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			matches = false
			for descriptor := range strings.SplitSeq(strings.TrimSuffix(line, ":"), ",") {
				descriptor = strings.Trim(strings.TrimSpace(descriptor), `"`)
				spec, found := strings.CutPrefix(descriptor, clientPackage+"@")
				if !found {
					continue
				}
				if slices.ContainsFunc([]string{"workspace:", "link:", "portal:", "file:"}, func(protocol string) bool {
					return strings.HasPrefix(spec, protocol)
				}) {
					matches = false
					break
				}
				matches = true
			}
			continue
		}
		if !matches || strings.HasPrefix(line, "    ") {
			continue
		}
		value, found := strings.CutPrefix(trimmed, "version ")
		if !found {
			value, found = strings.CutPrefix(trimmed, "version: ")
		}
		if found {
			versions = appendRegistryVersion(versions, strings.Trim(strings.TrimSpace(value), `"`))
		}
	}
	return sortedVersions(versions)
}

// appendRegistryVersion appends pin when it is a registry version: it starts
// with a digit and holds no colon or slash, and it is no tarball file name.
// Every other pin is a source, not a version: workspace:, link:, a URL, or a
// path to a tarball.
func appendRegistryVersion(versions []string, pin string) []string {
	if !isRegistryVersion(pin) {
		return versions
	}
	return append(versions, pin)
}

func isRegistryVersion(pin string) bool {
	return pin != "" && pin[0] >= '0' && pin[0] <= '9' && !strings.ContainsAny(pin, ":/") && !isTarballName(pin)
}

func sortedVersions(versions []string) []string {
	slices.Sort(versions)
	return slices.Compact(versions)
}

// linkedFromWorkspace reports whether the package directory resolves to a
// directory of the workspace that lies under no node_modules: a workspace
// package the manager links, whose sources the task's emitter input keys, so
// no lockfile version describes it.
func linkedFromWorkspace(workspaceRoot, packageDir string) bool {
	root, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(packageDir)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return !slices.Contains(strings.Split(filepath.ToSlash(relative), "/"), "node_modules")
}

// Lockfiles are the workspace-relative paths of every lockfile the install
// check may read or refuse. The cache key of client-generate-ts folds each of
// them.
func Lockfiles() []string {
	files := slices.Concat(rootLockfiles, rushLockfiles)
	files = slices.DeleteFunc(files, func(file string) bool { return file == pnpmWorkspace })
	slices.Sort(files)
	return files
}
