package deliverycli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// --- fixtures -----------------------------------------------------------------

// goFixtureEntry is one entry of a synthetic upstream Go tarball. Every field
// that MUST NOT reach the produced layer is settable here, so the tests can
// hand the producer an archive whose mtimes, ownership, owner names and modes
// are hostile and still demand byte-identical output.
type goFixtureEntry struct {
	name     string
	body     string
	typeflag byte
	mode     int64
	linkname string
	modTime  time.Time
	uid      int
	gid      int
	uname    string
	gname    string
}

// goFixtureArchive packs the entries into a gzip tar, in the given slice order.
func goFixtureArchive(t *testing.T, entries []goFixtureEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := tar.NewWriter(gz)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Typeflag: typeflag,
			Name:     entry.name,
			Linkname: entry.linkname,
			Mode:     entry.mode,
			ModTime:  entry.modTime,
			Uid:      entry.uid,
			Gid:      entry.gid,
			Uname:    entry.uname,
			Gname:    entry.gname,
			Format:   tar.FormatPAX,
		}
		if typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write fixture header %s: %v", entry.name, err)
		}
		if typeflag == tar.TypeReg {
			if _, err := io.WriteString(writer, entry.body); err != nil {
				t.Fatalf("write fixture body %s: %v", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close fixture tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close fixture gzip: %v", err)
	}
	return buf.Bytes()
}

// goFixtureLayout is the logical content of the synthetic toolchain: the trees
// the runner keeps, the four trees the Dockerfile prunes, an entry whose parent
// directory is never declared (so the producer must synthesize it), and a
// symlink.
func goFixtureLayout() []goFixtureEntry {
	return []goFixtureEntry{
		{name: "go/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "go/VERSION", body: "go1.26.1\n", mode: 0o644},
		{name: "go/bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "go/bin/go", body: "#!go binary\n", mode: 0o755},
		{name: "go/bin/gofmt", body: "#!gofmt binary\n", mode: 0o755},
		// pkg/ and pkg/tool/ are deliberately NOT declared: a real archive can
		// omit intermediate directories and the layer must still carry them.
		{name: "go/pkg/tool/linux_amd64/compile", body: "compile\n", mode: 0o755},
		{name: "go/src/runtime/proc.go", body: "package runtime\n", mode: 0o644},
		{name: "go/src/runtime/README", body: "notes\n", mode: 0o644},
		{name: "go/lib/time/zoneinfo.zip", body: "zones\n", mode: 0o644},
		{name: "go/bin/go-latest", typeflag: tar.TypeSymlink, linkname: "go", mode: 0o777},
		// The four pruned trees.
		{name: "go/test/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "go/test/bench.go", body: "package main\n", mode: 0o644},
		{name: "go/doc/go1.26.html", body: "<html/>\n", mode: 0o644},
		{name: "go/api/go1.26.txt", body: "pkg net\n", mode: 0o644},
		{name: "go/misc/wasm/go_js_wasm_exec", body: "#!/bin/sh\n", mode: 0o755},
	}
}

// goFixtureCalm packs the layout with tidy metadata.
func goFixtureCalm(t *testing.T) []byte {
	t.Helper()
	entries := slices.Clone(goFixtureLayout())
	stamp := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	for i := range entries {
		entries[i].modTime = stamp
		entries[i].uid, entries[i].gid = 0, 0
		entries[i].uname, entries[i].gname = "root", "root"
	}
	return goFixtureArchive(t, entries)
}

// goFixtureAdversarial packs the SAME logical layout with everything a second
// machine could plausibly differ on: reversed creation order, per-entry clock
// values, a non-root numeric owner, symbolic owner names, and group/other write
// bits that a loose umask would leave behind.
func goFixtureAdversarial(t *testing.T) []byte {
	t.Helper()
	entries := slices.Clone(goFixtureLayout())
	slices.Reverse(entries)
	for i := range entries {
		entries[i].modTime = time.Date(1999, 12, 31, 23, 59, 59, 0, time.FixedZone("weird", 7*3600))
		entries[i].modTime = entries[i].modTime.Add(time.Duration(i) * time.Hour)
		entries[i].uid, entries[i].gid = 501+i, 20+i
		entries[i].uname, entries[i].gname = "builder", "staff"
		if entries[i].typeflag == tar.TypeReg || entries[i].typeflag == 0 {
			entries[i].mode |= 0o022
		}
	}
	return goFixtureArchive(t, entries)
}

// fakeGoDist is the go.dev/dl seam under test control: it publishes a checksum
// and serves bytes, and each can be made to disagree.
type fakeGoDist struct {
	payload      []byte
	publish      string // "" → the honest digest of payload
	checksumErr  error
	openErr      error
	askedFor     []string
	downloadedAs []string
}

func (f *fakeGoDist) URL(filename string) string { return "https://go.dev/dl/" + filename }

func (f *fakeGoDist) Checksum(filename string) (string, error) {
	f.askedFor = append(f.askedFor, filename)
	if f.checksumErr != nil {
		return "", f.checksumErr
	}
	if f.publish != "" {
		return f.publish, nil
	}
	sum := sha256.Sum256(f.payload)
	return hex.EncodeToString(sum[:]), nil
}

func (f *fakeGoDist) Open(_ context.Context, filename string) (io.ReadCloser, error) {
	f.downloadedAs = append(f.downloadedAs, filename)
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(bytes.NewReader(f.payload)), nil
}

// installGoDist routes the producer's go.dev source at the fixture and restores
// the real one afterwards. No test in this file touches the network.
func installGoDist(t *testing.T, dist *fakeGoDist) {
	t.Helper()
	previous := imageLayersGoDistFor
	t.Cleanup(func() { imageLayersGoDistFor = previous })
	imageLayersGoDistFor = func(*http.Client) imageLayersGoDist { return dist }
}

// imageLayersWorkspace materializes a workspace root whose go.work declares the
// Go version — the ONLY place the producer may learn it from.
func imageLayersWorkspace(t *testing.T, goWork string) string {
	t.Helper()
	ws := t.TempDir()
	if goWork != "" {
		if err := os.WriteFile(filepath.Join(ws, "go.work"), []byte(goWork), 0o600); err != nil {
			t.Fatalf("write go.work: %v", err)
		}
	}
	return ws
}

// imageLayersProject materializes an image project with the given manifest body.
func imageLayersProject(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, imageLayersManifestName), []byte(manifest), 0o600); err != nil {
			t.Fatalf("write %s: %v", imageLayersManifestName, err)
		}
	}
	return dir
}

const imageLayersGoToolchainManifest = `{
  "layers": [
    {"name": "go-toolchain", "producer": "go-toolchain", "path": "/usr/local/go"}
  ]
}`

func imageLayersIO(out *[]string) clicore.IO {
	return clicore.IO{
		Stdout: func(line string) { *out = append(*out, line) },
		Stderr: func(string) {},
	}
}

// readTarEntries decodes a produced layer into headers plus contents.
func readTarEntries(t *testing.T, data []byte) []*tar.Header {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	var headers []*tar.Header
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read produced tar: %v", err)
		}
		headers = append(headers, header)
	}
	return headers
}

func headerNames(headers []*tar.Header) []string {
	names := make([]string, 0, len(headers))
	for _, header := range headers {
		names = append(names, header.Name)
	}
	return names
}

// --- normalization ------------------------------------------------------------

// TestImageLayersNormalizationIsByteStable is image invariant 1 at
// unit scale: the same logical tree, packed with adversarial mtimes, ownership,
// owner names, modes and creation order, normalizes to BYTE-IDENTICAL layers.
// If this ever fails, the image content key churns and publish-if-missing stops
// hitting — which is the whole model, not a detail.
func TestImageLayersNormalizationIsByteStable(t *testing.T) {
	calm := imageLayersNormalizeFixture(t, goFixtureCalm(t))
	adversarial := imageLayersNormalizeFixture(t, goFixtureAdversarial(t))
	if !bytes.Equal(calm, adversarial) {
		t.Fatalf("normalized layers differ: %d bytes vs %d bytes (sha256 %s vs %s)",
			len(calm), len(adversarial), sha256Hex(calm), sha256Hex(adversarial))
	}
	if len(calm) == 0 {
		t.Fatal("normalized layer is empty")
	}
}

// imageLayersNormalizeFixture runs one fixture archive through the exact
// read → normalize path the producer uses.
func imageLayersNormalizeFixture(t *testing.T, archive []byte) []byte {
	t.Helper()
	work := t.TempDir()
	path := filepath.Join(work, "download.tar.gz")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatalf("write fixture archive: %v", err)
	}
	entries, err := imageLayersReadGoArchive(path, "usr/local/go", work)
	if err != nil {
		t.Fatalf("read fixture archive: %v", err)
	}
	var buf bytes.Buffer
	if err := imageLayersWriteTar(&buf, entries); err != nil {
		t.Fatalf("normalize fixture archive: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestImageLayersNormalizedHeadersCarryNothingFromTheHost pins each field the
// content key depends on: epoch mtime, numeric root ownership with no owner
// names, exec-or-not modes, and a strictly sorted entry order.
func TestImageLayersNormalizedHeadersCarryNothingFromTheHost(t *testing.T) {
	layer := imageLayersNormalizeFixture(t, goFixtureAdversarial(t))
	headers := readTarEntries(t, layer)
	if len(headers) == 0 {
		t.Fatal("normalized layer has no entries")
	}
	names := headerNames(headers)
	if !slices.IsSorted(names) {
		t.Fatalf("entries are not lexicographically sorted: %v", names)
	}
	for _, header := range headers {
		if !header.ModTime.Equal(imageLayersEpoch) {
			t.Fatalf("%s: mtime = %s, want the fixed epoch", header.Name, header.ModTime)
		}
		if !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() {
			t.Fatalf("%s: carries atime/ctime (%s/%s)", header.Name, header.AccessTime, header.ChangeTime)
		}
		if header.Uid != 0 || header.Gid != 0 {
			t.Fatalf("%s: uid/gid = %d/%d, want 0/0", header.Name, header.Uid, header.Gid)
		}
		if header.Uname != "" || header.Gname != "" {
			t.Fatalf("%s: owner names = %q/%q, want empty (numeric owners only)", header.Name, header.Uname, header.Gname)
		}
		// PAXRecords is where archive/tar surfaces every extended attribute,
		// xattrs (SCHILY.xattr.*) included — so an empty map is the whole "no
		// PAX headers, no xattrs" claim.
		if len(header.PAXRecords) != 0 {
			t.Fatalf("%s: carries PAX records %v", header.Name, header.PAXRecords)
		}
		if header.Format != tar.FormatUSTAR {
			t.Fatalf("%s: format = %v, want USTAR (no extended headers)", header.Name, header.Format)
		}
		if strings.HasPrefix(header.Name, "/") {
			t.Fatalf("%s: layer paths must be relative", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Mode != imageLayersDirMode {
				t.Fatalf("%s: dir mode = %o, want %o", header.Name, header.Mode, imageLayersDirMode)
			}
		case tar.TypeReg:
			if header.Mode != imageLayersFileModeExec && header.Mode != imageLayersFileModePlain {
				t.Fatalf("%s: file mode = %o, want %o or %o", header.Name, header.Mode, imageLayersFileModeExec, imageLayersFileModePlain)
			}
		}
	}
}

// TestImageLayersNormalizationPrunesAndReroots proves the layer keeps exactly
// what the Dockerfile's `rm -rf` leaves, re-rooted under the declared in-image
// path, with every ancestor directory synthesized.
func TestImageLayersNormalizationPrunesAndReroots(t *testing.T) {
	headers := readTarEntries(t, imageLayersNormalizeFixture(t, goFixtureCalm(t)))
	names := headerNames(headers)
	for _, want := range []string{
		"usr/", "usr/local/", "usr/local/go/", "usr/local/go/bin/",
		"usr/local/go/bin/go", "usr/local/go/VERSION",
		"usr/local/go/pkg/", "usr/local/go/pkg/tool/", "usr/local/go/pkg/tool/linux_amd64/",
		"usr/local/go/pkg/tool/linux_amd64/compile",
		"usr/local/go/src/runtime/proc.go", "usr/local/go/lib/time/zoneinfo.zip",
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("layer is missing %q (entries: %v)", want, names)
		}
	}
	for _, pruned := range imageLayersGoPrune {
		prefix := "usr/local/go/" + pruned
		for _, name := range names {
			if name == prefix+"/" || strings.HasPrefix(name, prefix+"/") {
				t.Fatalf("layer still carries the pruned tree %q", name)
			}
		}
	}
	// Modes survive as the single executable distinction.
	byName := map[string]*tar.Header{}
	for _, header := range headers {
		byName[header.Name] = header
	}
	if got := byName["usr/local/go/bin/go"].Mode; got != imageLayersFileModeExec {
		t.Fatalf("bin/go mode = %o, want %o", got, imageLayersFileModeExec)
	}
	if got := byName["usr/local/go/VERSION"].Mode; got != imageLayersFileModePlain {
		t.Fatalf("VERSION mode = %o, want %o", got, imageLayersFileModePlain)
	}
	link := byName["usr/local/go/bin/go-latest"]
	if link == nil || link.Typeflag != tar.TypeSymlink || link.Linkname != "go" {
		t.Fatalf("symlink entry not preserved: %+v", link)
	}
}

// TestImageLayersWriteTarIgnoresInputOrder pins the writer's own contract: the
// caller's slice order never reaches the bytes.
func TestImageLayersWriteTarIgnoresInputOrder(t *testing.T) {
	work := t.TempDir()
	blob := filepath.Join(work, "blob")
	if err := os.WriteFile(blob, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	entries := []imageLayerEntry{
		{name: "usr/local/go/bin/go", typeflag: tar.TypeReg, mode: imageLayersFileModeExec, size: 1, blob: blob},
		{name: "usr/local/go/VERSION", typeflag: tar.TypeReg, mode: imageLayersFileModePlain, size: 1, blob: blob},
		{name: "usr/local/go/", typeflag: tar.TypeDir, mode: imageLayersDirMode},
	}
	var forward, reverse bytes.Buffer
	if err := imageLayersWriteTar(&forward, entries); err != nil {
		t.Fatalf("write forward: %v", err)
	}
	shuffled := slices.Clone(entries)
	slices.Reverse(shuffled)
	if err := imageLayersWriteTar(&reverse, shuffled); err != nil {
		t.Fatalf("write reverse: %v", err)
	}
	if !bytes.Equal(forward.Bytes(), reverse.Bytes()) {
		t.Fatal("entry slice order reached the layer bytes")
	}
}

// TestImageLayersRejectsEscapingEntries keeps a hostile or malformed archive
// from being half-copied: an entry outside `go/` is a loud error, never a skip.
func TestImageLayersRejectsEscapingEntries(t *testing.T) {
	cases := map[string]string{
		"traversal": "go/../../etc/passwd",
		"absolute":  "/etc/passwd",
		"foreign":   "golang/bin/go",
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			archive := goFixtureArchive(t, []goFixtureEntry{
				{name: "go/", typeflag: tar.TypeDir, mode: 0o755},
				{name: entry, body: "x", mode: 0o644},
			})
			work := t.TempDir()
			path := filepath.Join(work, "download.tar.gz")
			if err := os.WriteFile(path, archive, 0o600); err != nil {
				t.Fatalf("write archive: %v", err)
			}
			if _, err := imageLayersReadGoArchive(path, "usr/local/go", work); err == nil {
				t.Fatalf("entry %q was accepted", entry)
			}
		})
	}
}

// TestImageLayersRejectsDuplicateEntries: two entries with the same normalized
// name have no well-defined order, so the layer would not be reproducible.
func TestImageLayersRejectsDuplicateEntries(t *testing.T) {
	archive := goFixtureArchive(t, []goFixtureEntry{
		{name: "go/bin/go", body: "one", mode: 0o755},
		{name: "go/bin/go", body: "two", mode: 0o755},
	})
	work := t.TempDir()
	path := filepath.Join(work, "download.tar.gz")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	_, err := imageLayersReadGoArchive(path, "usr/local/go", work)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate entry error = %v, want a 'twice' refusal", err)
	}
}

// TestImageLayersRejectsUnsupportedEntryTypes: guessing how to normalize a
// device node or fifo would be inventing layer bytes.
func TestImageLayersRejectsUnsupportedEntryTypes(t *testing.T) {
	archive := goFixtureArchive(t, []goFixtureEntry{
		{name: "go/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "go/dev/null", typeflag: tar.TypeChar, mode: 0o666},
	})
	work := t.TempDir()
	path := filepath.Join(work, "download.tar.gz")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	_, err := imageLayersReadGoArchive(path, "usr/local/go", work)
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("unsupported type error = %v, want a refusal", err)
	}
}

func TestImageLayersPrefixValidation(t *testing.T) {
	if got, err := imageLayersPrefix("/usr/local/go/"); err != nil || got != "usr/local/go" {
		t.Fatalf("prefix = %q, %v; want usr/local/go", got, err)
	}
	for _, bad := range []string{"", "usr/local/go", "/", "/.."} {
		if _, err := imageLayersPrefix(bad); err == nil {
			t.Fatalf("path %q was accepted", bad)
		}
	}
}

// --- the producer end to end ---------------------------------------------------

