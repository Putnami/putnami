// Package runner defines portable, credential-free runner wire contracts.
package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Source manifest version and admission bounds apply before materialization.
const (
	ProviderCommandName   = "runner-provider"
	SourceManifestVersion = 1
	MaxManifestBytes      = 32 << 20
	MaxSourceEntries      = 100_000
	MaxSourceFileBytes    = 1 << 30
	MaxSourceTotalBytes   = 16 << 30
	MaxSourcePathBytes    = 1024
	MaxSourcePathDepth    = 64
)

// SourceManifest describes worktree content only. Directories are implicit;
// absent entries represent absent files, including tracked deletions.
type SourceManifest struct {
	// Version is the source-manifest wire version and must equal 1.
	Version int `json:"version"`
	// Entries lists unique source paths in UTF-8 byte order; an empty array is valid.
	Entries []SourceEntry `json:"entries"`
}

// SourceEntry is either a regular file (digest, size and mode) or a relative
// symlink (target). Mode carries only the executable distinction.
type SourceEntry struct {
	// Path is the canonical portable path relative to the source root.
	Path string `json:"path"`
	// Kind selects the disjoint file or symlink metadata shape.
	Kind string `json:"kind"`
	// Digest is the lowercase sha256 digest of file bytes; symlinks omit it.
	Digest string `json:"digest,omitempty"`
	// Size is the file length in bytes, including zero for an empty file.
	Size int64 `json:"size,omitempty"`
	// Mode is 0644 or 0755 for files and is omitted for symlinks.
	Mode string `json:"mode,omitempty"`
	// Target is a relative symlink target whose resolution stays inside the source root.
	Target string `json:"target,omitempty"`
	// Bound marks a path Git ignores in the source worktree that was captured
	// because a planned task declares it as an input. It is present only as
	// true; a bound path is part of the canonical bytes, so binding one changes
	// the source digest.
	Bound bool `json:"bound,omitempty"`
}

