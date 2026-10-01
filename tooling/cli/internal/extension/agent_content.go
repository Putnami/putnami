package extension

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// DeclaredExtension is one extension declaration of the workspace resolved to
// the name the extension answers to: an `extensions` entry of the workspace
// config, or a devDependency of the root package.json.
type DeclaredExtension struct {
	// Ref is the entry exactly as the workspace declares it: the `extensions`
	// entry, or the devDependencies key.
	Ref string
	// Name is the extension's resolved name.
	Name string
	// Local reports a development source declared by path: an absolute path
	// or a workspace-relative `/` or `./` reference. A local extension is read
	// from its directory and never pinned.
	Local bool
	// Package reports an extension the root package.json declares in
	// devDependencies. The package manager installs it into
	// node_modules/<name>, discovery loads its commands and tools from there,
	// and the package manager's lock pins it.
	Package bool
	// Dir is the extension's root directory: the declared directory of a local
	// extension, node_modules/<name> of a package. It is empty for a registry
	// extension, whose installed tree is located through its lock pin.
	Dir string
}

// FindDeclaredExtension resolves name to the one declaration of the workspace
// that provides it, in discovery's order. An `extensions` entry comes first: a
// registry entry answers to its own key, and a local entry to the name its
// manifest resolves to, exactly as discovery names it. A root package.json
// devDependency answers to its key when no `extensions` entry provides the
// name, because discovery reads devDependencies last.
//
// An extension the workspace does not declare, or declares twice, is an error:
// agent content is only ever taken from an extension the workspace chose, and
// from exactly one source. A registry entry and a devDependency of one name
// are two sources — the lock pins one release, the package manager installs
// another — so the pair is refused. A local entry shadows a devDependency in
// discovery, so it is the one source.
func FindDeclaredExtension(wsRoot string, cfg *wsproto.Config, name string) (DeclaredExtension, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return DeclaredExtension{}, fmt.Errorf("no extension name given")
	}
	if cfg == nil {
		return DeclaredExtension{}, undeclaredExtension(name)
	}
	var matches []DeclaredExtension
	for _, ref := range cfg.Extensions.Names() {
		if !isLocalExtensionReference(ref) {
			if ref == name {
				matches = append(matches, DeclaredExtension{Ref: ref, Name: ref})
			}
			continue
		}
		dir := localExtensionReferenceDir(wsRoot, ref)
		if dir == "" {
			continue
		}
		ext, skip := tryLoadExtension(dir, ref, false)
		switch {
		case ext != nil && ext.Name == name:
			matches = append(matches, DeclaredExtension{Ref: ref, Name: name, Local: true, Dir: dir})
		case ext == nil && skip != nil && skip.Name == name:
			return DeclaredExtension{}, fmt.Errorf("extension %s (declared as %s) cannot be loaded: %w", name, ref, skip.Reason)
		}
	}
	switch len(matches) {
	case 0:
		if declaresPackageExtension(wsRoot, name) {
			return DeclaredExtension{Ref: name, Name: name, Package: true, Dir: installedPackageDir(wsRoot, name)}, nil
		}
		return DeclaredExtension{}, undeclaredExtension(name)
	case 1:
		if !matches[0].Local && declaresPackageExtension(wsRoot, name) {
			return DeclaredExtension{}, fmt.Errorf(
				"extension %s is declared twice: in %s extensions, where %s pins its release, and as a development dependency of the root package manifest, where the package manager installs its own. "+
					"The two can be different releases, so its commands and its agent content could disagree; declare it once",
				name, wsproto.WorkspaceConfigFilename, lockfile.LockFilename)
		}
		return matches[0], nil
	default:
		refs := make([]string, 0, len(matches))
		for _, match := range matches {
			refs = append(refs, match.Ref)
		}
		return DeclaredExtension{}, fmt.Errorf("extension %s is declared more than once in %s (%s); declare it once",
			name, wsproto.WorkspaceConfigFilename, strings.Join(refs, ", "))
	}
}

