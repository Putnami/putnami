package extension

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// config.
type DeclaredExtension struct {
	// Ref is the entry exactly as the workspace declares it: the `extensions`
	// entry.
	Ref string
	// Name is the extension's resolved name.
	Name string
	// Local reports a development source declared by path: an absolute path
	// or a workspace-relative `/` or `./` reference. A local extension is read
	// from its directory and never pinned.
	Local bool
	// Dir is the extension's root directory: the declared directory of a local
	// extension. It is empty for a registry extension, whose installed tree is
	// located through its lock pin.
	Dir string
	// RequiredBy is the dependent of an extension the workspace does not
	// declare but its graph holds (WalkWorkspaceGraph). Such a dependency is a
	// registry extension, located through its lock pin.
	RequiredBy string
}

// FindDeclaredExtension resolves name to the one declaration of the workspace
// that provides it, in discovery's order. An `extensions` entry comes first: a
// registry entry answers to its own key, and a local entry to the name its
// manifest resolves to, exactly as discovery names it.
//
// A dependency of the workspace's graph answers to its name next: the
// workspace chose it through the extension that requires it (ADR 0066).
//
// An extension the workspace does not declare, or declares twice, is an error:
// agent content is only ever taken from an extension the workspace chose, and
// from exactly one source.
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
		if requiredBy := graphDependent(wsRoot, cfg, name); requiredBy != "" {
			return DeclaredExtension{Ref: name, Name: name, RequiredBy: requiredBy}, nil
		}
		return DeclaredExtension{}, undeclaredExtension(name)
	case 1:
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
// entry to the name its manifest resolves to. A local entry that cannot be
// loaded is left out.
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
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// graphDependent returns the dependent of name when name is a dependency of
// the workspace's graph, and "" otherwise.
func graphDependent(wsRoot string, cfg *wsproto.Config, name string) string {
	if !extproto.IsRegistryName(name) {
		return ""
	}
	lock, _ := lockfile.ReadLockFile(wsRoot)
	dep, ok := WalkWorkspaceGraph(wsRoot, cfg, lock, WalkOptions{}).Dependency(name)
	if !ok || len(dep.RequiredBy) == 0 {
		return ""
	}
	return dep.RequiredBy[0]
}

func undeclaredExtension(name string) error {
	return undeclaredExtensionError{name: name}
}

// undeclaredExtensionError reports an extension the workspace does not declare.
type undeclaredExtensionError struct{ name string }

func (e undeclaredExtensionError) Error() string {
	return fmt.Sprintf("%s declares no extension %s; add it to extensions before opting into its agent content",
		wsproto.WorkspaceConfigFilename, e.name)
}

// IsUndeclaredExtension reports whether err says the workspace declares no
// such extension, as opposed to a declaration that cannot be used.
func IsUndeclaredExtension(err error) bool {
	var undeclared undeclaredExtensionError
	return errors.As(err, &undeclared)
}

// ErrNoAgentContent is wrapped by the error LocateAgentContent returns for an
// extension whose manifest declares no agent content.
var ErrNoAgentContent = errors.New("declares no agent content")

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
	// ManifestHash is the SHA-256 of the extension manifest bytes. For an
	// installed extension it is verified equal to the manifest digest its lock
	// pin records, which is what binds the contribution to that pin.
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
// every byte the materializer may write to the one release the lock pins.
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
	if declared.Local {
		return locateLocalAgentContent(declared)
	}
	return locateInstalledAgentContent(wsRoot, declared.Name)
}

func locateLocalAgentContent(declared DeclaredExtension) (*AgentContentSource, error) {
	verified, err := readLocalManifest(declared)
	if err != nil {
		return nil, err
	}
	contribution, err := validatedContribution(declared.Name, verified.Manifest)
	if err != nil {
		return nil, err
	}
	return &AgentContentSource{
		Name:         declared.Name,
		Version:      verified.Version,
		Root:         verified.Root,
		Local:        true,
		ManifestHash: verified.ManifestHash,
		Contribution: contribution,
	}, nil
}

func locateInstalledAgentContent(wsRoot, name string) (*AgentContentSource, error) {
	verified, err := readPinnedManifest(wsRoot, name, "agent content")
	if err != nil {
		return nil, err
	}
	contribution, err := validatedContribution(name, verified.Manifest)
	if err != nil {
		return nil, err
	}
	if !contribution.Packaged() {
		return nil, fmt.Errorf("installed extension %s@%s ships its agent content as source only; a published extension carries the built tree bound by agentContent.manifestSha256, so it must be re-packaged",
			name, verified.Version)
	}
	return &AgentContentSource{
		Name:         name,
		Version:      verified.Version,
		Root:         verified.Root,
		ManifestHash: verified.ManifestHash,
		Contribution: contribution,
	}, nil
}

// verifiedManifest is a declared extension's manifest, read from where
// discovery loads the extension and negotiated by the protocol's loader.
type verifiedManifest struct {
	Manifest *extproto.Manifest
	// Version is the lock pin of an installed extension, or the manifest's own
	// version for a local one.
	Version string
	// Root is the directory the manifest came from.
	Root string
	// ManifestHash is the SHA-256 of the manifest bytes. For an installed
	// extension it equals the digest its lock pin records.
	ManifestHash string
}

// readLocalManifest reads a local extension's manifest from its declared
// directory.
func readLocalManifest(declared DeclaredExtension) (*verifiedManifest, error) {
	manifestPath := filepath.Join(declared.Dir, ManifestFilename)
	data, err := readRegularManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("extension %s (declared as %s): %w", declared.Name, declared.Ref, err)
	}
	m, err := extproto.NegotiateManifest(manifestPath, data)
	if err != nil {
		return nil, err
	}
	return &verifiedManifest{Manifest: m, Version: strings.TrimSpace(m.Version), Root: declared.Dir, ManifestHash: lockfile.HashBytes(data)}, nil
}

// readPinnedManifest reads an installed extension's manifest only through its
// lock pin: the lock must pin the extension with a manifest digest, and the
// manifest discovery loads must hash to exactly that digest and declare the
// pinned version. purpose names what the caller verifies, for the error that
// says the pin has no digest.
func readPinnedManifest(wsRoot, name, purpose string) (*verifiedManifest, error) {
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
		return nil, fmt.Errorf("%s pins extension %s@%s without a manifest digest, so its %s cannot be verified: run `putnami extensions update %s`",
			lockfile.LockFilename, name, entry.Version, purpose, name)
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
	return &verifiedManifest{Manifest: m, Version: entry.Version, Root: root, ManifestHash: entry.ManifestHash}, nil
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
		return extproto.AgentContentContribution{}, fmt.Errorf("extension %s %w", name, ErrNoAgentContent)
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
