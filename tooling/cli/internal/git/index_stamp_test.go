package git

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stampOf reads the stamp of dir and fails unless it is one.
func stampOf(t *testing.T, dir string) IndexStamp {
	t.Helper()
	stamp, ok := ReadIndexStamp(dir)
	if !ok {
		t.Fatalf("ReadIndexStamp(%s) answered no stamp", dir)
	}
	return stamp
}

// An index stamp holds when nothing touched the index and changes when git
// rewrites it, a rewrite that keeps the size and the modification time
// included: the checksum at the end of the index changes.
func TestReadIndexStampFollowsTheIndex(t *testing.T) {
	repo := initGitRepo(t)
	write(t, filepath.Join(repo, "a.txt"), "a\n")
	runGit(t, repo, "add", "a.txt")
	first := stampOf(t, filepath.Join(repo, "sub"))
	if !first.exists {
		t.Fatalf("stamp = %+v, want the index of the repository", first)
	}
	if again := stampOf(t, repo); again != first {
		t.Fatalf("stamp of an untouched index = %+v, want %+v", again, first)
	}

	write(t, filepath.Join(repo, "b.txt"), "b\n")
	runGit(t, repo, "add", "b.txt")
	added := stampOf(t, repo)
	if added == first {
		t.Fatalf("stamp after git add = %+v, want a change", added)
	}

	// git mv between names of one length rewrites the index at its size;
	// restoring the modification time leaves the checksum alone to tell.
	runGit(t, repo, "mv", "b.txt", "c.txt")
	index := filepath.Join(repo, ".git", "index")
	if err := os.Chtimes(index, added.mod, added.mod); err != nil {
		t.Fatal(err)
	}
	moved := stampOf(t, repo)
	if moved.size != added.size || !moved.mod.Equal(added.mod) {
		t.Fatalf("the rewrite moved size or time: %+v, %+v", moved, added)
	}
	if moved == added {
		t.Fatalf("stamp after a same-size rewrite in the same tick = %+v, want a change", moved)
	}
}

// writeIndex writes a fake index of content under gitDir, at the time at.
func writeIndex(t *testing.T, gitDir string, content []byte, at time.Time) {
	t.Helper()
	path := filepath.Join(gitDir, "index")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// Each member of the stamp tells two indexes apart on its own: the size, the
// modification time and the checksum. An index whose checksum is zeros
// (index.skipHash) has no stamp, so its reader reads it again.
func TestAnIndexStampComparesEachMember(t *testing.T) {
	ws := t.TempDir()
	gitDir := filepath.Join(ws, ".git")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	trailer := bytes.Repeat([]byte{0xab}, indexTrailerLen)
	base := append([]byte("DIRC-entries"), trailer...)

	writeIndex(t, gitDir, base, at)
	first := stampOf(t, ws)
	writeIndex(t, gitDir, append([]byte("DIRC-entries-and-more"), trailer...), at)
	if grown := stampOf(t, ws); grown == first {
		t.Error("an index of another size with the same time and checksum has the same stamp")
	}
	writeIndex(t, gitDir, base, at.Add(time.Second))
	if later := stampOf(t, ws); later == first {
		t.Error("an index of another time with the same size and checksum has the same stamp")
	}
	writeIndex(t, gitDir, append([]byte("DIRC-entries"), bytes.Repeat([]byte{0xcd}, indexTrailerLen)...), at)
	if rewritten := stampOf(t, ws); rewritten == first {
		t.Error("an index of another checksum with the same size and time has the same stamp")
	}

	// A SHA-1 index with index.skipHash ends in 20 zero bytes, after entry
	// bytes that are not zero.
	skipHash := append(bytes.Repeat([]byte{0x5a}, indexTrailerLen-sha1TrailerLen), make([]byte, sha1TrailerLen)...)
	writeIndex(t, gitDir, append([]byte("DIRC-entries"), skipHash...), at)
	if _, ok := ReadIndexStamp(ws); ok {
		t.Error("an index without a checksum has a stamp")
	}
}

// The stamp finds the index where git does: through a gitfile whose relative
// gitdir names it from the gitfile's directory, as a submodule's does. A
// GIT_CEILING_DIRECTORIES entry that hides it from WorkTreeEntry hides nothing
// from the git a run starts, which never sees the variable, so the stamp then
// answers none, and so it does with $GIT_DIR set. Outside any work tree it
// says there is no index.
func TestReadIndexStampFindsTheIndexGitReads(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	content := append([]byte("DIRC"), bytes.Repeat([]byte{0x11}, indexTrailerLen)...)
	writeIndex(t, filepath.Join(root, "modules", "sub"), content, at)
	ws := filepath.Join(root, "super", "sub")
	write(t, filepath.Join(ws, ".git"), "gitdir: ../../modules/sub\n")
	inner := filepath.Join(ws, "apps", "api")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := stampOf(t, inner)
	if !stamp.exists || stamp.size != int64(len(content)) {
		t.Fatalf("stamp through a relative gitdir = %+v, want the index of modules/sub", stamp)
	}

	t.Setenv("GIT_CEILING_DIRECTORIES", ws)
	if _, ok := ReadIndexStamp(inner); ok {
		t.Error("a stamp with a ceiling between the workspace and its repository")
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", "")

	t.Setenv("GIT_DIR", filepath.Join(root, "modules", "sub"))
	if _, ok := ReadIndexStamp(inner); ok {
		t.Error("a stamp with $GIT_DIR set")
	}
	t.Setenv("GIT_DIR", "")

	outside := t.TempDir()
	if none := stampOf(t, outside); none.exists {
		t.Fatalf("stamp outside any work tree = %+v, want no index", none)
	}
}

// A workspace inside a real submodule and a linked worktree each read their
// own index.
func TestReadIndexStampReadsASubmoduleAndALinkedWorktree(t *testing.T) {
	module := initGitRepo(t)
	super := initGitRepo(t)
	runGit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", module, "sub")
	sub := filepath.Join(super, "sub")
	before := stampOf(t, sub)
	if !before.exists {
		t.Fatalf("submodule stamp = %+v, want its index", before)
	}
	superStamp := stampOf(t, super)
	write(t, filepath.Join(sub, "z.txt"), "z\n")
	runGit(t, sub, "add", "z.txt")
	if after := stampOf(t, sub); after == before {
		t.Fatalf("submodule stamp after git add = %+v, want a change", after)
	}
	if again := stampOf(t, super); again != superStamp {
		t.Fatalf("superproject stamp moved with its submodule's index: %+v, want %+v", again, superStamp)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, module, "worktree", "add", "-q", linked)
	worktree := stampOf(t, linked)
	if !worktree.exists || worktree == stampOf(t, module) {
		t.Fatalf("linked worktree stamp = %+v, want its own index", worktree)
	}
}
