package lockfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLockFileRoundTrip(t *testing.T) {
	dir := t.TempDir()

	lf := NewLockFile()
	lf.SetExtension("@putnami/typescript", LockEntry{
		Version:   "2.0.3",
		Integrity: "sha256-abc123",
		Source:    "https://putnami.dev/dl/putnami-typescript?version=^2.0.0",
	})
	lf.SetExtension("@putnami/go", LockEntry{
		Version:   "1.0.0",
		Integrity: "sha256-def456",
	})
	lf.SetTemplate("typescript-library", LockEntry{
		Version:      "1.2.0",
		Integrity:    "abc123",
		ManifestHash: "def456",
		Source:       "https://putnami.dev/dl/typescript-library?version=latest",
	})

	// Write
	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, LockFilename)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}

	// Read back
	lf2, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}

	if lf2.Version != DefaultWriteVersion {
		t.Errorf("Version = %d, want DefaultWriteVersion (%d)", lf2.Version, DefaultWriteVersion)
	}

	if len(lf2.Extensions) != 2 {
		t.Errorf("Extensions has %d entries, want 2", len(lf2.Extensions))
	}
	if len(lf2.Templates) != 1 {
		t.Errorf("Templates has %d entries, want 1", len(lf2.Templates))
	}

	ts, ok := lf2.GetExtension("@putnami/typescript")
	if !ok {
		t.Fatal("@putnami/typescript not found in lock file")
	}
	if ts.Version != "2.0.3" {
		t.Errorf("typescript version = %q, want %q", ts.Version, "2.0.3")
	}
	if ts.Integrity != "sha256-abc123" {
		t.Errorf("typescript integrity = %q, want %q", ts.Integrity, "sha256-abc123")
	}

	goEntry, ok := lf2.GetExtension("@putnami/go")
	if !ok {
		t.Fatal("@putnami/go not found in lock file")
	}
	if goEntry.Version != "1.0.0" {
		t.Errorf("go version = %q, want %q", goEntry.Version, "1.0.0")
	}

	tpl, ok := lf2.GetTemplate("typescript-library")
	if !ok {
		t.Fatal("typescript-library not found in lock file")
	}
	if tpl.Version != "1.2.0" || tpl.Integrity != "abc123" || tpl.ManifestHash != "def456" {
		t.Errorf("unexpected template entry: %+v", tpl)
	}
}

func TestLockFileNotFound(t *testing.T) {
	dir := t.TempDir()

	lf, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile should not error for missing file: %v", err)
	}
	if lf != nil {
		t.Error("ReadLockFile should return nil for missing file")
	}
}

func TestLockFileSetRemove(t *testing.T) {
	lf := NewLockFile()

	lf.SetExtension("@putnami/go", LockEntry{Version: "1.0.0"})
	lf.SetTemplate("go-server", LockEntry{Version: "2.0.0"})

	if _, ok := lf.GetExtension("@putnami/go"); !ok {
		t.Error("expected extension entry to exist after SetExtension")
	}
	if _, ok := lf.GetTemplate("go-server"); !ok {
		t.Error("expected template entry to exist after SetTemplate")
	}

	lf.RemoveExtension("@putnami/go")
	lf.RemoveTemplate("go-server")

	if _, ok := lf.GetExtension("@putnami/go"); ok {
		t.Error("expected extension entry to be removed after RemoveExtension")
	}
	if _, ok := lf.GetTemplate("go-server"); ok {
		t.Error("expected template entry to be removed after RemoveTemplate")
	}
}

