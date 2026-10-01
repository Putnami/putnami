package lockfile

import (
	"os"
	"path/filepath"
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
