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
