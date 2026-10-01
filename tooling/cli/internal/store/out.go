package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.putnami.dev/sdk/extension/dirlink"
)

// ErrFastPathRetired reports an attempt to point an out/ symlink at a
// task-owned blob. The CAS-symlink all-hit fast path is RETIRED for the v3
// entry model — see the decision (and its measurement) in task_entry.go. A
// task-owned entry holds one task's declared outputs, while the linked path is
// the whole command's shared directory, so such a link would be structurally
// wrong; and that directory must stay writable for the SIBLING steps that write
// into it while another step's entry is materialized, which a link into the
// hardlink CAS tree would corrupt in place.
// Refusing is louder than silently linking the wrong thing.
var ErrFastPathRetired = errors.New("out/ symlink fast path is retired for task-owned cache entries")

// OutManager manages the .putnami/out/ symlink layer.
// Each successful job gets a symlink:
//
//	.putnami/out/{project}/{command}/{step} → store/blobs/{hash}/files
//
// This provides a stable path for downstream tools to consume outputs.
//
// It is a LEGACY-ENTRY-ONLY facility. Task-owned entries (entry format 2,
// task_entry.go) are materialized by MaterializeTaskOutput instead, and Link /
// ReplaceLink reject them with ErrFastPathRetired.
type OutManager struct {
	outRoot   string // absolute path to .putnami/out
	storeRoot string // absolute path to .putnami/store
}

// NewOutManager creates an OutManager whose out/ symlink layer is per-workspace
// but whose blob targets resolve into storeRoot. storeRoot is the machine-global
// per-repo store (see ResolveStoreRoot), so a per-worktree out/ symlink points
// into the shared store — exactly the cross-worktree sharing the global store
// provides. The symlink target is absolute (Link), so it bridges the
// worktree→global-store boundary correctly.
func NewOutManager(workspaceRoot, storeRoot string) *OutManager {
	return &OutManager{
		outRoot:   filepath.Join(workspaceRoot, ".putnami", "out"),
		storeRoot: storeRoot,
	}
}

// Link creates or updates a symlink from the out/ path to the blob files directory.
// Uses atomic symlink update: create temp symlink → rename over old.
func (m *OutManager) Link(project, command, step, hash string) error {
	linkPath := m.linkPath(project, command, step)
	targetDir := m.blobFilesDir(hash)
	return m.link(linkPath, targetDir, false)
}

// ReplaceLink replaces any existing output path, including a real directory,
// with a symlink to the cached blob. Callers must only use this when the real
// directory is stale from a prior session: removing a directory written by a
// cache miss in the current session would discard that sibling step's output.
func (m *OutManager) ReplaceLink(project, command, step, hash string) error {
	linkPath := m.linkPath(project, command, step)
	targetDir := m.blobFilesDir(hash)
	return m.link(linkPath, targetDir, true)
}

func (m *OutManager) link(linkPath, targetDir string, replace bool) error {
	// The blob directory is the parent of its files/ tree; a task-owned entry
	// carries a descriptor there that a legacy entry never has.
	if isTaskOwnedBlob(filepath.Dir(targetDir)) {
		return fmt.Errorf("%w: %s", ErrFastPathRetired, linkPath)
	}

	// Ensure target exists
	if _, err := os.Stat(targetDir); err != nil {
		if os.IsNotExist(err) {
			// No files directory → nothing to link
			return nil
		}
		return fmt.Errorf("stat target: %w", err)
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}
	if replace {
		if err := os.RemoveAll(linkPath); err != nil {
			return fmt.Errorf("remove stale output dir: %w", err)
		}
	}

	// Atomic symlink on Unix: create temp → rename. Windows swaps a junction
	// under a lock instead (dirlink.Replace).
	if err := dirlink.Replace(targetDir, linkPath, linkPath+".tmp"); err != nil {
		return fmt.Errorf("link output: %w", err)
	}

	return nil
}

// LinkedBlobHash reports the blob hash an out/ path points at, or "" when path
// is not a symlink this manager could have written. It is the inverse of Link:
// callers that find a symlink left by an earlier session use it to recover the
// cache entry behind that link, so the prior-session case can run through the
// same restore path as an in-run hit instead of a blind directory copy.
//
// The link text is read literally (not resolved) so a dangling link — whose blob
// GC has since reclaimed — still yields its hash, and so a store root reached
// through a symlinked prefix does not defeat the match. Shape is authoritative:
// callers must still look the hash up to learn whether the entry survives.
func (m *OutManager) LinkedBlobHash(path string) string {
	info, err := os.Lstat(path)
	if err != nil || !dirlink.IsLink(path, info) {
		return ""
	}
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	// .../blobs/<hash[:2]>/<hash>/files
	if filepath.Base(target) != "files" {
		return ""
	}
	hashDir := filepath.Dir(target)
	hash := filepath.Base(hashDir)
	prefixDir := filepath.Dir(hashDir)
	if filepath.Base(filepath.Dir(prefixDir)) != "blobs" {
		return ""
	}
	if len(hash) < 2 || filepath.Base(prefixDir) != hash[:2] || !isHexDigest(hash) {
		return ""
	}
	return hash
}

// isHexDigest reports whether s is a plausible content hash, so a path that
// merely resembles the blob layout cannot be mistaken for one.
func isHexDigest(s string) bool {
	if len(s) < 16 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// linkPath returns the path for a symlink: .putnami/out/{project}/{command}/{step}
func (m *OutManager) linkPath(project, command, step string) string {
	if step == "" {
		return filepath.Join(m.outRoot, project, command)
	}
	return filepath.Join(m.outRoot, project, command, step)
}

// blobFilesDir returns the files directory inside a blob.
func (m *OutManager) blobFilesDir(hash string) string {
	prefix := hash
	if len(hash) >= 2 {
		prefix = hash[:2]
	}
	return filepath.Join(m.storeRoot, "blobs", prefix, hash, "files")
}
