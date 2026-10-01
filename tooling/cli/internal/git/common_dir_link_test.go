package git

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

// A checkout reached through a directory link, a symbolic link on Unix and a
// junction on Windows, yields the same common dir as the checkout itself, so
// both spellings key one store.
func TestCommonDir_DirectoryLinkedCheckoutAgrees(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	link := filepath.Join(t.TempDir(), "checkout")
	if err := dirlink.Create(dir, link); err != nil {
		t.Fatal(err)
	}
	direct, linked := CommonDir(dir), CommonDir(link)
	if direct == "" || linked != direct {
		t.Errorf("CommonDir through the link = %q, want the checkout's %q", linked, direct)
	}
}
