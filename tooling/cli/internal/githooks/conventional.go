// Package githooks installs putnami's git hooks (pre-commit, pre-push,
// commit-msg) and validates commit messages against conventional commit format.
// It is the core home for the hooks that previously lived in @putnami/ci, so they
// survive that extension's removal. It is distinct from
// tooling/cli/internal/hooks (the build lifecycle-hook executor).
package githooks

import (
	"regexp"
	"strings"
)

// knownTypes are the recognized conventional commit types.
var knownTypes = map[string]bool{
	"feat":     true,
	"fix":      true,
	"docs":     true,
	"style":    true,
	"refactor": true,
	"perf":     true,
	"test":     true,
	"build":    true,
	"ci":       true,
	"chore":    true,
	"revert":   true,
}

// headerPattern matches: type(scope)!: subject
var headerPattern = regexp.MustCompile(`^(\w+)(?:\(([^)]+)\))?(!)?\s*:\s*(.+?)$`)

// Validate checks whether a commit message follows conventional commit format.
// It returns an error message when invalid, or the empty string when valid.
func Validate(message string) string {
	lines := strings.SplitN(message, "\n", 2)
	header := strings.TrimSpace(lines[0])

	if header == "" {
		return "empty commit message"
	}

	matches := headerPattern.FindStringSubmatch(header)
	if matches == nil {
		return "commit message does not match conventional commit format: type(scope): subject"
	}

	commitType := strings.ToLower(matches[1])
	if !knownTypes[commitType] {
		return "unknown commit type: " + commitType + ". Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert"
	}

	subject := strings.TrimSpace(matches[4])
	if subject == "" {
		return "commit subject is empty"
	}

	return ""
}
