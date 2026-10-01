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

// Lock format v2. v2's delta over v1
// is one field — LockEntry.TaskContract — and the rule that a reader accepts
// exactly [MinSupportedVersion, MaxSupportedVersion] and refuses anything
// outside it, in EITHER direction.
//
// DUAL-READ TESTS, CONVERTED: the rows asserting that v1 and v2 both load
// were declared temporary, and a later change spent them. A v1 lock is now an
// *OutdatedVersionError from every reader except the migration's
// (ReadMigratableLockFile), so the v1 fixture below survives as the INPUT to
// the rejection and to the conversion — not as something that loads.
//
// DELETION DEFERRED, deliberately. A later change deleted the rest of the bridge
// and left this: the v1 fixture, FormatVersionV1, dropFieldsAbove and
// ReadMigratableLockFile stay until the FIRST RELEASED BUILD carrying
// `putnami migrate vnext` has shipped. No published build has carried the
// migration yet, and v1 locks are live in consumer repositories, so deleting
// the read path now would leave those repositories with a lock no released CLI
// can either load or convert. The gate is a released build, not a slice.

// v1Golden is a lock file in the exact canonical bytes today's CLI writes. It
// is the byte-identity fixture for the additive invariant: v1 in, v1 out,
// unchanged. Editing it is a lock-format change and must be reviewed as one.
const v1Golden = `{
  "version": 1,
  "cli": {
    "version": "1.4.2",
    "integrities": {
      "darwin/arm64": "climachash",
      "linux/amd64": "clilinuxhash"
    },
    "source": "https://put.putnami.dev/putnami/cli/download?channel=1.4.2"
  },
  "extensions": {
    "@putnami/go": {
      "version": "2.0.0",
      "manifestHash": "gomanifest"
    },
    "@putnami/typescript": {
      "version": "3.1.0",
      "integrity": "legacydigest",
      "integrities": {
        "darwin/arm64": "tsmachash"
      },
      "manifestHash": "tsmanifest",
      "source": "https://put.putnami.dev/putnami/typescript/download?channel=3.1.0"
    }
  },
  "templates": {
    "typescript-web": {
      "version": "1.0.0"
    }
  }
}
`

// TestReadLockFileV1IsAHardError is B0d's byte-identical v1 round-trip,
// converted (slice B6c). A v1 lock no longer loads at all: it is refused with a
// versioned error that names both numbers and the command that converts the
// file, because the alternative — reading it as v2 — would mean inventing the
// task-contract records v2 exists to carry.
func TestReadLockFileV1IsAHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LockFilename)
	if err := os.WriteFile(path, []byte(v1Golden), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	lf, err := ReadLockFile(dir)
	if lf != nil {
		t.Error("a refused lock must not be returned; a caller could act on it")
	}
	var outdated *OutdatedVersionError
	if !errors.As(err, &outdated) {
		t.Fatalf("ReadLockFile(v1) error = %v, want *OutdatedVersionError", err)
	}
	if outdated.Found != FormatVersionV1 || outdated.Min != MinSupportedVersion {
		t.Errorf("OutdatedVersionError = %+v, want Found=%d Min=%d",
			outdated, FormatVersionV1, MinSupportedVersion)
	}
	for _, want := range []string{LockFilename, "version 1", "version 2", "putnami migrate vnext --apply"} {
		if !strings.Contains(outdated.Error(), want) {
			t.Errorf("error message %q does not mention %q", outdated.Error(), want)
		}
	}

	// The refusal must not have touched the file: a reader that rewrote a lock
	// it refused would convert workspaces as a side effect of any command.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != v1Golden {
		t.Errorf("refusing a v1 lock modified it.\n--- got ---\n%s\n--- want ---\n%s", got, v1Golden)
	}
}

