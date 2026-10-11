package cli

import "strings"

// ConventionalCommit is what a commit message says as a conventional commit:
// its type, its scope, and whether it declares a breaking change.
type ConventionalCommit struct {
	// Type is the word before the colon, such as "feat" or "fix".
	Type string
	// Scope is the text between the parentheses, or "" when the subject has
	// none.
	Scope string
	// Breaking is true when the subject carries the "!" marker or the body
	// holds a breaking-change footer.
	Breaking bool
}

// breakingFooters are the footers that declare a breaking change without the
// "!" marker. Conventional Commits 1.0.0 makes the hyphenated form a synonym.
// One is matched at the start of a body line.
var breakingFooters = []string{"BREAKING CHANGE:", "BREAKING-CHANGE:"}

// ParseConventionalCommit reads a commit message as a conventional commit:
// "type(scope)!: summary" in the subject, and an optional "BREAKING CHANGE:"
// or "BREAKING-CHANGE:" footer line in the body.
//
// ok is false when the subject does not have that shape: no colon, a scope
// that is not closed, or a type that is empty or holds anything but lowercase
// ASCII letters. A merge commit, a Git revert subject and a hand-written
// message are recognized that way. Such a message declares nothing, whatever
// its body says.
//
// Every reader of the breaking marker calls this function, so the version
// bump, the changelog and the API checks agree on which commit is breaking.
func ParseConventionalCommit(subject, body string) (commit ConventionalCommit, ok bool) {
	head, _, found := strings.Cut(subject, ":")
	if !found {
		return ConventionalCommit{}, false
	}
	head = strings.TrimSpace(head)
	if strings.HasSuffix(head, "!") {
		commit.Breaking = true
		head = strings.TrimSuffix(head, "!")
	}
	if open := strings.IndexByte(head, '('); open >= 0 {
		if !strings.HasSuffix(head, ")") {
			return ConventionalCommit{}, false
		}
		commit.Scope = head[open+1 : len(head)-1]
		head = head[:open]
	}
	commit.Type = strings.TrimSpace(head)
	if !isConventionalType(commit.Type) {
		return ConventionalCommit{}, false
	}
	if !commit.Breaking {
		commit.Breaking = hasBreakingFooter(body)
	}
	return commit, true
}

// hasBreakingFooter reports whether a line of body starts with a breaking
// footer, leading spaces aside.
func hasBreakingFooter(body string) bool {
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

// isConventionalType reports whether s is a conventional commit type: one or
// more lowercase ASCII letters. A sentence, a ticket id, a path or "WIP" is
// not a type.
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
