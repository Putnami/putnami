package gitbase

import (
	"context"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestAddedLinesAreTheZeroContextHunksOfTheDiff(t *testing.T) {
	spectest.Proves(t, "tooling/observe-analyses", "changed-line-coverage", "changed-lines-are-measured-against-line-coverage")
	dir := t.TempDir()
	run(t, dir, "init", "--quiet", "--initial-branch=main")
	write(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
	write(t, dir, "gone.txt", "bye\n")
	write(t, dir, "old name.txt", "same\ncontent\nhere\n")
	run(t, dir, "add", "--all")
	run(t, dir, "commit", "--quiet", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")
	write(t, dir, "a.txt", "one\nTWO\nthree\nfour\nfive\nsix\n")
	write(t, dir, "new.txt", "x\n")
	run(t, dir, "rm", "--quiet", "gone.txt")
	run(t, dir, "mv", "old name.txt", "new name.txt")
	write(t, dir, "new name.txt", "same\ncontent\nhere\nmore\n")
	run(t, dir, "add", "--all")
	run(t, dir, "commit", "--quiet", "-m", "head")
	head := run(t, dir, "rev-parse", "HEAD")

	lines, err := AddedLines(context.Background(), dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int{"a.txt": {2, 5, 6}, "new.txt": {1}, "new name.txt": {4}}
	if len(lines) != len(want) {
		t.Errorf("lines %v, want %v", lines, want)
	}
	for file, numbers := range want {
		if !slices.Equal(lines[file], numbers) {
			t.Errorf("%s: %v, want %v", file, lines[file], numbers)
		}
	}
	data, found, err := Show(context.Background(), dir, base, "gone.txt")
	if err != nil || !found || string(data) != "bye\n" {
		t.Errorf("show base gone.txt: %q %v %v", data, found, err)
	}
	if _, found, err := Show(context.Background(), dir, head, "gone.txt"); err != nil || found {
		t.Errorf("show head gone.txt: %v %v", found, err)
	}
	if _, err := AddedLines(context.Background(), dir, "nope", head); err == nil {
		t.Error("an unknown base reads")
	}
}

func TestParseAddedLines(t *testing.T) {
	spectest.Proves(t, "tooling/observe-analyses", "changed-line-coverage", "changed-lines-are-measured-against-line-coverage")
	diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n@@ -5,2 +5,0 @@\n-c\n-d\n" +
		"diff --git a/q b/q\n--- a/q\n+++ \"b/tab\\there\"\n@@ -0,0 +1,2 @@\n+1\n+2\n" +
		"diff --git a/d b/d\n--- a/d\n+++ /dev/null\n@@ -1 +0,0 @@\n-gone\n" +
		"diff --git a/bin b/bin\nBinary files a/bin and b/bin differ\n"
	lines, err := parseAddedLines([]byte(diff))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines["x"], []int{1}) || !slices.Equal(lines["tab\there"], []int{1, 2}) || len(lines) != 2 {
		t.Errorf("lines %v", lines)
	}
	if _, err := parseAddedLines([]byte("+++ b/x\n@@ -1 +a @@\n")); err == nil {
		t.Error("a malformed hunk header parses")
	}
	if _, err := parseAddedLines([]byte("+++ b/x\n@@ -1\n")); err == nil {
		t.Error("a truncated hunk header parses")
	}
}