// TestImageLayersProducesGoToolchainDeterministically is the acceptance claim:
// the verb writes .gen/layers/go-toolchain.tar plus its checksum sidecar, and
// two runs of the same inputs produce byte-identical output.
func TestImageLayersProducesGoToolchainDeterministically(t *testing.T) {
	dist := &fakeGoDist{payload: goFixtureCalm(t)}
	installGoDist(t, dist)
	ws := imageLayersWorkspace(t, "go 1.26.1\n\nuse (\n\t./libs/cli\n)\n")
	project := imageLayersProject(t, imageLayersGoToolchainManifest)

	run := func() []byte {
		t.Helper()
		var out []string
		if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(&out)); err != nil {
			t.Fatalf("cloud image-layers: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "go-toolchain.tar"))
		if err != nil {
			t.Fatalf("read produced layer: %v", err)
		}
		return data
	}

	first := run()
	second := run()
	if !bytes.Equal(first, second) {
		t.Fatalf("double run is not byte-equal: sha256 %s vs %s", sha256Hex(first), sha256Hex(second))
	}

	// The version is DERIVED: the producer asked go.dev for exactly the release
	// go.work declares, never a literal.
	wantFile := "go1.26.1.linux-amd64.tar.gz"
	if !slices.Equal(dist.askedFor, []string{wantFile, wantFile}) {
		t.Fatalf("checksum lookups = %v, want two %s lookups", dist.askedFor, wantFile)
	}
	if !slices.Equal(dist.downloadedAs, []string{wantFile, wantFile}) {
		t.Fatalf("downloads = %v, want two %s downloads", dist.downloadedAs, wantFile)
	}

	// The sidecar states the layer's own sha256 — the value the framework folds
	// into the image content key — plus the verified upstream artifact.
	recordPath := filepath.Join(project, ".gen", "layers", "go-toolchain.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read %s: %v", recordPath, err)
	}
	var record imageLayerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode %s: %v", recordPath, err)
	}
	if record.SHA256 != sha256Hex(first) {
		t.Fatalf("record sha256 = %s, layer hashes to %s", record.SHA256, sha256Hex(first))
	}
	if record.Size != int64(len(first)) {
		t.Fatalf("record size = %d, layer is %d bytes", record.Size, len(first))
	}
	if record.Version != "1.26.1" {
		t.Fatalf("record version = %q, want the go.work version 1.26.1", record.Version)
	}
	if record.Path != "/usr/local/go" || record.Tar != "go-toolchain.tar" {
		t.Fatalf("record path/tar = %q/%q", record.Path, record.Tar)
	}
	if record.Source.URL != "https://go.dev/dl/"+wantFile {
		t.Fatalf("record source url = %q", record.Source.URL)
	}
	if record.Source.SHA256 != sha256Hex(dist.payload) {
		t.Fatalf("record source sha256 = %q, want the published %s", record.Source.SHA256, sha256Hex(dist.payload))
	}
	if !slices.Equal(record.Pruned, imageLayersGoPrune) {
		t.Fatalf("record pruned = %v, want %v", record.Pruned, imageLayersGoPrune)
	}
}

// TestImageLayersGoToolchainVersionFollowsGoWorkToolchain proves the derived
// pin honors Go's own precedence: a `toolchain` line outranks the `go` line,
// exactly as imageBuildGoWorkVersion forwards it to the Dockerfile today.
func TestImageLayersGoToolchainVersionFollowsGoWorkToolchain(t *testing.T) {
	dist := &fakeGoDist{payload: goFixtureCalm(t)}
	installGoDist(t, dist)
	ws := imageLayersWorkspace(t, "go 1.26.1\n\ntoolchain go1.26.4\n")
	project := imageLayersProject(t, imageLayersGoToolchainManifest)
	var out []string
	if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(&out)); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	if want := "go1.26.4.linux-amd64.tar.gz"; !slices.Contains(dist.askedFor, want) {
		t.Fatalf("checksum lookups = %v, want %s", dist.askedFor, want)
	}
}

// TestImageLayersGoToolchainRefusesWithoutAGoWorkVersion: the layer has no
// version literal to fall back on, so an unreadable workspace is a hard failure.
func TestImageLayersGoToolchainRefusesWithoutAGoWorkVersion(t *testing.T) {
	dist := &fakeGoDist{payload: goFixtureCalm(t)}
	installGoDist(t, dist)
	project := imageLayersProject(t, imageLayersGoToolchainManifest)
	var out []string
	err := ImageLayers(map[string]any{"project": project}, nil, imageLayersWorkspace(t, ""), nil, imageLayersIO(&out))
	if err == nil || !strings.Contains(err.Error(), "declares no Go version") {
		t.Fatalf("error = %v, want a refusal naming the missing go.work version", err)
	}
	if len(dist.askedFor) != 0 {
		t.Fatalf("go.dev was consulted before the pin resolved: %v", dist.askedFor)
	}
}

// TestImageLayersHardFailsOnChecksumMismatch is image invariant 3: bytes that do
// not match the checksum go.dev published never become a layer, and the failure
// is loud rather than a warning. Nothing is written on the way out.
func TestImageLayersHardFailsOnChecksumMismatch(t *testing.T) {
	dist := &fakeGoDist{
		payload: goFixtureCalm(t),
		publish: strings.Repeat("ab", 32),
	}
	installGoDist(t, dist)
	ws := imageLayersWorkspace(t, "go 1.26.1\n")
	project := imageLayersProject(t, imageLayersGoToolchainManifest)

	var out []string
	err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(&out))
	if err == nil {
		t.Fatal("a checksum mismatch produced a layer")
	}
	for _, want := range []string{"checksum mismatch", dist.publish, sha256Hex(dist.payload), "unverified artifact"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err.Error(), want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "go-toolchain.tar")); !os.IsNotExist(statErr) {
		t.Fatalf("a layer survived the checksum mismatch (stat: %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "go-toolchain.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a record survived the checksum mismatch (stat: %v)", statErr)
	}
}

// TestImageLayersHardFailsOnUnreadablePublishedChecksum: comparing against a
// value that is not a hex digest would compare two different alphabets.
func TestImageLayersHardFailsOnUnreadablePublishedChecksum(t *testing.T) {
	dist := &fakeGoDist{payload: goFixtureCalm(t), publish: "sha256:NOTHEX"}
	installGoDist(t, dist)
	err := ImageLayers(
		map[string]any{"project": imageLayersProject(t, imageLayersGoToolchainManifest)},
		nil, imageLayersWorkspace(t, "go 1.26.1\n"), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "unreadable checksum") {
		t.Fatalf("error = %v, want a refusal on the unreadable published checksum", err)
	}
}

// TestImageLayersSurfacesChecksumLookupFailures: if the published checksum
// cannot be read there is nothing to verify against, so the run stops before it
// downloads anything.
func TestImageLayersSurfacesChecksumLookupFailures(t *testing.T) {
	dist := &fakeGoDist{payload: goFixtureCalm(t), checksumErr: fmt.Errorf("read the go.dev download index: 503 Service Unavailable")}
	installGoDist(t, dist)
	err := ImageLayers(
		map[string]any{"project": imageLayersProject(t, imageLayersGoToolchainManifest)},
		nil, imageLayersWorkspace(t, "go 1.26.1\n"), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("error = %v, want the index failure surfaced", err)
	}
	if len(dist.downloadedAs) != 0 {
		t.Fatalf("the toolchain was downloaded with nothing to verify it against: %v", dist.downloadedAs)
	}
}

// TestImageLayersSurfacesDownloadFailures keeps a transport failure from
// reading as "nothing to do".
func TestImageLayersSurfacesDownloadFailures(t *testing.T) {
	dist := &fakeGoDist{payload: goFixtureCalm(t), openErr: fmt.Errorf("dial go.dev: connection refused")}
	installGoDist(t, dist)
	err := ImageLayers(
		map[string]any{"project": imageLayersProject(t, imageLayersGoToolchainManifest)},
		nil, imageLayersWorkspace(t, "go 1.26.1\n"), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want the transport failure surfaced", err)
	}
}

// --- the manifest gate ---------------------------------------------------------

// TestImageLayersRefusesWithoutAManifest: image-layers.json IS the activation
// marker, so a bare invocation elsewhere is a caller mistake, not a skip.
func TestImageLayersRefusesWithoutAManifest(t *testing.T) {
	err := ImageLayers(map[string]any{"project": t.TempDir()}, nil, t.TempDir(), nil, imageLayersIO(new([]string)))
	if err == nil {
		t.Fatal("a project without image-layers.json was accepted")
	}
	if !strings.Contains(err.Error(), imageLayersManifestName) {
		t.Fatalf("error %q does not name %s", err.Error(), imageLayersManifestName)
	}
	if got := clicore.ExitCode(err); got != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want ExitUsage %d", got, clicore.ExitUsage)
	}
}

func TestImageLayersManifestValidation(t *testing.T) {
	cases := map[string]struct {
		manifest string
		wants    string
	}{
		"malformed":         {manifest: `{`, wants: "parse"},
		"no layers":         {manifest: `{"layers": []}`, wants: "declares no layers"},
		"unknown producer":  {manifest: `{"layers":[{"name":"x","producer":"nope","path":"/x"}]}`, wants: "unknown producer"},
		"escaping name":     {manifest: `{"layers":[{"name":"../../evil","producer":"go-toolchain","path":"/x"}]}`, wants: "invalid layer name"},
		"duplicate name":    {manifest: `{"layers":[{"name":"a","producer":"go-toolchain","path":"/x"},{"name":"a","producer":"go-toolchain","path":"/y"}]}`, wants: "twice"},
		"relative path":     {manifest: `{"layers":[{"name":"a","producer":"go-toolchain","path":"usr/local/go"}]}`, wants: "non-absolute"},
		"filesystem root":   {manifest: `{"layers":[{"name":"a","producer":"go-toolchain","path":"/"}]}`, wants: "filesystem root"},
		"unnamed component": {manifest: `{"layers":[{"producer":"go-toolchain","path":"/x"}]}`, wants: "invalid layer name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dist := &fakeGoDist{payload: goFixtureCalm(t)}
			installGoDist(t, dist)
			project := imageLayersProject(t, tc.manifest)
			err := ImageLayers(map[string]any{"project": project}, nil, imageLayersWorkspace(t, "go 1.26.1\n"), nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wants)
			}
			if len(dist.askedFor) != 0 {
				t.Fatalf("a rejected manifest still reached the network seam: %v", dist.askedFor)
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen")); !os.IsNotExist(statErr) {
				t.Fatalf("a rejected manifest still created .gen (stat: %v)", statErr)
			}
		})
	}
}

// TestImageLayersWritesOnlyInsideTheProject pins the `.gen` containment rule:
// this repository has a cross-worktree `.gen` poisoning history, so the
// producer must never write beside the project it was pointed at.
func TestImageLayersWritesOnlyInsideTheProject(t *testing.T) {
	dist := &fakeGoDist{payload: goFixtureCalm(t)}
	installGoDist(t, dist)
	ws := imageLayersWorkspace(t, "go 1.26.1\n")
	project := imageLayersProject(t, imageLayersGoToolchainManifest)
	var out []string
	if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(&out)); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatalf("read workspace root: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "go.work" {
			t.Fatalf("the producer wrote %q into the workspace root", entry.Name())
		}
	}
	// The scratch tree is cleaned up: only the layer and its record remain.
	produced, err := os.ReadDir(filepath.Join(project, ".gen", "layers"))
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	got := make([]string, 0, len(produced))
	for _, entry := range produced {
		got = append(got, entry.Name())
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"go-toolchain.json", "go-toolchain.tar"}) {
		t.Fatalf("output dir = %v, want exactly the layer and its record", got)
	}
}

// --- the real go.dev source ----------------------------------------------------

