package apicheck

import "testing"

// The cases are the version bump's own (tooling/cli/internal/git
// TestParseConventional): a message this check reads as breaking is one the
// bump reads as breaking, and the reverse.
func TestDeclaresBreakingReadsMessagesAsTheVersionBumpDoes(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, subject, body string
		breaking            bool
	}{
		{name: "plain type", subject: "feat: add"},
		{name: "scoped type", subject: "fix(cli): stop"},
		{name: "breaking marker", subject: "feat!: change", breaking: true},
		{name: "scoped breaking marker", subject: "feat(cli)!: change", breaking: true},
		{name: "breaking footer", subject: "refactor: move", body: "why\n\nBREAKING CHANGE: the flag is gone", breaking: true},
		{name: "indented footer", subject: "refactor: move", body: "why\n\n  BREAKING CHANGE: gone", breaking: true},
		{name: "footer in prose", subject: "docs: explain", body: "a line that mentions BREAKING CHANGE: later"},
		{name: "no colon", subject: "just a message"},
		{name: "no colon with footer", subject: "just a message", body: "BREAKING CHANGE: ignored"},
		{name: "unclosed scope", subject: "feat(cli!: add"},
		{name: "non-letter type", subject: "WIP!: add"},
		{name: "empty type", subject: "!: add"},
		{name: "merge commit", subject: "Merge pull request #1 from a/b", body: "BREAKING CHANGE: ignored"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := declaresBreaking(testCase.subject, testCase.body); got != testCase.breaking {
				t.Errorf("declaresBreaking(%q, %q) = %v, want %v", testCase.subject, testCase.body, got, testCase.breaking)
			}
		})
	}
}