// DeclaredExtensionNames returns the names of the extensions the workspace
// declares, in sorted order: a registry entry answers to its own key, a local
// entry to the name its manifest resolves to, and a development dependency of
// the root package manifest to its key once the package manager installed an
// extension manifest under it. A local entry that cannot be loaded is left out.
func DeclaredExtensionNames(wsRoot string, cfg *wsproto.Config) []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, ref := range cfg.Extensions.Names() {
		if !isLocalExtensionReference(ref) {
			seen[ref] = true
			continue
		}
		dir := localExtensionReferenceDir(wsRoot, ref)
		if dir == "" {
			continue
		}
		if ext, _ := tryLoadExtension(dir, ref, false); ext != nil && ext.Name != "" {
			seen[ext.Name] = true
		}
	}
	for name := range rootPackageDeclarations(wsRoot) {
		if seen[name] || !validPackageName(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(installedPackageDir(wsRoot, name), ManifestFilename)); err == nil {
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

func undeclaredExtension(name string) error {
	return fmt.Errorf("%s declares no extension %s; add it to extensions before opting into its agent content",
		wsproto.WorkspaceConfigFilename, name)
}

// packageNameFormat is the package-name grammar a declared dependency key must
// follow before it becomes a path under the package manager's install
// directory: an optional @scope/ and one lowercase name, never a path
// traversal.
var packageNameFormat = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)

func validPackageName(name string) bool {
	return packageNameFormat.MatchString(name) && !strings.Contains(name, "..")
}

// declaresPackageExtension reports whether the root package manifest declares
// name among the development dependencies discovery reads extensions from
// (rootPackageDeclarations). The declaration is read from the committed
// manifest, so it holds before the package manager has installed anything.
func declaresPackageExtension(wsRoot, name string) bool {
	if !validPackageName(name) {
		return false
	}
	_, declared := rootPackageDeclarations(wsRoot)[name]
	return declared
}

// workspaceDisplayPath renders target relative to the workspace root, with
// forward slashes, for a message.
func workspaceDisplayPath(wsRoot, target string) string {
	if rel, err := filepath.Rel(wsRoot, target); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(target)
}

// PackageExtensionInstalled reports whether the package manager has installed
// the extension manifest of a package declaration.
func PackageExtensionInstalled(declared DeclaredExtension) bool {
	if !declared.Package || declared.Dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(declared.Dir, ManifestFilename))
	return err == nil
}

// isLocalExtensionReference reports whether an `extensions` entry names a
// directory rather than a registry artifact, by the rule the extension
// commands apply.
func isLocalExtensionReference(ref string) bool {
	return strings.HasPrefix(ref, "/") ||
		strings.HasPrefix(ref, "./") ||
		strings.HasPrefix(ref, "../") ||
		filepath.IsAbs(filepath.FromSlash(ref))
}

// localExtensionReferenceDir resolves a local entry in discovery's order: an
// absolute path that holds a manifest is itself, and anything else is relative
// to the workspace root. It returns "" for an entry discovery never loads as a
// local directory (one that escapes the workspace).
func localExtensionReferenceDir(wsRoot, ref string) string {
	native := filepath.FromSlash(ref)
	if filepath.IsAbs(native) {
		if _, err := os.Stat(filepath.Join(native, ManifestFilename)); err == nil {
			return filepath.Clean(native)
		}
	}
	if rel, ok := normalizeWorkspaceExtensionPath(ref); ok {
		return filepath.Join(wsRoot, rel)
	}
	return ""
}

// AgentContentSource is one declared extension's agent-content contribution,
// located and bound to the extension version the workspace resolved.
type AgentContentSource struct {
	// Name is the extension's resolved name, which is also the content's
	// identity.
	Name string
	// Version is the resolved extension version: its lock pin for an
	// installed extension, the manifest's own version for a local one (which
	// may be empty — a local source is identified by its content).
	Version string
	// Root is the extension's root directory.
	Root string
	// Local reports a development source declared by path.
	Local bool
	// Package reports an extension the package manager installed into
	// node_modules from a root package.json devDependency.
	Package bool
	// ManifestHash is the SHA-256 of the extension manifest bytes. For an
	// installed extension it is verified equal to the manifest digest its lock
	// pin records, which is what binds the contribution to that pin. For a
	// package it is the digest of the manifest the package manager installed.
	ManifestHash string
	// Contribution is the validated agentContent section.
	Contribution extproto.AgentContentContribution
}

// LocateAgentContent finds the agent-content contribution of the declared
// extension name and binds it to the extension the workspace resolved. The
// content is read from the directory discovery loads the extension's commands
// and tools from, so both come from one release.
//
// An installed registry extension is read only through its lock pin: the lock
// must pin the extension with a manifest digest, the manifest under the
// workspace's stable link must hash to exactly that digest, and it must carry
// the packaged contribution form. The chain lock → extension manifest →
// agentContent.manifestSha256 → content manifest → file digests then binds
// every byte the materializer may write to the one release the lock pins. A
// registry extension that node_modules also provides is refused: discovery
// loads its commands from node_modules first, so they need not be the pinned
// release.
//
// A package extension is read from node_modules/<name>, the package the
// package manager installed: its package.json must carry the extension's name
// and the version its manifest declares, and the manifest must carry the
// packaged contribution form. The chain package → extension manifest →
// agentContent.manifestSha256 → content manifest → file digests binds every
// byte to that installed package, which the package manager's lock pins.
//
// A local extension is read from its declared directory and may be built from
// its authored source.
//
// Nothing here touches the workspace's agent files, fetches, or writes: a
// missing install is an error naming the command that performs it.
func LocateAgentContent(wsRoot string, cfg *wsproto.Config, name string) (*AgentContentSource, error) {
	declared, err := FindDeclaredExtension(wsRoot, cfg, name)
	if err != nil {
		return nil, err
	}
	switch {
	case declared.Local:
		return locateLocalAgentContent(declared)
	case declared.Package:
		return locatePackageAgentContent(wsRoot, declared)
	}
	if err := refuseShadowingPackage(wsRoot, declared.Name); err != nil {
		return nil, err
	}
	return locateInstalledAgentContent(wsRoot, declared.Name)
}

