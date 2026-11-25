//go:build linux || darwin

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestARewriteInPlaceChangesTheIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("one"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, ok := StatFileIdentity(path)
	if !ok {
		t.Fatal("no identity for a regular file")
	}
	again, _ := StatFileIdentity(path)
	if again != before {
		t.Fatalf("an unchanged file changed identity: %+v then %+v", before, again)
	}
	modTime := time.Unix(0, before.ModTimeNs)
	time.Sleep(10 * time.Millisecond)
	// Same size, and the modification time set back: only the change time moves.
	if err := os.WriteFile(path, []byte("two"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	after, ok := StatFileIdentity(path)
	if !ok {
		t.Fatal("no identity after the rewrite")
	}
	if after.Size != before.Size || after.ModTimeNs != before.ModTimeNs {
		t.Fatalf("the fixture should keep size and modification time: %+v then %+v", before, after)
	}
	if after == before {
		t.Fatalf("a rewrite in place kept the identity %+v", after)
	}
}

func TestADirectoryAndAMissingFileHaveNoIdentity(t *testing.T) {
	dir := t.TempDir()
	if _, ok := StatFileIdentity(dir); ok {
		t.Fatal("a directory has an identity")
	}
	if _, ok := StatFileIdentity(filepath.Join(dir, "missing")); ok {
		t.Fatal("a missing file has an identity")
	}
}
