package extension

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	registryproto "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

// errNotFromTheStore is the reason a hosted run skips an extension that is
// neither installed from the artifact store nor one of the workspace's own path
// extensions.
var errNotFromTheStore = errors.New("a hosted run (--credential-fd) runs only extensions installed from the artifact store " +
	"and the workspace's own path extensions; pin this extension from the registry in the workspace lock, " +
	"or declare it by its path inside the workspace, such as \"/tools/my-extension\"")

// errInstalledPackage is the reason a hosted run skips an extension whose
// manifest directory lies inside a directory that a package manager installs
// into (installedPackagesDirName), whatever path declared it.
var errInstalledPackage = fmt.Errorf("a hosted run (--credential-fd) runs no extension from a %s directory; "+
	"pin this extension from the registry in the workspace lock", installedPackagesDirName)

// errPinnedBuildMissing is the reason a hosted run skips a workspace project
// whose manifest name a workspace config key pins (pinnedByName) when the
// pinned build did not load: the run never runs the project in its place.
var errPinnedBuildMissing = errors.New("a hosted run (--credential-fd) runs the build the workspace lock pins, " +
	"never the workspace project of the same name; pin this extension from the registry in the workspace lock and install it")

// errPathExtensionProvider is the reason a hosted run removes a provider
// capability from a path extension.
var errPathExtensionProvider = errors.New("a hosted run takes every provider from an extension installed from the artifact store")

// hostedProviderCommands are the reserved commands through which an extension
// serves a provider capability: the CLI starts the provider for the whole run
// and hands it the run credential, a credential derived from it, or the run's
// evidence. registryproto.CredentialProviderCommand also serves the publish
// purpose and publication-v1, and distribution.ProviderCommandName serves the
// release sets a publish or a deploy resolves.
var hostedProviderCommands = []string{
	registryproto.CredentialProviderCommand,
	cache.ProviderCommandName,
	runner.ProviderCommandName,
	protocolcli.SessionReporterCommand,
	protocolcli.LogReporterCommand,
	distribution.ProviderCommandName,
}

// InArtifactStore reports whether ext is installed from the artifact store:
// it is not a local source, and its manifest directory resolves inside the
// artifact store of workspaceRoot and outside workspaceRoot. The store holds
// only artifacts the CLI downloaded and verified against the lock, so such an
// extension is registry code, never repository code.
func InArtifactStore(workspaceRoot string, ext *ExtensionDescription) bool {
	path, ok := storeInstalledPath(store.ResolveArtifactStoreRoot(workspaceRoot), ext)
	if !ok {
		return false
	}
	if root, err := filepath.EvalSymlinks(workspaceRoot); err == nil && within(root, path) {
		return false
	}
	return true
}

// InStoreRoot reports whether ext is installed in the artifact store at
// storeRoot: it is not a local source, and its manifest directory resolves
// inside storeRoot. Unlike InArtifactStore, it knows no workspace, so it does
// not check that the store lies outside one; a hosted run's discovery already
// did (keepHostedExtensions).
func InStoreRoot(storeRoot string, ext *ExtensionDescription) bool {
	_, ok := storeInstalledPath(storeRoot, ext)
	return ok
}

// storeInstalledPath is the resolved manifest directory of ext when ext is
// not a local source and that directory lies inside storeRoot.
func storeInstalledPath(storeRoot string, ext *ExtensionDescription) (string, bool) {
	if ext == nil || ext.LocalSource || ext.Path == "" {
		return "", false
	}
	path, err := filepath.EvalSymlinks(ext.Path)
	if err != nil {
		return "", false
	}
	resolvedStore, err := filepath.EvalSymlinks(storeRoot)
	if err != nil || !within(resolvedStore, path) {
		return "", false
	}
	return path, true
}

// within reports whether path is root or lies under it. Both are resolved.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

// WorkspacePathExtension reports whether ext is one of the workspace's own path
// extensions (pathExtensionRefusal).
func WorkspacePathExtension(workspaceRoot string, ext *ExtensionDescription) bool {
	return pathExtensionRefusal(workspaceRoot, ext) == nil
}

// pathExtensionRefusal returns nil when ext is one of the workspace's own path
// extensions: a local source the workspace declares by a path inside it, as a
// workspace project or a path-shaped `extensions` entry (RelPath), whose
// manifest directory resolves inside workspaceRoot and inside no directory
// that a package manager installs into (inInstalledPackages). Otherwise it
// returns the reason a hosted run skips ext: errInstalledPackage for such a
// directory, and errNotFromTheStore for any other extension, one loaded from
// an absolute path included.
func pathExtensionRefusal(workspaceRoot string, ext *ExtensionDescription) error {
	if ext == nil || !ext.LocalSource || ext.RelPath == "" || ext.Path == "" {
		return errNotFromTheStore
	}
	root, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return errNotFromTheStore
	}
	path, err := filepath.EvalSymlinks(ext.Path)
	if err != nil || !within(root, path) {
		return errNotFromTheStore
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return errNotFromTheStore
	}
	if inInstalledPackages(root, rel) {
		return errInstalledPackage
	}
	return nil
}

