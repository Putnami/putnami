package history

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
	"go.putnami.dev/intelligence/agent-readiness/internal/gittest"
)

func TestReadFindsMergeRequestsInTheBody(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	r := gittest.New(t)
	r.Write("a.txt", "one\n")
	r.Commit("init", "dev@acme.example", now.Add(-72*time.Hour))
	r.Git("checkout", "-q", "-b", "feature")
	r.Write("a.txt", "one\ntwo\nthree\n")
	r.Commit("feat: two", "dev@acme.example", now.Add(-48*time.Hour))
	r.Git("checkout", "-q", "main")
	r.Merge("feature", "merged: KRA-1 two\n\nacme/core!12 'feature' merged into 'main'\n\n* 3 files changed", "dev@acme.example", now.Add(-24*time.Hour))
	r.Write("a.txt", "one\n")
	r.Commit("direct push\n\nnothing to see here", "dev@acme.example", now.Add(-time.Hour))

	repo, err := gitrepo.Open(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Read(context.Background(), repo, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Changes) != 3 {
		t.Fatalf("read %d changes, want 3: %+v", len(h.Changes), h.Changes)
	}
	direct, merged := h.Changes[0], h.Changes[1]
	if direct.ViaPullRequest || direct.Lines != 2 {
		t.Errorf("direct push = %+v, want no merge request and 2 lines", direct)
	}
	if !merged.ViaPullRequest || merged.Lines != 2 {
		t.Errorf("merge = %+v, want a merge request of 2 lines", merged)
	}
}

func TestAddedMatchesCountsOnlyTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	r := gittest.New(t)
	r.Write("a_test.go", "t.Skip(\"old\")\n")
	r.Write("b_test.go", "ok\n")
	r.Write("dir with space/c.ts", "x\n")
	r.Commit("init", "dev@acme.example", now.Add(-200*24*time.Hour))
	r.Write("a_test.go", "t.Skip(\"old\")\nt.Skip(\"new\")\n")
	r.Write("b_test.go", "ok\nit.only('x')\n")
	r.Write("dir with space/c.ts", "x\ndescribe.skip('y')\n")
	r.Commit("skip more", "dev@acme.example", now.Add(-24*time.Hour))
	r.Write("b_test.go", "ok\n")
	r.Commit("unfocus", "dev@acme.example", now.Add(-time.Hour))
	r.Git("mv", "a_test.go", "moved_test.go")
	r.Write("moved_test.go", "t.Skip(\"old\")\nt.Skip(\"new\")\nok\n")
	r.Commit("move", "dev@acme.example", now.Add(-time.Minute))

	repo, err := gitrepo.Open(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AddedMatches(context.Background(), repo, now, `t\.Skip\(|\.skip\(|\.only\(`, []string{"moved_test.go", "b_test.go", "dir with space/c.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A move carries its lines over, so none of them count as added. The
	// pass follows the file's current name only: the skip added before the
	// move is not counted either.
	want := map[string]int{"b_test.go": 1, "dir with space/c.ts": 1}
	if len(got) != len(want) {
		t.Fatalf("AddedMatches = %v, want %v", got, want)
	}
	for file, count := range want {
		if got[file] != count {
			t.Fatalf("AddedMatches = %v, want %v", got, want)
		}
	}
	if _, err := AddedMatches(context.Background(), repo, now, `(`, nil, nil); err == nil {
		t.Fatal("an invalid pattern was accepted")
	}
}

func TestAddedMatchesNeverRunsRepositoryTextconv(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sentinel := filepath.Join(t.TempDir(), "textconv-ran")
	t.Setenv("TEXTCONV_SENTINEL", sentinel)
	script := filepath.Join(t.TempDir(), "textconv.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'ran\\n' >> \"$TEXTCONV_SENTINEL\"\ncat \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := gittest.New(t)
	r.Write(".gitattributes", "*.go diff=spy\n")
	r.Write("a_test.go", "package a\n")
	r.Commit("initial", "dev@acme.example", now.Add(-48*time.Hour))
	r.Git("config", "diff.spy.textconv", "sh "+strconv.Quote(script))
	r.Write("a_test.go", "package a\nt.Skip(\"new\")\n")
	r.Commit("add skip", "dev@acme.example", now.Add(-24*time.Hour))

	// Verify the fixture's configured helper would run for an ordinary log.
	r.Git("log", "-p", "--format=", "HEAD", "--", "a_test.go")
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("textconv fixture did not run: %v", err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}

	repo, err := gitrepo.Open(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AddedMatches(context.Background(), repo, now, `t\.Skip\(`, []string{"a_test.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["a_test.go"] != 1 {
		t.Fatalf("AddedMatches = %v, want one added skip", got)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("repository textconv helper ran during collection: %v", err)
	}
}

// TestAddedMatchesHandsTheGuardToKeep checks that keep sees the unchanged
// lines around an added match, such as the condition that guards a skip.
func TestAddedMatchesHandsTheGuardToKeep(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	r := gittest.New(t)
	r.Write("a_test.go", "func TestA(t *testing.T) {\n\tif runtime.GOOS == \"windows\" {\n\t}\n}\n")
	r.Commit("init", "dev@acme.example", now.Add(-200*24*time.Hour))
	r.Write("a_test.go", "func TestA(t *testing.T) {\n\tif runtime.GOOS == \"windows\" {\n\t\tt.Skip(\"x\")\n\t}\n\tt.Skip(\"y\")\n}\n")
	r.Commit("skip", "dev@acme.example", now.Add(-24*time.Hour))

	repo, err := gitrepo.Open(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	got, err := AddedMatches(context.Background(), repo, now, `t\.Skip\(`, []string{"a_test.go"}, func(_ string, lines []string, i int) bool {
		seen = append(seen, lines[i-1])
		return lines[i-1] != "\tif runtime.GOOS == \"windows\" {"
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["a_test.go"] != 1 || len(seen) != 2 {
		t.Fatalf("AddedMatches = %v after seeing %q, want the unguarded skip only", got, seen)
	}
}

func TestReadMarksRevertedChanges(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	r := gittest.New(t)
	r.Write("a.go", "package a\n")
	r.Commit("init", "dev@acme.example", now.Add(-30*day))
	r.Git("checkout", "-q", "-b", "feature")
	r.Write("a.go", "package a\n\nvar two = 2\n")
	branch := r.Commit("feat: two", "dev@acme.example", now.Add(-22*day))
	r.Write("a_test.go", "package a\n")
	r.Commit("test: two", "dev@acme.example", now.Add(-21*day))
	r.Git("checkout", "-q", "main")
	merge := r.Merge("feature", "Merge pull request #5 from acme/feature\n\nfeat: two", "dev@acme.example", now.Add(-20*day))
	// A revert made on a branch before it lands undoes nothing on HEAD.
	r.Git("checkout", "-q", "-b", "draft")
	r.Write("c.go", "package a\n")
	tried := r.Commit("feat: draft", "dev@acme.example", now.Add(-19*day))
	r.Git("revert", "--no-commit", tried)
	r.Commit("Revert \"feat: draft\"\n\nThis reverts commit "+tried+".", "dev@acme.example", now.Add(-19*day+time.Hour))
	r.Write("d.go", "package a\n")
	r.Commit("feat: draft, again", "dev@acme.example", now.Add(-19*day+2*time.Hour))
	r.Git("checkout", "-q", "main")
	drafted := r.Merge("draft", "Merge pull request #6 from acme/draft\n\nfeat: draft", "dev@acme.example", now.Add(-17*day))
	r.Write("b.go", "package a\n")
	three := r.Commit("feat: three", "dev@acme.example", now.Add(-15*day))
	r.Git("revert", "--no-commit", branch)
	r.Commit("Revert \"feat: two\"\n\nThis reverts commit "+branch+".", "dev@acme.example", now.Add(-16*day))
	r.Git("rm", "-q", "b.go")
	r.Commit("Revert \"feat: three\"", "dev@acme.example", now.Add(-12*day))

	repo, err := gitrepo.Open(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Read(context.Background(), repo, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	byHash := map[string]Change{}
	for _, change := range h.Changes {
		byHash[change.Hash] = change
	}
	landed := byHash[merge]
	if landed.Title != "feat: two" || len(landed.Files) != 2 || landed.Files[0] != "a.go" || landed.Lines != 3 {
		t.Fatalf("merge change = %+v, want title \"feat: two\", 2 files and 3 lines", landed)
	}
	if landed.RevertedAt == 0 {
		t.Fatalf("merge change = %+v, want it reverted through a branch commit that is not its last", landed)
	}
	if byHash[drafted].RevertedAt != 0 {
		t.Fatalf("merge change = %+v, want a revert made on its branch before it landed to undo nothing", byHash[drafted])
	}
	if byHash[three].RevertedAt == 0 {
		t.Fatalf("feat: three = %+v, want it reverted by the subject its revert quotes", byHash[three])
	}
}

// TestMarkChangesRevertedPrefersIds pins that a revert naming a commit git
// knows never falls back to the subject it quotes.
func TestMarkChangesRevertedPrefersIds(t *testing.T) {
	changes := []Change{
		{Hash: "bbbb", Time: 20, Subject: "feat: a"},
		{Hash: "aaaa", Time: 10, Subject: "feat: a"},
	}
	landing := func(hash string) string {
		if hash == "cccc" {
			return "aaaa"
		}
		return ""
	}
	MarkChangesReverted(changes, []Revert{{Time: 30, Hashes: []string{"cccc"}, Subjects: []string{"feat: a"}}}, landing)
	if changes[0].RevertedAt != 0 || changes[1].RevertedAt != 30 {
		t.Fatalf("changes = %+v, want only the change that landed cccc reverted", changes)
	}
	MarkChangesReverted(changes, []Revert{{Time: 25, Hashes: []string{"dddd"}, Subjects: []string{"feat: a"}}}, landing)
	if changes[0].RevertedAt != 25 || changes[1].RevertedAt != 30 {
		t.Fatalf("changes = %+v, want the unknown id to fall back to the newest change with the subject", changes)
	}
	squashed := []Change{{Hash: "eeee", Time: 10, Subject: "Make `FailFast` hashable (#12)", Title: "Make `FailFast` hashable (#12)"}}
	MarkChangesReverted(squashed, []Revert{{Time: 20, Subjects: []string{"Make `FailFast` hashable"}}}, landing)
	if squashed[0].RevertedAt != 20 {
		t.Fatalf("squashed change = %+v, want it reverted by the title its revert quotes", squashed[0])
	}
}
