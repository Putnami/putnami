package apicheck

import "strings"

// breakingFooters are the conventional-commit footers that declare a breaking
// change without the "!" marker; Conventional Commits 1.0.0 makes the
// hyphenated form a synonym. One is matched at the start of a body line.
var breakingFooters = []string{"BREAKING CHANGE:", "BREAKING-CHANGE:"}

// declaresBreaking reports whether a commit is a conventional commit that
// declares a breaking change, with the "!" marker ("feat!: ...",
// "feat(scope)!: ...") or a "BREAKING CHANGE:" or "BREAKING-CHANGE:" footer
// line in its body.
//
// It reads a message exactly as the version bump does
// (tooling/cli/internal/git ParseConventional), because the two must agree: a
// marker this check accepted and the bump ignored would ship a breaking change
// in a release that promises none. A subject without the conventional shape
// declares nothing, whatever its body says.
func declaresBreaking(subject, body string) bool {
	head, _, found := strings.Cut(subject, ":")
	if !found {
		return false
	}
	head = strings.TrimSpace(head)
	breaking := false
	if strings.HasSuffix(head, "!") {
		breaking = true
		head = strings.TrimSuffix(head, "!")
	}
	if open := strings.IndexByte(head, '('); open >= 0 {
		if !strings.HasSuffix(head, ")") {
			return false
		}
		head = head[:open]
	}
	if !isConventionalType(strings.TrimSpace(head)) {
		return false
	}
	if breaking {
		return true
	}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		for _, footer := range breakingFooters {
			if strings.HasPrefix(line, footer) {
				return true
			}
		}
	}
	return false
}

// isConventionalType reports whether s is a conventional commit type:
// lowercase letters only, as the convention spells them.
func isConventionalType(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}