func TestLockFileDeterministic(t *testing.T) {
	dir := t.TempDir()

	lf := NewLockFile()
	// Add in reverse alphabetical order to test sorting
	lf.SetExtension("@putnami/typescript", LockEntry{Version: "2.0.0"})
	lf.SetExtension("@putnami/go", LockEntry{Version: "1.0.0"})
	lf.SetExtension("@putnami/ci", LockEntry{Version: "1.0.0"})
	lf.SetTemplate("go-server", LockEntry{Version: "1.0.0"})
	lf.SetTemplate("api-client", LockEntry{Version: "1.0.0"})

	// Write twice and compare
	WriteLockFile(dir, lf)
	data1, _ := os.ReadFile(filepath.Join(dir, LockFilename))

	WriteLockFile(dir, lf)
	data2, _ := os.ReadFile(filepath.Join(dir, LockFilename))

	if string(data1) != string(data2) {
		t.Error("lock file output is not deterministic")
	}

	// Verify sorted order in the file
	content := string(data1)
	ciIdx := indexOf(content, "@putnami/ci")
	goIdx := indexOf(content, "@putnami/go")
	tsIdx := indexOf(content, "@putnami/typescript")

	if ciIdx > goIdx || goIdx > tsIdx {
		t.Errorf("extension entries not sorted: ci@%d go@%d ts@%d", ciIdx, goIdx, tsIdx)
	}

	apiIdx := indexOf(content, "api-client")
	srvIdx := indexOf(content, "go-server")

	if apiIdx > srvIdx {
		t.Errorf("template entries not sorted: api@%d srv@%d", apiIdx, srvIdx)
	}
}

func TestLockEntryPlatformIntegrity(t *testing.T) {
	var e LockEntry
	if got := e.IntegrityFor("linux", "amd64"); got != "" {
		t.Errorf("IntegrityFor on empty entry = %q, want empty", got)
	}

	e.SetPlatformIntegrity(PlatformKey("linux", "amd64"), "linuxhash")
	e.SetPlatformIntegrity(PlatformKey("darwin", "arm64"), "machash")
	// Empty values must be ignored.
	e.SetPlatformIntegrity("", "x")
	e.SetPlatformIntegrity("plan9/abc", "")

	if got := e.IntegrityFor("linux", "amd64"); got != "linuxhash" {
		t.Errorf("IntegrityFor(linux/amd64) = %q, want linuxhash", got)
	}
	if got := e.IntegrityFor("darwin", "arm64"); got != "machash" {
		t.Errorf("IntegrityFor(darwin/arm64) = %q, want machash", got)
	}
	if got := e.IntegrityFor("windows", "amd64"); got != "" {
		t.Errorf("IntegrityFor(windows/amd64) = %q, want empty", got)
	}
	if _, ok := e.Integrities["plan9/abc"]; ok {
		t.Error("empty digest should not have been recorded")
	}
}

// TestLockFileIntegritiesRoundTrip ensures the per-platform map survives a
// write/read cycle so a committed lock carries every platform it has seen.
func TestLockFileIntegritiesRoundTrip(t *testing.T) {
	dir := t.TempDir()

	lf := NewLockFile()
	lf.SetExtension("@putnami/typescript", LockEntry{
		Version:      "2.0.3",
		ManifestHash: "manifesthash",
		Integrities: map[string]string{
			"darwin/arm64": "machash",
			"linux/amd64":  "linuxhash",
		},
	})
	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	got, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	entry, ok := got.GetExtension("@putnami/typescript")
	if !ok {
		t.Fatal("extension entry missing after round-trip")
	}
	if entry.IntegrityFor("linux", "amd64") != "linuxhash" || entry.IntegrityFor("darwin", "arm64") != "machash" {
		t.Errorf("per-platform digests did not round-trip: %+v", entry.Integrities)
	}
}

// TestLockFileCLIPin verifies the CLI pin round-trips with its per-platform
// integrity map, reusing the same LockEntry machinery as extensions.
func TestLockFileCLIPin(t *testing.T) {
	dir := t.TempDir()

	lf := NewLockFile()
	if _, ok := lf.GetCLI(); ok {
		t.Fatal("a fresh lock file must not pin a CLI")
	}
	lf.SetCLI(LockEntry{
		Version: "1.4.2",
		Source:  "https://put.putnami.dev/putnami/cli/download?channel=v1.4.2",
		Integrities: map[string]string{
			"darwin/arm64": "machash",
			"linux/amd64":  "linuxhash",
		},
	})

	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	got, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	cli, ok := got.GetCLI()
	if !ok {
		t.Fatal("CLI pin missing after round-trip")
	}
	if cli.Version != "1.4.2" {
		t.Errorf("CLI version = %q, want 1.4.2", cli.Version)
	}
	if cli.IntegrityFor("linux", "amd64") != "linuxhash" || cli.IntegrityFor("darwin", "arm64") != "machash" {
		t.Errorf("CLI per-platform digests did not round-trip: %+v", cli.Integrities)
	}
}

