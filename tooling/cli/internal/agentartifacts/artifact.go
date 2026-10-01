// Package agentartifacts materializes verified agent content into a workspace
// WITHOUT ever overwriting a file the user owns.
//
// The package is deliberately split along the seams that carry its guarantees,
// and every phase completes before the next one starts:
//
//  1. VERIFY — bind an extracted content tree to the exact pin its caller
//     resolved (version, identity digest, manifest digest), strictly parse the
//     manifest, and check every declared path and content digest. This
//     finishes before a single byte of the workspace is read.
//  2. PLAN — classify every target (and every previously-managed path the new
//     content dropped) into created / updated / removed / unchanged / collided.
//  3. APPLY — only when the plan carries ZERO collisions: stage every byte,
//     then commit with per-file atomic renames.
//  4. COMMIT — record the managed paths and the digest of what was installed,
//     so "locally modified" stays decidable on the next run without the
//     content.
//
// Where the pin and the tree come from is not this package's business: the
// lifecycle reads them from the extension release the workspace pins
// (internal/extension), and nothing here resolves or downloads.
//
// The ownership rule the whole package exists to hold: this materializer owns
// only what it can PROVE it wrote. A file it did not record, or recorded and
// then found changed, is the user's — it is preserved and reported as a
// collision, and one collision aborts the entire mutation before the first
// write. There is deliberately no force-overwrite lever; see
// doc/adr/0004-agent-artifact-ownership.md.
//
// Command lifecycle wiring (install/upgrade/context generate) is NOT here: this
// package is a library with no command surface, so its guarantees can be
// tested without a CLI invocation.
package agentartifacts

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// File binds one workspace-relative path to the exact content digest the
// artifact authorizes for it. It is the shared shape of a manifest entry, a
// plan entry's expected content, and an ownership-state record — one type, so
// the three can never disagree about what "this path, these bytes" means.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Artifact is extracted agent content that has passed every check this
// package makes before the workspace is touched: the manifest hashes to the
// value the pin binds, the manifest parses and validates strictly, and every
// declared file exists in the extracted tree as a REGULAR file whose bytes
// hash to the declared digest.
//
// Holding a *Artifact is therefore the proof that the VERIFY phase completed.
// Nothing downstream re-derives any of it from the tree.
type Artifact struct {
	// Name is the artifact identity, cross-checked against the manifest.
	Name string
	// Version is the exact pinned version, cross-checked against the manifest.
	Version string
	// ArchiveDigest is the pin's identity digest: for an installed extension,
	// the digest of the extension manifest its lock entry binds.
	ArchiveDigest string
	// ManifestHash is the pin's content manifest SHA-256.
	ManifestHash string
	// Dir is the verified, extracted tree.
	Dir string
	// Files are the declared files, sorted by path.
	Files []File
}

// ValidatePin rejects a pin that cannot secure a materialization. Both digests
// are required and must be bare lowercase hex: a digest may become a path
// component, so a malformed value is a path-traversal payload, not merely an
// unusable hash.
func ValidatePin(name string, entry lockfile.AgentArtifactLockEntry) error {
	if strings.TrimSpace(entry.Version) == "" || entry.Version == "0.0.0" {
		return fmt.Errorf("%s pins %s at no exact version (got %q)", lockfile.LockFilename, name, entry.Version)
	}
	if !isSHA256Hex(entry.Integrity) {
		return fmt.Errorf("%s pins %s with a malformed archive integrity %q: want 64 lowercase hex characters",
			lockfile.LockFilename, name, entry.Integrity)
	}
	if !isSHA256Hex(entry.ManifestHash) {
		return fmt.Errorf("%s pins %s with a malformed manifest hash %q: want 64 lowercase hex characters",
			lockfile.LockFilename, name, entry.ManifestHash)
	}
	return nil
}

