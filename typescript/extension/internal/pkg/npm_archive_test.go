package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// bunUnixArchive is the archive `bun pm pack` (bun 1.4.0 on macOS) writes for
// a staged package whose files carry the modes normalizeNPMPackageModes gives
// them: bin/probe.js, a bin entry, and cli.js, a build output with a "#!" line,
// are 0755, and every other file is 0644. Its entries use each header form bun
// writes: a plain ustar name, a name split into a ustar prefix (deep/...), and
// a pax extended header (été.js).
const bunUnixArchive = `
H4sIAAAAAAAC/+2XQWuDMBSAe/ZXZO6ywdBEjcJGYdtpu4yx246phtatMxLTIpT+oP6O/rHF2g4U
Rlumstb3XYJJBIPve+8lZeEnG3M7LUfrIxPJoGEwxr7noWLU1EeMiYcRdqmPCfF97CFMXILJAOFB
B8wyxaT+lAYOWTncibAwEDIT9sXNW2TeK54pO5VixM2bYmHOZRaLpFgjFrZwOTuKi5niTf1Q7tYb
LFvPly/rKDL16tJYGgPgX7P1vvLvWvA/oPR3/7Hj1/3Hrg/+d0EokkxMuTUV46uty9d3YG3v/A+n
cQvmH+g/rdd/Ejgu+N8Flxf2LJOb9M+TOUpExI1KTtCBARnhfBkdjR0eyE8+2df/63pf9d/zA3rq
/u8Sa8R5arM/0O7xeZ4KqdBiCYr3uv7HScTzljqAvf679frvOBT6/07Y+l+UfIU2/T8aIgLJoG/+
v7L8ibOIS3u9UutVo5lgr/+0fv/3Nv1/Dv63jkPQJJLhhMmMq+Hj88vD27vhUJQyNRnugmMXEpAV
ztb/5q0/wv96/0+cwIP7f5f1H/p/AACAnvENUGG0aAAgAAA=`

// bunArchiveExecutables are the executable files of bunUnixArchive.
var bunArchiveExecutables = map[string]struct{}{"bin/probe.js": {}, "cli.js": {}}

func bunUnixArchiveBytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(bunUnixArchive), ""))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gunzipBytes(t *testing.T, compressed []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// withBunWindowsModes returns tarball with the modes bun writes on a Windows
// host, measured on Windows Server 2022: 0777 for a bin entry, 0666 for every
// other file, and a pax extended header's mode equal to its entry's. It writes
// the fields in libarchive's own format and is independent of the code under
// test.
func withBunWindowsModes(t *testing.T, tarball []byte) []byte {
	t.Helper()
	out := bytes.Clone(tarball)
	var paxHeaders []int
	for offset := 0; offset+512 <= len(out); {
		header := out[offset : offset+512]
		if bytes.Equal(header, make([]byte, 512)) {
			return out
		}
		size, err := strconv.ParseInt(strings.Trim(string(header[124:136]), " \x00"), 8, 64)
		if err != nil {
			t.Fatal(err)
		}
		if header[156] == 'x' {
			paxHeaders = append(paxHeaders, offset)
		} else {
			mode := int64(0o666)
			if strings.HasPrefix(string(header[:100]), "package/bin/probe.js\x00") {
				mode = 0o777
			}
			for _, at := range append(paxHeaders, offset) {
				writeLibarchiveMode(out[at:at+512], mode)
			}
			paxHeaders = nil
		}
		offset += 512 + int((size+511)/512*512)
	}
	t.Fatal("the archive has no end-of-archive block")
	return nil
}

func writeLibarchiveMode(header []byte, mode int64) {
	copy(header[100:108], fmt.Sprintf("%06o \x00", mode))
	copy(header[148:156], "        ")
	var sum int64
	for _, b := range header {
		sum += int64(b)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

// tarModes returns the mode of each entry of an uncompressed tar archive.
func tarModes(t *testing.T, tarball []byte) map[string]int64 {
	t.Helper()
	modes := make(map[string]int64)
	reader := tar.NewReader(bytes.NewReader(tarball))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return modes
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[header.Name] = header.Mode
	}
}

func writeArchive(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe-1.0.0.tgz")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A package bun packs on Windows gets back the entries a Unix host packs: the
// uncompressed archive is byte-identical once the modes are decided from the
// package instead of the disk.
func TestNormalizeNPMArchiveGivesAWindowsPackTheUnixBytes(t *testing.T) {
	unixTar := gunzipBytes(t, bunUnixArchiveBytes(t))
	windowsTar := withBunWindowsModes(t, unixTar)
	if bytes.Equal(windowsTar, unixTar) {
		t.Fatal("the Windows fixture has the Unix modes; the test proves nothing")
	}
	archive := writeArchive(t, gzipBytes(t, windowsTar))

	if err := normalizeNPMArchiveModes(archive, bunArchiveExecutables); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	normalized := gunzipBytes(t, data)
	if !bytes.Equal(normalized, unixTar) {
		t.Fatalf("normalized Windows archive differs from the Unix archive:\nmodes %v\nwant  %v", tarModes(t, normalized), tarModes(t, unixTar))
	}
	for name, want := range map[string]int64{
		"package/package.json": 0o644,
		"package/bin/probe.js": 0o755,
		"package/cli.js":       0o755,
		"package/index.js":     0o644,
		"package/été.js":       0o644,
	} {
		if got := tarModes(t, normalized)[name]; got != want {
			t.Errorf("%s mode = %o, want %o", name, got, want)
		}
	}
}

// An archive that already has the Unix modes keeps its exact bytes: nothing is
// compressed again.
func TestNormalizeNPMArchiveKeepsTheBytesOfACanonicalArchive(t *testing.T) {
	original := bunUnixArchiveBytes(t)
	archive := writeArchive(t, original)
	if err := normalizeNPMArchiveModes(archive, bunArchiveExecutables); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(archive); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("a canonical archive was rewritten (err %v)", err)
	}
}

// stageBunFixturePackage lays out, under a new workspace, the build output and
// the staged package bunUnixArchive was packed from, and returns the workspace,
// the project path and the staged package directory.
func stageBunFixturePackage(t *testing.T) (string, string, string) {
	t.Helper()
	workspace := t.TempDir()
	projectPath := "packages/probe"
	packageRoot := filepath.Join(workspace, ".putnami", "out", projectPath, "package")
	for path, content := range map[string]string{
		filepath.Join(packageRoot, "lib", "cli.js"):       "#!/usr/bin/env node\nconsole.log(\"cli\");\n",
		filepath.Join(packageRoot, "lib", "index.js"):     "export const probe = 1;\n",
		filepath.Join(packageRoot, "npm", "package.json"): `{"name":"@test/probe","version":"1.0.0","bin":{"probe":"./bin/probe.js"}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return workspace, projectPath, filepath.Join(packageRoot, "npm")
}

// On Windows the executables come from the staged package: its bin entries and
// the build outputs that start with a "#!" line.
func TestNormalizeNPMArchiveOnWindowsDecidesFromTheStagedPackage(t *testing.T) {
	workspace, projectPath, packageDir := stageBunFixturePackage(t)
	unixTar := gunzipBytes(t, bunUnixArchiveBytes(t))
	archive := writeArchive(t, gzipBytes(t, withBunWindowsModes(t, unixTar)))

	if err := NormalizeNPMArchive("windows", workspace, projectPath, packageDir, archive); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gunzipBytes(t, data), unixTar) {
		t.Fatalf("modes = %v, want %v", tarModes(t, gunzipBytes(t, data)), tarModes(t, unixTar))
	}

	// Without the build output there is no "#!" decision to make: the archive
	// is refused rather than packed with guessed modes.
	if err := os.RemoveAll(filepath.Join(workspace, ".putnami", "out", projectPath, "package", "lib")); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeNPMArchive("windows", workspace, projectPath, packageDir, archive); err == nil {
		t.Fatal("an archive without the build output it was packed from was accepted")
	}
}

// A Unix host's archive is never read or rewritten, whatever its modes: the
// packer already read the staged modes.
func TestNormalizeNPMArchiveLeavesAUnixHostsArchiveAlone(t *testing.T) {
	windowsArchive := gzipBytes(t, withBunWindowsModes(t, gunzipBytes(t, bunUnixArchiveBytes(t))))
	archive := writeArchive(t, windowsArchive)
	for _, goos := range []string{"linux", "darwin"} {
		if err := NormalizeNPMArchive(goos, t.TempDir(), "packages/probe", t.TempDir(), archive); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
	}
	if data, err := os.ReadFile(archive); err != nil || !bytes.Equal(data, windowsArchive) {
		t.Fatalf("a Unix host rewrote the archive (err %v)", err)
	}
}

// An archive the normalizer cannot read exactly is refused and left as it is.
func TestNormalizeNPMArchiveRefusesWhatItCannotReadExactly(t *testing.T) {
	unixTar := gunzipBytes(t, bunUnixArchiveBytes(t))
	windowsTar := withBunWindowsModes(t, unixTar)

	corrupt := bytes.Clone(windowsTar)
	corrupt[0] = 'q' // the recorded checksum no longer matches
	link := bytes.Clone(windowsTar)
	link[156] = '2' // package.json becomes a symbolic link
	writeLibarchiveMode(link[:512], 0o666)
	truncated := windowsTar[:1024+512]

	for name, tarball := range map[string][]byte{
		"checksum mismatch":        corrupt,
		"symbolic link":            link,
		"no end-of-archive blocks": truncated,
	} {
		t.Run(name, func(t *testing.T) {
			original := gzipBytes(t, tarball)
			archive := writeArchive(t, original)
			if err := normalizeNPMArchiveModes(archive, bunArchiveExecutables); err == nil {
				t.Fatal("the archive was accepted")
			}
			if data, err := os.ReadFile(archive); err != nil || !bytes.Equal(data, original) {
				t.Fatalf("a refused archive was rewritten (err %v)", err)
			}
		})
	}
}