// TestLockFileCLIAbsentIsOmitted guarantees a lock without a CLI pin stays
// byte-compatible with pre-CLI-pin locks: no "cli" key is emitted, and such a
// lock reads back with no pin and no error.
func TestLockFileCLIAbsentIsOmitted(t *testing.T) {
	dir := t.TempDir()

	lf := NewLockFile()
	lf.SetExtension("@putnami/go", LockEntry{Version: "1.0.0"})
	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, LockFilename))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if indexOf(string(data), `"cli"`) != -1 {
		t.Errorf("lock without a CLI pin must not emit a \"cli\" key:\n%s", data)
	}

	got, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	if _, ok := got.GetCLI(); ok {
		t.Error("GetCLI should report no pin for a lock without one")
	}
}

// TestLockFileCLISetRemove covers the CLI accessors and confirms SetCLI copies
// the entry rather than aliasing the caller's value.
func TestLockFileCLISetRemove(t *testing.T) {
	lf := NewLockFile()

	entry := LockEntry{Version: "2.0.0"}
	lf.SetCLI(entry)
	entry.Version = "9.9.9" // must not affect the stored pin
	if got, ok := lf.GetCLI(); !ok || got.Version != "2.0.0" {
		t.Errorf("after SetCLI: got %+v ok=%v, want version 2.0.0", got, ok)
	}

	lf.RemoveCLI()
	if _, ok := lf.GetCLI(); ok {
		t.Error("expected no CLI pin after RemoveCLI")
	}
}