// TestReadLockFileVersionlessIsAHardError pins the spelling: a file that
// records NO version is v1, so it is refused for the same reason and with the
// same message rather than being defaulted into the current format.
func TestReadLockFileVersionlessIsAHardError(t *testing.T) {
	dir := t.TempDir()
	seed := `{"extensions": {}, "templates": {}}`
	if err := os.WriteFile(filepath.Join(dir, LockFilename), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	_, err := ReadLockFile(dir)
	var outdated *OutdatedVersionError
	if !errors.As(err, &outdated) {
		t.Fatalf("ReadLockFile(versionless) error = %v, want *OutdatedVersionError", err)
	}
	if !strings.Contains(outdated.Error(), "version 1") {
		t.Errorf("a versionless lock must be reported as v1, got %q", outdated.Error())
	}
}

// TestReadMigratableLockFileAcceptsV1 pins the one exception. The migration is
// what moves a workspace onto the floor, so it — and only it — reads below it.
func TestReadMigratableLockFileAcceptsV1(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFilename), []byte(v1Golden), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	lf, err := ReadMigratableLockFile(dir)
	if err != nil {
		t.Fatalf("ReadMigratableLockFile: %v", err)
	}
	if lf.Version != FormatVersionV1 {
		t.Fatalf("Version = %d, want %d", lf.Version, FormatVersionV1)
	}
	if entry, ok := lf.GetExtension("@putnami/typescript"); !ok || entry.Version != "3.1.0" {
		t.Errorf("v1 entry did not survive the migration read: %+v (ok=%v)", entry, ok)
	}
	if cli, ok := lf.GetCLI(); !ok || cli.IntegrityFor("linux", "amd64") != "clilinuxhash" {
		t.Errorf("v1 CLI pin did not survive the migration read: %+v (ok=%v)", cli, ok)
	}
	// Still above the ceiling in the other direction.
	if _, err := ReadMigratableLockFile(t.TempDir()); err != nil {
		t.Errorf("a missing lock is still (nil, nil): %v", err)
	}
}

// TestWriteLockFileV1DropsV2Vocabulary proves the version field is load-bearing
// on the WRITE side: a lock still at v1 cannot emit a v2-only field, even when
// one is set in memory. Without this, a partially-migrated process would leave
// a file that claims v1 while carrying v2 vocabulary — exactly the "garbage" an
// older client would then have to interpret. The v1 write survives the read
// floor because dropFieldsAbove is the rule every FUTURE bump reuses.
func TestWriteLockFileV1DropsV2Vocabulary(t *testing.T) {
	dir := t.TempDir()

	lf := NewLockFile()
	lf.Version = FormatVersionV1
	lf.SetExtension("@putnami/go", LockEntry{Version: "2.0.0", TaskContract: 3})
	if err := WriteLockFile(dir, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, LockFilename))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if strings.Contains(string(data), "taskContract") {
		t.Errorf("a v1 write emitted the v2-only taskContract field:\n%s", data)
	}
	// The drop must not damage the caller's in-memory lock: rendering is a
	// projection, not a mutation.
	if entry, _ := lf.GetExtension("@putnami/go"); entry.TaskContract != 3 {
		t.Errorf("WriteLockFile mutated the caller's entry: TaskContract = %d, want 3", entry.TaskContract)
	}
}

