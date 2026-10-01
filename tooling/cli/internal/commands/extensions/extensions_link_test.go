package extensions

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

// The artifact-store guard sees through a directory link, a symbolic link on
// Unix and a junction on Windows, so a link to the store is the store.
func TestSameDirPathFollowsADirectoryLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "store")
	if err := dirlink.Create(dir, link); err != nil {
		t.Fatal(err)
	}
	if !sameDirPath(link, dir) || !sameDirPath(dir, link) {
		t.Errorf("sameDirPath does not match %s with the directory it links to, %s", link, dir)
	}
	if sameDirPath(link, t.TempDir()) {
		t.Errorf("sameDirPath matches %s with an unrelated directory", link)
	}
}
