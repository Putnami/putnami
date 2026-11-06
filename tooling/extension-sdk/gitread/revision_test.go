package gitread_test

import (
	"go.putnami.dev/sdk/extension/gitread"
	"reflect"
	"strings"
	"testing"
)

func TestHeadSHA(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	got, err := gitread.HeadSHA(dir)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	want := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	if got != want {
		t.Errorf("HeadSHA = %q, want %q", got, want)
	}
}

func TestResolveCommitRejectsUnsafeAndAmbiguousRevisionArguments(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	sha := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	resolved, err := gitread.ResolveCommit(repo, sha)
	if err != nil || resolved != strings.ToLower(sha) {
		t.Fatalf("ResolveCommit(full SHA) = %q, %v", resolved, err)
	}

	runGit(t, repo, "branch", "shared", sha)
	runGit(t, repo, "tag", "shared", sha)
	if _, err := gitread.ResolveCommit(repo, "shared"); err == nil {
		t.Fatal("ambiguous short ref resolved without an error")
	}

	for _, revision := range []string{"", " HEAD", "HEAD ", "-c", "HEAD\nmain", strings.Repeat("x", 1025)} {
		if _, err := gitread.ResolveCommit(repo, revision); err == nil {
			t.Errorf("unsafe revision %q resolved without an error", revision)
		}
	}
}

func TestValidObjectIDAcceptsAbbreviatedAndFullNamesOnly(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"abcdef0":               true,
		"ABCDEF0":               true,
		strings.Repeat("f", 64): true,
		"abcdef":                false,
		strings.Repeat("a", 65): false,
		"abcdefg":               false,
		"-abcdef0":              false,
	} {
		if got := gitread.ValidObjectID(id); got != want {
			t.Errorf("ValidObjectID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestSplitNULDropsOnlyTheFinalTerminator(t *testing.T) {
	t.Parallel()
	cases := []struct {
		output string
		want   []string
	}{
		{"", []string{}},
		{"a\x00b", []string{"a", "b"}},
		{"\x00", []string{""}},
		{"a b\x00c\nd\x00", []string{"a b", "c\nd"}},
	}
	for _, c := range cases {
		if got := gitread.SplitNUL(c.output); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitNUL(%q) = %q, want %q", c.output, got, c.want)
		}
	}
}

func TestParseStageEntryReadsModeObjectStageAndPath(t *testing.T) {
	t.Parallel()
	mode, objectID, stage, path, ok := gitread.ParseStageEntry("100644 0123456789abcdef0123456789abcdef01234567 0\tdir/a file.txt")
	if !ok || mode != "100644" || objectID != "0123456789abcdef0123456789abcdef01234567" || stage != "0" || path != "dir/a file.txt" {
		t.Fatalf("ParseStageEntry = %q %q %q %q %v", mode, objectID, stage, path, ok)
	}
	for _, malformed := range []string{"malformed", "100644 abc\tpath", "100644 abc 0 extra\tpath"} {
		if _, _, _, _, ok := gitread.ParseStageEntry(malformed); ok {
			t.Errorf("ParseStageEntry(%q) accepted a malformed entry", malformed)
		}
	}
}