func TestHashBytes(t *testing.T) {
	h1 := HashBytes([]byte("hello"))
	h2 := HashBytes([]byte("hello"))
	h3 := HashBytes([]byte("world"))

	if h1 != h2 {
		t.Error("identical input should produce identical hash")
	}
	if h1 == h3 {
		t.Error("different input should produce different hash")
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (hex-encoded SHA-256)", len(h1))
	}
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// TestSplitPlatformKey pins SplitPlatformKey as the exact inverse of
// PlatformKey: a recorded platform must round-trip back into the os/arch pair
// used to re-resolve its digest from the registry, and anything that is
// not a well-formed key must be rejected rather than turned into a bogus query.
func TestSplitPlatformKey(t *testing.T) {
	for _, pair := range [][2]string{
		{"linux", "amd64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
		{"linux", "x64"},
	} {
		key := PlatformKey(pair[0], pair[1])
		gotOS, gotArch, ok := SplitPlatformKey(key)
		if !ok || gotOS != pair[0] || gotArch != pair[1] {
			t.Errorf("SplitPlatformKey(%q) = (%q, %q, %v), want (%q, %q, true)", key, gotOS, gotArch, ok, pair[0], pair[1])
		}
	}

	for _, bad := range []string{"", "linux", "/amd64", "linux/", "linux/amd64/v3", "/"} {
		if _, _, ok := SplitPlatformKey(bad); ok {
			t.Errorf("SplitPlatformKey(%q) accepted a malformed key", bad)
		}
	}
}

// Clone is a deep copy: editing the copy's entries, including a per-platform
// digest map, leaves the original untouched. An install relies on it to hold
// the lock it would write beside the lock a consumer requested.
func TestLockFileCloneIsDeep(t *testing.T) {
	original := NewLockFile()
	original.SetCLI(LockEntry{Version: "1.0.0", Integrities: map[string]string{"darwin/arm64": "cli"}})
	original.SetExtension("@fake/ext", LockEntry{Version: "1.0.0", Integrities: map[string]string{"darwin/arm64": "machash"}})
	original.SetTemplate("@fake/tpl", LockEntry{Version: "1.0.0"})

	clone := original.Clone()
	entry, _ := clone.GetExtension("@fake/ext")
	entry.Integrities["linux/amd64"] = "linuxhash"
	clone.SetExtension("@fake/other", LockEntry{Version: "2.0.0"})
	clone.CLI.Integrities["linux/amd64"] = "cli-linux"
	clone.RemoveTemplate("@fake/tpl")

	if got, _ := original.GetExtension("@fake/ext"); got.IntegrityFor("linux", "amd64") != "" {
		t.Errorf("editing the clone's digest map reached the original: %+v", got.Integrities)
	}
	if _, ok := original.GetExtension("@fake/other"); ok {
		t.Error("an entry added to the clone appeared in the original")
	}
	if original.CLI.IntegrityFor("linux", "amd64") != "" {
		t.Errorf("editing the clone's CLI digests reached the original: %+v", original.CLI.Integrities)
	}
	if _, ok := original.GetTemplate("@fake/tpl"); !ok {
		t.Error("removing a template from the clone removed it from the original")
	}
	if (*LockFile)(nil).Clone() != nil {
		t.Error("a nil lock cloned to a non-nil one")
	}
}

// DiffersFromDisk answers the question WriteLockFileIfChanged acts on, and
// writes nothing.
func TestDiffersFromDiskAgreesWithWriteLockFileIfChanged(t *testing.T) {
	dir := t.TempDir()
	lf := NewLockFile()
	lf.SetExtension("@fake/ext", LockEntry{Version: "1.0.0", Integrities: map[string]string{"darwin/arm64": "machash"}})

	if changed, err := DiffersFromDisk(dir, lf.Clone()); err != nil || !changed {
		t.Fatalf("DiffersFromDisk with no file = %t, %v; want true", changed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, LockFilename)); !os.IsNotExist(err) {
		t.Fatalf("DiffersFromDisk wrote the lock: %v", err)
	}
	if written, err := WriteLockFileIfChanged(dir, lf.Clone()); err != nil || !written {
		t.Fatalf("WriteLockFileIfChanged = %t, %v; want a write", written, err)
	}
	if changed, err := DiffersFromDisk(dir, lf.Clone()); err != nil || changed {
		t.Fatalf("DiffersFromDisk after the write = %t, %v; want false", changed, err)
	}
	edited := lf.Clone()
	entry, _ := edited.GetExtension("@fake/ext")
	entry.SetPlatformIntegrity("linux/amd64", "linuxhash")
	edited.SetExtension("@fake/ext", entry)
	if changed, err := DiffersFromDisk(dir, edited); err != nil || !changed {
		t.Fatalf("DiffersFromDisk after an edit = %t, %v; want true", changed, err)
	}
}

// TestLockFileCompatibilityMatrix states the (client, lock) pairs at the
// window's edges that the read-window and prior-release tests leave out: a
// lock that records no version is refused as outdated, and a reader whose
// window ends one version lower refuses a newer lock with an error that names
// both numbers.
func TestLockFileCompatibilityMatrix(t *testing.T) {
	t.Run("current client refuses a lock that records no version", func(t *testing.T) {
		_, err := ParseLockFile([]byte(`{"extensions": {}, "templates": {}}`))
		var outdated *OutdatedVersionError
		if !errors.As(err, &outdated) || outdated.Found != 0 {
			t.Fatalf("ParseLockFile(versionless) error = %v, want *OutdatedVersionError with Found=0", err)
		}
	})

	t.Run("v4-only client rejects a v5 lock with a versioned error", func(t *testing.T) {
		// parseLockFile with maxVersion=v4 is a v4-only reader on the real code
		// path: same parser, one supported version lower. A reader that meets a
		// newer lock refuses it and names both numbers, so the operator learns
		// which side to move.
		_, err := parseLockFile(mustMarshal(t, NewLockFile()), FormatVersionV4, FormatVersionV4)
		var versionErr *UnsupportedVersionError
		if !errors.As(err, &versionErr) {
			t.Fatalf("parseLockFile(v5, max=v4) error = %v, want *UnsupportedVersionError", err)
		}
		if versionErr.Found != FormatVersionV5 || versionErr.Max != FormatVersionV4 {
			t.Errorf("UnsupportedVersionError = %+v, want Found=5 Max=4", versionErr)
		}
		for _, want := range []string{LockFilename, "version 5", "version 4"} {
			if !strings.Contains(versionErr.Error(), want) {
				t.Errorf("error message %q does not mention %q", versionErr.Error(), want)
			}
		}
	})
}

// TestWriteLockFilePreservesAnExistingV4Lock pins that a default write keeps
// the version the file already declares: an install never promotes a lock as a
// side effect.
func TestWriteLockFilePreservesAnExistingV4Lock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFilename), mustMarshal(t, v4Fixture()), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	lf, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	lf.SetExtension("@putnami/python", LockEntry{Version: "1.0.0"})
	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	got, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Version != FormatVersionV4 {
		t.Fatalf("WriteLockFile moved a v4 lock to v%d", got.Version)
	}
	if entry, _ := got.GetExtension("@putnami/typescript"); entry.TaskContract != 3 {
		t.Errorf("contract record lost through a default write: %+v", entry)
	}
}

