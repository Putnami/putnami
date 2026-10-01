package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestArchiveRetryIgnoresStagingMetadata(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "reproducible-archives", "independent-staging-produces-identical-archive-bytes")
	produce := func(timestamp int64, content string) []byte {
		t.Helper()
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "compiled", "tool"), content)
		if err := os.Chmod(filepath.Join(root, "compiled", "tool"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("compiled/tool", filepath.Join(root, "tool")); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("creating a symbolic link on Windows needs SeCreateSymbolicLinkPrivilege or Developer Mode: %v", err)
			}
			t.Fatal(err)
		}
		for _, path := range []string{root, filepath.Join(root, "compiled"), filepath.Join(root, "compiled", "tool")} {
			if err := os.Chtimes(path, time.Unix(timestamp, 123), time.Unix(timestamp, 456)); err != nil {
				t.Fatal(err)
			}
		}
		output := filepath.Join(t.TempDir(), "archive.tar.gz")
		if err := writeReproducibleArchive(output, root, []string{"compiled/tool", "tool"}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := produce(1000, "binary bytes")
	if !bytes.Equal(first, produce(2000, "binary bytes")) {
		t.Fatal("independent staging timestamps changed immutable archive bytes")
	}
	if bytes.Equal(first, produce(2000, "changed binary")) {
		t.Fatal("changed payload did not change archive bytes")
	}
	gz, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[h.Name] = true
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || !h.ModTime.Equal(time.Unix(0, 0)) {
			t.Fatalf("host metadata leaked into %s: %+v", h.Name, h)
		}
		if h.Name == "./compiled/tool" && h.Mode != 0o755 {
			t.Fatal("executable mode was lost")
		}
		if h.Name == "./tool" && (h.Typeflag != tar.TypeSymlink || h.Linkname != "compiled/tool") {
			t.Fatal("symbolic link was not preserved")
		}
	}
	if !seen["./compiled/tool"] || !seen["./tool"] {
		t.Fatal("archive omitted staged files")
	}
}

func TestArchiveWriterRejectsMissingStage(t *testing.T) {
	output := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := writeReproducibleArchive(output, filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("missing stage accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed archive was retained")
	}
}

