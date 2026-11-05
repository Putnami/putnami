package gitread

import (
	"fmt"
	"strings"
)

// HeadSHA returns the full SHA for HEAD.
func HeadSHA(repoRoot string) (string, error) {
	output, err := Run(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// ResolveCommit resolves ref to its full immutable commit ID, in lowercase. A
// caller that publishes a revision records this rather than a branch or tag
// name, whose meaning can move after the document was emitted. A ref that
// ValidRevisionArgument refuses, that names no commit, or that names one
// ambiguously is an error.
func ResolveCommit(repoRoot, ref string) (string, error) {
	if !ValidRevisionArgument(ref) {
		return "", fmt.Errorf("resolve commit: invalid revision argument")
	}
	output, stderr, err := RunCapture(repoRoot, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || strings.TrimSpace(stderr) != "" {
		// Name what git said, so a failure that is not an ambiguity (a
		// warning, a killed process) can be told apart from one.
		detail := strings.TrimSpace(stderr)
		if detail == "" && err != nil {
			detail = err.Error()
		}
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return "", fmt.Errorf("resolve commit: revision does not identify one unambiguous commit: %s", detail)
	}
	sha := strings.ToLower(strings.TrimSpace(output))
	if !ValidObjectID(sha) {
		return "", fmt.Errorf("resolve commit: git returned invalid object ID")
	}
	return sha, nil
}

// ValidRevisionArgument reports whether ref can be passed to git as one
// revision: non-empty, at most 1024 bytes, without surrounding space or a
// control character, and not starting with "-", so git never reads it as an
// option.
func ValidRevisionArgument(ref string) bool {
	if ref == "" || len(ref) > 1024 || ref != strings.TrimSpace(ref) || strings.HasPrefix(ref, "-") {
		return false
	}
	for _, character := range ref {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// ValidObjectID accepts an abbreviated or full SHA-1 or SHA-256 object name:
// 7 to 64 hexadecimal digits, in either case.
func ValidObjectID(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

// SplitNUL splits git's NUL-terminated output into its records. A final NUL
// ends the last record rather than starting an empty one, so empty output has
// no record.
func SplitNUL(output string) []string {
	parts := strings.Split(output, "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// ParseStageEntry reads one record of `git ls-files --stage -z`: the mode,
// object ID and stage, then a tab and the repository path. ok is false for a
// record of another shape.
func ParseStageEntry(entry string) (mode, objectID, stage, repoPath string, ok bool) {
	metadata, repoPath, found := strings.Cut(entry, "\t")
	if !found {
		return "", "", "", "", false
	}
	fields := strings.Fields(metadata)
	if len(fields) != 3 {
		return "", "", "", "", false
	}
	return fields[0], fields[1], fields[2], repoPath, true
}