// TestGoDevDistReadsThePublishedChecksum exercises the real index reader
// against a local server: the artifact's row, not a neighboring release's.
func TestGoDevDistReadsThePublishedChecksum(t *testing.T) {
	want := strings.Repeat("0f", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mode") != "json" {
			t.Errorf("index request = %q, want mode=json", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[
 {"version":"go1.27rc2","files":[{"filename":"go1.27rc2.linux-amd64.tar.gz","sha256":"%s"}]},
 {"version":"go1.26.1","files":[
   {"filename":"go1.26.1.darwin-arm64.tar.gz","sha256":"%s"},
   {"filename":"go1.26.1.linux-amd64.tar.gz","sha256":"%s"}]}
]`, strings.Repeat("ff", 32), strings.Repeat("ee", 32), want)
	}))
	defer server.Close()

	dist := goDevDist{client: server.Client(), base: server.URL + "/"}
	got, err := dist.Checksum("go1.26.1.linux-amd64.tar.gz")
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	if got != want {
		t.Fatalf("checksum = %q, want %q", got, want)
	}
	if _, err := dist.Checksum("go9.9.9.linux-amd64.tar.gz"); err == nil {
		t.Fatal("an unpublished artifact returned a checksum")
	}
}

// TestGoDevDistDownloadsThePublishedArtifact exercises the real streaming path.
func TestGoDevDistDownloadsThePublishedArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/go1.26.1.linux-amd64.tar.gz" {
			t.Errorf("download path = %q", r.URL.Path)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("download carries no User-Agent")
		}
		_, _ = w.Write([]byte("toolchain-bytes"))
	}))
	defer server.Close()
	dist := goDevDist{client: server.Client(), base: server.URL + "/"}
	body, err := dist.Open(context.Background(), "go1.26.1.linux-amd64.tar.gz")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != "toolchain-bytes" {
		t.Fatalf("body = %q", data)
	}
}

// TestGoDevDistOpenSurfacesStatus keeps a 404 (a version go.dev never shipped)
// from being read as an empty download.
func TestGoDevDistOpenSurfacesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	dist := goDevDist{client: server.Client(), base: server.URL + "/"}
	body, err := dist.Open(context.Background(), "go1.26.1.linux-amd64.tar.gz")
	if err == nil {
		_ = body.Close()
		t.Fatal("a 404 download was accepted")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want the status surfaced", err)
	}
}

// TestGoDevDistURL pins the published location the sidecar records.
func TestGoDevDistURL(t *testing.T) {
	if got := (goDevDist{}).URL("go1.26.1.linux-amd64.tar.gz"); got != "https://go.dev/dl/go1.26.1.linux-amd64.tar.gz" {
		t.Fatalf("url = %q", got)
	}
}

// --- single-file layer fixtures ------------------------------------------------

// fakeGoBuild stands in for the cross-compiler AND for the binary it would
// write. It reads the scratch go.mod the producer just wrote and derives the
// build info an honest toolchain would embed, so the default path proves the
// producer asked for exactly the module, version and toolchain it claims — and
// `mutate` then hands the producer a binary built with the wrong Go, or from
// the wrong module, without anyone compiling anything.
type fakeGoBuild struct {
	dirs []string
	envs [][]string
	argv [][]string
	// mutate rewrites the embedded build info before the producer reads it.
	// A RESTORED tool has no counterpart here on purpose: its build info comes
	// out of the fixture bytes (fakeToolBinary), so a restore test states the
	// artifact it ships rather than patching one this fake invented.
	mutate   func(info *debug.BuildInfo)
	buildErr error
	infoErr  error
	info     map[string]*debug.BuildInfo
}

func (f *fakeGoBuild) run(_ context.Context, dir string, env, args []string, _ clicore.IO) error {
	f.dirs = append(f.dirs, dir)
	f.envs = append(f.envs, env)
	f.argv = append(f.argv, args)
	if f.buildErr != nil {
		return f.buildErr
	}
	goVersion, module, version := parseFakeGoMod(dir)
	output := ""
	for i, arg := range args {
		if arg == "-o" && i+1 < len(args) {
			output = args[i+1]
		}
	}
	if output == "" {
		return fmt.Errorf("no -o in %v", args)
	}
	// 0o600, deliberately: the producer must set the mode itself rather than
	// inherit whatever the compiler and the host umask left behind.
	if err := os.WriteFile(output, []byte("ELF:"+filepath.Base(output)+":"+version), 0o600); err != nil {
		return err
	}
	info := &debug.BuildInfo{
		GoVersion: "go" + goVersion,
		Path:      args[len(args)-1],
		Main:      debug.Module{Path: module, Version: "v" + version},
		Settings: []debug.BuildSetting{
			{Key: "-trimpath", Value: "true"},
			{Key: "CGO_ENABLED", Value: "0"},
			{Key: "GOARCH", Value: "amd64"},
			{Key: "GOOS", Value: "linux"},
		},
	}
	if f.mutate != nil {
		f.mutate(info)
	}
	if f.info == nil {
		f.info = map[string]*debug.BuildInfo{}
	}
	f.info[output] = info
	return nil
}

func (f *fakeGoBuild) readBuildInfo(path string) (*debug.BuildInfo, error) {
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	if info, ok := f.info[path]; ok {
		return info, nil
	}
	// A RESTORED tool was never compiled here, so its build info comes out of
	// the fixture bytes themselves (fakeToolBinary). That keeps the restore
	// tests driving every verification rule by writing a different artifact,
	// exactly as a real @putnami/go release would.
	if info, err := readFakeToolBuildInfo(path); err == nil {
		return info, nil
	} else if !os.IsNotExist(err) && !errors.Is(err, errNotAFakeToolBinary) {
		return nil, err
	} else if os.IsNotExist(err) {
		return nil, err
	}
	return nil, fmt.Errorf("%s: not a Go binary", path)
}

// errNotAFakeToolBinary marks a file that exists but carries no fixture build
// info, so the caller can fall through to the "not a Go binary" answer.
var errNotAFakeToolBinary = errors.New("no fixture build info")

// fakeToolBinary is the body a fixture writes for a tool the @putnami/go
// artifact ships: the embedded build info a real binary would carry, in a form
// readFakeToolBuildInfo decodes without a compiler.
func fakeToolBinary(goVersion, module, version, pkg string, settings ...string) string {
	fields := append([]string{"BUILDINFO", goVersion, module, version, pkg}, settings...)
	return strings.Join(fields, " ") + "\n"
}

// readFakeToolBuildInfo decodes a fakeToolBinary body. Settings default to the
// linux/amd64 pure-Go build the layer requires and are overridden pairwise.
func readFakeToolBuildInfo(path string) (*debug.BuildInfo, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a fixture path under the test's own scratch tree
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 5 || fields[0] != "BUILDINFO" {
		return nil, errNotAFakeToolBinary
	}
	settings := map[string]string{
		"GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1", "CGO_ENABLED": "0", "-trimpath": "true",
	}
	for i := 5; i+1 < len(fields); i += 2 {
		settings[fields[i]] = fields[i+1]
	}
	info := &debug.BuildInfo{
		GoVersion: fields[1],
		Path:      fields[4],
		Main:      debug.Module{Path: fields[2], Version: fields[3]},
	}
	for _, key := range slices.Sorted(maps.Keys(settings)) {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: key, Value: settings[key]})
	}
	return info, nil
}

// parseFakeGoMod reads back the scratch module the producer wrote.
func parseFakeGoMod(dir string) (goVersion, module, version string) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: the scratch dir the producer under test just wrote
	if err != nil {
		return "", "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "go":
			goVersion = fields[1]
		case len(fields) == 3 && fields[0] == "require":
			module, version = fields[1], strings.TrimPrefix(fields[2], "v")
		}
	}
	return goVersion, module, version
}

// installGoBuild routes both the compiler and the build-info reader at the
// fixture. No test in this file compiles anything or touches the network.
func installGoBuild(t *testing.T, fake *fakeGoBuild) {
	t.Helper()
	previousRun, previousInfo := imageLayersGoRun, imageLayersReadBuildInfo
	t.Cleanup(func() { imageLayersGoRun, imageLayersReadBuildInfo = previousRun, previousInfo })
	imageLayersGoRun = fake.run
	imageLayersReadBuildInfo = fake.readBuildInfo
}

// fakeCLIDist is the put-server seam under test control.
type fakeCLIDist struct {
	payload []byte
	openErr error
	asked   []string
}

func (f *fakeCLIDist) URL(version string) string {
	return "https://put.putnami.dev/putnami/cli/download?arch=x64&channel=" + version + "&os=linux"
}

func (f *fakeCLIDist) Open(_ context.Context, version string) (io.ReadCloser, error) {
	f.asked = append(f.asked, version)
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(bytes.NewReader(f.payload)), nil
}

func installCLIDist(t *testing.T, dist *fakeCLIDist) {
	t.Helper()
	previous := imageLayersCLIDistFor
	t.Cleanup(func() { imageLayersCLIDistFor = previous })
	imageLayersCLIDistFor = func(*http.Client) imageLayersCLIDist { return dist }
}

// imageLayersToolVersions writes the @putnami/go extension's tool manifest at
// the exact workspace-local path imageBuildGoToolPath reads.
func imageLayersToolVersions(t *testing.T, ws string, tools map[string]string) {
	t.Helper()
	path := imageBuildGoToolPath(ws)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the extension tool dir: %v", err)
	}
	manifest := map[string]any{"tools": map[string]any{}}
	for tool, version := range tools {
		manifest["tools"].(map[string]any)[tool] = map[string]string{"version": version}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode versions.json: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write versions.json: %v", err)
	}
}

// imageLayersLock writes putnami.lock.json's cli block.
func imageLayersLock(t *testing.T, ws, version string, integrities map[string]string) {
	t.Helper()
	// The extension block travels with the CLI block: every derived `go-tool`
	// layer restores its binary from the lock-pinned @putnami/go artifact, so a
	// workspace that declares a CLI pin and no extension pin is not a workspace
	// any tool layer can be produced from.
	lock := map[string]any{
		"cli": map[string]any{"version": version, "integrities": integrities},
		"extensions": map[string]any{
			"@putnami/go":         map[string]any{"version": "0.1.0-8885222db", "integrities": map[string]string{"linux/amd64": warmGoDigest}},
			"@putnami/typescript": map[string]any{"version": "0.1.0-8885222db", "integrities": map[string]string{"linux/amd64": warmTSDigest}},
		},
	}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("encode putnami.lock.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "putnami.lock.json"), data, 0o600); err != nil {
		t.Fatalf("write putnami.lock.json: %v", err)
	}
}

// imageLayersToolWorkspace is a workspace declaring every source the single-file
// producers derive from: the Go pin, the lint tool pins, and the CLI pin with
// its platform integrity.
func imageLayersToolWorkspace(t *testing.T) string {
	t.Helper()
	ws := imageLayersToolWorkspaceBase(t)
	// Derived tool layers no longer compile: they take the linux/amd64 binary
	// out of the materialized @putnami/go artifact, so the seam that stands in
	// for `putnami extensions install` belongs to every tool workspace. A test
	// that drives the materializer itself uses imageLayersToolWorkspaceBase.
	installWarmRun(t, &fakeWarmInstall{tree: warmFixtureStore()})
	return ws
}

// imageLayersToolWorkspaceBase is the same workspace with no materializer
// installed, for the tests that install their own.
func imageLayersToolWorkspaceBase(t *testing.T) string {
	t.Helper()
	ws := imageLayersWorkspace(t, "go 1.26.1\n")
	imageLayersToolVersions(t, ws, map[string]string{"golangci-lint": "v2.10.1", "staticcheck": "v0.7.0"})
	imageLayersLock(t, ws, "0.1.0-8885222db", map[string]string{"linux/amd64": strings.Repeat("cd", 32)})
	return ws
}

const imageLayersStaticcheckLayer = `{"name":"staticcheck","producer":"go-tool","path":"/usr/local/bin/staticcheck",` +
	`"tool":"staticcheck","package":"honnef.co/go/tools/cmd/staticcheck","module":"honnef.co/go/tools"}`

const imageLayersCraneLayer = `{"name":"crane","producer":"go-tool","path":"/usr/local/bin/crane",` +
	`"version":"0.21.6","package":"github.com/google/go-containerregistry/cmd/crane","module":"github.com/google/go-containerregistry"}`

const imageLayersCLILayer = `{"name":"putnami","producer":"putnami-cli","path":"/usr/local/bin/putnami"}`

const imageLayersToolManifest = `{"layers":[` + imageLayersStaticcheckLayer + `,` + imageLayersCraneLayer + `,` + imageLayersCLILayer + `]}`

// --- the Go tool producer -------------------------------------------------------

// TestImageLayersProducesToolBinaries is the acceptance claim for the
// single-file layers: the verb writes each one into .gen/layers/bin/ with its
// sidecar, on a host with no docker and no linux runtime.
func TestImageLayersProducesToolBinaries(t *testing.T) {
	build := &fakeGoBuild{}
	installGoBuild(t, build)
	cli := &fakeCLIDist{payload: []byte("putnami-cli-elf")}
	installCLIDist(t, cli)
	ws := imageLayersToolWorkspace(t)
	imageLayersLock(t, ws, "0.1.0-8885222db", map[string]string{"linux/amd64": sha256Hex(cli.payload)})
	project := imageLayersProject(t, imageLayersToolManifest)

	var out []string
	if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(&out)); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}

	binDir := filepath.Join(project, ".gen", "layers", "bin")
	for _, name := range []string{"staticcheck", "crane", "putnami"} {
		info, err := os.Stat(filepath.Join(binDir, name))
		if err != nil {
			t.Fatalf("stat produced %s: %v", name, err)
		}
		// The mode is set by the producer, not inherited from the compiler or
		// the host umask.
		if info.Mode().Perm() != imageLayersToolFileMode {
			t.Fatalf("%s mode = %o, want %o", name, info.Mode().Perm(), imageLayersToolFileMode)
		}
	}

	// The lint tool's version is DERIVED from the extension manifest; crane's is
	// the image-owned pin the manifest declares. Each is recorded inside the
	// label that states the whole build input — module, version, package — so
	// `--check` compares everything that selected these bytes, not just the
	// version. The derived tool adds a fourth input: the @putnami/go release its
	// bytes were RESTORED from, because the framework can republish one tool
	// pin built with another Go.
	staticcheck := imageLayersReadRecord(t, project, "staticcheck")
	wantStaticcheck := "honnef.co/go/tools v0.7.0 honnef.co/go/tools/cmd/staticcheck" +
		" from @putnami/go 0.1.0-8885222db " + warmGoDigest
	if staticcheck.Version != wantStaticcheck {
		t.Fatalf("staticcheck version = %q, want %q", staticcheck.Version, wantStaticcheck)
	}
	// It was restored, not compiled: only crane reached the compiler, and the
	// produced bytes ARE the artifact's.
	if len(build.argv) != 1 || build.argv[0][len(build.argv[0])-1] != "github.com/google/go-containerregistry/cmd/crane" {
		t.Fatalf("go invocations = %v, want crane alone — a derived tool is restored from the extension artifact", build.argv)
	}
	if len(staticcheck.Source.Extensions) != 1 ||
		staticcheck.Source.Extensions[0].Name != "@putnami/go" ||
		staticcheck.Source.Extensions[0].SHA256 != warmGoDigest {
		t.Fatalf("staticcheck source extensions = %+v, want the verified @putnami/go artifact", staticcheck.Source.Extensions)
	}
	if staticcheck.File != "bin/staticcheck" || staticcheck.Tar != "" {
		t.Fatalf("staticcheck record artifact = %q/%q, want the file form", staticcheck.File, staticcheck.Tar)
	}
	if staticcheck.Path != "/usr/local/bin/staticcheck" {
		t.Fatalf("staticcheck in-image path = %q", staticcheck.Path)
	}
	if staticcheck.Source.Package != "honnef.co/go/tools/cmd/staticcheck@v0.7.0" {
		t.Fatalf("staticcheck source package = %q", staticcheck.Source.Package)
	}
	if staticcheck.Source.Toolchain != "go1.26.1" || staticcheck.Source.Module != "honnef.co/go/tools" {
		t.Fatalf("staticcheck source = %+v, want the verified build info", staticcheck.Source)
	}
	produced, err := os.ReadFile(filepath.Join(binDir, "staticcheck"))
	if err != nil {
		t.Fatalf("read produced staticcheck: %v", err)
	}
	if want := warmFixtureStore()[warmGoDigest]["compiled/tools/staticcheck"]; string(produced) != want {
		t.Fatalf("produced staticcheck = %q, want the artifact's own %q", produced, want)
	}
	if staticcheck.SHA256 != sha256Hex(produced) || staticcheck.Size != int64(len(produced)) {
		t.Fatalf("staticcheck record %s/%d does not describe the %d produced bytes", staticcheck.SHA256, staticcheck.Size, len(produced))
	}
	wantCrane := "github.com/google/go-containerregistry v0.21.6 github.com/google/go-containerregistry/cmd/crane"
	if crane := imageLayersReadRecord(t, project, "crane"); crane.Version != wantCrane {
		t.Fatalf("crane version = %q, want the image-owned %q", crane.Version, wantCrane)
	}

	// The CLI carries the lock's integrity as its verified source, not a URL
	// nobody checked — and its identity is version AND integrity, so a rebuild
	// published under an unchanged version does not read as current.
	putnami := imageLayersReadRecord(t, project, "putnami")
	if want := "0.1.0-8885222db " + sha256Hex(cli.payload); putnami.Version != want {
		t.Fatalf("cli version = %q, want the lock pin and the integrity it was verified against %q", putnami.Version, want)
	}
	if putnami.Source.SHA256 != sha256Hex(cli.payload) {
		t.Fatalf("cli source sha256 = %q, want the lock integrity %s", putnami.Source.SHA256, sha256Hex(cli.payload))
	}
	if !strings.Contains(putnami.Source.URL, "channel=0.1.0-8885222db") {
		t.Fatalf("cli source url = %q, want the pinned channel", putnami.Source.URL)
	}
	if !slices.Equal(cli.asked, []string{"0.1.0-8885222db"}) {
		t.Fatalf("cli downloads = %v", cli.asked)
	}

	// Containment: the scratch modules are gone and nothing was written beside
	// the project.
	entries, err := os.ReadDir(filepath.Join(project, ".gen", "layers"))
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"bin", "crane.json", "putnami.json", "staticcheck.json"}) {
		t.Fatalf("output dir = %v, want the bin/ tree and one record per layer", got)
	}
}

// imageLayersReadRecord decodes one produced sidecar.
func imageLayersReadRecord(t *testing.T, project, name string) imageLayerRecord {
	t.Helper()
	path := filepath.Join(project, ".gen", "layers", name+".json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path this test just produced
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var record imageLayerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return record
}

// TestImageLayersGoToolBuildIsHermetic pins the command that makes an
// IMAGE-OWNED tool reproducible — crane is the only one this producer still
// compiles. Every element is load-bearing: -p=2 bounds the compiler even
// when the host injects a conflicting GOFLAGS value, -trimpath and CGO_ENABLED=0
// are what make two hosts agree, -buildvcs=false stops the checkout the scratch
// module happens to sit in from being stamped into the binary, and GOTOOLCHAIN
// is the pinned compiler.
func TestImageLayersGoToolBuildIsHermetic(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=vendor -tags=host-only -p=64")
	build := &fakeGoBuild{}
	installGoBuild(t, build)
	installCLIDist(t, &fakeCLIDist{payload: []byte("x")})
	ws := imageLayersToolWorkspace(t)
	imageLayersLock(t, ws, "1.2.3", map[string]string{"linux/amd64": sha256Hex([]byte("x"))})
	project := imageLayersProject(t, `{"layers":[`+imageLayersCraneLayer+`]}`)
	if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string))); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	if len(build.argv) != 1 {
		t.Fatalf("go invocations = %d, want exactly one build", len(build.argv))
	}
	argv := build.argv[0]
	want := []string{"build", "-p=2", "-mod=mod", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o"}
	if !slices.Equal(argv[:len(want)], want) {
		t.Fatalf("argv = %v, want the deterministic build flags %v", argv, want)
	}
	if argv[len(argv)-1] != "github.com/google/go-containerregistry/cmd/crane" {
		t.Fatalf("argv builds %q", argv[len(argv)-1])
	}
	// The build happens in the scratch module inside the project's own .gen.
	if !strings.HasPrefix(build.dirs[0], filepath.Join(project, ".gen", "layers")) {
		t.Fatalf("build ran in %q, outside the project's .gen tree", build.dirs[0])
	}

	env := map[string]string{}
	for _, entry := range build.envs[0] {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value // a later assignment wins, exactly as exec resolves it
	}
	for key, wantValue := range map[string]string{
		"GOTOOLCHAIN":  "go1.26.1",
		"GOOS":         "linux",
		"GOARCH":       "amd64",
		"GOAMD64":      "v1",
		"CGO_ENABLED":  "0",
		"GOWORK":       "off",
		"GOFLAGS":      "",
		"GOEXPERIMENT": "",
	} {
		if env[key] != wantValue {
			t.Fatalf("build env %s = %q, want %q", key, env[key], wantValue)
		}
	}
}

// TestImageLayersGoModRequiresThePinnedModule: the scratch module is how a
// `pkg@version` reference is resolved without `go install` (which refuses to
// write a cross-compiled binary to an explicit GOBIN), so it must require
// exactly the pinned version under exactly the pinned Go.
func TestImageLayersGoModRequiresThePinnedModule(t *testing.T) {
	got := imageLayersGoMod("1.26.1", "honnef.co/go/tools", "0.7.0")
	for _, want := range []string{"go 1.26.1\n", "require honnef.co/go/tools v0.7.0\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("go.mod %q does not contain %q", got, want)
		}
	}
}

// TestImageLayersVerifyGoToolAcceptsThePinnedBuild and its rejection table are
// the execution-free verification: what `go version -m` proves inside the image
// today, proved here on a host that cannot run the binary at all.
func TestImageLayersVerifyGoToolAcceptsThePinnedBuild(t *testing.T) {
	if err := imageLayersVerifyGoTool(imageLayersBuildInfoFixture(nil), "1.26.1", "honnef.co/go/tools", "0.7.0"); err != nil {
		t.Fatalf("an honest build was rejected: %v", err)
	}
}

func TestImageLayersVerifyGoToolRejects(t *testing.T) {
	cases := map[string]struct {
		mutate func(*debug.BuildInfo)
		wants  string
	}{
		"another toolchain": {
			mutate: func(info *debug.BuildInfo) { info.GoVersion = "go1.25.7" },
			wants:  "not the pinned go1.26.1",
		},
		"another module": {
			mutate: func(info *debug.BuildInfo) { info.Main.Path = "example.com/impostor" },
			wants:  "not the declared",
		},
		"another version": {
			mutate: func(info *debug.BuildInfo) { info.Main.Version = "v0.6.0" },
			wants:  `not the pinned "v0.7.0"`,
		},
		"host platform": {
			mutate: func(info *debug.BuildInfo) { info.Settings[3].Value = "darwin" },
			wants:  "GOOS",
		},
		"cgo": {
			mutate: func(info *debug.BuildInfo) { info.Settings[1].Value = "1" },
			wants:  "CGO_ENABLED",
		},
		"untrimmed paths": {
			mutate: func(info *debug.BuildInfo) { info.Settings = info.Settings[1:] },
			wants:  "-trimpath",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := imageLayersVerifyGoTool(imageLayersBuildInfoFixture(tc.mutate), "1.26.1", "honnef.co/go/tools", "0.7.0")
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wants)
			}
		})
	}
	if err := imageLayersVerifyGoTool(nil, "1.26.1", "honnef.co/go/tools", "0.7.0"); err == nil {
		t.Fatal("a binary with no embedded build info was accepted")
	}
}

// imageLayersBuildInfoFixture is an honest linux/amd64 staticcheck build, with
// an optional mutation. Settings order is fixed so the table above can name one.
func imageLayersBuildInfoFixture(mutate func(*debug.BuildInfo)) *debug.BuildInfo {
	info := &debug.BuildInfo{
		GoVersion: "go1.26.1",
		Main:      debug.Module{Path: "honnef.co/go/tools", Version: "v0.7.0"},
		Settings: []debug.BuildSetting{
			{Key: "-trimpath", Value: "true"},
			{Key: "CGO_ENABLED", Value: "0"},
			{Key: "GOARCH", Value: "amd64"},
			{Key: "GOOS", Value: "linux"},
		},
	}
	if mutate != nil {
		mutate(info)
	}
	return info
}

// TestImageLayersGoToolRefusesAWrongToolchainBuild is the loud failure the
// Dockerfile's grep provides today: a tool compiled with another Go panics on
// every `putnami lint` run, so it must never reach .gen/layers/bin.
func TestImageLayersGoToolRefusesAWrongToolchainBuild(t *testing.T) {
	build := &fakeGoBuild{mutate: func(info *debug.BuildInfo) { info.GoVersion = "go1.25.7" }}
	installGoBuild(t, build)
	ws := imageLayersToolWorkspace(t)
	project := imageLayersProject(t, `{"layers":[`+imageLayersCraneLayer+`]}`)
	err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "go1.25.7") || !strings.Contains(err.Error(), "panics") {
		t.Fatalf("error = %v, want a refusal naming the wrong toolchain", err)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "bin", "crane")); !os.IsNotExist(statErr) {
		t.Fatalf("a mis-built tool survived into bin/ (stat: %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "crane.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a record survived the verification failure (stat: %v)", statErr)
	}
}

// TestImageLayersGoToolSurfacesBuildFailures keeps a failed cross-compile from
// reading as "nothing to do".
func TestImageLayersGoToolSurfacesBuildFailures(t *testing.T) {
	installGoBuild(t, &fakeGoBuild{buildErr: fmt.Errorf("exit status 1")})
	err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersCraneLayer+`]}`)},
		nil, imageLayersToolWorkspace(t), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %v, want the build failure surfaced", err)
	}
}

// TestImageLayersGoToolRefusesANonGoArtifact: build info that cannot be read is
// not evidence, so it cannot be treated as a pass.
func TestImageLayersGoToolRefusesANonGoArtifact(t *testing.T) {
	installGoBuild(t, &fakeGoBuild{infoErr: fmt.Errorf("not a Go executable")})
	err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersCraneLayer+`]}`)},
		nil, imageLayersToolWorkspace(t), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "not a Go executable") {
		t.Fatalf("error = %v, want the unreadable build info surfaced", err)
	}
}

// TestImageLayersGoToolRefusesWithoutADerivedPin: the lint tools have no version
// literal to fall back on, so an extension manifest that is missing (or not
// installed) is a hard failure naming the path it was looked for at.
func TestImageLayersGoToolRefusesWithoutADerivedPin(t *testing.T) {
	build := &fakeGoBuild{}
	installGoBuild(t, build)
	ws := imageLayersWorkspace(t, "go 1.26.1\n") // no .putnami extension tree
	err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersStaticcheckLayer+`]}`)},
		nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "declares no \"staticcheck\" version") {
		t.Fatalf("error = %v, want a refusal naming the missing derived pin", err)
	}
	if len(build.argv) != 0 {
		t.Fatalf("a build ran with no resolved pin: %v", build.argv)
	}
}

// TestImageLayersGoToolRefusesWithoutAGoWorkVersion: the toolchain a lint tool
// is built with is the whole point of the layer, so it is never guessed.
func TestImageLayersGoToolRefusesWithoutAGoWorkVersion(t *testing.T) {
	build := &fakeGoBuild{}
	installGoBuild(t, build)
	ws := imageLayersWorkspace(t, "")
	imageLayersToolVersions(t, ws, map[string]string{"staticcheck": "v0.7.0"})
	err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersStaticcheckLayer+`]}`)},
		nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "declares no Go version") {
		t.Fatalf("error = %v, want a refusal naming the missing go.work version", err)
	}
	if len(build.argv) != 0 {
		t.Fatalf("a build ran with no pinned toolchain: %v", build.argv)
	}
}

// --- the restored tool layers --------------------------------------------------

// imageLayersGolangciLayer is the second DERIVED tool layer, so a manifest can
// prove two of them share one materialization.
const imageLayersGolangciLayer = `{"name":"golangci-lint","producer":"go-tool","path":"/usr/local/libexec/golangci-lint",` +
	`"tool":"golangci-lint","package":"github.com/golangci/golangci-lint/v2/cmd/golangci-lint",` +
	`"module":"github.com/golangci/golangci-lint/v2"}`

// TestImageLayersRestoresDerivedToolsFromOneMaterialization is the acceptance
// claim for restored tools: the pinned lint tools are taken out of the @putnami/go
// artifact instead of compiled, three layers read ONE materialization, and no
// `go` invocation happens at all. That last fact is what let the CI entrypoint
// delete its Go warm-up — a compiled lint tool is what made the runner
// pre-download two large module graphs before the offline task graph.
func TestImageLayersRestoresDerivedToolsFromOneMaterialization(t *testing.T) {
	build := &fakeGoBuild{}
	installGoBuild(t, build)
	ws := imageLayersToolWorkspaceBase(t)
	fake := &fakeWarmInstall{tree: warmFixtureStore()}
	installWarmRun(t, fake)
	project := imageLayersProject(t,
		`{"layers":[`+imageLayersGolangciLayer+`,`+imageLayersStaticcheckLayer+`,`+imageLayersWarmLayer+`]}`)

	if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string))); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	if len(build.argv) != 0 {
		t.Fatalf("go invocations = %v, want none — a derived tool is restored, never compiled", build.argv)
	}
	if len(fake.stores) != 1 {
		t.Fatalf("the extension set was materialized %d time(s), want once for the whole run", len(fake.stores))
	}
	store := warmFixtureStore()[warmGoDigest]
	for _, layer := range []struct{ name, entry string }{
		{"golangci-lint", "compiled/tools/golangci-lint"},
		{"staticcheck", "compiled/tools/staticcheck"},
	} {
		produced, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "bin", layer.name))
		if err != nil {
			t.Fatalf("read the restored %s: %v", layer.name, err)
		}
		if string(produced) != store[layer.entry] {
			t.Fatalf("%s = %q, want the artifact's %s", layer.name, produced, layer.entry)
		}
		record := imageLayersReadRecord(t, project, layer.name)
		if record.Source.Toolchain != "go1.26.1" {
			t.Fatalf("%s toolchain = %q, want what the artifact's build info reported", layer.name, record.Source.Toolchain)
		}
		if record.SHA256 != sha256Hex(produced) || record.Size != int64(len(produced)) {
			t.Fatalf("%s record %s/%d does not describe the %d restored bytes", layer.name, record.SHA256, record.Size, len(produced))
		}
	}
	// The warm layer still carries the artifact the two tools were taken from:
	// the store is LENT to them, never consumed.
	tar, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "putnami-warm.tar"))
	if err != nil {
		t.Fatalf("read the warm layer: %v", err)
	}
	want := "root/.putnami/artifacts/sha256/" + warmGoDigest[:2] + "/" + warmGoDigest + "/compiled/tools/staticcheck"
	if !slices.Contains(headerNames(readTarEntries(t, tar)), want) {
		t.Fatalf("the warm layer lost %s to the restore", want)
	}
}

// TestImageLayersRestoredGoToolRejects is the execution-free verification of a
// binary this producer did NOT build. Each case ships an artifact that is wrong
// in exactly one way, and none of them may become a layer.
func TestImageLayersRestoredGoToolRejects(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"another module": {
			body: fakeToolBinary("go1.26.1", "honnef.co/go/other", "v0.7.0", "honnef.co/go/tools/cmd/staticcheck"),
			want: `built from module "honnef.co/go/other"`,
		},
		"another module version": {
			body: fakeToolBinary("go1.26.1", "honnef.co/go/tools", "v0.6.0", "honnef.co/go/tools/cmd/staticcheck"),
			want: `reports module version "v0.6.0"`,
		},
		"a sibling command out of the same module": {
			body: fakeToolBinary("go1.26.1", "honnef.co/go/tools", "v0.7.0", "honnef.co/go/tools/cmd/structlayout"),
			want: `is the "honnef.co/go/tools/cmd/structlayout" command`,
		},
		"a Go minor behind the workspace": {
			body: fakeToolBinary("go1.25.7", "honnef.co/go/tools", "v0.7.0", "honnef.co/go/tools/cmd/staticcheck"),
			want: "built with go1.25.7 (go1.25), and the workspace pins go1.26.1 (go1.26)",
		},
		"another platform": {
			body: fakeToolBinary("go1.26.1", "honnef.co/go/tools", "v0.7.0", "honnef.co/go/tools/cmd/staticcheck", "GOARCH", "arm64"),
			want: `was built with GOARCH="arm64"`,
		},
		"a newer microarchitecture level": {
			body: fakeToolBinary("go1.26.1", "honnef.co/go/tools", "v0.7.0", "honnef.co/go/tools/cmd/staticcheck", "GOAMD64", "v3"),
			want: `was built with GOAMD64="v3"`,
		},
		"a cgo build": {
			body: fakeToolBinary("go1.26.1", "honnef.co/go/tools", "v0.7.0", "honnef.co/go/tools/cmd/staticcheck", "CGO_ENABLED", "1"),
			want: `was built with CGO_ENABLED="1"`,
		},
		"no Go binary at all": {
			body: "#!/bin/sh\nexec staticcheck \"$@\"\n",
			want: "not a Go binary",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			installGoBuild(t, &fakeGoBuild{})
			ws := imageLayersToolWorkspaceBase(t)
			tree := warmFixtureStore()
			tree[warmGoDigest]["compiled/tools/staticcheck"] = tc.body
			installWarmRun(t, &fakeWarmInstall{tree: tree})
			project := imageLayersProject(t, `{"layers":[`+imageLayersStaticcheckLayer+`]}`)
			err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want a refusal containing %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "bin", "staticcheck")); !os.IsNotExist(statErr) {
				t.Fatalf("an unverified artifact became a layer (stat: %v)", statErr)
			}
		})
	}
	// debug/buildinfo can answer (nil, nil) for a file it neither rejects nor
	// understands, and the restored path has no compile that would have failed
	// first. Same refusal as the compiled twin.
	spec := imageLayerSpec{Module: "honnef.co/go/tools", Package: "honnef.co/go/tools/cmd/staticcheck"}
	if err := imageLayersVerifyRestoredGoTool(nil, "1.26.1", spec, "0.7.0"); err == nil {
		t.Fatal("a restored binary with no embedded build info was accepted")
	}
}

// TestImageLayersRestoredGoToolAcceptsAnotherPatchToolchain states the rule
// deliberately: the upstream tool is built with the FRAMEWORK's Go, which is
// another repository's pin, so the patch release legitimately differs. The Go
// MINOR is what a type checker needs, and it is what the framework's own
// restore rule compares.
func TestImageLayersRestoredGoToolAcceptsAnotherPatchToolchain(t *testing.T) {
	installGoBuild(t, &fakeGoBuild{})
	ws := imageLayersToolWorkspaceBase(t)
	tree := warmFixtureStore()
	tree[warmGoDigest]["compiled/tools/staticcheck"] =
		fakeToolBinary("go1.26.4", "honnef.co/go/tools", "v0.7.0", "honnef.co/go/tools/cmd/staticcheck")
	installWarmRun(t, &fakeWarmInstall{tree: tree})
	project := imageLayersProject(t, `{"layers":[`+imageLayersStaticcheckLayer+`]}`)
	if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string))); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	if record := imageLayersReadRecord(t, project, "staticcheck"); record.Source.Toolchain != "go1.26.4" {
		t.Fatalf("recorded toolchain = %q, want the artifact's own go1.26.4", record.Source.Toolchain)
	}
}

// TestImageLayersRestoredGoToolRefusesAnArtifactWithoutTheTool: an @putnami/go
// release that stopped shipping compiled/tools is a refusal naming the missing
// entry, not a silent fallback to compiling — the runner no longer prefetches
// the module graph a fallback would need.
func TestImageLayersRestoredGoToolRefusesAnArtifactWithoutTheTool(t *testing.T) {
	build := &fakeGoBuild{}
	installGoBuild(t, build)
	ws := imageLayersToolWorkspaceBase(t)
	tree := warmFixtureStore()
	delete(tree[warmGoDigest], "compiled/tools/staticcheck")
	installWarmRun(t, &fakeWarmInstall{tree: tree})
	err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersStaticcheckLayer+`]}`)},
		nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "compiled/tools/staticcheck") {
		t.Fatalf("error = %v, want a refusal naming the missing artifact entry", err)
	}
	if len(build.argv) != 0 {
		t.Fatalf("a compile fallback ran: %v", build.argv)
	}
}

// TestImageLayersRestoredGoToolSurfacesMaterializationFailures: the restore
// depends on the same install the warm layer does, and a failure there must not
// read as "the tool is fine".
func TestImageLayersRestoredGoToolSurfacesMaterializationFailures(t *testing.T) {
	installGoBuild(t, &fakeGoBuild{})
	ws := imageLayersToolWorkspaceBase(t)
	installWarmRun(t, &fakeWarmInstall{installErr: fmt.Errorf("exit status 1: resolve @putnami/go: connection refused")})
	err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersStaticcheckLayer+`]}`)},
		nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want the materialization failure surfaced", err)
	}
}

// TestImageLayersGoMinor pins the reduction the toolchain rule compares with.
func TestImageLayersGoMinor(t *testing.T) {
	for version, want := range map[string]string{
		"go1.26.1": "go1.26",
		"go1.26":   "go1.26",
		"1.26.1":   "go1.26",
		"go1.9":    "go1.9",
		"go2":      "go2",
	} {
		if got := imageLayersGoMinor(version); got != want {
			t.Errorf("imageLayersGoMinor(%q) = %q, want %q", version, got, want)
		}
	}
}

// --- the putnami CLI producer ---------------------------------------------------

// TestImageLayersCLIRefusesWithoutALockIntegrity is the issue's explicit hard
// fail: the integrity entry has gone missing across CLI bumps, and tolerating
// that would bake whatever the endpoint served.
func TestImageLayersCLIRefusesWithoutALockIntegrity(t *testing.T) {
	cases := map[string]struct {
		lock  func(t *testing.T, ws string)
		wants string
	}{
		"no integrity for the platform": {
			lock: func(t *testing.T, ws string) {
				imageLayersLock(t, ws, "1.2.3", map[string]string{"darwin/arm64": strings.Repeat("ab", 32)})
			},
			wants: "records no cli.integrities",
		},
		"no version": {
			lock: func(t *testing.T, ws string) {
				imageLayersLock(t, ws, "", map[string]string{"linux/amd64": strings.Repeat("ab", 32)})
			},
			wants: "pins no cli.version",
		},
		"unreadable integrity": {
			lock: func(t *testing.T, ws string) {
				imageLayersLock(t, ws, "1.2.3", map[string]string{"linux/amd64": "sha256:NOTHEX"})
			},
			wants: "unreadable cli.integrities",
		},
		"no lock at all": {
			lock:  func(t *testing.T, ws string) { _ = os.Remove(filepath.Join(ws, "putnami.lock.json")) },
			wants: "pins no cli.version",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dist := &fakeCLIDist{payload: []byte("cli")}
			installCLIDist(t, dist)
			ws := imageLayersToolWorkspace(t)
			tc.lock(t, ws)
			err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersCLILayer+`]}`)},
				nil, ws, nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error = %v, want a refusal naming %q", err, tc.wants)
			}
			if len(dist.asked) != 0 {
				t.Fatalf("the CLI was downloaded with nothing to verify it against: %v", dist.asked)
			}
		})
	}
}

// TestImageLayersCLIHardFailsOnIntegrityMismatch: bytes that do not hash to what
// the lock records never become a layer. This is what the Dockerfile's
// `--version` smoke test cannot do — it proves the download runs, not that it is
// the pinned artifact.
func TestImageLayersCLIHardFailsOnIntegrityMismatch(t *testing.T) {
	dist := &fakeCLIDist{payload: []byte("something-else")}
	installCLIDist(t, dist)
	ws := imageLayersToolWorkspace(t)
	project := imageLayersProject(t, `{"layers":[`+imageLayersCLILayer+`]}`)
	err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil {
		t.Fatal("an integrity mismatch produced a layer")
	}
	for _, want := range []string{"integrity mismatch", strings.Repeat("cd", 32), sha256Hex(dist.payload), "unverified artifact"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err.Error(), want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "bin", "putnami")); !os.IsNotExist(statErr) {
		t.Fatalf("a layer survived the integrity mismatch (stat: %v)", statErr)
	}
}

// TestImageLayersCLISurfacesDownloadFailures keeps a transport failure loud.
func TestImageLayersCLISurfacesDownloadFailures(t *testing.T) {
	installCLIDist(t, &fakeCLIDist{openErr: fmt.Errorf("dial put.putnami.dev: connection refused")})
	err := ImageLayers(map[string]any{"project": imageLayersProject(t, `{"layers":[`+imageLayersCLILayer+`]}`)},
		nil, imageLayersToolWorkspace(t), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want the transport failure surfaced", err)
	}
}

// TestPutServerDistDownloadsThePinnedCLI exercises the real put-server reader
// against a local server: the exact query the endpoint needs (channel + the
// endpoint's own `x64` arch vocabulary), and the streaming path.
func TestPutServerDistDownloadsThePinnedCLI(t *testing.T) {
	useImageLayersRegistryToken(t, "")
	var requested string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.Method + " " + r.URL.RequestURI()
		if r.Header.Get("User-Agent") == "" {
			t.Error("download carries no User-Agent")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("an anonymous download carried a credential")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("cli-bytes"))
	}))
	defer server.Close()
	dist := putServerDist{client: server.Client(), base: server.URL}
	// The public install path stays byte-identical on the wire: the generated
	// client requests exactly the URL the layer record stores.
	defer func() {
		want := "GET " + strings.TrimPrefix(dist.URL("0.1.0-8885222db"), server.URL)
		if requested != want || want != "GET /putnami/cli/download?arch=x64&channel=0.1.0-8885222db&os=linux" {
			t.Errorf("download request = %q, want %q", requested, want)
		}
	}()
	body, err := dist.Open(context.Background(), "0.1.0-8885222db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != "cli-bytes" {
		t.Fatalf("body = %q", data)
	}
}

// TestPutServerDistOpenSurfacesStatus: an error body is not a CLI. The
// Dockerfile catches this with `--version`; the producer catches it here,
// before the integrity comparison it would also fail.
func TestPutServerDistOpenSurfacesStatus(t *testing.T) {
	useImageLayersRegistryToken(t, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	dist := putServerDist{client: server.Client(), base: server.URL}
	body, err := dist.Open(context.Background(), "9.9.9")
	if err == nil {
		_ = body.Close()
		t.Fatal("a 404 download was accepted")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want the status surfaced", err)
	}
}

// TestPutServerDistOpenRefusesAnAnswerWithoutMediaType: a 200 the client
// cannot type is not a CLI either, and the failure names the download.
func TestPutServerDistOpenRefusesAnAnswerWithoutMediaType(t *testing.T) {
	useImageLayersRegistryToken(t, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
		_, _ = w.Write([]byte("cli-bytes"))
	}))
	defer server.Close()
	dist := putServerDist{client: server.Client(), base: server.URL}
	body, err := dist.Open(context.Background(), "1.2.3")
	if err == nil {
		_ = body.Close()
		t.Fatal("an untyped download was accepted")
	}
	if !strings.Contains(err.Error(), "download "+dist.URL("1.2.3")) {
		t.Fatalf("error = %v, want the download URL named", err)
	}
}

// TestPutServerDistEndpoint keeps the recorded location on the origin a test
// or an override names.
func TestPutServerDistEndpoint(t *testing.T) {
	if got := (putServerDist{base: "http://127.0.0.1:1/"}).endpoint(); got != "http://127.0.0.1:1/putnami/cli/download" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := (putServerDist{}).endpoint(); got != "https://put.putnami.dev/putnami/cli/download" {
		t.Fatalf("published endpoint = %q", got)
	}
}

// TestPutServerDistURL pins the published location the sidecar records.
func TestPutServerDistURL(t *testing.T) {
	got := putServerDist{}.URL("0.1.0-8885222db")
	if !strings.HasPrefix(got, putServerDownloadURL+"?") {
		t.Fatalf("url = %q, want the put-server download endpoint", got)
	}
	for _, want := range []string{"channel=0.1.0-8885222db", "os=linux", "arch=x64"} {
		if !strings.Contains(got, want) {
			t.Fatalf("url %q is missing %q", got, want)
		}
	}
}

// --- manifest validation for the new producers ----------------------------------

func TestImageLayersToolManifestValidation(t *testing.T) {
	cases := map[string]struct {
		layer string
		wants string
	}{
		"no version source": {
			layer: `{"name":"crane","producer":"go-tool","path":"/usr/local/bin/crane","package":"a.dev/cmd/crane","module":"a.dev"}`,
			wants: "neither a `tool`",
		},
		"both version sources": {
			layer: `{"name":"crane","producer":"go-tool","path":"/x","package":"a.dev/cmd/crane","module":"a.dev","tool":"crane","version":"1.2.3"}`,
			wants: "must be DERIVED",
		},
		"no package": {
			layer: `{"name":"crane","producer":"go-tool","path":"/x","module":"a.dev","version":"1.2.3"}`,
			wants: "unusable go-tool package",
		},
		"package outside its module": {
			layer: `{"name":"crane","producer":"go-tool","path":"/x","package":"other.dev/cmd/crane","module":"a.dev","version":"1.2.3"}`,
			wants: "outside its declared module",
		},
		"versioned package reference": {
			layer: `{"name":"crane","producer":"go-tool","path":"/x","package":"a.dev/cmd/crane@v1.2.3","module":"a.dev","version":"1.2.3"}`,
			wants: "unusable go-tool package",
		},
		"v-prefixed image pin": {
			layer: `{"name":"crane","producer":"go-tool","path":"/x","package":"a.dev/cmd/crane","module":"a.dev","version":"v1.2.3"}`,
			wants: "unreadable image-owned version",
		},
		"unused pin on the CLI": {
			layer: `{"name":"putnami","producer":"putnami-cli","path":"/x","version":"1.2.3"}`,
			wants: "does not read",
		},
		"unused pin on the toolchain": {
			layer: `{"name":"go-toolchain","producer":"go-toolchain","path":"/usr/local/go","tool":"go"}`,
			wants: "does not read",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			build := &fakeGoBuild{}
			installGoBuild(t, build)
			project := imageLayersProject(t, `{"layers":[`+tc.layer+`]}`)
			err := ImageLayers(map[string]any{"project": project}, nil, imageLayersToolWorkspace(t), nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wants)
			}
			if clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("exit code = %d, want ExitUsage %d", clicore.ExitCode(err), clicore.ExitUsage)
			}
			if len(build.argv) != 0 {
				t.Fatalf("a rejected manifest still ran a build: %v", build.argv)
			}
		})
	}
}

// TestImageLayersGoToolDoubleBuildIsByteEqual is the real reproducibility
// claim: the same pinned tool, compiled twice into two different projects,
// must produce the same bytes. It needs a Go toolchain and the module proxy, so
// it is opt-in rather than part of the gate — a manual run records the hashes,
// and the fixture-driven tests above cover the command and
// verification logic that make the claim true.
func TestImageLayersGoToolDoubleBuildIsByteEqual(t *testing.T) {
	if os.Getenv("PUTNAMI_IMAGE_LAYERS_E2E") == "" {
		t.Skip("set PUTNAMI_IMAGE_LAYERS_E2E=1 to cross-compile the pinned tools for real (Go toolchain + module proxy)")
	}
	workspaceRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the workspace root: %v", err)
	}
	build := func() []byte {
		t.Helper()
		project := imageLayersProject(t, `{"layers":[`+imageLayersCraneLayer+`]}`)
		var out []string
		if err := ImageLayers(map[string]any{"project": project}, nil, workspaceRoot, nil, imageLayersIO(&out)); err != nil {
			t.Fatalf("cloud image-layers: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "bin", "crane"))
		if err != nil {
			t.Fatalf("read the produced crane: %v", err)
		}
		return data
	}
	first, second := build(), build()
	if !bytes.Equal(first, second) {
		t.Fatalf("double build is not byte-equal: sha256 %s vs %s", sha256Hex(first), sha256Hex(second))
	}
	t.Logf("crane sha256 %s (%d bytes)", sha256Hex(first), len(first))
}

// --- the official-image producer ------------------------------------------------

// ociFixtureEntry is one entry of a synthetic image layer.
type ociFixtureEntry struct {
	name     string
	body     string
	typeflag byte
	mode     int64
}

// ociFixtureLayer packs one layer's entries into a v1.Layer. The tar is written
// by hand rather than through a helper so the tests can place whiteouts and
// non-regular entries exactly where a real upstream image would.
func ociFixtureLayer(t *testing.T, entries []ociFixtureEntry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0o755
		}
		header := &tar.Header{Typeflag: typeflag, Name: entry.name, Mode: mode}
		if typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if typeflag == tar.TypeSymlink {
			header.Linkname = entry.body
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write layer header %s: %v", entry.name, err)
		}
		if typeflag == tar.TypeReg {
			if _, err := io.WriteString(writer, entry.body); err != nil {
				t.Fatalf("write layer body %s: %v", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close layer tar: %v", err)
	}
	packed := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(packed)), nil
	})
	if err != nil {
		t.Fatalf("build fixture layer: %v", err)
	}
	return layer
}

// ociFixtureImage assembles a synthetic linux/amd64 image, oldest layer first,
// and resolves it the way the real source would.
func ociFixtureImage(t *testing.T, layers ...[]ociFixtureEntry) imageLayersOCIImage {
	t.Helper()
	built := make([]v1.Layer, 0, len(layers))
	for _, entries := range layers {
		built = append(built, ociFixtureLayer(t, entries))
	}
	image, err := mutate.AppendLayers(empty.Image, built...)
	if err != nil {
		t.Fatalf("assemble fixture image: %v", err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("digest fixture image: %v", err)
	}
	return imageLayersOCIImage{Digest: digest.String(), Manifest: digest.String(), Image: image}
}

// fakeOCISource is the registry seam under test control: it answers references
// with fixture images, and can be made to resolve one to a digest nobody pinned.
type fakeOCISource struct {
	images   map[string]imageLayersOCIImage
	resolved map[string]string // reference → the digest to REPORT, overriding the fixture's
	err      error
	asked    []string
}

func (f *fakeOCISource) Resolve(_ context.Context, reference string) (imageLayersOCIImage, error) {
	f.asked = append(f.asked, reference)
	if f.err != nil {
		return imageLayersOCIImage{}, f.err
	}
	image, ok := f.images[reference]
	if !ok {
		return imageLayersOCIImage{}, fmt.Errorf("no fixture publishes %s", reference)
	}
	if digest, override := f.resolved[reference]; override {
		image.Digest = digest
	}
	return image, nil
}

func installOCISource(t *testing.T, source *fakeOCISource) {
	t.Helper()
	previous := imageLayersOCISourceFor
	t.Cleanup(func() { imageLayersOCISourceFor = previous })
	imageLayersOCISourceFor = func(*http.Client) imageLayersOCISource { return source }
}

// imageLayersPackageJSON writes the workspace's bun and node pins at the exact
// path imageBuildPins() reads them from.
func imageLayersPackageJSON(t *testing.T, ws, bun, node string) {
	t.Helper()
	manifest := map[string]any{"packageManager": "bun@" + bun, "engines": map[string]string{"node": node}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "package.json"), data, 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
}

const (
	imageLayersUVDigest = "sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c"
	imageLayersBunRef   = "oven/bun:1.3.14-slim"
	imageLayersUVRef    = "ghcr.io/astral-sh/uv:0.11.32@" + imageLayersUVDigest
)

const imageLayersBunLayer = `{"name":"bun","producer":"oci-file","path":"/usr/local/bin/bun",` +
	`"image":"oven/bun","tag":"{version}-slim","pin":"BUN_VERSION","source":"/usr/local/bin/bun"}`

const imageLayersUVLayer = `{"name":"uv","producer":"oci-file","path":"/usr/local/bin/uv",` +
	`"image":"ghcr.io/astral-sh/uv","tag":"{version}","version":"0.11.32","digest":"` + imageLayersUVDigest + `","source":"/uv"}`

const imageLayersOCIManifest = `{"layers":[` + imageLayersBunLayer + `,` + imageLayersUVLayer + `]}`

// imageLayersOCIWorkspace is a workspace declaring the bun and node pins the
// derived layers read.
func imageLayersOCIWorkspace(t *testing.T) string {
	t.Helper()
	ws := imageLayersWorkspace(t, "go 1.26.1\n")
	imageLayersPackageJSON(t, ws, "1.3.14", "24.19.0")
	return ws
}

// imageLayersOCIFixtures is the pair of synthetic images the tests pull from:
// a bun image whose binary sits under /usr/local/bin, and a scratch-style uv
// image with the two binaries at the root.
func imageLayersOCIFixtures(t *testing.T) *fakeOCISource {
	t.Helper()
	return &fakeOCISource{
		images: map[string]imageLayersOCIImage{
			imageLayersBunRef: ociFixtureImage(t,
				[]ociFixtureEntry{{name: "usr/local/bin/other", body: "unrelated\n"}},
				[]ociFixtureEntry{{name: "usr/local/bin/bun", body: "bun-elf-bytes\n"}},
			),
			imageLayersUVRef: ociFixtureImage(t,
				[]ociFixtureEntry{{name: "uv", body: "uv-elf-bytes\n"}, {name: "uvx", body: "uvx-elf-bytes\n"}},
			),
		},
		// A synthetic image cannot hash to the digest upstream published, so the
		// seam reports the pinned one — the honest registry answer for a
		// reference that resolves. The mismatch test overrides it.
		resolved: map[string]string{imageLayersUVRef: imageLayersUVDigest},
	}
}

// TestImageLayersExtractsOfficialImageBinaries is the acceptance claim for the
// official-image producer: the four `COPY --from` binaries are extracted into
// .gen/layers/bin/ on a machine with no docker daemon, with the version derived for bun and the
// digest verified for uv.
func TestImageLayersExtractsOfficialImageBinaries(t *testing.T) {
	source := imageLayersOCIFixtures(t)
	installOCISource(t, source)
	project := imageLayersProject(t, imageLayersOCIManifest)

	var out []string
	if err := ImageLayers(map[string]any{"project": project}, nil, imageLayersOCIWorkspace(t), nil, imageLayersIO(&out)); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}

	binDir := filepath.Join(project, ".gen", "layers", "bin")
	for name, want := range map[string]string{"bun": "bun-elf-bytes\n", "uv": "uv-elf-bytes\n"} {
		produced, err := os.ReadFile(filepath.Join(binDir, name)) //nolint:gosec // G304: a path this test just produced
		if err != nil {
			t.Fatalf("read produced %s: %v", name, err)
		}
		if string(produced) != want {
			t.Fatalf("%s content = %q, want the image's %q", name, produced, want)
		}
		info, err := os.Stat(filepath.Join(binDir, name))
		if err != nil {
			t.Fatalf("stat produced %s: %v", name, err)
		}
		if info.Mode().Perm() != imageLayersToolFileMode {
			t.Fatalf("%s mode = %o, want %o", name, info.Mode().Perm(), imageLayersToolFileMode)
		}
	}

	// bun's version is DERIVED from package.json, and the digest it resolved to
	// is recorded as evidence — the record states which bytes were pulled. Its
	// identity is the whole reference plus the extracted path, because the
	// repository, the tag template and the source path each select the artifact
	// without moving a version string.
	bun := imageLayersReadRecord(t, project, "bun")
	if want := imageLayersBunRef + " /usr/local/bin/bun"; bun.Version != want {
		t.Fatalf("bun version = %q, want the reference package.json's packageManager pin renders %q", bun.Version, want)
	}
	if bun.Source.Image != imageLayersBunRef {
		t.Fatalf("bun source image = %q, want %q", bun.Source.Image, imageLayersBunRef)
	}
	if bun.Source.Digest != source.images[imageLayersBunRef].Digest || bun.Source.Digest == "" {
		t.Fatalf("bun source digest = %q, want the digest the tag resolved to", bun.Source.Digest)
	}
	if bun.Source.Manifest == "" || bun.Source.Layer == "" {
		t.Fatalf("bun source = %+v, want the manifest and layer digests the bytes came from", bun.Source)
	}
	if bun.Source.Entry != "/usr/local/bin/bun" || bun.Path != "/usr/local/bin/bun" {
		t.Fatalf("bun record entry/path = %q/%q", bun.Source.Entry, bun.Path)
	}
	if bun.File != "bin/bun" || bun.Tar != "" {
		t.Fatalf("bun record artifact = %q/%q, want the file form", bun.File, bun.Tar)
	}
	produced, err := os.ReadFile(filepath.Join(binDir, "bun"))
	if err != nil {
		t.Fatalf("read produced bun: %v", err)
	}
	if bun.SHA256 != sha256Hex(produced) || bun.Size != int64(len(produced)) {
		t.Fatalf("bun record %s/%d does not describe the %d produced bytes", bun.SHA256, bun.Size, len(produced))
	}

	// uv's digest is the pin, and the record says it held — the identity carries
	// that digest, so editing it under an unchanged version cannot read as
	// current on the next check.
	uv := imageLayersReadRecord(t, project, "uv")
	if want := imageLayersUVRef + " /uv"; uv.Version != want {
		t.Fatalf("uv version = %q, want the image-owned reference %q", uv.Version, want)
	}
	if uv.Source.Digest != imageLayersUVDigest {
		t.Fatalf("uv source digest = %q, want the pinned %s", uv.Source.Digest, imageLayersUVDigest)
	}
	if !slices.Equal(source.asked, []string{imageLayersBunRef, imageLayersUVRef}) {
		t.Fatalf("resolved references = %v", source.asked)
	}

	// Containment: the scratch trees are gone and only the produced tree remains.
	entries, err := os.ReadDir(filepath.Join(project, ".gen", "layers"))
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"bin", "bun.json", "uv.json"}) {
		t.Fatalf("output dir = %v, want the bin/ tree and one record per layer", got)
	}
}

// TestImageLayersOCITakesTheNewestLayer pins the walk order. The assembled
// filesystem shows the TOPMOST layer's copy of a path, so extracting a lower
// one would ship an older binary that nothing in the produced bytes gives away.
func TestImageLayersOCITakesTheNewestLayer(t *testing.T) {
	image := ociFixtureImage(t,
		[]ociFixtureEntry{{name: "usr/local/bin/bun", body: "stale\n"}},
		[]ociFixtureEntry{{name: "usr/local/bin/bun", body: "current\n"}},
	)
	dest := filepath.Join(t.TempDir(), "bun")
	layer, err := imageLayersExtractEntry(image.Image, "usr/local/bin/bun", dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	extracted, err := os.ReadFile(dest) //nolint:gosec // G304: a path this test just produced
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(extracted) != "current\n" {
		t.Fatalf("extracted %q, want the newest layer's copy", extracted)
	}
	layers, err := image.Image.Layers()
	if err != nil {
		t.Fatalf("read fixture layers: %v", err)
	}
	newest, err := layers[len(layers)-1].Digest()
	if err != nil {
		t.Fatalf("digest the newest layer: %v", err)
	}
	if layer != newest.String() {
		t.Fatalf("recorded layer = %q, want the newest %q", layer, newest)
	}
}

// TestImageLayersOCIHardFailsOnDigestMismatch is uv's pin doing its job: its
// digest IS the pin, so a reference that resolves elsewhere is a substituted
// image, and nothing is written.
func TestImageLayersOCIHardFailsOnDigestMismatch(t *testing.T) {
	source := imageLayersOCIFixtures(t)
	other := "sha256:" + strings.Repeat("ab", 32)
	source.resolved = map[string]string{imageLayersUVRef: other}
	installOCISource(t, source)
	project := imageLayersProject(t, `{"layers":[`+imageLayersUVLayer+`]}`)

	err := ImageLayers(map[string]any{"project": project}, nil, imageLayersOCIWorkspace(t), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want a digest mismatch", err)
	}
	if !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), imageLayersUVDigest) {
		t.Fatalf("error %q names neither the pinned nor the resolved digest", err)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "bin")); !os.IsNotExist(statErr) {
		t.Fatalf("a mismatched digest still installed something (stat: %v)", statErr)
	}
}

// TestImageLayersOCIHardFailsOnAMissingPath covers the upstream layout moving:
// an image that no longer carries the COPY'd path must fail loudly rather than
// produce an empty or absent binary.
func TestImageLayersOCIHardFailsOnAMissingPath(t *testing.T) {
	source := &fakeOCISource{images: map[string]imageLayersOCIImage{
		imageLayersBunRef: ociFixtureImage(t, []ociFixtureEntry{{name: "usr/local/bin/bunx", body: "elsewhere\n"}}),
	}}
	installOCISource(t, source)
	project := imageLayersProject(t, `{"layers":[`+imageLayersBunLayer+`]}`)

	err := ImageLayers(map[string]any{"project": project}, nil, imageLayersOCIWorkspace(t), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "no layer carries /usr/local/bin/bun") {
		t.Fatalf("error = %v, want one naming the missing path", err)
	}
	if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "bin")); !os.IsNotExist(statErr) {
		t.Fatalf("a missing path still installed something (stat: %v)", statErr)
	}
}

// TestImageLayersOCIHardFailsOnAWhiteout distinguishes "a later layer deleted
// it" from "the image never had it". Both are upstream changes, but a reader
// chasing a failed produce run needs to be told which one happened.
func TestImageLayersOCIHardFailsOnAWhiteout(t *testing.T) {
	cases := map[string][]ociFixtureEntry{
		"per-file":     {{name: "usr/local/bin/.wh.bun", body: ""}},
		"ancestor-dir": {{name: "usr/local/.wh.bin", body: ""}},
		"opaque":       {{name: "usr/local/bin/.wh..wh..opq", body: ""}},
		"root-wide":    {{name: ".wh..wh..opq", body: ""}},
	}
	for name, removal := range cases {
		t.Run(name, func(t *testing.T) {
			image := ociFixtureImage(t,
				[]ociFixtureEntry{{name: "usr/local/bin/bun", body: "bun-elf-bytes\n"}},
				removal,
			)
			source := &fakeOCISource{images: map[string]imageLayersOCIImage{imageLayersBunRef: image}}
			installOCISource(t, source)
			project := imageLayersProject(t, `{"layers":[`+imageLayersBunLayer+`]}`)

			err := ImageLayers(map[string]any{"project": project}, nil, imageLayersOCIWorkspace(t), nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), "deletes /usr/local/bin/bun") {
				t.Fatalf("error = %v, want one naming the deletion", err)
			}
		})
	}
}

// TestImageLayersOCIRefusesANonRegularEntry: a symlink at the COPY'd path is a
// layout this producer will not guess its way through — resolving it would mean
// re-implementing the image's own filesystem assembly.
func TestImageLayersOCIRefusesANonRegularEntry(t *testing.T) {
	image := ociFixtureImage(t, []ociFixtureEntry{
		{name: "usr/local/bin/bun", body: "../lib/bun", typeflag: tar.TypeSymlink},
	})
	installOCISource(t, &fakeOCISource{images: map[string]imageLayersOCIImage{imageLayersBunRef: image}})
	project := imageLayersProject(t, `{"layers":[`+imageLayersBunLayer+`]}`)

	err := ImageLayers(map[string]any{"project": project}, nil, imageLayersOCIWorkspace(t), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v, want one refusing the non-regular entry", err)
	}
}

// TestImageLayersOCIDoubleRunIsByteEqual is the reproducibility claim for this
// producer: the same resolved images, extracted into two different projects,
// give byte-identical files and identical provenance. The extracted bytes feed
// a `files` layer the framework normalizes, so equality here is equality of the
// image content key.
func TestImageLayersOCIDoubleRunIsByteEqual(t *testing.T) {
	source := imageLayersOCIFixtures(t)
	installOCISource(t, source)
	ws := imageLayersOCIWorkspace(t)

	run := func() (map[string][]byte, map[string]imageLayerRecord) {
		t.Helper()
		project := imageLayersProject(t, imageLayersOCIManifest)
		if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string))); err != nil {
			t.Fatalf("cloud image-layers: %v", err)
		}
		files := map[string][]byte{}
		records := map[string]imageLayerRecord{}
		for _, name := range []string{"bun", "uv"} {
			data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "bin", name)) //nolint:gosec // G304: a path this test just produced
			if err != nil {
				t.Fatalf("read produced %s: %v", name, err)
			}
			files[name] = data
			records[name] = imageLayersReadRecord(t, project, name)
		}
		return files, records
	}

	firstFiles, firstRecords := run()
	secondFiles, secondRecords := run()
	for _, name := range []string{"bun", "uv"} {
		if !bytes.Equal(firstFiles[name], secondFiles[name]) {
			t.Fatalf("%s is not byte-equal across runs: sha256 %s vs %s", name, sha256Hex(firstFiles[name]), sha256Hex(secondFiles[name]))
		}
		if !reflect.DeepEqual(firstRecords[name], secondRecords[name]) {
			t.Fatalf("%s record differs across runs: %+v vs %+v", name, firstRecords[name], secondRecords[name])
		}
	}
}

// TestImageLayersOCIRefusesWithoutADerivedPin: a workspace that declares no bun
// version has nothing to pull. Falling back to a literal is what the derivation
// rule exists to forbid, so the producer stops instead.
func TestImageLayersOCIRefusesWithoutADerivedPin(t *testing.T) {
	source := imageLayersOCIFixtures(t)
	installOCISource(t, source)
	project := imageLayersProject(t, `{"layers":[`+imageLayersBunLayer+`]}`)

	// A workspace with no package.json at all: absence, not drift.
	err := ImageLayers(map[string]any{"project": project}, nil, imageLayersWorkspace(t, "go 1.26.1\n"), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "declares no BUN_VERSION") {
		t.Fatalf("error = %v, want one naming the missing derivation", err)
	}
	if len(source.asked) != 0 {
		t.Fatalf("a layer with no pin still reached the registry: %v", source.asked)
	}
}

// TestImageLayersOCIRefusesAnUnreadableDerivedPin: `engines.node` may hold a
// RANGE, which states what the workspace tolerates rather than what CI runs.
// The reader reports absence for one, and this is where that becomes a failure
// instead of a tag like `node:^24-slim`.
func TestImageLayersOCIRefusesAnUnreadableDerivedPin(t *testing.T) {
	ws := imageLayersWorkspace(t, "go 1.26.1\n")
	imageLayersPackageJSON(t, ws, "1.3.14", "^24")
	source := imageLayersOCIFixtures(t)
	installOCISource(t, source)
	node := `{"name":"node","producer":"oci-file","path":"/usr/local/bin/node",` +
		`"image":"node","tag":"{version}-slim","pin":"NODE_VERSION","source":"/usr/local/bin/node"}`
	project := imageLayersProject(t, `{"layers":[`+node+`]}`)

	err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "declares no NODE_VERSION") {
		t.Fatalf("error = %v, want one naming the missing derivation", err)
	}
	if len(source.asked) != 0 {
		t.Fatalf("a layer with no pin still reached the registry: %v", source.asked)
	}
}

// TestImageLayersOCISurfacesResolveFailures: a registry that will not answer is
// reported as itself, not as a missing path.
func TestImageLayersOCISurfacesResolveFailures(t *testing.T) {
	installOCISource(t, &fakeOCISource{err: errors.New("registry unreachable")})
	project := imageLayersProject(t, `{"layers":[`+imageLayersBunLayer+`]}`)
	err := ImageLayers(map[string]any{"project": project}, nil, imageLayersOCIWorkspace(t), nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("error = %v, want the transport failure", err)
	}
}

// TestImageLayersOCIManifestValidation covers the rules that run BEFORE any
// producer does. The load-bearing two: a version the workspace declares must be
// derived, and a digest may only pin an image-owned version — committing one
// beside a derived pin creates a second pin nobody bumps with the first.
func TestImageLayersOCIManifestValidation(t *testing.T) {
	cases := map[string]struct {
		manifest string
		wants    string
	}{
		"tagged image": {
			manifest: `{"layers":[{"name":"bun","producer":"oci-file","path":"/x","image":"oven/bun:1.3.14","tag":"{version}","pin":"BUN_VERSION","source":"/bun"}]}`,
			wants:    "unusable image repository",
		},
		"literal tag": {
			manifest: `{"layers":[{"name":"bun","producer":"oci-file","path":"/x","image":"oven/bun","tag":"1.3.14-slim","pin":"BUN_VERSION","source":"/bun"}]}`,
			wants:    "never substitutes {version}",
		},
		"no version source": {
			manifest: `{"layers":[{"name":"bun","producer":"oci-file","path":"/x","image":"oven/bun","tag":"{version}","source":"/bun"}]}`,
			wants:    "neither a `pin`",
		},
		"both version sources": {
			manifest: `{"layers":[{"name":"bun","producer":"oci-file","path":"/x","image":"oven/bun","tag":"{version}","pin":"BUN_VERSION","version":"1.3.14","source":"/bun"}]}`,
			wants:    "must be DERIVED",
		},
		"unknown pin": {
			manifest: `{"layers":[{"name":"bun","producer":"oci-file","path":"/x","image":"oven/bun","tag":"{version}","pin":"RUST_VERSION","source":"/bun"}]}`,
			wants:    "no workspace source derives",
		},
		"committed digest beside a derived pin": {
			manifest: `{"layers":[{"name":"bun","producer":"oci-file","path":"/x","image":"oven/bun","tag":"{version}","pin":"BUN_VERSION","digest":"` + imageLayersUVDigest + `","source":"/bun"}]}`,
			wants:    "second pin nobody bumps",
		},
		"image-owned version with no digest": {
			manifest: `{"layers":[{"name":"uv","producer":"oci-file","path":"/x","image":"ghcr.io/astral-sh/uv","tag":"{version}","version":"0.11.32","source":"/uv"}]}`,
			wants:    "unusable digest",
		},
		"v-prefixed image pin": {
			manifest: `{"layers":[{"name":"uv","producer":"oci-file","path":"/x","image":"ghcr.io/astral-sh/uv","tag":"{version}","version":"v0.11.32","digest":"` + imageLayersUVDigest + `","source":"/uv"}]}`,
			wants:    "unreadable image-owned version",
		},
		"relative source": {
			manifest: `{"layers":[{"name":"uv","producer":"oci-file","path":"/x","image":"ghcr.io/astral-sh/uv","tag":"{version}","version":"0.11.32","digest":"` + imageLayersUVDigest + `","source":"uv"}]}`,
			wants:    "non-absolute in-image `source`",
		},
		"source at the root": {
			manifest: `{"layers":[{"name":"uv","producer":"oci-file","path":"/x","image":"ghcr.io/astral-sh/uv","tag":"{version}","version":"0.11.32","digest":"` + imageLayersUVDigest + `","source":"/"}]}`,
			wants:    "resolves to the filesystem root",
		},
		"unused image field on a go tool": {
			manifest: `{"layers":[{"name":"crane","producer":"go-tool","path":"/x","package":"a.dev/cmd/crane","module":"a.dev","version":"1.2.3","image":"a/b"}]}`,
			wants:    "does not read",
		},
		"one image on two references": {
			manifest: `{"layers":[` + imageLayersUVLayer + `,` +
				`{"name":"uvx","producer":"oci-file","path":"/y","image":"ghcr.io/astral-sh/uv","tag":"{version}","version":"0.11.33","digest":"sha256:` + strings.Repeat("ab", 32) + `","source":"/uvx"}]}`,
			wants: "one image, one pin",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			source := imageLayersOCIFixtures(t)
			installOCISource(t, source)
			project := imageLayersProject(t, tc.manifest)
			err := ImageLayers(map[string]any{"project": project}, nil, imageLayersOCIWorkspace(t), nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wants)
			}
			if clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("exit code = %d, want ExitUsage %d", clicore.ExitCode(err), clicore.ExitUsage)
			}
			if len(source.asked) != 0 {
				t.Fatalf("a rejected manifest still reached the registry: %v", source.asked)
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen")); !os.IsNotExist(statErr) {
				t.Fatalf("a rejected manifest still created .gen (stat: %v)", statErr)
			}
		})
	}
}

// TestOCIRegistrySourceSelectsLinuxAMD64 exercises the real registry path
// against an in-process registry, and pins the one decision in it that the
// produced bytes depend on: the platform is ALWAYS explicit. bun and node
// publish multi-platform tags, so a producer that inherited a library default —
// or the host's own arch — would extract a darwin/arm64 binary into a
// linux/amd64 runner image and only find out in CI.
func TestOCIRegistrySourceSelectsLinuxAMD64(t *testing.T) {
	amd64 := ociFixtureImage(t, []ociFixtureEntry{{name: "usr/local/bin/bun", body: "amd64-bun\n"}})
	arm64 := ociFixtureImage(t, []ociFixtureEntry{{name: "usr/local/bin/bun", body: "arm64-bun\n"}})
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: arm64.Image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
		mutate.IndexAddendum{Add: amd64.Image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
	)

	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer server.Close()
	reference := strings.TrimPrefix(server.URL, "http://") + "/oven/bun:1.3.14-slim"
	tag, err := name.NewTag(reference)
	if err != nil {
		t.Fatalf("parse the fixture tag: %v", err)
	}
	if err := remote.WriteIndex(tag, index, remote.WithAuth(authn.Anonymous)); err != nil {
		t.Fatalf("publish the fixture index: %v", err)
	}

	resolved, err := (ociRegistrySource{authenticator: authn.Anonymous}).Resolve(context.Background(), reference)
	if err != nil {
		t.Fatalf("resolve %s: %v", reference, err)
	}
	// The tag resolves to the INDEX, and the manifest recorded beside it is the
	// linux/amd64 child chosen out of it — two different digests, both real
	// provenance.
	indexDigest, err := index.Digest()
	if err != nil {
		t.Fatalf("digest the fixture index: %v", err)
	}
	if resolved.Digest != indexDigest.String() {
		t.Fatalf("resolved digest = %q, want the index %q", resolved.Digest, indexDigest)
	}
	if resolved.Manifest != amd64.Digest {
		t.Fatalf("manifest digest = %q, want the linux/amd64 child %q", resolved.Manifest, amd64.Digest)
	}
	dest := filepath.Join(t.TempDir(), "bun")
	if _, err := imageLayersExtractEntry(resolved.Image, "usr/local/bin/bun", dest); err != nil {
		t.Fatalf("extract from the resolved image: %v", err)
	}
	extracted, err := os.ReadFile(dest) //nolint:gosec // G304: a path this test just produced
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(extracted) != "amd64-bun\n" {
		t.Fatalf("extracted %q, want the linux/amd64 binary", extracted)
	}
}

// TestOCIRegistrySourceSurfacesAMissingReference: a tag nobody published is
// reported as itself. A produce run that quietly fell back to `latest` would
// bake an unpinned runtime into the runner image.
func TestOCIRegistrySourceSurfacesAMissingReference(t *testing.T) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer server.Close()
	reference := strings.TrimPrefix(server.URL, "http://") + "/oven/bun:0.0.0-slim"
	if _, err := (ociRegistrySource{authenticator: authn.Anonymous}).Resolve(context.Background(), reference); err == nil {
		t.Fatal("an unpublished tag resolved")
	} else if !strings.Contains(err.Error(), reference) {
		t.Fatalf("error %q does not name the reference", err)
	}
}

// TestOCIRegistrySourceRefusesAnUnparseableReference keeps a manifest typo from
// reaching the network as some other repository's name.
func TestOCIRegistrySourceRefusesAnUnparseableReference(t *testing.T) {
	if _, err := (ociRegistrySource{}).Resolve(context.Background(), "oven/bun:not a tag"); err == nil {
		t.Fatal("an unparseable reference was accepted")
	}
}

// --- the warm extension store producer -----------------------------------------
//
// No test here runs the CLI: the materializer is a seam, and the fixture writes
// the tree `putnami extensions install --platform linux/amd64 --dest <root>`
// would have written. That is the whole point of the seam — every verification
// this producer makes is about the SHAPE of that tree against the lock, so the
// tests can stage each disagreement exactly.

// warmFixtureGoTools is the lint-tool manifest a warmed @putnami/go carries.
func warmFixtureGoTools(golangci, staticcheck string) string {
	return `{"schemaVersion":1,"goVersion":"1.25.7","tools":{` +
		`"golangci-lint":{"install":"github.com/golangci/golangci-lint/v2/cmd/golangci-lint@` + golangci + `","version":"` + golangci + `"},` +
		`"staticcheck":{"install":"honnef.co/go/tools/cmd/staticcheck@` + staticcheck + `","version":"` + staticcheck + `"}}}`
}

// warmFixtureTree is the store a materialization writes, keyed by the
// content-addressed digest the lock records for each artifact: digest → the
// files under <dest>/sha256/<shard>/<digest>/.
type warmFixtureTree map[string]map[string]string

// warmFixtureStore is the honest tree for the fixture workspace: the two
// extensions at the lock's linux/amd64 integrities, with the go extension
// carrying the lint-tool pins the same workspace declares.
func warmFixtureStore() warmFixtureTree {
	return warmFixtureTree{
		warmGoDigest: {
			"putnami.extension.json": `{"name":"@putnami/go"}`,
			"tools/versions.json":    warmFixtureGoTools("v2.10.1", "v0.7.0"),
			"bin/prepare":            "#!/bin/sh\nexec true\n",
			"compiled/putnami-go":    "ELF:putnami-go",
			"compiled/tools/golangci-lint": fakeToolBinary("go1.26.1", "github.com/golangci/golangci-lint/v2", "v2.10.1",
				"github.com/golangci/golangci-lint/v2/cmd/golangci-lint"),
			"compiled/tools/staticcheck": fakeToolBinary("go1.26.1", "honnef.co/go/tools", "v0.7.0",
				"honnef.co/go/tools/cmd/staticcheck"),
			"README.md": "# @putnami/go\n",
		},
		warmTSDigest: {
			"putnami.extension.json": `{"name":"@putnami/typescript"}`,
			"compiled/putnami-ts":    "ELF:putnami-ts",
			"config/biome.json":      `{"linter":{"enabled":true}}`,
		},
	}
}

// The lock integrities the fixture workspace records for the two extensions.
// They are the store's directory names too: that identity is what makes the
// materialized tree verifiable at all.
var (
	warmGoDigest = strings.Repeat("1a", 32)
	warmTSDigest = strings.Repeat("2b", 32)
)

// fakeWarmInstall stands in for `putnami extensions install --platform … --dest
// …`: it records what it was asked for — including the throwaway workspace
// manifest, so a test can prove the producer pinned the lock's versions rather
// than resolving "latest" — and materializes the tree it was configured with.
type fakeWarmInstall struct {
	tree       warmFixtureTree
	installErr error
	// executable marks the store-relative paths written with the execute bit, so
	// the mode reduction is exercised on a real tree.
	executable map[string]bool
	// links maps a store-relative path to the symlink target to create there.
	links map[string]string

	workspaces []string
	stores     []string
	manifests  []string
	argv       [][]string
}