// MarshalJSON emits the disjoint entry shapes, including empty-file size zero.
// The bound member is written only when true: an absent member and false are
// one canonical form, so `"bound": false` is rejected on the way in.
func (entry SourceEntry) MarshalJSON() ([]byte, error) {
	if err := validateEntry(entry); err != nil {
		return nil, err
	}
	if entry.Kind == "file" {
		return json.Marshal(struct {
			Path   string `json:"path"`
			Kind   string `json:"kind"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
			Mode   string `json:"mode"`
			Bound  bool   `json:"bound,omitempty"`
		}{entry.Path, entry.Kind, entry.Digest, entry.Size, entry.Mode, entry.Bound})
	}
	return json.Marshal(struct {
		Path   string `json:"path"`
		Kind   string `json:"kind"`
		Target string `json:"target"`
		Bound  bool   `json:"bound,omitempty"`
	}{entry.Path, entry.Kind, entry.Target, entry.Bound})
}

// GitContext carries version-stamp context separately from source identity.
// It contains no remote URL, local path, Git configuration, hook or credential.
// Empty Head represents an unborn repository; empty Branch represents detached HEAD.
type GitContext struct {
	// Head is the lowercase Git object ID, or empty for an unborn repository.
	Head string `json:"head"`
	// Branch is the symbolic branch name, or empty for detached HEAD.
	Branch string `json:"branch"`
	// Dirty records whether captured source differs from the committed tree.
	Dirty bool `json:"dirty"`
}

// ValidateGitContext validates the bounded, credential-free metadata shape.
func ValidateGitContext(context GitContext) error {
	if context.Head != "" && !validHex(context.Head, 40) && !validHex(context.Head, 64) {
		return fmt.Errorf("runner: git head must be a lowercase Git object ID")
	}
	branch := context.Branch
	if branch == "" {
		return nil
	}
	if len(branch) > 1024 || !utf8.ValidString(branch) || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") || branch == "@" {
		return fmt.Errorf("runner: invalid git branch")
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("runner: invalid git branch component")
		}
	}
	for _, char := range branch {
		if char <= ' ' || unicode.IsControl(char) || char == utf8.RuneError || strings.ContainsRune("~^:?*[\\", char) {
			return fmt.Errorf("runner: invalid git branch character")
		}
	}
	return nil
}

// ParseGitContext strictly decodes the separate version-stamp context.
func ParseGitContext(data []byte) (GitContext, error) {
	if err := strictJSON(data); err != nil {
		return GitContext{}, err
	}
	if _, err := objectFields(data, "head", "branch", "dirty"); err != nil {
		return GitContext{}, err
	}
	var context GitContext
	if err := json.Unmarshal(data, &context); err != nil {
		return GitContext{}, err
	}
	if err := ValidateGitContext(context); err != nil {
		return GitContext{}, err
	}
	return context, nil
}

// CanonicalSourceManifest returns compact JSON in the documented member order,
// with Go JSON string escaping and no trailing newline. Entries must be sorted.
func CanonicalSourceManifest(manifest SourceManifest) ([]byte, error) {
	if err := ValidateSourceManifest(manifest); err != nil {
		return nil, err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("runner: manifest exceeds %d bytes", MaxManifestBytes)
	}
	return data, nil
}

// SourceDigest hashes the versioned canonical manifest, never Git context.
func SourceDigest(manifest SourceManifest) (string, error) {
	data, err := CanonicalSourceManifest(manifest)
	if err != nil {
		return "", err
	}
	return BlobDigest(data), nil
}

// BlobDigest names exact bytes using lowercase SHA-256.
func BlobDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// ParseSourceManifest rejects ambiguous JSON before validating source paths.
func ParseSourceManifest(data []byte) (SourceManifest, error) {
	if err := strictJSON(data); err != nil {
		return SourceManifest{}, err
	}
	fields, err := objectFields(data, "version", "entries")
	if err != nil {
		return SourceManifest{}, err
	}
	var manifest SourceManifest
	if err := json.Unmarshal(fields["version"], &manifest.Version); err != nil {
		return SourceManifest{}, fmt.Errorf("runner: version: %w", err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(fields["entries"], &entries); err != nil {
		return SourceManifest{}, fmt.Errorf("runner: entries: %w", err)
	}
	if len(entries) > MaxSourceEntries {
		return SourceManifest{}, fmt.Errorf("runner: too many source entries")
	}
	manifest.Entries = make([]SourceEntry, len(entries))
	for index, raw := range entries {
		var entry SourceEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return SourceManifest{}, fmt.Errorf("runner: entries[%d]: %w", index, err)
		}
		keys := []string{"path", "kind", "digest", "size", "mode"}
		if entry.Kind == "symlink" {
			keys = []string{"path", "kind", "target"}
		}
		fields, err := strictObject(raw, keys, []string{"bound"})
		if err != nil {
			return SourceManifest{}, fmt.Errorf("runner: entries[%d]: %w", index, err)
		}
		// The canonical form omits a false bound member, so its explicit
		// presence is a second spelling of the same entry and is refused.
		if bound, present := fields["bound"]; present && !bytes.Equal(bytes.TrimSpace(bound), []byte("true")) {
			return SourceManifest{}, fmt.Errorf("runner: entries[%d]: bound must be true when present", index)
		}
		manifest.Entries[index] = entry
	}
	if err := ValidateSourceManifest(manifest); err != nil {
		return SourceManifest{}, err
	}
	return manifest, nil
}

func objectFields(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if len(fields) != len(keys) {
		return nil, fmt.Errorf("runner: object has missing or unknown fields")
	}
	for _, key := range keys {
		if len(bytes.TrimSpace(fields[key])) == 0 {
			return nil, fmt.Errorf("runner: missing field %q", key)
		}
	}
	return fields, nil
}