// TestReadLockFileV1DiscardsV2Vocabulary is the same rule on the READ side: a
// hand-edited v1 lock carrying a v2 field must not have it honored, or the
// version stops meaning anything about what a reader may trust. It runs through
// the migration reader now, since that is the only path a v1 file still takes —
// and it is exactly the path where honoring a smuggled field would corrupt the
// conversion.
func TestReadLockFileV1DiscardsV2Vocabulary(t *testing.T) {
	dir := t.TempDir()
	seed := `{
  "version": 1,
  "extensions": {
    "@putnami/go": {
      "version": "2.0.0",
      "taskContract": 3
    }
  },
  "templates": {}
}
`
	if err := os.WriteFile(filepath.Join(dir, LockFilename), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	lf, err := ReadMigratableLockFile(dir)
	if err != nil {
		t.Fatalf("ReadMigratableLockFile: %v", err)
	}
	entry, ok := lf.GetExtension("@putnami/go")
	if !ok {
		t.Fatal("extension entry missing")
	}
	if entry.TaskContract != 0 {
		t.Errorf("a v1 lock exposed a task contract: TaskContract = %d, want 0", entry.TaskContract)
	}
}

// TestLockFileV2RoundTrip pins the v2 delta itself: the recorded task contract
// survives write → read, and the file declares the version that defines it.
func TestLockFileV2RoundTrip(t *testing.T) {
	dir := t.TempDir()

	lf := NewLockFile()
	lf.SetExtension("@putnami/typescript", LockEntry{Version: "3.1.0", TaskContract: 3})
	lf.SetExtension("@putnami/go", LockEntry{Version: "2.0.0", TaskContract: 2})
	lf.SetTemplate("typescript-web", LockEntry{Version: "1.0.0"})

	if err := WriteLockFileV2(dir, lf); err != nil {
		t.Fatalf("WriteLockFileV2: %v", err)
	}
	if lf.Version != FormatVersionV2 {
		t.Errorf("WriteLockFileV2 left Version = %d, want %d", lf.Version, FormatVersionV2)
	}

	got, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	if got.Version != FormatVersionV2 {
		t.Errorf("Version = %d, want %d", got.Version, FormatVersionV2)
	}
	ts, ok := got.GetExtension("@putnami/typescript")
	if !ok || ts.TaskContract != 3 {
		t.Errorf("typescript entry = %+v, want TaskContract 3", ts)
	}
	goEntry, _ := got.GetExtension("@putnami/go")
	if goEntry.TaskContract != 2 {
		t.Errorf("go entry TaskContract = %d, want 2", goEntry.TaskContract)
	}
	if tpl, _ := got.GetTemplate("typescript-web"); tpl.TaskContract != 0 {
		t.Errorf("template entry recorded a task contract: %+v", tpl)
	}
}

// TestLockFileCompatibilityMatrix is the B0 dataset row for lock v2: every
// (client, lock) pair stated once, so "additive" is a proven property rather
// than a claim.
//
// DUAL-READ, CONVERTED: rows 1 and 4 used to assert that a v1 lock loads
// under the current client. Row 1 is now the REFUSAL, and row 4 the refusal
// plus the migration reader that converts past it. Both outlive the bridge's
// deletion — see the deferral at the top of this file: they go when a
// RELEASED build has carried the migration, not when a slice says so.
func TestLockFileCompatibilityMatrix(t *testing.T) {
	v2Bytes := mustMarshal(t, v2Fixture())

	t.Run("new client refuses a v1 lock", func(t *testing.T) {
		_, err := ParseLockFile([]byte(v1Golden))
		var outdated *OutdatedVersionError
		if !errors.As(err, &outdated) {
			t.Fatalf("ParseLockFile(v1) error = %v, want *OutdatedVersionError", err)
		}
		if outdated.Min != MinSupportedVersion {
			t.Errorf("Min = %d, want MinSupportedVersion (%d)", outdated.Min, MinSupportedVersion)
		}
	})

	t.Run("new client reads v2 lock", func(t *testing.T) {
		lf, err := ParseLockFile(v2Bytes)
		if err != nil {
			t.Fatalf("ParseLockFile(v2): %v", err)
		}
		if lf.Version != FormatVersionV2 {
			t.Errorf("Version = %d, want %d", lf.Version, FormatVersionV2)
		}
		if entry, _ := lf.GetExtension("@putnami/typescript"); entry.TaskContract != 3 {
			t.Errorf("v2 contract record lost: %+v", entry)
		}
	})

	t.Run("v1-only client rejects a v2 lock with a versioned error", func(t *testing.T) {
		// parseLockFile with maxVersion=v1 is a v1-only reader on the real code
		// path: same parser, one supported version lower. What it pins is the
		// rule for every future bump — a reader that meets a newer lock refuses
		// it and names both numbers, so the operator learns which side to move.
		//
		// Honest scope: binaries released BEFORE B0d have no version guard at
		// all and will read a v2 lock as v1, ignoring the fields they do not
		// know. B6c is where that stops mattering — such a binary also predates
		// CLI contract 3 and cannot run a contract-3 workspace at all.
		_, err := parseLockFile(v2Bytes, FormatVersionV1, FormatVersionV1)
		var versionErr *UnsupportedVersionError
		if !errors.As(err, &versionErr) {
			t.Fatalf("parseLockFile(v2, max=v1) error = %v, want *UnsupportedVersionError", err)
		}
		if versionErr.Found != FormatVersionV2 || versionErr.Max != FormatVersionV1 {
			t.Errorf("UnsupportedVersionError = %+v, want Found=2 Max=1", versionErr)
		}
		for _, want := range []string{LockFilename, "version 2", "version 1"} {
			if !strings.Contains(versionErr.Error(), want) {
				t.Errorf("error message %q does not mention %q", versionErr.Error(), want)
			}
		}
	})

	t.Run("current client rejects a future lock version", func(t *testing.T) {
		future := strings.Replace(v1Golden, `"version": 1,`, `"version": 99,`, 1)
		_, err := ParseLockFile([]byte(future))
		var versionErr *UnsupportedVersionError
		if !errors.As(err, &versionErr) {
			t.Fatalf("ParseLockFile(v99) error = %v, want *UnsupportedVersionError", err)
		}
		if versionErr.Max != MaxSupportedVersion {
			t.Errorf("Max = %d, want MaxSupportedVersion (%d)", versionErr.Max, MaxSupportedVersion)
		}
	})

	t.Run("v1 upgraded in place equals a direct v2 write", func(t *testing.T) {
		// The round-trip row: converting a v1 lock and writing it at v2 must
		// produce exactly the bytes of a lock authored at v2 with the same
		// content. Anything else means the migration is not purely mechanical.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, LockFilename), []byte(v1Golden), 0o644); err != nil {
			t.Fatalf("seed lock file: %v", err)
		}
		converted, err := ReadMigratableLockFile(dir)
		if err != nil {
			t.Fatalf("ReadMigratableLockFile: %v", err)
		}
		entry, _ := converted.GetExtension("@putnami/typescript")
		entry.TaskContract = 3
		converted.SetExtension("@putnami/typescript", entry)
		goEntry, _ := converted.GetExtension("@putnami/go")
		goEntry.TaskContract = 2
		converted.SetExtension("@putnami/go", goEntry)

		if err := WriteLockFileV2(dir, converted); err != nil {
			t.Fatalf("WriteLockFileV2: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, LockFilename))
		if err != nil {
			t.Fatalf("read migrated lock: %v", err)
		}
		if string(got) != string(v2Bytes) {
			t.Errorf("v1→v2 conversion differs from a direct v2 write.\n--- got ---\n%s\n--- want ---\n%s", got, v2Bytes)
		}

		reread, err := ReadLockFile(dir)
		if err != nil {
			t.Fatalf("re-read migrated lock: %v", err)
		}
		if reread.Version != FormatVersionV2 {
			t.Errorf("migrated lock version = %d, want %d", reread.Version, FormatVersionV2)
		}
	})
}

// TestWriteLockFilePreservesAnExistingV2Lock guards the other half of "write
// stays v1 by default": default means the version the file already declares.
// An install that silently rewrote a migrated lock at v1 would drop every
// contract record it carries.
func TestWriteLockFilePreservesAnExistingV2Lock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFilename), mustMarshal(t, v2Fixture()), 0o644); err != nil {
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
	if got.Version != FormatVersionV2 {
		t.Fatalf("WriteLockFile downgraded a v2 lock to v%d", got.Version)
	}
	if entry, _ := got.GetExtension("@putnami/typescript"); entry.TaskContract != 3 {
		t.Errorf("contract record lost through a default write: %+v", entry)
	}
}

