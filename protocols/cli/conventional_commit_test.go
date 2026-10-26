package cli

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// conventionalCase is one message and what ParseConventionalCommit reads in
// it. A case with ok false expects the zero commit.
type conventionalCase struct {
	name, subject, body string
	want                ConventionalCommit
	ok                  bool
}

func runConventionalCases(t *testing.T, cases []conventionalCase) {
	t.Helper()
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseConventionalCommit(testCase.subject, testCase.body)
			if got != testCase.want || ok != testCase.ok {
				t.Errorf("ParseConventionalCommit(%q, %q) = %+v/%v, want %+v/%v",
					testCase.subject, testCase.body, got, ok, testCase.want, testCase.ok)
			}
		})
	}
}

// The subject gives the type, the scope and the "!" marker.
func TestParseConventionalCommitReadsTheSubject(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "a-commit-is-read-one-way", "subjects-and-footers-are-read")
	t.Parallel()
	runConventionalCases(t, []conventionalCase{
		{name: "plain type", subject: "feat: add", want: ConventionalCommit{Type: "feat"}, ok: true},
		{name: "scoped type", subject: "fix(cli): stop", want: ConventionalCommit{Type: "fix", Scope: "cli"}, ok: true},
		{name: "breaking marker", subject: "feat!: change", want: ConventionalCommit{Type: "feat", Breaking: true}, ok: true},
		{name: "scoped breaking marker", subject: "feat(cli)!: change",
			want: ConventionalCommit{Type: "feat", Scope: "cli", Breaking: true}, ok: true},
		{name: "revert type", subject: "revert: let us never again speak of the noodle incident",
			want: ConventionalCommit{Type: "revert"}, ok: true},
		{name: "scope with a path", subject: "fix(tooling/cli): stop",
			want: ConventionalCommit{Type: "fix", Scope: "tooling/cli"}, ok: true},
		{name: "colon in the summary", subject: "docs: explain feat!: and fix:", want: ConventionalCommit{Type: "docs"}, ok: true},
	})
}

// The body declares a breaking change with a footer line. The token is
// uppercase, and the hyphenated form is its synonym.
func TestParseConventionalCommitReadsTheBreakingFooter(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "a-commit-is-read-one-way", "subjects-and-footers-are-read")
	t.Parallel()
	runConventionalCases(t, []conventionalCase{
		{name: "breaking footer", subject: "refactor: move", body: "why\n\nBREAKING CHANGE: the flag is gone",
			want: ConventionalCommit{Type: "refactor", Breaking: true}, ok: true},
		{name: "hyphenated footer", subject: "refactor: move", body: "why\n\nBREAKING-CHANGE: the flag is gone",
			want: ConventionalCommit{Type: "refactor", Breaking: true}, ok: true},
		{name: "hyphenated footer on the first body line", subject: "fix: move", body: "BREAKING-CHANGE: the flag is gone",
			want: ConventionalCommit{Type: "fix", Breaking: true}, ok: true},
		{name: "indented footer", subject: "refactor: move", body: "why\n\n  BREAKING CHANGE: gone",
			want: ConventionalCommit{Type: "refactor", Breaking: true}, ok: true},
		{name: "footer after other footers", subject: "fix: move", body: "why\n\nRefs: #1\nBREAKING CHANGE: gone\nReviewed-by: Z",
			want: ConventionalCommit{Type: "fix", Breaking: true}, ok: true},
		{name: "footer with carriage returns", subject: "fix: move", body: "why\r\n\r\nBREAKING CHANGE: gone\r\n",
			want: ConventionalCommit{Type: "fix", Breaking: true}, ok: true},
		{name: "marker and footer", subject: "feat(cli)!: change", body: "BREAKING CHANGE: gone",
			want: ConventionalCommit{Type: "feat", Scope: "cli", Breaking: true}, ok: true},
		{name: "footer in prose", subject: "docs: explain", body: "a line that mentions BREAKING CHANGE: later",
			want: ConventionalCommit{Type: "docs"}, ok: true},
		{name: "lowercase footer token", subject: "fix: move", body: "breaking change: gone",
			want: ConventionalCommit{Type: "fix"}, ok: true},
		{name: "footer token without a colon", subject: "fix: move", body: "BREAKING CHANGE #12",
			want: ConventionalCommit{Type: "fix"}, ok: true},
		{name: "multi-line body without a footer", subject: "fix: move", body: "first paragraph\n\nsecond paragraph\n\nRefs: #1",
			want: ConventionalCommit{Type: "fix"}, ok: true},
	})
}

// A subject without the conventional shape is no conventional commit, and its
// body declares nothing.
func TestParseConventionalCommitRejectsOtherSubjects(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "a-commit-is-read-one-way", "subjects-and-footers-are-read")
	t.Parallel()
	runConventionalCases(t, []conventionalCase{
		{name: "no colon", subject: "just a message"},
		{name: "no colon with footer", subject: "just a message", body: "BREAKING CHANGE: ignored"},
		{name: "unclosed scope", subject: "feat(cli: add"},
		{name: "unclosed scope with marker", subject: "feat(cli!: add"},
		{name: "space between the scope and the marker", subject: "feat(cli) !: add"},
		{name: "text after the scope", subject: "feat(cli)x: add"},
		{name: "non-letter type", subject: "WIP: add"},
		{name: "non-letter type with marker", subject: "WIP!: add"},
		{name: "empty type", subject: "!: add"},
		{name: "empty subject"},
		{name: "merge commit", subject: "Merge pull request #1 from a/b", body: "BREAKING CHANGE: ignored"},
		{name: "merge commit with a colon", subject: "Merge branch 'main' of github.com:a/b"},
		{name: "git revert subject", subject: `Revert "feat!: drop the flag"`, body: "This reverts commit 0123abc.\n\nBREAKING CHANGE: ignored"},
		{name: "fixup subject", subject: "fixup! feat: add"},
	})
}

// These readings differ from Conventional Commits 1.0.0, which reads a type
// in any case, wants a space and a description after the colon, and reads a
// footer only in the last paragraph. They are held because the version bump
// and the API checks read them this way.
func TestParseConventionalCommitHoldsItsReadingsOutsideTheSpecification(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "a-commit-is-read-one-way", "the-departures-from-the-specification-are-pinned")
	t.Parallel()
	runConventionalCases(t, []conventionalCase{
		{name: "capitalized type", subject: "Feat: add"},
		{name: "uppercase type", subject: "FIX(cli)!: stop"},
		{name: "no space after the colon", subject: "feat:add", want: ConventionalCommit{Type: "feat"}, ok: true},
		{name: "no description", subject: "feat:", want: ConventionalCommit{Type: "feat"}, ok: true},
		{name: "empty scope", subject: "feat(): add", want: ConventionalCommit{Type: "feat"}, ok: true},
		{name: "spaces around the type", subject: " feat (cli)!: add",
			want: ConventionalCommit{Type: "feat", Scope: "cli", Breaking: true}, ok: true},
		{name: "space before the marker", subject: "feat !: add", want: ConventionalCommit{Type: "feat", Breaking: true}, ok: true},
		{name: "footer before the last paragraph", subject: "fix: move", body: "BREAKING CHANGE: gone\n\na closing paragraph",
			want: ConventionalCommit{Type: "fix", Breaking: true}, ok: true},
		{name: "footer without a space after the colon", subject: "fix: move", body: "BREAKING CHANGE:gone",
			want: ConventionalCommit{Type: "fix", Breaking: true}, ok: true},
	})
}
