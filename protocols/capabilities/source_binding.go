package capabilities

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const sourceBindingDomain = "putnami-source-binding-v1\n"

// SourceFileMode is the canonical Git-compatible mode of a bound source entry.
type SourceFileMode string

// Supported source-v1 file modes preserve executable, symlink, and gitlink identity.
const (
	SourceModeRegular    SourceFileMode = "100644"
	SourceModeExecutable SourceFileMode = "100755"
	SourceModeSymlink    SourceFileMode = "120000"
	SourceModeGitlink    SourceFileMode = "160000"
)

var gitObjectPattern = regexp.MustCompile(`^git:[0-9a-f]{40}([0-9a-f]{24})?$`)

// SourceBindingFile is the protocol-layer input record. Workspace and history
// loaders own Git tracked/untracked/ignored enumeration; this package owns the
// exclusion, validation, ordering, canonical JSON, domain separation, and hash.
type SourceBindingFile struct {
	// Path is the canonical source-root-relative entry path.
	Path string `json:"path"`
	// Mode is the entry's canonical Git-compatible mode.
	Mode SourceFileMode `json:"mode"`
	// Digest is the mode-appropriate content or Git-object address.
	Digest string `json:"digest"`
}

type sourceBindingPayload struct {
	BindingVersion int                 `json:"bindingVersion"`
	Files          []SourceBindingFile `json:"files"`
}

// SourceDigest returns the lower-case SHA-256 content address used for regular
// file bytes and symlink-target bytes.
func SourceDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CanonicalSourceBindingInput validates and canonicalizes source-v1 input
// records. Entries excluded by source-v1 are omitted at any depth.
func CanonicalSourceBindingInput(records []SourceBindingFile) ([]byte, error) {
	filtered := make([]SourceBindingFile, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if code, message := validateProtocolPath(record.Path); code != "" {
			return nil, fmt.Errorf("%s: %s", record.Path, message)
		}
		if sourceBindingExcluded(record.Path) {
			continue
		}
		if seen[record.Path] {
			return nil, fmt.Errorf("duplicate source-binding path %q", record.Path)
		}
		seen[record.Path] = true
		switch record.Mode {
		case SourceModeRegular, SourceModeExecutable, SourceModeSymlink:
			if !sha256Pattern.MatchString(record.Digest) {
				return nil, fmt.Errorf("%s: mode %s requires sha256:<64-lower-hex>", record.Path, record.Mode)
			}
		case SourceModeGitlink:
			if !gitObjectPattern.MatchString(record.Digest) {
				return nil, fmt.Errorf("%s: gitlink requires git:<full-lower-hex-object-id>", record.Path)
			}
		default:
			return nil, fmt.Errorf("%s: unsupported Git mode %q", record.Path, record.Mode)
		}
		filtered = append(filtered, record)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Path < filtered[j].Path })
	return canonicalJSON(sourceBindingPayload{BindingVersion: 1, Files: filtered})
}

// ComputeSourceBinding hashes the canonical input-record payload using the
// source-v1 domain separator.
func ComputeSourceBinding(records []SourceBindingFile) (string, error) {
	payload, err := CanonicalSourceBindingInput(records)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(sourceBindingDomain))
	_, _ = hash.Write(payload)
	return "source-v1:sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func sourceBindingExcluded(value string) bool {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		if part == ".git" || part == ".gen" {
			return true
		}
		if part == "schema" && i+1 < len(parts) {
			if parts[i+1] == ManifestFilename {
				return true
			}
			if parts[i+1] == "feature-evidence" {
				return true
			}
		}
	}
	return false
}
