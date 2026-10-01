package gitread

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitTimeout is the default timeout for git subprocess calls. Copied from
// tooling/cli/internal/git/git.go.
const gitTimeout = 30 * time.Second

func run(repoRoot string, args ...string) (string, error) {
	stdout, stderr, err := runCapture(repoRoot, args...)
	if err != nil {
		return "", fmt.Errorf("git %s (in %s): %w: %s", args[0], repoRoot, err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

func runCapture(repoRoot string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", stderr.String(), fmt.Errorf("timed out after %s", gitTimeout)
		}
		return "", stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// ResolveCommit resolves ref to its full immutable commit ID. Callers that
// publish a revision use this rather than carrying a branch or tag name, whose
// meaning can move after the document was emitted.
func ResolveCommit(repoRoot, ref string) (string, error) {
	if !validRevisionArgument(ref) {
		return "", fmt.Errorf("resolve commit: invalid revision argument")
	}
	output, stderr, err := runCapture(repoRoot, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
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
	if !validObjectID(sha) {
		return "", fmt.Errorf("resolve commit: git returned invalid object ID")
	}
	return sha, nil
}

func validRevisionArgument(ref string) bool {
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

func validObjectID(s string) bool {
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

func splitNUL(output string) []string {
	parts := strings.Split(output, "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
