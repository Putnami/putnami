package git

import (
	"errors"
	"testing"
)

func TestParseConventional(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, subject, body, typ string
		breaking, ok             bool
	}{
		{name: "plain type", subject: "feat: add", typ: "feat", ok: true},
		{name: "scoped type", subject: "fix(cli): stop", typ: "fix", ok: true},
		{name: "breaking marker", subject: "feat(cli)!: change", typ: "feat", breaking: true, ok: true},
		{name: "breaking footer", subject: "refactor: move", body: "why\n\nBREAKING CHANGE: the flag is gone",
			typ: "refactor", breaking: true, ok: true},
		{name: "hyphenated breaking footer", subject: "fix: move", body: "BREAKING-CHANGE: the flag is gone",
			typ: "fix", breaking: true, ok: true},
		{name: "no colon", subject: "just a message"},
		{name: "unclosed scope", subject: "feat(cli: add"},
		{name: "non-letter type", subject: "WIP: add"},
		{name: "merge commit", subject: "Merge pull request #1 from a/b"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			typ, breaking, ok := ParseConventional(testCase.subject, testCase.body)
			if typ != testCase.typ || breaking != testCase.breaking || ok != testCase.ok {
				t.Errorf("ParseConventional(%q) = %q/%v/%v, want %q/%v/%v",
					testCase.subject, typ, breaking, ok, testCase.typ, testCase.breaking, testCase.ok)
			}
		})
	}
}

// BumpFor reduces a range to its strongest advance. The pre-1.0 reading makes
// a breaking change a minor and a feature a patch: before 1.0 the major number
// is not yet a compatibility promise, and a minor is kept for what may need a
// migration.
func TestBumpFor(t *testing.T) {
	t.Parallel()
	commits := []Commit{
		{Subject: "docs: rewrite"},
		{Subject: "fix: stop"},
		{Subject: "feat: add"},
	}
	if got, _ := BumpFor(commits, true, nil); got != BumpPatch {
		t.Errorf("BumpFor(feat+fix+docs, preOne) = %v, want patch before 1.0", got)
	}
	if got, _ := BumpFor(commits, false, nil); got != BumpMinor {
		t.Errorf("BumpFor(feat+fix+docs, post 1.0) = %v, want minor", got)
	}
	breaking := append(append([]Commit(nil), commits...), Commit{Subject: "refactor!: move"})
	if got, _ := BumpFor(breaking, true, nil); got != BumpMinor {
		t.Errorf("BumpFor(breaking, preOne) = %v, want minor before 1.0", got)
	}
	if got, _ := BumpFor(breaking, false, nil); got != BumpMajor {
		t.Errorf("BumpFor(breaking, post 1.0) = %v, want major", got)
	}
	if got, _ := BumpFor([]Commit{{Subject: "docs: rewrite"}, {Subject: "chore: tidy"}}, true, nil); got != BumpNone {
		t.Errorf("BumpFor(docs+chore) = %v, want none", got)
	}
	if got, _ := BumpFor(nil, true, nil); got != BumpNone {
		t.Errorf("BumpFor(nothing) = %v, want none", got)
	}
}

// A commit the stability test reads as touching no stable project advances a
// patch at most. The test is asked only where its answer can raise the result,
// and its error stops the reduction.
func TestBumpForCapsACommitThatTouchesNoStableProject(t *testing.T) {
	t.Parallel()
	var asked []string
	stable := func(commit Commit) (bool, error) {
		asked = append(asked, commit.SHA)
		return commit.SHA == "stable", nil
	}
	bump := func(preOne bool, commits ...Commit) Bump {
		t.Helper()
		got, err := BumpFor(commits, preOne, stable)
		if err != nil {
			t.Fatalf("BumpFor: %v", err)
		}
		return got
	}
	if got := bump(true, Commit{SHA: "lab", Subject: "feat!: drop"}); got != BumpPatch {
		t.Errorf("BumpFor(breaking, unstable) = %v, want patch", got)
	}
	if got := bump(false, Commit{SHA: "lab", Subject: "feat: add"}); got != BumpPatch {
		t.Errorf("BumpFor(feat post 1.0, unstable) = %v, want patch", got)
	}
	if got := bump(false, Commit{SHA: "stable", Subject: "feat!: drop"}); got != BumpMajor {
		t.Errorf("BumpFor(breaking post 1.0, stable) = %v, want major", got)
	}

	asked = nil
	if got := bump(true, Commit{SHA: "a", Subject: "feat: add"}, Commit{SHA: "b", Subject: "fix: stop"}); got != BumpPatch {
		t.Errorf("BumpFor(feat+fix, preOne) = %v, want patch", got)
	}
	if len(asked) != 0 {
		t.Errorf("the stability test was asked for %v, a range it cannot raise past a patch", asked)
	}

	asked = nil
	if got := bump(true, Commit{SHA: "stable", Subject: "fix!: rename"}, Commit{SHA: "later", Subject: "feat!: drop"}); got != BumpMinor {
		t.Errorf("BumpFor(two breaking, preOne) = %v, want minor", got)
	}
	if len(asked) != 1 {
		t.Errorf("the stability test was asked for %v; the walk stops at the pre-1.0 ceiling", asked)
	}

	failing := func(Commit) (bool, error) { return false, errors.New("git failed") }
	if _, err := BumpFor([]Commit{{Subject: "feat!: drop"}}, true, failing); err == nil {
		t.Error("BumpFor swallowed the stability test's error")
	}
}

// A pre-release is always strictly above the last tag; an explicit release
// applies the bump exactly, and BumpNone still releases — as a patch.
func TestNextVersion(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, last string
		bump       Bump
		prerelease bool
		want       string
	}{
		{name: "prerelease floors at a patch", last: "0.4.0", bump: BumpNone, prerelease: true, want: "0.4.1"},
		{name: "prerelease keeps a minor", last: "0.4.0", bump: BumpMinor, prerelease: true, want: "0.5.0"},
		{name: "explicit none is a patch", last: "0.4.0", bump: BumpNone, want: "0.4.1"},
		{name: "explicit major", last: "1.2.3", bump: BumpMajor, want: "2.0.0"},
		{name: "explicit minor resets the patch", last: "1.2.3", bump: BumpMinor, want: "1.3.0"},
		{name: "v prefix is tolerated", last: "v1.2.3", bump: BumpPatch, want: "1.2.4"},
		{name: "a pre-release tag is read as its core", last: "1.2.3-rc.1", bump: BumpPatch, want: "1.2.4"},
		{name: "an unreadable last version starts at zero", last: "not-a-version", bump: BumpMinor, want: "0.1.0"},
		{name: "no last version starts at zero", last: "", bump: BumpPatch, want: "0.0.1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := NextVersion(testCase.last, testCase.bump, testCase.prerelease); got != testCase.want {
				t.Errorf("NextVersion(%q, %v, %v) = %q, want %q",
					testCase.last, testCase.bump, testCase.prerelease, got, testCase.want)
			}
		})
	}
}

func TestBumpString(t *testing.T) {
	t.Parallel()
	for bump, want := range map[Bump]string{BumpNone: "none", BumpPatch: "patch", BumpMinor: "minor", BumpMajor: "major"} {
		if got := bump.String(); got != want {
			t.Errorf("Bump(%d).String() = %q, want %q", bump, got, want)
		}
	}
}