// TestMarshalLockFileIsCanonical pins the determinism B0e's digests depend on:
// the same content marshals to the same bytes regardless of how the maps were
// populated, and repeated marshaling never varies with map iteration order.
func TestMarshalLockFileIsCanonical(t *testing.T) {
	forward := NewLockFile()
	for _, name := range []string{"@putnami/ci", "@putnami/go", "@putnami/typescript"} {
		forward.SetExtension(name, LockEntry{
			Version:      "1.0.0",
			TaskContract: 3,
			Integrities:  map[string]string{"darwin/arm64": "mac", "linux/amd64": "linux"},
		})
	}
	reverse := NewLockFile()
	for _, name := range []string{"@putnami/typescript", "@putnami/go", "@putnami/ci"} {
		reverse.SetExtension(name, LockEntry{
			Version:      "1.0.0",
			TaskContract: 3,
			Integrities:  map[string]string{"linux/amd64": "linux", "darwin/arm64": "mac"},
		})
	}

	want := string(mustMarshal(t, forward))
	for i := 0; i < 20; i++ {
		if got := string(mustMarshal(t, forward)); got != want {
			t.Fatalf("repeated marshal of the same lock differed on iteration %d", i)
		}
		if got := string(mustMarshal(t, reverse)); got != want {
			t.Fatalf("insertion order changed the bytes on iteration %d:\n--- got ---\n%s\n--- want ---\n%s", i, got, want)
		}
	}
}

// downloadURLWithAmpersand is a CLI download URL shaped like the one the
// registry actually issues: multiple query parameters joined by '&'. It is the
// reproduction case for the HTML-escaping papercut — encoding/json's default
// escapes '&' to \u0026, which turns a byte-identical "read, then write back
// unchanged" into a diff on every `putnami install`.
const downloadURLWithAmpersand = "https://put.putnami.dev/putnami/cli/download?channel=1.4.2&os=darwin&arch=arm64"

// TestMarshalLockFileDoesNotEscapeHTMLCharacters pins the fix directly: a
// source URL containing '&' comes out of MarshalLockFile with the plain byte,
// never encoding/json's default \u0026 escape (or an HTML-entity &amp;, in case
// a future change routes the value through a template instead). encoding/json's
// escaping exists for values embedded in an HTML <script> tag, which this
// on-disk lock file never is.
func TestMarshalLockFileDoesNotEscapeHTMLCharacters(t *testing.T) {
	lf := v4Fixture()
	cli, _ := lf.GetCLI()
	cli.Source = downloadURLWithAmpersand
	lf.SetCLI(cli)

	data := mustMarshal(t, lf)

	if strings.Contains(string(data), `\u0026`) {
		t.Errorf("marshaled lock still HTML-escapes '&' as \\u0026:\n%s", data)
	}
	if strings.Contains(string(data), "&amp;") {
		t.Errorf("marshaled lock HTML-entity-escapes '&' as &amp;:\n%s", data)
	}
	if !strings.Contains(string(data), downloadURLWithAmpersand) {
		t.Errorf("marshaled lock does not contain the source URL verbatim:\n%s", data)
	}
}