// A declared program is executable in the archive whatever mode the packaging
// machine's disk gives it. A Windows host, whose filesystem reports every
// writable file as 0666 and every directory as 0777, writes the bytes a Unix
// host with umask 022 writes. The declaration changes nothing for a file that
// is already executable, and a file nobody declares keeps its mode.
func TestArchiveMarksDeclaredExecutablesWhateverTheDiskMode(t *testing.T) {
	root := t.TempDir()
	produce := func(name, goos string, dirMode, fileMode, programMode os.FileMode, executables []string) []byte {
		t.Helper()
		stage := filepath.Join(root, name)
		mustWrite(t, filepath.Join(stage, "compiled", "putnami-go.exe"), "binary bytes")
		mustWrite(t, filepath.Join(stage, "README.md"), "readme")
		for path, mode := range map[string]os.FileMode{
			stage:                             dirMode,
			filepath.Join(stage, "compiled"):  dirMode,
			filepath.Join(stage, "README.md"): fileMode,
			filepath.Join(stage, "compiled", "putnami-go.exe"): programMode,
		} {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
		output := filepath.Join(root, name+".tar.gz")
		if err := writeArchiveOn(goos, output, stage, executables); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	declared := []string{"compiled/putnami-go.exe"}
	windows := produce("windows", "windows", 0o777, 0o666, 0o666, declared)
	// The Unix legs need the disk to hold the modes they set, which a Windows
	// filesystem does not: there every writable file reads back as 0666.
	// TestArchiveModeOnAWindowsHost covers the Windows host itself.
	if runtime.GOOS != "windows" {
		unix := produce("unix", "linux", 0o755, 0o644, 0o755, nil)
		if !bytes.Equal(unix, produce("unix-declared", "linux", 0o755, 0o644, 0o755, declared)) {
			t.Fatal("declaring an executable that is already executable changed the archive bytes")
		}
		if !bytes.Equal(unix, produce("unix-no-execute-bit", "linux", 0o755, 0o644, 0o644, declared)) {
			t.Fatal("a declared program without an execute bit on disk archived differently from an executable one")
		}
		if !bytes.Equal(unix, windows) {
			t.Fatal("a Windows host archived differently from a Unix host")
		}
		if bytes.Equal(unix, produce("undeclared", "linux", 0o755, 0o644, 0o644, nil)) {
			t.Fatal("an undeclared file lost its disk mode")
		}
	}
	archivePath := filepath.Join(root, "windows.tar.gz")
	for name, want := range map[string]int64{"compiled/": 0o755, "compiled/putnami-go.exe": 0o755, "README.md": 0o644} {
		if mode := archiveEntryMode(t, archivePath, name); mode != want {
			t.Errorf("Windows host: %s mode = %o, want %o", name, mode, want)
		}
	}
}

// A Windows host records one permission, read-only; the archive gives each
// entry the mode a Unix host with umask 022 gives it, and a declared program
// the execute bits of the classes that may read it.
func TestArchiveModeOnAWindowsHost(t *testing.T) {
	stage := t.TempDir()
	mustWrite(t, filepath.Join(stage, "bin", "tool.exe"), "binary bytes")
	mustWrite(t, filepath.Join(stage, "bin", "readonly.exe"), "binary bytes")
	mustWrite(t, filepath.Join(stage, "notes.txt"), "notes")
	mustWrite(t, filepath.Join(stage, "locked.txt"), "locked")
	for path, mode := range map[string]os.FileMode{
		stage:                                       0o777,
		filepath.Join(stage, "bin"):                 0o777,
		filepath.Join(stage, "bin", "tool.exe"):     0o666,
		filepath.Join(stage, "bin", "readonly.exe"): 0o444,
		filepath.Join(stage, "notes.txt"):           0o666,
		filepath.Join(stage, "locked.txt"):          0o444,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := writeArchiveOn("windows", output, stage, []string{"bin/tool.exe", "bin/readonly.exe"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{
		"bin/":             0o755,
		"bin/tool.exe":     0o755,
		"bin/readonly.exe": 0o555,
		"notes.txt":        0o644,
		"locked.txt":       0o444,
	} {
		if mode := archiveEntryMode(t, output, name); mode != want {
			t.Errorf("%s mode = %o, want %o", name, mode, want)
		}
	}
}

// A declared program gains the execute bit of each class that may read it and
// of no other: a Unix host's umask stays visible in the archive.
func TestArchiveGrantsExecuteOnlyToClassesThatRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the disk modes 0640, 0600 and 0750 exist only on a Unix filesystem; TestArchiveModeOnAWindowsHost covers a Windows host")
	}
	for disk, want := range map[os.FileMode]int64{
		0o644: 0o755,
		0o640: 0o750,
		0o600: 0o700,
		0o750: 0o750,
		0o755: 0o755,
		0o444: 0o555,
	} {
		stage := t.TempDir()
		mustWrite(t, filepath.Join(stage, "tool"), "binary bytes")
		if err := os.Chmod(filepath.Join(stage, "tool"), disk); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "archive.tar.gz")
		if err := writeArchiveOn("linux", output, stage, []string{"tool"}); err != nil {
			t.Fatal(err)
		}
		if mode := archiveEntryMode(t, output, "tool"); mode != want {
			t.Errorf("declared program with disk mode %o archived as %o, want %o", disk, mode, want)
		}
	}
}

// A Windows host stores a symbolic link's target with backslashes; the archive
// records it with slashes, as a Unix host records the same link. A Unix host
// records a target verbatim, where a backslash is a file name character.
func TestArchiveRecordsLinkTargetsInSlashFormOnAWindowsHost(t *testing.T) {
	stage := t.TempDir()
	mustWrite(t, filepath.Join(stage, "compiled", "tool"), "binary bytes")
	if err := os.Symlink(`compiled\tool`, filepath.Join(stage, "tool")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symbolic link on Windows needs SeCreateSymbolicLinkPrivilege or Developer Mode: %v", err)
		}
		t.Fatal(err)
	}
	for goos, want := range map[string]string{
		"windows": "compiled/tool",
		"linux":   `compiled\tool`,
	} {
		if runtime.GOOS == "windows" && goos != "windows" {
			continue // the disk target already has backslashes on this host
		}
		output := filepath.Join(t.TempDir(), "archive.tar.gz")
		if err := writeArchiveOn(goos, output, stage, nil); err != nil {
			t.Fatal(err)
		}
		if got := archiveEntryLinkname(t, output, "tool"); got != want {
			t.Errorf("%s host: link target = %q, want %q", goos, got, want)
		}
	}
}

// archiveEntryLinkname returns the link target the archive at path records
// for the entry named name.
func archiveEntryLinkname(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("archive has no entry %s", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "./"+name {
			if h.Typeflag != tar.TypeSymlink {
				t.Fatalf("%s is not a symbolic link: type %c", name, h.Typeflag)
			}
			return h.Linkname
		}
	}
}
