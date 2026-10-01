// Package layout defines the .putnami/ directory structure and provides
// helpers for resolving artifact paths and managing version symlinks.
//
// Directory layout:
//
//	.putnami/
//	  bin/
//	    putnami                                          # CLI binary
//	    extensions/
//	      putnami-go  → ../artifacts/extensions/putnami-go@1.0.0/
//	      putnami-ts  → ../artifacts/extensions/putnami-ts@1.0.0/
//	    templates/
//	      go-server   → ../artifacts/templates/go-server@1.0.0/
//	    agent-artifacts/
//	      workflows   → ../artifacts/agent-artifacts/workflows@1.0.0/
//	    artifacts/
//	      extensions/
//	        putnami-go@1.0.0/
//	      templates/
//	        go-server@1.0.0/
//	      agent-artifacts/
//	        workflows@1.0.0/
//
// The bin/{extensions,templates,agent-artifacts}/<name> entries are stable symlinks
// (directory junctions on Windows, see dirlink). They point
// either at a local per-worktree artifact dir (a relative link, via
// LinkArtifact) or — for verified installs — at the machine-global,
// content-addressed artifact store under ~/.putnami/artifacts (an absolute link,
// via LinkArtifactGlobal), which every worktree and repo on the machine shares.
// See internal/artifactstore and internal/store (ResolveArtifactStoreRoot).
package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/dirlink"
)

// Kind distinguishes registry artifact classes in the layout.
type Kind string

const (
	Extensions     Kind = "extensions"
	Templates      Kind = "templates"
	AgentArtifacts Kind = "agent-artifacts"
)

// ArtifactDir returns the versioned artifact directory:
// .putnami/bin/artifacts/{kind}/{name}@{version}/
func ArtifactDir(wsRoot string, kind Kind, name, version string) string {
	return filepath.Join(wsRoot, ".putnami", "bin", "artifacts", string(kind), EncodeName(name)+"@"+version)
}

// StableDir returns the stable symlink path (without trailing slash):
// .putnami/bin/{kind}/{name}
func StableDir(wsRoot string, kind Kind, name string) string {
	return filepath.Join(wsRoot, ".putnami", "bin", string(kind), EncodeName(name))
}

// ArtifactsBaseDir returns the base directory for all artifacts of a kind:
// .putnami/bin/artifacts/{kind}/
func ArtifactsBaseDir(wsRoot string, kind Kind) string {
	return filepath.Join(wsRoot, ".putnami", "bin", "artifacts", string(kind))
}

// StableBaseDir returns the base directory for all stable symlinks of a kind:
// .putnami/bin/{kind}/
func StableBaseDir(wsRoot string, kind Kind) string {
	return filepath.Join(wsRoot, ".putnami", "bin", string(kind))
}

// EncodeName converts a package name to a filesystem-safe form.
// Scoped names like "@putnami/go" become "putnami-go".
// Bare names like "simple-ext" are returned as-is.
func EncodeName(name string) string {
	if strings.HasPrefix(name, "@") {
		name = strings.TrimPrefix(name, "@")
		return strings.Replace(name, "/", "-", 1)
	}
	return name
}

// LinkArtifact creates or atomically replaces a symlink at stableDir
// pointing to the given artifactDir. The symlink target is relative.
func LinkArtifact(wsRoot string, kind Kind, name, version string) error {
	stableLink := StableDir(wsRoot, kind, name)
	artifactDir := ArtifactDir(wsRoot, kind, name, version)

	// Ensure the stable symlinks directory exists
	if err := os.MkdirAll(filepath.Dir(stableLink), 0o755); err != nil {
		return fmt.Errorf("create stable dir: %w", err)
	}

	// Compute relative target from the symlink location to the artifact
	relTarget, err := filepath.Rel(filepath.Dir(stableLink), artifactDir)
	if err != nil {
		return fmt.Errorf("compute relative path: %w", err)
	}

	// Atomic swap on Unix: create temp symlink then rename. The temp name is
	// process-unique so two worktrees (separate processes) swapping the same
	// stable link don't delete each other's in-flight temp link mid-rename.
	// Windows swaps a junction under a lock instead (dirlink.Replace).
	tmpLink := fmt.Sprintf("%s.tmp.%d", stableLink, os.Getpid())
	if err := dirlink.Replace(relTarget, stableLink, tmpLink); err != nil {
		return fmt.Errorf("swap symlink: %w", err)
	}
	return nil
}

// LinkArtifactGlobal creates or atomically replaces the stable symlink at
// StableDir(wsRoot, kind, name) pointing to absTargetDir — an ABSOLUTE path,
// typically a digest dir in the machine-global artifact store under $HOME.
//
// Unlike LinkArtifact (whose relative target is valid only within one
// worktree), this target crosses the worktree<->$HOME boundary, so a relative
// "../../.." chain would break on worktree relocation and on symlinked path
// components (e.g. macOS /var -> /private/var). An absolute target is stable;
// the kernel resolves it at access time, so it is NOT pre-resolved here.
func LinkArtifactGlobal(wsRoot string, kind Kind, name, absTargetDir string) error {
	stableLink := StableDir(wsRoot, kind, name)

	if err := os.MkdirAll(filepath.Dir(stableLink), 0o755); err != nil {
		return fmt.Errorf("create stable dir: %w", err)
	}

	// Atomic swap on Unix: create temp symlink then rename. Never rewrite the
	// existing link's inode in place (the macOS code-sign vnode rule for the CLI
	// binary). The temp name is process-unique so concurrent worktrees don't
	// clobber each other's in-flight temp link mid-rename. Windows swaps a
	// junction under a lock instead (dirlink.Replace).
	tmpLink := fmt.Sprintf("%s.tmp.%d", stableLink, os.Getpid())
	if err := dirlink.Replace(absTargetDir, stableLink, tmpLink); err != nil {
		return fmt.Errorf("swap symlink: %w", err)
	}
	return nil
}

// UnlinkArtifact removes only the stable symlink for name, leaving the
// machine-global artifact bytes intact for other worktrees that still reference
// them. This is the removal primitive for the shared store: reclaiming bytes is
// the artifact GC's job, not remove's. A missing link is not an error.
func UnlinkArtifact(wsRoot string, kind Kind, name string) error {
	if err := os.Remove(StableDir(wsRoot, kind, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RemoveArtifact removes the versioned artifact directory and its stable symlink.
// If version is empty, removes all versions matching the name.
func RemoveArtifact(wsRoot string, kind Kind, name, version string) error {
	// Remove symlink
	os.Remove(StableDir(wsRoot, kind, name))

	if version != "" {
		return os.RemoveAll(ArtifactDir(wsRoot, kind, name, version))
	}

	// Remove all versions: scan artifacts dir for name@ prefix
	base := ArtifactsBaseDir(wsRoot, kind)
	prefix := EncodeName(name) + "@"
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
	return nil
}

// ListVersions returns all installed versions for a given name and kind.
func ListVersions(wsRoot string, kind Kind, name string) ([]string, error) {
	base := ArtifactsBaseDir(wsRoot, kind)
	prefix := EncodeName(name) + "@"
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			versions = append(versions, strings.TrimPrefix(e.Name(), prefix))
		}
	}
	return versions, nil
}