// TestMarshalLockFileIsIdempotentAcrossAmpersandURL is the write half of the
// dirty-tree reproduction: write a lock whose CLI source carries a '&', read it
// back, and marshal it again. The second marshal must be byte-identical to the
// first, because that identity is what makes `putnami install` a no-op on a
// workspace whose lock the install did not actually need to change — the
// precondition for a clean git tree and a version stamp with no
// `-<dirtyhash>` suffix.
func TestMarshalLockFileIsIdempotentAcrossAmpersandURL(t *testing.T) {
	lf := v4Fixture()
	cli, _ := lf.GetCLI()
	cli.Source = downloadURLWithAmpersand
	lf.SetCLI(cli)

	first := mustMarshal(t, lf)

	reparsed, err := ParseLockFile(first)
	if err != nil {
		t.Fatalf("ParseLockFile: %v", err)
	}
	second := mustMarshal(t, reparsed)

	if !bytes.Equal(first, second) {
		t.Fatalf("marshal was not idempotent across a read/write round trip:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestRewritingAnHTMLEscapedLockConvergesInOneWrite is the convergence half of
// the dirty-tree reproduction: a lock an HTML-escaping writer produced has
// \u0026 on disk. Reading it and writing it back must both drop the escape (so
// the working tree goes clean on the very next install, not after N more of
// them) and reach a fixed point (so that first rewrite is the last one this
// file ever needs for that reason).
func TestRewritingAnHTMLEscapedLockConvergesInOneWrite(t *testing.T) {
	const escaped = `{
  "version": 4,
  "cli": {
    "version": "1.4.2",
    "source": "https://put.putnami.dev/putnami/cli/download?channel=1.4.2\u0026os=darwin\u0026arch=arm64"
  },
  "extensions": {},
  "templates": {}
}
`
	lf, err := ParseLockFile([]byte(escaped))
	if err != nil {
		t.Fatalf("ParseLockFile(escaped): %v", err)
	}
	if lf.CLI == nil || lf.CLI.Source != downloadURLWithAmpersand {
		t.Fatalf("reader did not decode the \\u0026 escape back to '&': %+v", lf.CLI)
	}

	rewritten := mustMarshal(t, lf)
	if strings.Contains(string(rewritten), `\u0026`) {
		t.Fatalf("rewrite of an escaped lock kept the \\u0026 escape:\n%s", rewritten)
	}
	if !strings.Contains(string(rewritten), downloadURLWithAmpersand) {
		t.Fatalf("rewrite of an escaped lock does not contain the plain URL:\n%s", rewritten)
	}

	reparsed, err := ParseLockFile(rewritten)
	if err != nil {
		t.Fatalf("ParseLockFile(rewritten): %v", err)
	}
	again := mustMarshal(t, reparsed)
	if !bytes.Equal(rewritten, again) {
		t.Fatalf("second write after convergence was not a no-op:\n--- rewritten ---\n%s\n--- again ---\n%s", rewritten, again)
	}
}

// TestMarshalLockFileOmitsLegacyExtensionScalarIntegrity pins the
// cross-platform canonical form. A reader must still accept the scalar a
// committed v4 lock may carry alone, but writing a platform-mapped entry drops
// its redundant scalar and never mutates the parsed value in memory. A
// scalar-only entry retains its sole verification datum. This is what makes a
// complete lock authored on one OS byte-stable on another.
func TestMarshalLockFileOmitsLegacyExtensionScalarIntegrity(t *testing.T) {
	const legacy = `{
  "version": 4,
  "extensions": {
    "@putnami/legacy": {
      "version": "1.0.0",
      "integrity": "only-verification-datum"
    },
    "@putnami/typescript": {
      "version": "3.1.0",
      "integrity": "last-writer-platform",
      "integrities": {
        "darwin/arm64": "machash",
        "linux/amd64": "linuxhash"
      }
    }
  },
  "templates": {}
}
`
	lf, err := ParseLockFile([]byte(legacy))
	if err != nil {
		t.Fatalf("ParseLockFile(legacy v4): %v", err)
	}
	before, _ := lf.GetExtension("@putnami/typescript")
	if before.Integrity != "last-writer-platform" {
		t.Fatalf("legacy scalar was not retained on read: %+v", before)
	}

	data := mustMarshal(t, lf)
	canonical, err := ParseLockFile(data)
	if err != nil {
		t.Fatalf("ParseLockFile(canonical v4): %v", err)
	}
	entry, _ := canonical.GetExtension("@putnami/typescript")
	if entry.Integrity != "" {
		t.Errorf("canonical extension scalar = %q, want empty", entry.Integrity)
	}
	if entry.IntegrityFor("darwin", "arm64") != "machash" || entry.IntegrityFor("linux", "amd64") != "linuxhash" {
		t.Errorf("canonical projection changed the authoritative platform map: %+v", entry.Integrities)
	}
	legacyEntry, _ := canonical.GetExtension("@putnami/legacy")
	if legacyEntry.Integrity != "only-verification-datum" {
		t.Errorf("canonical projection dropped a scalar-only legacy pin: %+v", legacyEntry)
	}
	after, _ := lf.GetExtension("@putnami/typescript")
	if after.Integrity != "last-writer-platform" {
		t.Errorf("canonical rendering mutated the parsed legacy value: %+v", after)
	}
}

// TestWriteLockFileIsAtomic pins the write's crash behavior: the bytes arrive
// by rename, so no reader ever sees a half-written lock and no temp file is
// left behind.
func TestWriteLockFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFilename), []byte(`{"version": 4}`), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	if err := WriteLockFile(dir, v4Fixture()); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != LockFilename {
			t.Errorf("write left a stray file behind: %s", entry.Name())
		}
	}

	info, err := os.Stat(filepath.Join(dir, LockFilename))
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	// On Windows the permission bits reduce to a read-only attribute, and a
	// readable, writable file reports 0666.
	wantPerm := os.FileMode(0o644)
	if runtime.GOOS == "windows" {
		wantPerm = 0o666
	}
	if perm := info.Mode().Perm(); perm != wantPerm {
		t.Errorf("lock file mode = %v, want %v", perm, wantPerm)
	}
}

