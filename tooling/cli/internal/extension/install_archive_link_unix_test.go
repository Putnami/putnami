//go:build !windows

package extension

import (
	"os"
	"path/filepath"
	"testing"
)

// On Unix, a contained link, to a file or to a directory, is recreated with
// its archive target verbatim, and the extracted tree reads through it.
func TestExtractTarGzRecreatesContainedSymlinks(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "links.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")
	createLinkTestArchive(t, archivePath,
		[][2]string{{"docs/NOTICE", "notice\n"}},
		[]archiveLink{{name: "LICENSE", target: "docs/NOTICE"}, {name: "lib/docs", target: "../docs"}})

	if err := ExtractTarGz(archivePath, destDir); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	for _, link := range []archiveLink{{"LICENSE", "docs/NOTICE"}, {"lib/docs", "../docs"}} {
		got, err := os.Readlink(filepath.Join(destDir, filepath.FromSlash(link.name)))
		if err != nil || got != link.target {
			t.Errorf("link %s = %q, %v; want the archive target %q", link.name, got, err, link.target)
		}
	}
	for _, through := range []string{"LICENSE", "lib/docs/NOTICE"} {
		if data, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(through))); err != nil || string(data) != "notice\n" {
			t.Errorf("read %s = %q, %v; want the linked content", through, data, err)
		}
	}
}

// A link whose parent path runs through a link extracted earlier fails the
// extraction: os.Root follows `d -> .`, so `d/l -> ../x`, lexically `x`, would
// be created as `l -> ../x` at the top of the tree, outside it.
func TestExtractTarGzRefusesSymlinkUnderSymlinkedDirectory(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "chain.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")
	createLinkTestArchive(t, archivePath, nil,
		[]archiveLink{{name: "d", target: "."}, {name: "d/l", target: "../x"}})

	assertLinkRefused(t, ExtractTarGz(archivePath, destDir), "d/l", "through the symbolic link d")
	if _, err := os.Lstat(filepath.Join(destDir, "l")); !os.IsNotExist(err) {
		t.Fatalf("the refused link exists at the top of the tree: %v", err)
	}
}

// A link whose target walks back up with .. after descending fails the
// extraction: `d -> .` makes `l -> d/..`, lexically `.`, resolve to the
// parent of the tree.
func TestExtractTarGzRefusesSymlinkTargetWithInnerDotDot(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "inner.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")
	createLinkTestArchive(t, archivePath, nil,
		[]archiveLink{{name: "d", target: "."}, {name: "l", target: "d/.."}})

	assertLinkRefused(t, ExtractTarGz(archivePath, destDir), "l", "symbolic link to d/..")
	if _, err := os.Lstat(filepath.Join(destDir, "l")); !os.IsNotExist(err) {
		t.Fatalf("the refused link exists: %v", err)
	}
}
