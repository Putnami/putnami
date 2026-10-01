//go:build windows

package dirlink

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The package relies on these Go 1.23+ modes for a junction (winsymlink=1). A
// change here means IsLink and every reader of these links need a new look.
func TestJunctionModeIsIrregularWithoutSymlinkOrDir(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	link := filepath.Join(dir, "link")
	if err := Create(target, link); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode()
	if mode&fs.ModeIrregular == 0 || mode&fs.ModeSymlink != 0 || mode.IsDir() {
		t.Fatalf("os.Lstat of a junction reports mode %v, want irregular without symlink or dir", mode)
	}
}

// A standard user creates, replaces and reads a junction: no Developer Mode,
// no elevation. A relative target is stored absolute.
func TestJunctionCreateReplaceRead(t *testing.T) {
	dir := t.TempDir()
	a := targetDir(t, dir, "a")
	b := targetDir(t, dir, "b")
	link := filepath.Join(dir, "link")
	if err := Replace("a", link, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != a {
		t.Fatalf("os.Readlink = %q, %v, want the absolute %q", got, err, a)
	}
	if err := Replace(b, link, ""); err != nil {
		t.Fatal(err)
	}
	if got := readThrough(t, link); got != "b" {
		t.Fatalf("read %q through the replaced junction, want %q", got, "b")
	}
	entries, err := os.ReadDir(link)
	if err != nil || len(entries) != 1 || entries[0].Name() != "name" {
		t.Fatalf("os.ReadDir through the junction = %v, %v", entries, err)
	}
	if _, err := os.Stat(link + ".lock"); err != nil {
		t.Fatalf("Replace left no lock file: %v", err)
	}
}

func TestSubstituteName(t *testing.T) {
	for target, want := range map[string]string{
		`C:\work\a`:          `\??\C:\work\a`,
		`\\server\share\a`:   `\??\UNC\server\share\a`,
		`\\?\C:\long\path\a`: `\??\C:\long\path\a`,
	} {
		if got := substituteName(target); got != want {
			t.Errorf("substituteName(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestWin32Path(t *testing.T) {
	for final, want := range map[string]string{
		`\\?\C:\work\a`:           `C:\work\a`,
		`\\?\UNC\server\share\a`:  `\\server\share\a`,
		`\\?\Volume{0000}\work\a`: `\\?\Volume{0000}\work\a`,
		`C:\already\plain`:        `C:\already\plain`,
	} {
		if got := win32Path(final); got != want {
			t.Errorf("win32Path(%q) = %q, want %q", final, got, want)
		}
	}
}

// A create that fails after its Mkdir removes the directory: here the target
// is too long for a reparse point.
func TestAFailedCreateLeavesNoDirectory(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	target := filepath.Join(dir, strings.Repeat("t", windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE))
	if err := Create(target, link); err == nil {
		t.Fatal("Create accepted a target too long for a reparse point")
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the failed create left %s behind: %v", link, err)
	}
}

// A create stopped between its Mkdir and its reparse point, by the death of
// its process, leaves an empty directory. Replace takes it for the link it
// was meant to be; a directory with content stays refused.
func TestReplaceRecoversATornCreate(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(link, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, link, ""); err != nil {
		t.Fatalf("Replace over a torn create: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil || !IsLink(link, info) {
		t.Fatalf("Replace left no link: %v, %v", info, err)
	}
	assertResolvesTo(t, link, target)
}