// v4Fixture is a v4 lock with a two-platform CLI pin, a scalar-plus-map
// extension entry and contract records.
func v4Fixture() *LockFile {
	lf := &LockFile{
		Version:    FormatVersionV4,
		Extensions: make(map[string]LockEntry),
		Templates:  make(map[string]LockEntry),
	}
	lf.SetCLI(LockEntry{
		Version:     "1.4.2",
		Integrities: map[string]string{"darwin/arm64": "climachash", "linux/amd64": "clilinuxhash"},
		Source:      "https://put.putnami.dev/putnami/cli/download?channel=1.4.2",
	})
	lf.SetExtension("@putnami/go", LockEntry{
		Version:      "2.0.0",
		ManifestHash: "gomanifest",
		TaskContract: 2,
	})
	lf.SetExtension("@putnami/typescript", LockEntry{
		Version:      "3.1.0",
		Integrity:    "legacydigest",
		Integrities:  map[string]string{"darwin/arm64": "tsmachash"},
		ManifestHash: "tsmanifest",
		Source:       "https://put.putnami.dev/putnami/typescript/download?channel=3.1.0",
		TaskContract: 3,
	})
	lf.SetTemplate("typescript-web", LockEntry{Version: "1.0.0"})
	return lf
}

func mustMarshal(t *testing.T, lf *LockFile) []byte {
	t.Helper()
	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatalf("MarshalLockFile: %v", err)
	}
	return data
}
