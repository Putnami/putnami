// Shared path rules for every contract surface that names a file.
//
// Three surfaces declare paths — task outputs (task_contract.go), extension
// runtimes (runtime.go), and the workspace adapter (workspace.go) — and they
// must agree on what a declared path may be, because they are all resolved
// against a root the manifest never sees. One implementation, two shapes:
//
//   - NormalizeRelativePath is the CONCRETE form: a single file or subtree.
//     Globs and template variables are rejected, because a declaration that
//     matches a set of paths cannot have one owner and cannot be resolved
//     without running something first.
//   - NormalizeInputPattern is the SET form: a glob over a bounded tree. The
//     glob metacharacters are allowed and everything else is not.
//
// Both reject the same three classes of unsafe path, and this is the whole
// "strict path validation" rule of the task contract:
//
//   - ABSOLUTE paths, which ignore the root they were declared against and
//     bind a manifest to one machine's layout;
//   - ESCAPES above the root (".." segments that leave the tree), which would
//     let a declaration reach a directory the root was chosen to bound;
//   - BACKSLASH separators, which are a Windows spelling of a path the rest of
//     the contract compares as slash-separated text.
//
// The root ITSELF is never a valid declaration either: an output, an
// executable, or a marker that names its own root claims everything under it.

package extension

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// NormalizeRelativePath returns the canonical form of a concrete
// contract-declared path, or an error explaining why the path cannot name a
// region of the tree.
//
// Canonical means: slash-separated, cleaned (no "." or ".." segments, no
// duplicate or trailing separators), relative, and concrete.
func NormalizeRelativePath(p string) (string, error) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.ContainsRune(trimmed, '\\') {
		return "", fmt.Errorf(`path must use "/" separators`)
	}
	if strings.ContainsAny(trimmed, "*?[]") {
		return "", fmt.Errorf("path must be a concrete file or directory, not a glob")
	}
	if strings.ContainsAny(trimmed, "{}") {
		return "", fmt.Errorf("path must not use template variables; use root instead")
	}
	if hasWindowsDrivePrefix(trimmed) {
		return "", fmt.Errorf("path must not use a Windows drive prefix")
	}
	if path.IsAbs(trimmed) {
		return "", fmt.Errorf("path must be relative to its root")
	}
	clean := path.Clean(trimmed)
	if clean == "." {
		return "", fmt.Errorf("path must name a file or subtree, not the root itself")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path must not escape its root")
	}
	return clean, nil
}

// NormalizeInputPattern returns the canonical form of a root-relative glob
// pattern, or an error explaining why the pattern cannot bound a file set.
//
// It is NormalizeRelativePath with the glob metacharacters allowed: an input
// set legitimately matches many files ("cmd/**", "tsconfig*.json"). Everything
// else is identical, so a pattern can no more escape its root or hard-code an
// absolute path than a declared output can.
func NormalizeInputPattern(p string) (string, error) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", fmt.Errorf("pattern is empty")
	}
	if strings.ContainsRune(trimmed, '\\') {
		return "", fmt.Errorf(`pattern must use "/" separators`)
	}
	if strings.ContainsAny(trimmed, "{}") {
		return "", fmt.Errorf("pattern must not use template variables; it is already relative to its root")
	}
	if hasWindowsDrivePrefix(trimmed) {
		return "", fmt.Errorf("pattern must not use a Windows drive prefix")
	}
	if path.IsAbs(trimmed) {
		return "", fmt.Errorf("pattern must be relative to its root")
	}
	clean := path.Clean(trimmed)
	if clean == "." {
		return "", fmt.Errorf("pattern must name files under the root, not the root itself")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("pattern must not escape its root")
	}
	if _, err := path.Match(clean, ""); err != nil {
		return "", fmt.Errorf("pattern has invalid glob syntax: %w", err)
	}
	return clean, nil
}

// hasWindowsDrivePrefix reports paths that Windows resolves against a drive,
// including both drive-absolute ("C:/x") and drive-relative ("C:x") forms.
// Neither is relative to the contract root, and accepting one would make the
// same manifest resolve differently across operating systems.
func hasWindowsDrivePrefix(s string) bool {
	if len(s) < 2 || s[1] != ':' {
		return false
	}
	letter := s[0]
	return (letter >= 'a' && letter <= 'z') || (letter >= 'A' && letter <= 'Z')
}

// normalizePatternList canonicalizes and sorts a pattern set, dropping nothing:
// a pattern that cannot be normalized is kept as authored so validation reports
// it instead of the manifest silently meaning something else. The result is a
// new slice, so normalizing never writes through a slice a caller still holds.
func normalizePatternList(patterns []string) []string {
	if len(patterns) == 0 {
		return patterns
	}
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		normalized, err := NormalizeInputPattern(pattern)
		if err != nil {
			out = append(out, pattern)
			continue
		}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}