// refuseShadowingPackage refuses a registry extension the package manager also
// installed. For an `extensions` entry, discovery probes the package manager's
// install directory before the pinned install (installedPackageDir), so a
// loadable package there serves the extension's commands, and content bound to
// the lock pin could belong to another release.
func refuseShadowingPackage(wsRoot, name string) error {
	if !validPackageName(name) {
		return nil
	}
	dir := installedPackageDir(wsRoot, name)
	if ext, _ := tryLoadExtension(dir, name, false); ext == nil {
		return nil
	}
	return fmt.Errorf(
		"extension %s is pinned by %s, but %s also provides it, and its commands load from there first, so they need not be the pinned release. "+
			"Remove the package, or declare the extension only as a development dependency of the root package manifest",
		name, lockfile.LockFilename, workspaceDisplayPath(wsRoot, dir))
}

// locatePackageAgentContent reads the contribution of an extension the package
// manager installed. The package's own manifest names the release: it must be
// the package declared under this name, at the version the extension manifest
// carries, which is the version the package's packager stamps into it.
func locatePackageAgentContent(wsRoot string, declared DeclaredExtension) (*AgentContentSource, error) {
	name := declared.Name
	display := workspaceDisplayPath(wsRoot, filepath.Join(declared.Dir, ManifestFilename))
	data, err := readRegularManifest(filepath.Join(declared.Dir, ManifestFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the root package manifest declares extension %s as a development dependency, but the package manager has not installed it (%s): run `putnami install`",
			name, display)
	}
	if err != nil {
		return nil, fmt.Errorf("extension %s (%s): %w", name, display, err)
	}
	pkgName, pkgVersion := readPackageNameVersion(declared.Dir)
	if pkgName != name {
		return nil, fmt.Errorf("%s holds package %q, not %s: an extension's agent content is read only from the package installed under its own name",
			workspaceDisplayPath(wsRoot, declared.Dir), pkgName, name)
	}
	m, err := extproto.NegotiateManifest(display, data)
	if err != nil {
		return nil, err
	}
	if manifestName := strings.TrimSpace(m.Name); manifestName != "" && manifestName != name {
		return nil, fmt.Errorf("package %s ships the manifest of extension %s: an extension package is published under the extension's own name", name, manifestName)
	}
	version := strings.TrimSpace(m.Version)
	if version == "" || version != strings.TrimSpace(pkgVersion) {
		return nil, fmt.Errorf("installed extension %s declares version %q, but its package is version %q: a packaged extension carries its package version in its manifest",
			name, version, pkgVersion)
	}
	contribution, err := validatedContribution(name, m)
	if err != nil {
		return nil, err
	}
	if !contribution.Packaged() {
		return nil, fmt.Errorf("installed extension %s@%s ships its agent content as source only; a published package carries the built tree bound by agentContent.manifestSha256. "+
			"To build it from source, declare its directory in %s extensions",
			name, version, wsproto.WorkspaceConfigFilename)
	}
	return &AgentContentSource{
		Name:         name,
		Version:      version,
		Root:         declared.Dir,
		Package:      true,
		ManifestHash: lockfile.HashBytes(data),
		Contribution: contribution,
	}, nil
}

func locateLocalAgentContent(declared DeclaredExtension) (*AgentContentSource, error) {
	manifestPath := filepath.Join(declared.Dir, ManifestFilename)
	data, err := readRegularManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("extension %s (declared as %s): %w", declared.Name, declared.Ref, err)
	}
	m, err := extproto.NegotiateManifest(manifestPath, data)
	if err != nil {
		return nil, err
	}
	contribution, err := validatedContribution(declared.Name, m)
	if err != nil {
		return nil, err
	}
	return &AgentContentSource{
		Name:         declared.Name,
		Version:      strings.TrimSpace(m.Version),
		Root:         declared.Dir,
		Local:        true,
		ManifestHash: lockfile.HashBytes(data),
		Contribution: contribution,
	}, nil
}