// TestNewLockFileDefaultsToV4 pins the current write default: a lock
// this CLI creates is born at the version this CLI requires, so `putnami
// install` in a fresh workspace never produces a file its own next run refuses.
func TestNewLockFileDefaultsToV4(t *testing.T) {
	if got := NewLockFile().Version; got != FormatVersionV4 {
		t.Errorf("NewLockFile().Version = %d, want %d", got, FormatVersionV4)
	}
	if DefaultWriteVersion != FormatVersionV4 {
		t.Errorf("DefaultWriteVersion = %d, want %d — flipping it is a lock-format change",
			DefaultWriteVersion, FormatVersionV4)
	}
	if MinSupportedVersion != FormatVersionV2 {
		t.Errorf("MinSupportedVersion = %d, want %d", MinSupportedVersion, FormatVersionV2)
	}
	// The write default must be inside the read window, or a CLI would write
	// files it cannot read back.
	if DefaultWriteVersion < MinSupportedVersion || DefaultWriteVersion > MaxSupportedVersion {
		t.Errorf("DefaultWriteVersion %d is outside the read window [%d, %d]",
			DefaultWriteVersion, MinSupportedVersion, MaxSupportedVersion)
	}
}

// TestMarshalLockFileIsCanonical pins the determinism B0e's digests depend on:
// the same content marshals to the same bytes regardless of how the maps were
// populated, and repeated marshaling never varies with map iteration order.
func TestMarshalLockFileIsCanonical(t *testing.T) {
	forward := NewLockFile()
	forward.Version = FormatVersionV2
	for _, name := range []string{"@putnami/ci", "@putnami/go", "@putnami/typescript"} {
		forward.SetExtension(name, LockEntry{
			Version:      "1.0.0",
			TaskContract: 3,
			Integrities:  map[string]string{"darwin/arm64": "mac", "linux/amd64": "linux"},
		})
	}
	reverse := NewLockFile()
	reverse.Version = FormatVersionV2
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
	lf := v2Fixture()
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
	lf := v2Fixture()
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

// TestRewritingAnHTMLEscapedLockConvergesInOneWrite is the migration half of
// the dirty-tree reproduction: a lock committed by the OLD (HTML-escaping)
// writer has \u0026 on disk. Reading it and writing it back must both drop the
// escape (so the working tree goes clean on the very next install, not after
// N more of them) and reach a fixed point (so that first rewrite is the last
// one this file ever needs for that reason).
func TestRewritingAnHTMLEscapedLockConvergesInOneWrite(t *testing.T) {
	const escaped = `{
  "version": 2,
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
		t.Fatalf("rewrite of a legacy escaped lock kept the \\u0026 escape:\n%s", rewritten)
	}
	if !strings.Contains(string(rewritten), downloadURLWithAmpersand) {
		t.Fatalf("rewrite of a legacy escaped lock does not contain the plain URL:\n%s", rewritten)
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

// TestMarshalLockFileV2OmitsLegacyExtensionScalarIntegrity pins the
// cross-platform canonical form. A v2 reader must still accept an older scalar
// so an already-committed lock remains usable, but writing a platform-mapped
// entry drops its redundant scalar and never mutates the parsed value in
// memory. A scalar-only legacy entry retains its sole verification datum. This
// is what makes a complete lock authored on one OS byte-stable on another.
func TestMarshalLockFileV2OmitsLegacyExtensionScalarIntegrity(t *testing.T) {
	const legacy = `{
  "version": 2,
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
		t.Fatalf("ParseLockFile(legacy v2): %v", err)
	}
	before, _ := lf.GetExtension("@putnami/typescript")
	if before.Integrity != "last-writer-platform" {
		t.Fatalf("legacy scalar was not retained on read: %+v", before)
	}

	data := mustMarshal(t, lf)
	canonical, err := ParseLockFile(data)
	if err != nil {
		t.Fatalf("ParseLockFile(canonical v2): %v", err)
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

// TestWriteLockFileV2IsAtomic pins the migration's crash behavior: the bytes
// arrive by rename, so no reader ever sees a half-written lock and no temp file
// is left behind.
func TestWriteLockFileV2IsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFilename), []byte(v1Golden), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	lf := v2Fixture()
	if err := WriteLockFileV2(dir, lf); err != nil {
		t.Fatalf("WriteLockFileV2: %v", err)
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
		t.Errorf("lock file mode = %v, want %v (the mode the v1 writer uses)", perm, wantPerm)
	}
}

// v2Fixture is the canonical v2 form of v1Golden: identical pins, plus the
// contract records the migration adds.
func v2Fixture() *LockFile {
	lf := &LockFile{
		Version:    FormatVersionV2,
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
