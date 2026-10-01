package extension

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

// errNotFromTheStore is the reason a hosted run skips an extension that is not
// installed from the artifact store.
var errNotFromTheStore = errors.New("a hosted run (--credential-fd) runs only extensions installed from the artifact store; " +
	"pin this extension from the registry in the workspace lock")

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
// did (keepStoreInstalled).
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

// keepStoreInstalled returns the extensions a hosted run may run, those
// InArtifactStore accepts, and a skip record for each other one. A run that
// holds no run credential keeps every extension.
func keepStoreInstalled(workspaceRoot string, extensions []*ExtensionDescription) ([]*ExtensionDescription, []SkippedExtension) {
	if !runcredential.Hosted() {
		return extensions, nil
	}
	kept := extensions[:0:0]
	var skipped []SkippedExtension
	for _, ext := range extensions {
		if InArtifactStore(workspaceRoot, ext) {
			kept = append(kept, ext)
			continue
		}
		skipped = append(skipped, SkippedExtension{
			Ref: ext.Name, Name: ext.Name, Path: ext.Path, Version: ext.Version,
			Reason: errNotFromTheStore,
		})
	}
	return kept, skipped
}
