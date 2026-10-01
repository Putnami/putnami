package extension

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveLink is one symbolic-link entry of a test archive.
type archiveLink struct {
	name, target string
}

// createLinkTestArchive writes a .tar.gz holding the regular files, in the
// given order, followed by the links.
func createLinkTestArchive(t *testing.T, archivePath string, files [][2]string, links []archiveLink) {
	t.Helper()
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for _, file := range files {
		if err := tw.WriteHeader(&tar.Header{Name: file[0], Mode: 0o644, Size: int64(len(file[1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file[1])); err != nil {
			t.Fatal(err)
		}
	}
	for _, link := range links {
		if err := tw.WriteHeader(&tar.Header{Name: link.name, Linkname: link.target, Typeflag: tar.TypeSymlink}); err != nil {
			t.Fatal(err)
		}
	}
	for _, closer := range []interface{ Close() error }{tw, gw, f} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// assertLinkRefused checks that err refuses the archive entry name and says why.
func assertLinkRefused(t *testing.T, err error, name, why string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "archive entry "+name+" is a ") || !strings.Contains(err.Error(), why) {
		t.Fatalf("ExtractTarGz error = %v, want a refusal of %s that says it %s", err, name, why)
	}
}

// A hard link fails the extraction and names its entry on every platform:
// skipping it installed a tree without the file and reported success.
func TestExtractTarGzRefusesHardLinks(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "hardlink.tar.gz")
	destDir := filepath.Join(t.TempDir(), "extracted")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	body := []byte("binary")
	if err := tw.WriteHeader(&tar.Header{Name: "bin/tool", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "bin/tool-alias", Linkname: "bin/tool", Typeflag: tar.TypeLink}); err != nil {
		t.Fatal(err)
	}
	for _, closer := range []interface{ Close() error }{tw, gw, f} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}

	assertLinkRefused(t, ExtractTarGz(archivePath, destDir), "bin/tool-alias", "hard link to bin/tool")
	if _, err := os.Lstat(filepath.Join(destDir, "bin", "tool-alias")); !os.IsNotExist(err) {
		t.Fatalf("the refused hard link exists: %v", err)
	}
}