func (f *fakeWarmInstall) run(_ context.Context, workspaceDir, storeDir string, _ clicore.IO) error {
	f.workspaces = append(f.workspaces, workspaceDir)
	f.stores = append(f.stores, storeDir)
	f.argv = append(f.argv, imageLayersWarmInstallArgs(storeDir))
	manifest, err := os.ReadFile(filepath.Join(workspaceDir, imageLayersWarmWorkspaceName)) //nolint:gosec // G304: the scratch workspace the producer under test just wrote
	if err != nil {
		return fmt.Errorf("the producer ran the install without a workspace manifest: %w", err)
	}
	f.manifests = append(f.manifests, string(manifest))
	if f.installErr != nil {
		return f.installErr
	}
	for digest, files := range f.tree {
		root := filepath.Join(storeDir, imageLayersWarmStoreDir, digest[:imageLayersWarmShardLen], digest)
		for rel, body := range files {
			path := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if f.executable[rel] {
				mode = 0o755
			}
			if err := os.WriteFile(path, []byte(body), mode); err != nil {
				return err
			}
		}
		for rel, target := range f.links {
			path := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func installWarmRun(t *testing.T, fake *fakeWarmInstall) {
	t.Helper()
	previous := imageLayersWarmRun
	t.Cleanup(func() { imageLayersWarmRun = previous })
	imageLayersWarmRun = fake.run
}

// imageLayersWarmLock writes putnami.lock.json's extensions block beside the
// cli block the CLI layer needs, so one workspace serves both producers.
func imageLayersWarmLock(t *testing.T, ws string, extensions map[string]map[string]string) {
	t.Helper()
	blocks := map[string]any{}
	for name, fields := range extensions {
		entry := map[string]any{"version": fields["version"]}
		if integrity, ok := fields["linux/amd64"]; ok {
			entry["integrities"] = map[string]string{"linux/amd64": integrity}
		}
		blocks[name] = entry
	}
	lock := map[string]any{
		"cli":        map[string]any{"version": "0.1.0-8885222db", "integrities": map[string]string{"linux/amd64": strings.Repeat("cd", 32)}},
		"extensions": blocks,
	}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("encode putnami.lock.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "putnami.lock.json"), data, 0o600); err != nil {
		t.Fatalf("write putnami.lock.json: %v", err)
	}
}

// imageLayersWarmWorkspaceFixture declares every source the warm producer reads:
// the two extension pins with their linux/amd64 integrities, and the installed
// go extension's lint-tool manifest the warmed store is cross-checked against.
func imageLayersWarmWorkspaceFixture(t *testing.T) string {
	t.Helper()
	ws := imageLayersToolWorkspaceBase(t)
	imageLayersWarmLock(t, ws, map[string]map[string]string{
		"@putnami/go":         {"version": "0.1.0-8885222db", "linux/amd64": warmGoDigest},
		"@putnami/typescript": {"version": "0.1.0-8885222db", "linux/amd64": warmTSDigest},
	})
	return ws
}

const imageLayersWarmLayer = `{"name":"putnami-warm","producer":"putnami-warm","path":"/root/.putnami/artifacts"}`

const imageLayersWarmManifestJSON = `{"layers":[` + imageLayersWarmLayer + `]}`

// imageLayersWarmProduce runs the verb over the warm layer alone.
func imageLayersWarmProduce(t *testing.T, ws, project string) error {
	t.Helper()
	var out []string
	return ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(&out))
}

func TestImageLayersWarmIncludesLockedSDD(t *testing.T) {
	for _, integrity := range []string{strings.Repeat("3c", 32), ""} {
		t.Run("integrity="+integrity, func(t *testing.T) {
			ws := imageLayersWarmWorkspaceFixture(t)
			imageLayersWarmLock(t, ws, map[string]map[string]string{
				"@putnami/go":         {"version": "0.1.0-8885222db", "linux/amd64": warmGoDigest},
				"@putnami/typescript": {"version": "0.1.0-8885222db", "linux/amd64": warmTSDigest},
				"@putnami/sdd":        {"version": "0.1.0-sdd", "linux/amd64": integrity},
			})
			tree := warmFixtureStore()
			if integrity != "" {
				tree[integrity] = map[string]string{"putnami.extension.json": `{"name":"@putnami/sdd"}`}
			}
			fake := &fakeWarmInstall{tree: tree}
			installWarmRun(t, fake)
			project := imageLayersProject(t, imageLayersWarmManifestJSON)
			err := imageLayersWarmProduce(t, ws, project)
			if integrity == "" {
				if err == nil || !strings.Contains(err.Error(), "@putnami/sdd") || len(fake.manifests) != 0 {
					t.Fatalf("unverifiable SDD must fail before install: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(fake.manifests[0], `"@putnami/sdd": "0.1.0-sdd"`) {
				t.Fatalf("SDD missing from install: %s", fake.manifests[0])
			}
			layer, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "putnami-warm.tar"))
			if err != nil {
				t.Fatal(err)
			}
			want := "root/.putnami/artifacts/sha256/" + integrity[:2] + "/" + integrity + "/putnami.extension.json"
			if !slices.Contains(headerNames(readTarEntries(t, layer)), want) {
				t.Fatalf("SDD artifact absent from warm layer: %s", want)
			}
		})
	}
}

func TestImageLayersWarmPreservesHostCredentialProviderOutsideTheLayer(t *testing.T) {
	for _, kind := range []string{"project", "relative", "absolute", "installed", "array"} {
		t.Run(kind, func(t *testing.T) {
			ws := imageLayersWarmWorkspaceFixture(t)
			provider := filepath.Join(ws, "apps", "cli")
			ref := "/apps/cli"
			switch kind {
			case "relative":
				ref = "./apps/cli"
			case "absolute":
				provider = t.TempDir()
				ref = provider
			case "installed":
				provider = filepath.Join(ws, ".putnami", "bin", "extensions", "putnami-cloud")
				ref = "@putnami/cloud"
			}
			if err := os.MkdirAll(provider, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(provider, "putnami.extension.json"), []byte(`{"name":"@putnami/cloud"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var extensions any = map[string]string{ref: ""}
			if kind == "array" {
				extensions = []string{ref}
			}
			manifest, err := json.Marshal(map[string]any{"extensions": extensions})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, imageLayersWarmWorkspaceName), manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			fake := &fakeWarmInstall{tree: warmFixtureStore()}
			installWarmRun(t, fake)
			project := imageLayersProject(t, imageLayersWarmManifestJSON)
			if err := imageLayersWarmProduce(t, ws, project); err != nil {
				t.Fatal(err)
			}
			var installed struct {
				Extensions map[string]string `json:"extensions"`
			}
			if err := json.Unmarshal([]byte(fake.manifests[0]), &installed); err != nil {
				t.Fatal(err)
			}
			if version, found := installed.Extensions[provider]; !found || version != "" {
				t.Fatalf("provider must be an absolute local extension key: %s", fake.manifests[0])
			}
			layer, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "putnami-warm.tar"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(layer, []byte(provider)) || bytes.Contains(layer, []byte("@putnami/cloud")) {
				t.Fatal("host credential provider leaked into the warm artifact layer")
			}
		})
	}
}

// TestImageLayersProducesTheWarmStoreDeterministically is the acceptance claim:
// the materialized linux/amd64 store becomes .gen/layers/putnami-warm.tar plus
// its record, and two runs of the same inputs produce byte-identical output.
func TestImageLayersProducesTheWarmStoreDeterministically(t *testing.T) {
	fake := &fakeWarmInstall{tree: warmFixtureStore(), executable: map[string]bool{"bin/prepare": true, "compiled/putnami-go": true}}
	installWarmRun(t, fake)
	ws := imageLayersWarmWorkspaceFixture(t)

	run := func() []byte {
		t.Helper()
		project := imageLayersProject(t, imageLayersWarmManifestJSON)
		if err := imageLayersWarmProduce(t, ws, project); err != nil {
			t.Fatalf("cloud image-layers: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "putnami-warm.tar"))
		if err != nil {
			t.Fatalf("read the produced warm layer: %v", err)
		}
		return data
	}
	first, second := run(), run()
	if !bytes.Equal(first, second) {
		t.Fatalf("double run is not byte-equal: sha256 %s vs %s", sha256Hex(first), sha256Hex(second))
	}

	// The throwaway manifest pins the LOCK-resolved versions: the producer never
	// asks for "latest", which is what would make the layer disagree with the
	// image it ships in.
	if len(fake.manifests) != 2 {
		t.Fatalf("the materializer ran %d time(s), want one per produce run", len(fake.manifests))
	}
	for _, manifest := range fake.manifests {
		for _, want := range []string{`"@putnami/go": "0.1.0-8885222db"`, `"@putnami/typescript": "0.1.0-8885222db"`} {
			if !strings.Contains(manifest, want) {
				t.Fatalf("the throwaway workspace manifest %q does not pin %s", manifest, want)
			}
		}
	}
	if want := []string{"extensions", "install", "--platform", "linux/amd64", "--dest", fake.stores[0]}; !slices.Equal(fake.argv[0], want) {
		t.Fatalf("install argv = %v, want %v", fake.argv[0], want)
	}
	// The scratch workspace and the store both live inside the project's .gen,
	// never beside it.
	for _, dir := range []string{fake.workspaces[0], fake.stores[0]} {
		if !strings.Contains(dir, filepath.Join(".gen", "layers")) {
			t.Fatalf("the materialization ran against %q, outside the project's .gen/layers", dir)
		}
	}

	// The layer carries the content-addressed tree under the declared in-image
	// path, and nothing else.
	headers := readTarEntries(t, first)
	names := headerNames(headers)
	for _, want := range []string{
		"root/.putnami/artifacts/",
		"root/.putnami/artifacts/sha256/",
		"root/.putnami/artifacts/sha256/" + warmGoDigest[:2] + "/" + warmGoDigest + "/tools/versions.json",
		"root/.putnami/artifacts/sha256/" + warmTSDigest[:2] + "/" + warmTSDigest + "/compiled/putnami-ts",
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("the layer does not carry %s (entries: %v)", want, names)
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("layer entries are not sorted: %v", names)
	}
	for _, header := range headers {
		if !header.ModTime.Equal(imageLayersEpoch) || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Fatalf("entry %s carries host metadata: mtime %s uid %d gid %d uname %q gname %q",
				header.Name, header.ModTime, header.Uid, header.Gid, header.Uname, header.Gname)
		}
		if strings.HasSuffix(header.Name, "/bin/prepare") && header.Mode != imageLayersFileModeExec {
			t.Fatalf("the executable %s is emitted with mode %#o", header.Name, header.Mode)
		}
		if strings.HasSuffix(header.Name, "/README.md") && header.Mode != imageLayersFileModePlain {
			t.Fatalf("the plain %s is emitted with mode %#o", header.Name, header.Mode)
		}
	}
}

// TestImageLayersWarmRecordsWhatItVerified: the sidecar states both halves of
// the verification — the lock integrity each artifact was matched against, and
// the lint-tool table the warmed extension agreed on — so the claim is
// re-checkable without re-running the producer.
func TestImageLayersWarmRecordsWhatItVerified(t *testing.T) {
	installWarmRun(t, &fakeWarmInstall{tree: warmFixtureStore()})
	ws := imageLayersWarmWorkspaceFixture(t)
	project := imageLayersProject(t, imageLayersWarmManifestJSON)
	if err := imageLayersWarmProduce(t, ws, project); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	record := imageLayersReadRecord(t, project, "putnami-warm")
	if record.Producer != "putnami-warm" || record.Path != "/root/.putnami/artifacts" || record.Tar != "putnami-warm.tar" {
		t.Fatalf("record producer/path/tar = %q/%q/%q", record.Producer, record.Path, record.Tar)
	}
	// The lock integrity is part of the version label so `--check` reads an
	// integrity move under an unchanged version string as stale, not current.
	want := "@putnami/go 0.1.0-8885222db " + warmGoDigest + ", @putnami/typescript 0.1.0-8885222db " + warmTSDigest
	if record.Version != want {
		t.Fatalf("record version = %q, want %q", record.Version, want)
	}
	if len(record.Source.Extensions) != 2 {
		t.Fatalf("record extensions = %+v, want one row per warmed extension", record.Source.Extensions)
	}
	if record.Source.Extensions[0].Name != "@putnami/go" || record.Source.Extensions[0].SHA256 != warmGoDigest {
		t.Fatalf("record extension[0] = %+v, want @putnami/go at the lock integrity", record.Source.Extensions[0])
	}
	if record.Source.Tools["golangci-lint"] != "2.10.1" || record.Source.Tools["staticcheck"] != "0.7.0" {
		t.Fatalf("record tools = %v, want the verified lint-tool pins without their leading v", record.Source.Tools)
	}
	data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "putnami-warm.tar"))
	if err != nil {
		t.Fatalf("read the produced warm layer: %v", err)
	}
	if record.SHA256 != sha256Hex(data) || record.Size != int64(len(data)) {
		t.Fatalf("record %s (%d bytes) does not describe the produced tar %s (%d bytes)",
			record.SHA256, record.Size, sha256Hex(data), len(data))
	}
}

// TestImageLayersWarmHardFailsOnAStoreTheLockCannotAccountFor is image invariant
// 3 for this layer: the store is content-addressed BY the lock's integrities,
// so a materialization that resolved anything else — or that materialized an
// artifact no lock entry pins — never becomes a layer.
func TestImageLayersWarmHardFailsOnAStoreTheLockCannotAccountFor(t *testing.T) {
	cases := map[string]struct {
		tree  func() warmFixtureTree
		wants []string
	}{
		"a pinned artifact is missing": {
			tree: func() warmFixtureTree {
				tree := warmFixtureStore()
				delete(tree, warmTSDigest)
				return tree
			},
			wants: []string{"carries no artifact " + warmTSDigest, "@putnami/typescript", "cannot account for"},
		},
		"an artifact resolved to other bytes": {
			tree: func() warmFixtureTree {
				tree := warmFixtureStore()
				other := strings.Repeat("3c", 32)
				tree[other] = tree[warmTSDigest]
				delete(tree, warmTSDigest)
				return tree
			},
			wants: []string{"carries no artifact " + warmTSDigest, strings.Repeat("3c", 32)},
		},
		"an unpinned artifact came along": {
			tree: func() warmFixtureTree {
				tree := warmFixtureStore()
				tree[strings.Repeat("4d", 32)] = map[string]string{"putnami.extension.json": `{"name":"@putnami/other"}`}
				return tree
			},
			wants: []string{strings.Repeat("4d", 32), "no putnami.lock.json", "unverified artifact"},
		},
		"nothing was materialized at all": {
			tree:  func() warmFixtureTree { return warmFixtureTree{} },
			wants: []string{"materialized no sha256/ tree", "layout changed"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			installWarmRun(t, &fakeWarmInstall{tree: tc.tree()})
			project := imageLayersProject(t, imageLayersWarmManifestJSON)
			err := imageLayersWarmProduce(t, imageLayersWarmWorkspaceFixture(t), project)
			if err == nil {
				t.Fatal("a store the lock cannot account for produced a layer")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err.Error(), want)
				}
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "putnami-warm.tar")); !os.IsNotExist(statErr) {
				t.Fatalf("a layer survived the verification failure (stat: %v)", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "putnami-warm.json")); !os.IsNotExist(statErr) {
				t.Fatalf("a record survived the verification failure (stat: %v)", statErr)
			}
		})
	}
}

// TestImageLayersWarmHardFailsOnLintToolDrift preserves the retired
// Dockerfile's consistency check: the warmed go extension's tools/versions.json
// IS the source of truth the lint-tool layers derive from, so a store whose
// extension disagrees with the binaries baked beside it would ship a
// golangci-lint the extension driving it does not expect.
func TestImageLayersWarmHardFailsOnLintToolDrift(t *testing.T) {
	cases := map[string]struct {
		tools string
		// workspace rewrites the installed extension manifest instead.
		workspace map[string]string
		wants     []string
	}{
		"the warmed extension pins another golangci-lint": {
			tools: warmFixtureGoTools("v2.9.0", "v0.7.0"),
			wants: []string{"lint-tool drift", "golangci-lint 2.9.0", "2.10.1"},
		},
		"the warmed extension pins another staticcheck": {
			tools: warmFixtureGoTools("v2.10.1", "v0.6.1"),
			wants: []string{"lint-tool drift", "staticcheck 0.6.1", "0.7.0"},
		},
		"the warmed extension declares no pin at all": {
			tools: `{"tools":{"staticcheck":{"version":"v0.7.0"}}}`,
			wants: []string{`declares no "golangci-lint" version`},
		},
		"the workspace has no installed extension to check against": {
			workspace: map[string]string{},
			wants:     []string{"versions.json", `declares no "golangci-lint" version`, "putnami install"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tree := warmFixtureStore()
			if tc.tools != "" {
				tree[warmGoDigest]["tools/versions.json"] = tc.tools
			}
			installWarmRun(t, &fakeWarmInstall{tree: tree})
			ws := imageLayersWarmWorkspaceFixture(t)
			if tc.workspace != nil {
				imageLayersToolVersions(t, ws, tc.workspace)
			}
			project := imageLayersProject(t, imageLayersWarmManifestJSON)
			err := imageLayersWarmProduce(t, ws, project)
			if err == nil {
				t.Fatal("a warm store disagreeing with the lint-tool pins produced a layer")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err.Error(), want)
				}
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen", "layers", "putnami-warm.tar")); !os.IsNotExist(statErr) {
				t.Fatalf("a layer survived the drift (stat: %v)", statErr)
			}
		})
	}
}

// TestImageLayersWarmHardFailsWithoutAToolManifest: an artifact that carries no
// tools/versions.json cannot be the @putnami/go the image bakes lint tools for,
// and treating the absence as "nothing to check" is how the check would quietly
// stop existing.
func TestImageLayersWarmHardFailsWithoutAToolManifest(t *testing.T) {
	tree := warmFixtureStore()
	delete(tree[warmGoDigest], "tools/versions.json")
	installWarmRun(t, &fakeWarmInstall{tree: tree})
	err := imageLayersWarmProduce(t, imageLayersWarmWorkspaceFixture(t), imageLayersProject(t, imageLayersWarmManifestJSON))
	if err == nil || !strings.Contains(err.Error(), "carries no tools/versions.json") {
		t.Fatalf("error = %v, want a refusal naming the missing tool manifest", err)
	}
}

// TestImageLayersWarmRefusesWithoutADerivedPin: the layer has no version
// literal to fall back on, and no integrity means nothing to verify against —
// the exact state the retired Dockerfile shipped in.
func TestImageLayersWarmRefusesWithoutADerivedPin(t *testing.T) {
	cases := map[string]struct {
		extensions map[string]map[string]string
		wants      string
	}{
		"no version for an extension": {
			extensions: map[string]map[string]string{
				"@putnami/go":         {"version": "0.1.0-8885222db", "linux/amd64": warmGoDigest},
				"@putnami/typescript": {"linux/amd64": warmTSDigest},
			},
			wants: `pins no extensions["@putnami/typescript"].version`,
		},
		"no integrity for the platform": {
			extensions: map[string]map[string]string{
				"@putnami/go":         {"version": "0.1.0-8885222db"},
				"@putnami/typescript": {"version": "0.1.0-8885222db", "linux/amd64": warmTSDigest},
			},
			wants: `records no extensions["@putnami/go"].integrities["linux/amd64"]`,
		},
		"an unreadable integrity": {
			extensions: map[string]map[string]string{
				"@putnami/go":         {"version": "0.1.0-8885222db", "linux/amd64": "sha256:NOTHEX"},
				"@putnami/typescript": {"version": "0.1.0-8885222db", "linux/amd64": warmTSDigest},
			},
			wants: `unreadable extensions["@putnami/go"].integrities`,
		},
		"no extensions at all": {
			extensions: map[string]map[string]string{},
			wants:      `pins no extensions["@putnami/go"].version`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeWarmInstall{tree: warmFixtureStore()}
			installWarmRun(t, fake)
			ws := imageLayersWarmWorkspaceFixture(t)
			imageLayersWarmLock(t, ws, tc.extensions)
			err := imageLayersWarmProduce(t, ws, imageLayersProject(t, imageLayersWarmManifestJSON))
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error = %v, want a refusal naming %q", err, tc.wants)
			}
			if len(fake.stores) != 0 {
				t.Fatalf("the store was materialized with nothing to verify it against: %v", fake.stores)
			}
		})
	}
}

// TestImageLayersWarmSurfacesInstallFailures keeps a failed materialization
// from reading as an empty store — the Dockerfile hard-failed on a fetch error
// for the same reason: a cold, useless layer must never ship silently.
func TestImageLayersWarmSurfacesInstallFailures(t *testing.T) {
	installWarmRun(t, &fakeWarmInstall{installErr: fmt.Errorf("exit status 1: resolve @putnami/go: connection refused")})
	err := imageLayersWarmProduce(t, imageLayersWarmWorkspaceFixture(t), imageLayersProject(t, imageLayersWarmManifestJSON))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want the materialization failure surfaced", err)
	}
	if !strings.Contains(err.Error(), "materialize the linux/amd64 extension store") {
		t.Fatalf("error %q does not say what failed", err)
	}
}

// TestImageLayersWarmRefusesAnEscapingLink: the store is unpacked into the
// image, where an absolute or escaping link resolves against the IMAGE's
// filesystem rather than the layer.
func TestImageLayersWarmRefusesAnEscapingLink(t *testing.T) {
	for name, target := range map[string]string{
		"absolute": "/etc/passwd",
		"escaping": "../../../../../../etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			installWarmRun(t, &fakeWarmInstall{tree: warmFixtureStore(), links: map[string]string{"bin/escape": target}})
			err := imageLayersWarmProduce(t, imageLayersWarmWorkspaceFixture(t), imageLayersProject(t, imageLayersWarmManifestJSON))
			if err == nil {
				t.Fatal("a link pointing outside the layer produced one")
			}
			if !strings.Contains(err.Error(), "bin/escape") {
				t.Fatalf("error %q does not name the offending link", err)
			}
		})
	}
}

// TestImageLayersWarmKeepsALinkInsideTheLayer: the refusal above is about
// escaping, not about links — a relative link within the store is carried as a
// symlink entry with the layer's fixed link mode.
func TestImageLayersWarmKeepsALinkInsideTheLayer(t *testing.T) {
	installWarmRun(t, &fakeWarmInstall{tree: warmFixtureStore(), links: map[string]string{"bin/current": "prepare"}})
	project := imageLayersProject(t, imageLayersWarmManifestJSON)
	if err := imageLayersWarmProduce(t, imageLayersWarmWorkspaceFixture(t), project); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "putnami-warm.tar"))
	if err != nil {
		t.Fatalf("read the produced warm layer: %v", err)
	}
	for _, header := range readTarEntries(t, data) {
		if !strings.HasSuffix(header.Name, "/bin/current") {
			continue
		}
		if header.Typeflag != tar.TypeSymlink || header.Linkname != "prepare" || header.Mode != imageLayersSymlinkMode {
			t.Fatalf("link entry = %+v", header)
		}
		return
	}
	t.Fatal("the layer carries no bin/current link")
}

// TestImageLayersWarmManifestValidation: the producer reads no
// producer-specific field, so one that is declared anyway is a pin that looks
// maintained and is not.
func TestImageLayersWarmManifestValidation(t *testing.T) {
	for name, manifest := range map[string]string{
		"a version literal": `{"layers":[{"name":"putnami-warm","producer":"putnami-warm","path":"/root/.putnami/artifacts","version":"1.2.3"}]}`,
		"a pin":             `{"layers":[{"name":"putnami-warm","producer":"putnami-warm","path":"/root/.putnami/artifacts","pin":"BUN_VERSION"}]}`,
		"a tool":            `{"layers":[{"name":"putnami-warm","producer":"putnami-warm","path":"/root/.putnami/artifacts","tool":"staticcheck"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := imageLayersWarmProduce(t, imageLayersWarmWorkspaceFixture(t), imageLayersProject(t, manifest))
			if err == nil || !strings.Contains(err.Error(), "the putnami-warm producer does not read") {
				t.Fatalf("error = %v, want a manifest refusal", err)
			}
			if clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("exit code = %d, want usage", clicore.ExitCode(err))
			}
		})
	}
}

// TestImageLayersWarmAgainstTheRealCLI is the real determinism claim: the same
// lock, materialized twice through the actual `putnami extensions install`,
// must produce the same layer bytes. It needs the CLI and the network, so it is
// opt-in rather than part of the gate — a manual run records the hash, and the
// fixture-driven tests above cover the verification logic.
func TestImageLayersWarmAgainstTheRealCLI(t *testing.T) {
	if os.Getenv("PUTNAMI_IMAGE_LAYERS_E2E") == "" {
		t.Skip("set PUTNAMI_IMAGE_LAYERS_E2E=1 to materialize the extension store for real (putnami CLI + network)")
	}
	workspaceRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the workspace root: %v", err)
	}
	run := func() imageLayerRecord {
		t.Helper()
		project := imageLayersProject(t, imageLayersWarmManifestJSON)
		var out []string
		if err := ImageLayers(map[string]any{"project": project}, nil, workspaceRoot, nil, imageLayersIO(&out)); err != nil {
			t.Fatalf("cloud image-layers: %v", err)
		}
		return imageLayersReadRecord(t, project, "putnami-warm")
	}
	first, second := run(), run()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("double run differs: %+v vs %+v", first, second)
	}
	t.Logf("putnami-warm sha256 %s (%d bytes) for %s", first.SHA256, first.Size, first.Version)
}

// --- the directory layer --------------------------------------------------------
//
// The layer that carries a directory and nothing else. It is the fix for the
// rolled-back first packaged runner: the image named /workspace as its
// workingDir, no layer created it, and Cloud Run refused to start every
// container. The tests below pin the produced bytes, because a directory entry
// is small enough that a stray host attribute in it would be easy to miss.

const imageLayersDirsManifest = `{
  "layers": [
    {"name": "workspace", "producer": "dirs", "path": "/workspace"}
  ]
}`

// TestImageLayersProducesADirectoryLayer is the acceptance claim: the verb
// writes .gen/layers/workspace.tar plus its record, the tar carries exactly the
// declared directory with nothing from the host in it, and two runs are
// byte-identical (image invariant 1 — a churning layer re-keys the image).
func TestImageLayersProducesADirectoryLayer(t *testing.T) {
	project := imageLayersProject(t, imageLayersDirsManifest)
	run := func() []byte {
		t.Helper()
		var out []string
		// No workspace source is read and no network is touched: an empty
		// workspace root is enough to produce this layer.
		if err := ImageLayers(map[string]any{"project": project}, nil, t.TempDir(), nil, imageLayersIO(&out)); err != nil {
			t.Fatalf("cloud image-layers: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "workspace.tar"))
		if err != nil {
			t.Fatalf("read produced layer: %v", err)
		}
		return data
	}
	first := run()
	if second := run(); !bytes.Equal(first, second) {
		t.Fatalf("double run is not byte-equal: sha256 %s vs %s", sha256Hex(first), sha256Hex(second))
	}

	headers := readTarEntries(t, first)
	if len(headers) != 1 {
		t.Fatalf("the layer carries %v, want exactly the declared directory", headerNames(headers))
	}
	entry := headers[0]
	switch {
	case entry.Name != "workspace/":
		t.Errorf("entry name = %q, want workspace/ (no leading slash, trailing slash for a directory)", entry.Name)
	case entry.Typeflag != tar.TypeDir:
		t.Errorf("entry typeflag = %q, want a directory", string(entry.Typeflag))
	case entry.Mode != 0o755:
		t.Errorf("entry mode = %#o, want 0755", entry.Mode)
	case !entry.ModTime.Equal(imageLayersEpoch):
		t.Errorf("entry mtime = %s, want the epoch", entry.ModTime)
	case entry.Uid != 0 || entry.Gid != 0 || entry.Uname != "" || entry.Gname != "":
		t.Errorf("entry ownership = %d/%d %q/%q, want numeric root with no owner names",
			entry.Uid, entry.Gid, entry.Uname, entry.Gname)
	case entry.Size != 0:
		t.Errorf("entry size = %d, want an empty directory entry", entry.Size)
	}

	record := imageLayersReadRecord(t, project, "workspace")
	if record.Tar != "workspace.tar" || record.File != "" {
		t.Errorf("record artifact = %q/%q, want the tar form", record.Tar, record.File)
	}
	if record.Path != "/workspace" {
		t.Errorf("record path = %q, want /workspace", record.Path)
	}
	// The identity IS the path: there is no pin to derive, so an edit to the
	// declared directory is the only thing that can change these bytes, and the
	// recorded version has to state it or `--check` cannot see the edit.
	if record.Version != "/workspace 0755" {
		t.Errorf("record version = %q, want the path and mode that select the bytes", record.Version)
	}
	if record.SHA256 != sha256Hex(first) || record.Size != int64(len(first)) {
		t.Errorf("record = %s (%d bytes), layer hashes to %s (%d bytes)",
			record.SHA256, record.Size, sha256Hex(first), len(first))
	}
	if !reflect.DeepEqual(record.Source, imageLayerSource{}) {
		t.Errorf("record source = %+v, want none — this layer has no upstream artifact", record.Source)
	}
}

// TestImageLayersDirsCreatesEveryAncestor: a nested directory needs its parents
// to exist too, and the extractor's default mode for an invented parent is not
// something this image should inherit.
func TestImageLayersDirsCreatesEveryAncestor(t *testing.T) {
	project := imageLayersProject(t, `{"layers":[{"name":"cache","producer":"dirs","path":"/var/cache/putnami"}]}`)
	if err := ImageLayers(map[string]any{"project": project}, nil, t.TempDir(), nil, imageLayersIO(new([]string))); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "cache.tar"))
	if err != nil {
		t.Fatalf("read produced layer: %v", err)
	}
	headers := readTarEntries(t, data)
	if want := []string{"var/", "var/cache/", "var/cache/putnami/"}; !slices.Equal(headerNames(headers), want) {
		t.Fatalf("entries = %v, want %v", headerNames(headers), want)
	}
	for _, header := range headers {
		if header.Typeflag != tar.TypeDir || header.Mode != 0o755 {
			t.Errorf("%s = type %q mode %#o, want a 0755 directory", header.Name, string(header.Typeflag), header.Mode)
		}
	}
}

// TestImageLayersDirsManifestValidation: the path is this layer's whole
// identity, so every form that would make it ambiguous is refused before any
// producer runs — including the normalizable-but-not-normalized ones, which
// would produce the right layer while reading as a different directory to the
// image-spec agreement test.
func TestImageLayersDirsManifestValidation(t *testing.T) {
	cases := map[string]struct {
		path  string
		extra string
		wants string
	}{
		"the filesystem root": {path: "/", wants: "filesystem root"},
		"a relative path":     {path: "workspace", wants: "non-absolute"},
		"an empty path":       {path: "", wants: "non-absolute"},
		"a trailing slash":    {path: "/workspace/", wants: "unnormalized directory path"},
		"a dot-dot segment":   {path: "/workspace/../srv", wants: "unnormalized directory path"},
		"a doubled separator": {path: "//workspace", wants: "unnormalized directory path"},
		"a version nobody reads": {path: "/workspace", extra: `, "version": "1.2.3"`,
			wants: `a "version" field the dirs producer does not read`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			project := imageLayersProject(t,
				`{"layers":[{"name":"workspace","producer":"dirs","path":"`+tc.path+`"`+tc.extra+`}]}`)
			err := ImageLayers(map[string]any{"project": project}, nil, t.TempDir(), nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wants)
			}
			if clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("exit code = %d, want usage", clicore.ExitCode(err))
			}
			if _, statErr := os.Stat(filepath.Join(project, ".gen")); !os.IsNotExist(statErr) {
				t.Fatalf("a rejected manifest still created .gen (stat: %v)", statErr)
			}
		})
	}
}
