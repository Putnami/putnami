package extension

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"or declare it by its path inside the workspace")

// errPathExtensionProvider is the reason a hosted run skips a provider
// capability of a path extension.
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
// extensions: a local source the workspace declares by a path inside it, as a
// workspace project or a workspace-relative `extensions` entry (RelPath), whose
// manifest directory resolves inside workspaceRoot and outside the directory
// that discovery's package-manager sources load from (installedPackageDir). An
// extension loaded from an absolute path is not one.
func WorkspacePathExtension(workspaceRoot string, ext *ExtensionDescription) bool {
	if ext == nil || !ext.LocalSource || ext.RelPath == "" || ext.Path == "" {
		return false
	}
	root, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return false
	}
	path, err := filepath.EvalSymlinks(ext.Path)
	if err != nil || !within(root, path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return !inInstalledPackages(root, rel)
}

// inInstalledPackages reports whether rel, a resolved path relative to the
// resolved workspace root, lies inside the directory that discovery's
// package-manager sources load from (installedPackageDir). It compares the
// top directory of rel with that directory as files, so a different spelling
// on a case-insensitive file system, or a link inside the workspace to that
// directory, still matches. A workspace without that directory has nothing
// inside it.
func inInstalledPackages(root, rel string) bool {
	packages, err := os.Stat(installedPackageDir(root, ""))
	if err != nil {
		return false
	}
	top, _, _ := strings.Cut(rel, string(os.PathSeparator))
	info, err := os.Stat(filepath.Join(root, top))
	return err == nil && os.SameFile(info, packages)
}

// LoadWorkspacePathExtension loads the extension that ref, an `extensions`
// entry or an `extensions install` argument, names as a path inside
// workspaceRoot: "/tools/x", "./tools/x" or "tools/x". It returns the
// extension when it is one of the workspace's own path extensions
// (WorkspacePathExtension), and nil otherwise, for an absolute path or a path
// outside the workspace included.
func LoadWorkspacePathExtension(workspaceRoot, ref string) *ExtensionDescription {
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

// keepHostedExtensions returns the extensions a hosted run may run, and a skip
// record for each extension or capability it drops. It keeps an extension
// installed from the artifact store (InArtifactStore) whole. It keeps one of the
// workspace's own path extensions (WorkspacePathExtension) without its provider
// capabilities (withoutProviderCapabilities): a hosted run starts its runtime
// only after custody ended, and every handoff of the run credential happens
// before. It skips every other extension. A run that holds no run credential
// keeps every extension.
func keepHostedExtensions(workspaceRoot string, extensions []*ExtensionDescription) ([]*ExtensionDescription, []SkippedExtension) {
	if !runcredential.Hosted() {
		return extensions, nil
	}
	kept := extensions[:0:0]
	var skipped []SkippedExtension
	for _, ext := range extensions {
		switch {
		case InArtifactStore(workspaceRoot, ext):
			kept = append(kept, ext)
		case WorkspacePathExtension(workspaceRoot, ext):
			skipped = append(skipped, withoutProviderCapabilities(ext)...)
			kept = append(kept, ext)
		default:
			skipped = append(skipped, SkippedExtension{
				Ref: ext.Name, Name: ext.Name, Path: ext.Path, Version: ext.Version,
				Reason: errNotFromTheStore,
			})
		}
	}
	return kept, skipped
}

// withoutProviderCapabilities removes from ext every provider capability it
// declares (hostedProviderCommands): the command, its visibility and its job.
// It returns one skip record per capability removed, which names the
// capability and the extension. Every other command and job of ext stays.
func withoutProviderCapabilities(ext *ExtensionDescription) []SkippedExtension {
	var skipped []SkippedExtension
	for _, command := range hostedProviderCommands {
		_, declared := ext.Commands[command]
		_, hasJob := ext.Jobs[command]
		if !declared && !hasJob {
			continue
		}
		delete(ext.Commands, command)
		delete(ext.CommandVisibility, command)
		delete(ext.Jobs, command)
		skipped = append(skipped, SkippedExtension{
			Ref: ext.Name, Name: ext.Name, Path: ext.Path, Version: ext.Version,
			Reason: fmt.Errorf("%s: path extension %s does not serve the %s capability: %w",
				runcredential.Flag, ext.Name, command, errPathExtensionProvider),
		})
	}
	return skipped
}
