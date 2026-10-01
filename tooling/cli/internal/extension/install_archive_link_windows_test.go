//go:build windows

package extension

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// On Windows, a contained link fails the extraction and names its entry,
// whether it points at a file or a directory: nothing is skipped silently.
func TestExtractTarGzRefusesContainedSymlinksOnWindows(t *testing.T) {
	for _, link := range []archiveLink{{name: "LICENSE", target: "docs/NOTICE"}, {name: "lib/docs", target: "../docs"}} {
		t.Run(link.name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "links.tar.gz")
			destDir := filepath.Join(t.TempDir(), "extracted")
			createLinkTestArchive(t, archivePath, [][2]string{{"docs/NOTICE", "notice\n"}}, []archiveLink{link})

			err := ExtractTarGz(archivePath, destDir)
			if err == nil || !strings.Contains(err.Error(), "archive entry "+link.name+" is a symbolic link") ||
				!strings.Contains(err.Error(), "on Windows") {
				t.Fatalf("ExtractTarGz error = %v, want a refusal naming %s", err, link.name)
			}
			if _, err := os.Lstat(filepath.Join(destDir, filepath.FromSlash(link.name))); !os.IsNotExist(err) {
				t.Fatalf("refused link %s exists: %v", link.name, err)
			}
		})
	}
}

// A link that leaves the extracted tree, by a climb or by a Windows absolute
// path, is refused for that reason before the Windows link refusal, as on Unix.
func TestExtractTarGzRefusesEscapingSymlinkOnWindows(t *testing.T) {
	for _, link := range []archiveLink{{name: "evil", target: "../../outside"}, {name: "abs", target: `C:\Windows`}} {
		t.Run(link.name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "escape.tar.gz")
			destDir := filepath.Join(t.TempDir(), "extracted")
			createLinkTestArchive(t, archivePath, [][2]string{{"safe.txt", "safe"}}, []archiveLink{link})

			assertLinkRefused(t, ExtractTarGz(archivePath, destDir), link.name, "leaves the extracted tree")
			if _, err := os.Lstat(filepath.Join(destDir, link.name)); !os.IsNotExist(err) {
				t.Fatalf("refused link %s exists: %v", link.name, err)
			}
		})
	}
}
