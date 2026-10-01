package runner

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateSourcePath rejects unsafe paths before a producer reads source bytes.
func ValidateSourcePath(value string) error {
	return validatePath(value, false)
}

// ValidateSourceManifest checks canonical ordering, file metadata, portable
// paths and symlink resolution. It does not grant access to any referenced blob.
func ValidateSourceManifest(manifest SourceManifest) error {
	if manifest.Version != SourceManifestVersion {
		return fmt.Errorf("runner: unsupported source manifest version %d", manifest.Version)
	}
	if manifest.Entries == nil || len(manifest.Entries) > MaxSourceEntries {
		return fmt.Errorf("runner: entries must be an array with at most %d entries", MaxSourceEntries)
	}
	entries := make(map[string]SourceEntry, len(manifest.Entries))
	aliases := make(map[string]string, len(manifest.Entries))
	var total int64
	for index, entry := range manifest.Entries {
		if err := validateEntry(entry); err != nil {
			return fmt.Errorf("runner: entries[%d]: %w", index, err)
		}
		if index > 0 && manifest.Entries[index-1].Path >= entry.Path {
			return fmt.Errorf("runner: entries must have unique paths in UTF-8 byte order")
		}
		for prefix := entry.Path; prefix != "."; prefix = path.Dir(prefix) {
			alias := strings.ToLower(prefix)
			if previous, found := aliases[alias]; found && previous != prefix {
				return fmt.Errorf("runner: portable path alias %q and %q", previous, prefix)
			}
			aliases[alias] = prefix
			if prefix != entry.Path {
				if _, found := entries[prefix]; found {
					return fmt.Errorf("runner: entry %q has non-directory ancestor %q", entry.Path, prefix)
				}
			}
		}
		entries[entry.Path] = entry
		total += entry.Size
		if total > MaxSourceTotalBytes {
			return fmt.Errorf("runner: source exceeds %d total bytes", MaxSourceTotalBytes)
		}
	}
	for _, entry := range manifest.Entries {
		if entry.Kind == "symlink" {
			if err := resolveLink(entry.Path, entries, aliases); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEntry(entry SourceEntry) error {
	if err := validatePath(entry.Path, false); err != nil {
		return fmt.Errorf("path %q: %w", entry.Path, err)
	}
	switch entry.Kind {
	case "file":
		if !strings.HasPrefix(entry.Digest, "sha256:") || !validHex(strings.TrimPrefix(entry.Digest, "sha256:"), 64) {
			return fmt.Errorf("file digest must be lowercase sha256:<64 hex digits>")
		}
		if entry.Size < 0 || entry.Size > MaxSourceFileBytes {
			return fmt.Errorf("file size must be between 0 and %d", MaxSourceFileBytes)
		}
		if entry.Mode != "0644" && entry.Mode != "0755" {
			return fmt.Errorf("file mode must be 0644 or 0755")
		}
		if entry.Target != "" {
			return fmt.Errorf("file must not contain a symlink target")
		}
	case "symlink":
		if entry.Digest != "" || entry.Size != 0 || entry.Mode != "" {
			return fmt.Errorf("symlink must not contain file metadata")
		}
		if err := validatePath(entry.Target, true); err != nil {
			return fmt.Errorf("symlink target %q: %w", entry.Target, err)
		}
	default:
		return fmt.Errorf("unsupported entry kind %q", entry.Kind)
	}
	return nil
}

func validatePath(value string, target bool) error {
	if value == "" || len(value) > MaxSourcePathBytes || !utf8.ValidString(value) || path.IsAbs(value) || !target && path.Clean(value) != value {
		return fmt.Errorf("must be a bounded canonical relative path")
	}
	if !target && (value == "." || value == ".." || strings.HasPrefix(value, "../")) {
		return fmt.Errorf("source path escapes the workspace")
	}
	parts := strings.Split(value, "/")
	if len(parts) > MaxSourcePathDepth {
		return fmt.Errorf("path exceeds %d components", MaxSourcePathDepth)
	}
	for _, component := range parts {
		if component == "" {
			return fmt.Errorf("empty path component")
		}
		if target && (component == "." || component == "..") {
			continue
		}
		if len(component) > 255 || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return fmt.Errorf("nonportable path component")
		}
		lower := strings.ToLower(component)
		if lower == ".git" {
			return fmt.Errorf("git metadata is not source content")
		}
		base := strings.SplitN(lower, ".", 2)[0]
		if base == "con" || base == "prn" || base == "aux" || base == "nul" || len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '0' && base[3] <= '9' {
			return fmt.Errorf("reserved portable path component")
		}
		for _, char := range component {
			if unicode.IsControl(char) || char == utf8.RuneError || strings.ContainsRune(`\:*?"<>|`, char) {
				return fmt.Errorf("nonportable path character")
			}
		}
	}
	return nil
}

// Resolve component by component: cleaning the target before expanding nested
// links can hide an escape (a link to the root followed by another '..').
func resolveLink(name string, entries map[string]SourceEntry, aliases map[string]string) error {
	pending := strings.Split(name, "/")
	stack := []string{}
	expansions := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case ".":
			continue
		case "..":
			if len(stack) == 0 {
				return fmt.Errorf("runner: symlink %q escapes the workspace", name)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		candidate := strings.Join(append(stack, component), "/")
		if canonical, found := aliases[strings.ToLower(candidate)]; found && canonical != candidate {
			return fmt.Errorf("runner: symlink %q uses a nonportable case alias", name)
		}
		entry, found := entries[candidate]
		if found && entry.Kind == "symlink" {
			expansions++
			if expansions > MaxSourcePathDepth {
				return fmt.Errorf("runner: symlink %q is cyclic or exceeds the resolution limit", name)
			}
			pending = append(strings.Split(entry.Target, "/"), pending...)
			continue
		}
		if found && entry.Kind == "file" && len(pending) != 0 {
			return fmt.Errorf("runner: symlink %q traverses a regular file", name)
		}
		stack = append(stack, component)
		if len(stack) > MaxSourcePathDepth {
			return fmt.Errorf("runner: symlink %q exceeds the resolved path depth", name)
		}
	}
	return nil
}