func locateInstalledAgentContent(wsRoot, name string) (*AgentContentSource, error) {
	lock, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", lockfile.LockFilename, err)
	}
	if lock == nil {
		return nil, fmt.Errorf("%s pins no extension %s: run `putnami install` to install it", lockfile.LockFilename, name)
	}
	entry, ok := lock.GetExtension(name)
	if !ok {
		return nil, fmt.Errorf("%s pins no extension %s: run `putnami install` to install it", lockfile.LockFilename, name)
	}
	if !isBareSHA256(entry.ManifestHash) {
		return nil, fmt.Errorf("%s pins extension %s@%s without a manifest digest, so its agent content cannot be verified: run `putnami extensions update %s`",
			lockfile.LockFilename, name, entry.Version, name)
	}
	root, display, data, err := readInstalledManifest(wsRoot, name, entry.Version)
	if err != nil {
		return nil, fmt.Errorf("extension %s@%s is pinned but not installed (%s): run `putnami install`", name, entry.Version, display)
	}
	if got := lockfile.HashBytes(data); got != entry.ManifestHash {
		return nil, fmt.Errorf("installed extension %s does not match its pin: %s binds manifest %s, %s hashes to %s; run `putnami install` to restore the pinned release",
			name, lockfile.LockFilename, entry.ManifestHash, display, got)
	}
	m, err := extproto.NegotiateManifest(display, data)
	if err != nil {
		return nil, err
	}
	if m.Version != "" && m.Version != entry.Version {
		return nil, fmt.Errorf("installed extension %s declares version %s, but %s pins %s", name, m.Version, lockfile.LockFilename, entry.Version)
	}
	contribution, err := validatedContribution(name, m)
	if err != nil {
		return nil, err
	}
	if !contribution.Packaged() {
		return nil, fmt.Errorf("installed extension %s@%s ships its agent content as source only; a published extension carries the built tree bound by agentContent.manifestSha256, so it must be re-packaged",
			name, entry.Version)
	}
	return &AgentContentSource{
		Name:         name,
		Version:      entry.Version,
		Root:         root,
		ManifestHash: entry.ManifestHash,
		Contribution: contribution,
	}, nil
}

// readInstalledManifest reads a pinned extension's manifest from where
// discovery loads the extension (tryLoadInstalledExtension): the stable link,
// or the exact release's artifact directory when no manifest is behind the
// link. It returns the root the manifest came from and its workspace-relative
// display path; the caller binds the bytes to the pin. A manifest behind the
// link is never skipped for the artifact directory, because discovery loads
// that one.
func readInstalledManifest(wsRoot, name, version string) (root, display string, data []byte, err error) {
	root = layout.StableDir(wsRoot, layout.Extensions, name)
	display = filepath.ToSlash(filepath.Join(".putnami", "bin", string(layout.Extensions), layout.EncodeName(name), ManifestFilename))
	data, err = readRegularManifest(filepath.Join(root, ManifestFilename))
	if !errors.Is(err, fs.ErrNotExist) {
		return root, display, data, err
	}
	artifact := layout.ArtifactDir(wsRoot, layout.Extensions, name, version)
	artifactData, artifactErr := readRegularManifest(filepath.Join(artifact, ManifestFilename))
	if artifactErr != nil {
		return root, display, nil, err
	}
	relative, relErr := filepath.Rel(wsRoot, filepath.Join(artifact, ManifestFilename))
	if relErr != nil {
		relative = filepath.Join(artifact, ManifestFilename)
	}
	return artifact, filepath.ToSlash(relative), artifactData, nil
}

// validatedContribution returns the manifest's agent-content section once the
// protocol's own rules accept it.
func validatedContribution(name string, m *extproto.Manifest) (extproto.AgentContentContribution, error) {
	if !m.DeclaresAgentContent() {
		return extproto.AgentContentContribution{}, fmt.Errorf("extension %s declares no agent content", name)
	}
	if errs := diag.Errors(extproto.ValidateAgentContent(m)); len(errs) > 0 {
		messages := make([]string, 0, len(errs))
		for _, d := range errs {
			messages = append(messages, d.String())
		}
		return extproto.AgentContentContribution{}, fmt.Errorf("extension %s declares an invalid agent-content contribution: %s",
			name, strings.Join(messages, "; "))
	}
	return *m.AgentContent, nil
}

// readRegularManifest reads a manifest only when it is a regular file: the
// bytes are about to be trusted to name what may be written into a workspace,
// so a symlink standing in for them is refused rather than followed.
func readRegularManifest(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.ReadFile(path)
}

// isBareSHA256 reports a 64-character lowercase hex digest.
func isBareSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