// LoadArtifact binds an extracted artifact tree to its lock pin and verifies
// every byte the materializer is allowed to write.
//
// The order matters and is not an implementation detail: the manifest is bound
// to the lock BEFORE it is trusted to name files, and every declared file is
// verified BEFORE the workspace is read — so an artifact that fails any check
// cannot cause a partial plan, let alone a partial write.
//
// Extra files under dir are deliberately NOT rejected. The store's entry
// directory is not exclusively artifact content — it carries the store's own
// recency sidecar — so "every byte under dir is declared" is not a property
// the store can hold. The property that protects the workspace is the one
// enforced here: every byte this package WRITES is declared by a manifest the
// lock binds, and hashes to the digest that manifest declares.
func LoadArtifact(dir, name string, entry lockfile.AgentArtifactLockEntry) (*Artifact, error) {
	if err := ValidatePin(name, entry); err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, fmt.Errorf("agent artifact %s resolved to no directory", name)
	}

	manifestPath := filepath.Join(dir, wsproto.AgentArtifactManifestFilename)
	data, err := readRegularFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read %s manifest: %w", name, err)
	}
	if got := lockfile.HashBytes(data); got != entry.ManifestHash {
		return nil, fmt.Errorf(
			"manifest hash mismatch for %s@%s: %s pins %s, extracted tree has %s",
			name, entry.Version, lockfile.LockFilename, entry.ManifestHash, got)
	}

	manifest, diags := wsproto.ParseAndValidateAgentArtifactManifest(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid %s manifest: %s", name, formatDiagnostics(diags))
	}
	if manifest.Name != name {
		return nil, fmt.Errorf("agent artifact identity mismatch: lock pins %q, manifest declares %q", name, manifest.Name)
	}
	if manifest.Version != entry.Version {
		return nil, fmt.Errorf("agent artifact version mismatch for %s: lock pins %q, manifest declares %q",
			name, entry.Version, manifest.Version)
	}

	canonical := wsproto.CanonicalAgentArtifactManifest(manifest)
	files := make([]File, 0, len(canonical.Files))
	for _, file := range canonical.Files {
		// Re-check the path against the protocol's exported rule. The parser
		// already applied it; repeating it here means a future parser relaxation
		// cannot silently widen what reaches the filesystem.
		if !wsproto.ValidAgentArtifactPath(file.Path) {
			return nil, fmt.Errorf("agent artifact %s declares unsafe path %q", name, file.Path)
		}
		if file.Path == wsproto.AgentArtifactManifestFilename {
			return nil, fmt.Errorf(
				"agent artifact %s declares its own manifest %q as workspace content: the manifest describes the artifact and is never materialized",
				name, file.Path)
		}
		source := filepath.Join(dir, filepath.FromSlash(file.Path))
		content, err := readRegularFile(source)
		if err != nil {
			return nil, fmt.Errorf("agent artifact %s declares %s: %w", name, file.Path, err)
		}
		if got := lockfile.HashBytes(content); got != file.SHA256 {
			return nil, fmt.Errorf(
				"agent artifact %s drifted from its manifest at %s: manifest declares %s, archive contains %s",
				name, file.Path, file.SHA256, got)
		}
		files = append(files, File{Path: file.Path, SHA256: file.SHA256})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("agent artifact %s declares no files", name)
	}

	return &Artifact{
		Name:          name,
		Version:       entry.Version,
		ArchiveDigest: entry.Integrity,
		ManifestHash:  entry.ManifestHash,
		Dir:           dir,
		Files:         files,
	}, nil
}

// Read returns the verified bytes of one declared file. It re-hashes on every
// read so a tree mutated between verification and write (a concurrent store GC
// plus a re-admit, or a hand edit under $HOME) cannot slip past the check that
// LoadArtifact performed.
func (a *Artifact) Read(path string) ([]byte, error) {
	want := ""
	for _, file := range a.Files {
		if file.Path == path {
			want = file.SHA256
			break
		}
	}
	if want == "" {
		return nil, fmt.Errorf("agent artifact %s does not declare %s", a.Name, path)
	}
	content, err := readRegularFile(filepath.Join(a.Dir, filepath.FromSlash(path)))
	if err != nil {
		return nil, fmt.Errorf("read %s from agent artifact %s: %w", path, a.Name, err)
	}
	if got := lockfile.HashBytes(content); got != want {
		return nil, fmt.Errorf("agent artifact %s changed under us at %s: expected %s, got %s", a.Name, path, want, got)
	}
	return content, nil
}

// Digests returns the declared path→digest map, used by the planner.
func (a *Artifact) Digests() map[string]string {
	out := make(map[string]string, len(a.Files))
	for _, file := range a.Files {
		out[file.Path] = file.SHA256
	}
	return out
}

// readRegularFile reads path only when it is a REGULAR file. A symlink, a
// directory, or a device node is refused rather than followed: an artifact tree
// lives in a machine-global store shared by every repo on the host, so a
// symlink there would otherwise read (and later copy) whatever it points at.
func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink, which is never followed", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.ReadFile(path)
}

// isSHA256Hex reports whether d is a bare 64-character lowercase hex digest —
// the exact shape lockfile.HashBytes produces and the artifact store addresses
// by. It rejects "sha256:"-prefixed, uppercase, and traversal-shaped values.
func isSHA256Hex(d string) bool {
	if len(d) != 64 {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// formatDiagnostics renders protocol diagnostics deterministically so an error
// message does not depend on validation order.
func formatDiagnostics(diags []diag.Diagnostic) string {
	messages := make([]string, 0, len(diags))
	for _, d := range diags {
		if d.Severity == diag.Error {
			messages = append(messages, fmt.Sprintf("%s: %s", d.Code, d.Message))
		}
	}
	sort.Strings(messages)
	return strings.Join(messages, "; ")
}