// inInstalledPackages reports whether rel, a resolved path relative to the
// resolved workspace root, lies inside a directory that a package manager
// installs into: a component of rel named installedPackagesDirName, in any
// letter case, or a directory that is the same file as the one discovery's
// package-manager sources load from (installedPackageDir), which a link
// inside the workspace can make the same.
func inInstalledPackages(root, rel string) bool {
	packages, packagesErr := os.Stat(installedPackageDir(root, ""))
	dir := root
	for _, component := range strings.Split(rel, string(os.PathSeparator)) {
		if strings.EqualFold(component, installedPackagesDirName) {
			return true
		}
		dir = filepath.Join(dir, component)
		if packagesErr != nil {
			continue
		}
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, packages) {
			return true
		}
	}
	return false
}

// declaredByPath reports whether ref, a workspace config `extensions` key or
// an `extensions install` argument, is path-shaped: it starts with "/", the
// repo-local shorthand ("/tools/x"), or with "./" ("./tools/x"). A hosted run
// reads any other key as an extension name, never as a workspace path.
func declaredByPath(ref string) bool {
	return strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./")
}

// LoadWorkspacePathExtension loads the extension that ref, a path-shaped
// `extensions` entry or `extensions install` argument (declaredByPath), names
// inside workspaceRoot: "/tools/x" or "./tools/x". It returns the extension
// when it is one of the workspace's own path extensions
// (WorkspacePathExtension), and nil otherwise, for an absolute path or a path
// outside the workspace included.
func LoadWorkspacePathExtension(workspaceRoot, ref string) *ExtensionDescription {
	if !declaredByPath(ref) {
		return nil
	}
	relPath, ok := normalizeWorkspaceExtensionPath(ref)
	if !ok {
		return nil
	}
	ext, _ := tryLoadExtension(filepath.Join(workspaceRoot, relPath), ref, false)
	if ext == nil {
		return nil
	}
	ext.RelPath, ext.LocalSource = relPath, true
	if !WorkspacePathExtension(workspaceRoot, ext) {
		return nil
	}
	return ext
}

// RemovedCapability is a provider capability that a hosted run removed from
// one of the workspace's path extensions (withoutProviderCapabilities). The
// extension itself loaded and runs.
type RemovedCapability struct {
	// Extension is the name of the path extension.
	Extension string
	// Path is its manifest directory.
	Path string
	// Command is the reserved command the extension declares
	// (hostedProviderCommands).
	Command string
}

// Reason says that the path extension does not serve the capability, and why.
func (r RemovedCapability) Reason() error {
	return fmt.Errorf("%s: path extension %s does not serve the %s capability: %w",
		runcredential.Flag, r.Extension, r.Command, errPathExtensionProvider)
}

// ProviderCause explains why no extension of d serves command, for the
// message of a run that needs that provider: each path extension a hosted run
// removed command from (RemovedCapabilities), then the extensions discovery
// could not load (SkippedProviderCause). It returns "" when neither applies,
// and for a nil d.
func (d *DiscoveryResult) ProviderCause(command string) string {
	if d == nil {
		return ""
	}
	var causes []string
	for _, removed := range d.RemovedCapabilities {
		if removed.Command == command {
			causes = append(causes, removed.Reason().Error())
		}
	}
	slices.Sort(causes)
	if cause := SkippedProviderCause(d.Skipped); cause != "" {
		causes = append(causes, cause)
	}
	return strings.Join(causes, "; ")
}

// keepHostedExtensions returns the extensions a hosted run may run, a skip
// record for each extension it drops, and each provider capability it removes
// from an extension it keeps. It keeps an extension installed from the
// artifact store (InArtifactStore) whole. It keeps one of the workspace's own
// path extensions (WorkspacePathExtension) without its provider capabilities
// (withoutProviderCapabilities): a hosted run starts its runtime only after
// custody ended, and every handoff of the run credential happens before. It
// skips every other extension, with the reason pathExtensionRefusal gives. A
// run that holds no run credential keeps every extension.
func keepHostedExtensions(workspaceRoot string, extensions []*ExtensionDescription) ([]*ExtensionDescription, []SkippedExtension, []RemovedCapability) {
	if !runcredential.Hosted() {
		return extensions, nil, nil
	}
	kept := extensions[:0:0]
	var skipped []SkippedExtension
	var removed []RemovedCapability
	for _, ext := range extensions {
		if InArtifactStore(workspaceRoot, ext) {
			kept = append(kept, ext)
			continue
		}
		if refusal := pathExtensionRefusal(workspaceRoot, ext); refusal != nil {
			skipped = append(skipped, SkippedExtension{
				Ref: ext.Name, Name: ext.Name, Path: ext.Path, Version: ext.Version,
				Reason: refusal,
			})
			continue
		}
		removed = append(removed, withoutProviderCapabilities(ext)...)
		kept = append(kept, ext)
	}
	return kept, skipped, removed
}

// withoutProviderCapabilities removes from ext every provider capability it
// declares (hostedProviderCommands): the command, its visibility and its job.
// It returns each capability it removed. Every other command and job of ext
// stays.
func withoutProviderCapabilities(ext *ExtensionDescription) []RemovedCapability {
	var removed []RemovedCapability
	for _, command := range hostedProviderCommands {
		_, declared := ext.Commands[command]
		_, hasJob := ext.Jobs[command]
		if !declared && !hasJob {
			continue
		}
		delete(ext.Commands, command)
		delete(ext.CommandVisibility, command)
		delete(ext.Jobs, command)
		removed = append(removed, RemovedCapability{Extension: ext.Name, Path: ext.Path, Command: command})
	}
	return removed
}
