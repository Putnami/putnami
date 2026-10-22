package lockfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// v5GoEntry is a valid version 5 toolchain pin: two platforms, each with an
// archive and the digest that binds it, and no source.
func v5GoEntry() LockEntry {
	return LockEntry{
		Version: "1.26.9",
		Archives: map[string]LockArchive{
			"darwin/arm64": {URL: "https://go.dev/dl/go1.26.9.darwin-arm64.tar.gz", Format: "tar.gz", Executable: "go/bin/go"},
			"linux/amd64":  {URL: "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz", Format: "tar.gz", Executable: "go/bin/go"},
		},
		Integrities: map[string]string{
			"darwin/arm64": strings.Repeat("5", 64),
			"linux/amd64":  strings.Repeat("6", 64),
		},
	}
}

// v5Lock is a version 5 lock with a CLI pin, two toolchain pins, one of them
// under an identity no runtime name spells, and an extension pin.
func v5Lock() *LockFile {
	lf := NewLockFile()
	lf.SetCLI(LockEntry{
		Version:     "1.4.2",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)},
		Source:      "https://put.putnami.dev/cli/download?channel=1.4.2&os=linux",
	})
	lf.SetToolchain("go", v5GoEntry())
	compiler := v5GoEntry()
	compiler.Version = "3.0.0"
	compiler.Archives = map[string]LockArchive{
		"darwin/arm64": {URL: "https://example.com/compiler-3.0.0-darwin-arm64.zip", Format: "zip", Executable: "compiler/bin/compiler"},
		"linux/amd64":  {URL: "https://example.com/compiler-3.0.0-linux-amd64.zip", Format: "zip", Executable: "compiler/bin/compiler"},
	}
	lf.SetToolchain("compiler", compiler)
	lf.SetExtension("@putnami/go", LockEntry{
		Version:     "2.0.0",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("b", 64)},
		Source:      "https://put.putnami.dev/extensions/go?channel=2.0.0",
	})
	return lf
}

// The read window is exactly 2 through 5: a newer file names the upgrade.
func TestLockFileReadWindowAcceptsV2ThroughV5(t *testing.T) {
	for _, version := range []int{FormatVersionV2, FormatVersionV3, FormatVersionV4, FormatVersionV5} {
		data := []byte(fmt.Sprintf(`{"version":%d,"extensions":{},"templates":{}}`, version))
		if _, err := ParseLockFile(data); err != nil {
			t.Errorf("ParseLockFile(v%d): %v", version, err)
		}
	}
	_, err := ParseLockFile([]byte(`{"version":6,"extensions":{},"templates":{}}`))
	var unsupported *UnsupportedVersionError
	if !errors.As(err, &unsupported) || unsupported.Max != FormatVersionV5 {
		t.Fatalf("ParseLockFile(v6) error = %v, want max v5", err)
	}
}

// A version 5 lock round-trips byte for byte, its toolchain entries keep their
// archives and carry no source, and its CLI and extension entries keep theirs.
func TestLockFileV5RoundTrip(t *testing.T) {
	lf := v5Lock()
	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLockFile(data)
	if err != nil {
		t.Fatalf("ParseLockFile(written v5): %v\n%s", err, data)
	}
	if got.Version != FormatVersionV5 {
		t.Fatalf("Version = %d, want %d", got.Version, FormatVersionV5)
	}
	if !reflect.DeepEqual(got.Toolchains, lf.Toolchains) {
		t.Fatalf("toolchains = %+v, want %+v", got.Toolchains, lf.Toolchains)
	}
	if cli, _ := got.GetCLI(); cli.Source != lf.CLI.Source || cli.Archives != nil {
		t.Errorf("cli entry = %+v, want its source and no archives", cli)
	}
	if extension, _ := got.GetExtension("@putnami/go"); extension.Source == "" || extension.Archives != nil {
		t.Errorf("extension entry = %+v, want its source and no archives", extension)
	}
	again, err := MarshalLockFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatalf("v5 rewrite changed bytes:\n%s\n%s", data, again)
	}
	if strings.Contains(string(data), `\u0026`) || !strings.Contains(string(data), "channel=1.4.2&os=linux") {
		t.Fatalf("v5 write escaped a URL:\n%s", data)
	}
}

// The writer projects a version 5 toolchain entry onto its vocabulary without
// touching the caller's entry: a source the entry carries in memory is not
// written.
func TestLockFileV5ProjectionDropsAToolchainSourceWithoutMutation(t *testing.T) {
	lf := v5Lock()
	withSource := v5GoEntry()
	withSource.Source = "https://go.dev/dl/"
	lf.SetToolchain("go", withSource)

	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "https://go.dev/dl/\"") {
		t.Fatalf("v5 write recorded a toolchain source:\n%s", data)
	}
	if got, _ := lf.GetToolchain("go"); got.Source != "https://go.dev/dl/" {
		t.Fatal("v5 projection mutated the caller's toolchain entry")
	}
}

// lockedGoSHA256 is a Go copy of putnamiw's locked_go_sha256 awk program: it
// enters the go entry of the toolchains section and keeps the last line that
// starts with eight spaces and `"<platform>": ` as that platform's digest,
// taking the line's second whitespace-separated field without its quotes and
// comma.
func lockedGoSHA256(lock, platform string) (version, digest string) {
	field2 := func(line string) string {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return ""
		}
		return strings.NewReplacer(`"`, "", ",", "").Replace(fields[1])
	}
	inToolchains, inGo := false, false
	for _, line := range strings.Split(lock, "\n") {
		if strings.HasPrefix(line, `  "toolchains": {`) {
			inToolchains = true
			continue
		}
		if inToolchains && strings.HasPrefix(line, "  }") {
			inToolchains = false
		}
		if inToolchains && strings.HasPrefix(line, `    "go": {`) {
			inGo = true
			continue
		}
		if inGo && strings.HasPrefix(line, "    }") {
			inGo = false
		}
		if inGo && strings.HasPrefix(line, `      "version": `) {
			version = field2(line)
		}
		if inGo && strings.HasPrefix(line, `        "`+platform+`": `) {
			digest = field2(line)
		}
	}
	return version, digest
}

// putnamiw reads the committed lock without a JSON parser and keeps the last
// eight-space platform line of the go entry. Archives write the same platform
// keys at that indentation, so the CLI writes them before integrities, and
// putnamiw reads each platform's digest from a lock this writer produced.
func TestLockFileV5WritesArchivesBeforeIntegritiesForPutnamiw(t *testing.T) {
	data, err := MarshalLockFile(v5Lock())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, `    "go": {`)
	if start < 0 {
		t.Fatalf("lock writes no go entry:\n%s", text)
	}
	goEntry := text[start:]
	if archives, integrities := strings.Index(goEntry, `"archives"`), strings.Index(goEntry, `"integrities"`); archives < 0 || integrities < archives {
		t.Fatalf("go entry writes integrities before archives:\n%s", goEntry)
	}
	for platform, want := range v5GoEntry().Integrities {
		version, digest := lockedGoSHA256(text, platform)
		if version != "1.26.9" || digest != want {
			t.Errorf("putnamiw reads go %q digest %q for %s, want 1.26.9 and %q", version, digest, platform, want)
		}
	}
}

// Below version 5 no entry carries archives: a read refuses the file and names
// each offending member, and the writer drops them without touching the
// caller's entries.
func TestLockFileBelowV5RefusesArchives(t *testing.T) {
	for _, test := range []struct {
		name  string
		lock  string
		field string
	}{
		{"a v4 toolchain", `{"version":4,"toolchains":{"go":{"version":"1.26.9","archives":{"linux/amd64":{"url":"https://go.dev/dl/go.tar.gz","format":"tar.gz","executable":"go/bin/go"}},"integrities":{"linux/amd64":"x"},"source":"https://go.dev/dl/"}},"extensions":{},"templates":{}}`, "toolchains.go.archives"},
		{"a v3 extension", `{"version":3,"extensions":{"@putnami/go":{"version":"2.0.0","archives":{}}},"templates":{}}`, "extensions.@putnami/go.archives"},
		{"a v2 cli", `{"version":2,"cli":{"version":"1.0.0","archives":{}},"extensions":{},"templates":{}}`, "cli.archives"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseLockFile([]byte(test.lock))
			var vocabulary *VocabularyError
			if !errors.As(err, &vocabulary) {
				t.Fatalf("ParseLockFile = %v, want *VocabularyError", err)
			}
			if len(vocabulary.Diagnostics) != 1 || vocabulary.Diagnostics[0].Field != test.field || vocabulary.Diagnostics[0].Code != "field-version" {
				t.Fatalf("diagnostics = %v, want one field-version diagnostic on %s", vocabulary.Diagnostics, test.field)
			}
			if !strings.Contains(err.Error(), "archives requires lock format version 5") {
				t.Errorf("refusal %q does not name the version archives need", err)
			}
		})
	}

	lf := NewLockFile()
	lf.Version = FormatVersionV4
	lf.SetToolchain("go", LockEntry{
		Version:     "1.26.9",
		Archives:    v5GoEntry().Archives,
		Integrities: map[string]string{"linux/amd64": "go"},
		Source:      "https://go.dev/dl/",
	})
	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "archives") {
		t.Fatalf("v4 projection emitted v5 vocabulary:\n%s", data)
	}
	if got, _ := lf.GetToolchain("go"); got.Archives == nil || got.Source == "" {
		t.Fatal("v4 projection mutated the caller's toolchain entry")
	}
	if _, err := ParseLockFile(data); err != nil {
		t.Fatalf("the v4 projection does not read back: %v", err)
	}
}

// A version 5 toolchain entry follows the protocol's version 5 rules, so a
// source, a missing archive, or an archive outside a toolchain entry refuses
// the file.
func TestLockFileV5RefusesEntriesThatBreakItsRules(t *testing.T) {
	for _, test := range []struct {
		name  string
		lock  string
		code  string
		field string
	}{
		{"a toolchain source", `{"version":5,"toolchains":{"go":{"version":"1.26.9","archives":{"linux/amd64":{"url":"https://go.dev/dl/go.tar.gz","format":"tar.gz","executable":"go/bin/go"}},"integrities":{"linux/amd64":"` + strings.Repeat("6", 64) + `"},"source":"https://go.dev/dl/"}},"extensions":{},"templates":{}}`, "field-version", "toolchains.go.source"},
		{"a toolchain without archives", `{"version":5,"toolchains":{"go":{"version":"1.26.9","integrities":{"linux/amd64":"` + strings.Repeat("6", 64) + `"}}},"extensions":{},"templates":{}}`, "required-field", "toolchains.go.archives"},
		{"an extension with archives", `{"version":5,"toolchains":{},"extensions":{"@putnami/go":{"version":"2.0.0","archives":{}}},"templates":{}}`, "unknown-field", "extensions.@putnami/go.archives"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseLockFile([]byte(test.lock))
			var vocabulary *VocabularyError
			if !errors.As(err, &vocabulary) {
				t.Fatalf("ParseLockFile = %v, want *VocabularyError", err)
			}
			if vocabulary.Version != FormatVersionV5 || len(vocabulary.Diagnostics) != 1 ||
				vocabulary.Diagnostics[0].Code != test.code || vocabulary.Diagnostics[0].Field != test.field {
				t.Fatalf("refusal = %+v, want one %s diagnostic on %s", vocabulary, test.code, test.field)
			}
		})
	}
}

// The CLI never writes a version 5 lock its next read refuses: a toolchain
// entry without archives fails the write and leaves the file on disk as it was.
func TestWriteLockFileRefusesAV5ToolchainWithoutArchives(t *testing.T) {
	dir := t.TempDir()
	lf := NewLockFile()
	lf.SetToolchain("go", LockEntry{
		Version:     "1.26.9",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("6", 64)},
		Source:      "https://go.dev/dl/",
	})
	err := WriteLockFile(dir, lf)
	var vocabulary *VocabularyError
	if !errors.As(err, &vocabulary) {
		t.Fatalf("WriteLockFile = %v, want *VocabularyError", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, LockFilename)); !os.IsNotExist(statErr) {
		t.Fatalf("a refused write left a lock file: %v", statErr)
	}
}

// Migration never invents archives: a lock below version 5 that pins any
// toolchain is refused with every identity it pins, and the remedy is
// `putnami install`.
func TestMigrateLockFileToCurrentRefusesToolchainsWithoutArchives(t *testing.T) {
	for _, version := range []int{FormatVersionV3, FormatVersionV4} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			legacy, err := ParseLockFile([]byte(fmt.Sprintf(`{
  "version": %d,
  "toolchains": {
    "go": {"version":"1.26.1","integrities":{"linux/amd64":"go"},"source":"https://go.dev/dl/"},
    "bun": {"version":"1.3.0","integrities":{"linux/amd64":"bun"},"source":"https://github.com/oven-sh/bun/releases"}
  },
  "extensions": {},
  "templates": {}
}`, version)))
			if err != nil {
				t.Fatal(err)
			}
			_, err = MigrateLockFileToCurrent(legacy)
			var needsResolution *ToolchainsNeedResolutionError
			if !errors.As(err, &needsResolution) {
				t.Fatalf("MigrateLockFileToCurrent = %v, want *ToolchainsNeedResolutionError", err)
			}
			if needsResolution.Found != version || !reflect.DeepEqual(needsResolution.Identities, []string{"bun", "go"}) {
				t.Fatalf("refusal = %+v, want version %d and identities [bun go]", needsResolution, version)
			}
			for _, fragment := range []string{"bun, go", "only `putnami install` can pin them at lock format version 5", "each extension supplies the archives"} {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("refusal %q does not say %q", err, fragment)
				}
			}
			if legacy.Version != version || len(legacy.Toolchains) != 2 {
				t.Fatalf("a refused migration mutated the source lock: %+v", legacy)
			}
		})
	}
}

// A lock below version 5 that pins no toolchain migrates, and a version 5
// lock migrates to a deep copy of itself.
func TestMigrateLockFileToCurrentProjectsOntoV5(t *testing.T) {
	legacy, err := ParseLockFile([]byte(`{
  "version": 4,
  "toolchains": {},
  "extensions": {"@putnami/go":{"version":"2.0.0","integrities":{"linux/amd64":"ext"}}},
  "templates": {},
  "agentArtifacts": {}
}`))
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrateLockFileToCurrent(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != FormatVersionV5 || migrated.Toolchains == nil || migrated.AgentArtifacts == nil {
		t.Fatalf("migration result = %+v, want v5 with explicit empty dimensions", migrated)
	}
	if legacy.Version != FormatVersionV4 {
		t.Fatalf("migration mutated the source lock: %+v", legacy)
	}

	current := v5Lock()
	copied, err := MigrateLockFileToCurrent(current)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(copied.Toolchains, current.Toolchains) {
		t.Fatalf("v5 migration changed toolchains: %+v", copied.Toolchains)
	}
	goEntry := copied.Toolchains["go"]
	goEntry.Archives["linux/amd64"] = LockArchive{URL: "https://changed.example/go.tar.gz"}
	goEntry.Integrities["linux/amd64"] = "changed"
	if original := current.Toolchains["go"]; original.Archives["linux/amd64"].URL != "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz" ||
		original.Integrities["linux/amd64"] != strings.Repeat("6", 64) {
		t.Fatalf("v5 migration aliases the source toolchain maps: %+v", original)
	}
}

// Clone copies archive maps, so editing a clone never reaches the original.
func TestCloneCopiesArchives(t *testing.T) {
	original := v5Lock()
	clone := original.Clone()
	clone.Toolchains["go"].Archives["linux/amd64"] = LockArchive{URL: "https://changed.example/go.tar.gz"}
	if original.Toolchains["go"].Archives["linux/amd64"].URL != "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz" {
		t.Fatal("Clone aliases a toolchain's archive map")
	}
}

// The conversions between a lock entry and the protocol's toolchain entry are
// lossless for every toolchain member and share no map in either direction.
func TestToolchainEntryConversionsAreLosslessAndDeep(t *testing.T) {
	pin := wsproto.LockToolchainEntry{
		Version: "1.26.9",
		Archives: map[string]wsproto.LockToolchainArchive{
			"linux/amd64": {URL: "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz", Format: wsproto.LockArchiveFormatTarGz, Executable: "go/bin/go"},
		},
		Integrities: map[string]string{"linux/amd64": strings.Repeat("6", 64)},
	}
	entry := ToolchainEntryFromProtocol(pin)
	want := LockEntry{
		Version: "1.26.9",
		Archives: map[string]LockArchive{
			"linux/amd64": {URL: "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz", Format: "tar.gz", Executable: "go/bin/go"},
		},
		Integrities: map[string]string{"linux/amd64": strings.Repeat("6", 64)},
	}
	if !reflect.DeepEqual(entry, want) {
		t.Fatalf("ToolchainEntryFromProtocol = %+v, want %+v", entry, want)
	}
	if back := entry.ProtocolToolchainEntry(); !reflect.DeepEqual(back, pin) {
		t.Fatalf("ProtocolToolchainEntry round trip = %+v, want %+v", back, pin)
	}

	entry.Archives["linux/amd64"] = LockArchive{URL: "https://changed.example/go.tar.gz"}
	entry.Integrities["linux/amd64"] = "changed"
	if pin.Archives["linux/amd64"].URL != "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz" || pin.Integrities["linux/amd64"] != strings.Repeat("6", 64) {
		t.Fatal("ToolchainEntryFromProtocol aliases the protocol entry's maps")
	}
	source := ToolchainEntryFromProtocol(pin)
	converted := source.ProtocolToolchainEntry()
	converted.Archives["linux/amd64"] = wsproto.LockToolchainArchive{URL: "https://changed.example/go.tar.gz"}
	converted.Integrities["linux/amd64"] = "changed"
	if source.Archives["linux/amd64"].URL != "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz" || source.Integrities["linux/amd64"] != strings.Repeat("6", 64) {
		t.Fatal("ProtocolToolchainEntry aliases the lock entry's maps")
	}

	// A legacy toolchain entry keeps its source through the conversion, and the
	// members a toolchain entry does not define do not carry over.
	legacy := LockEntry{Version: "1.25.7", Integrity: "scalar", ManifestHash: "manifest", Source: "https://go.dev/dl/", TaskContract: 3}
	if got := legacy.ProtocolToolchainEntry(); !reflect.DeepEqual(got, wsproto.LockToolchainEntry{Version: "1.25.7", Source: "https://go.dev/dl/"}) {
		t.Fatalf("ProtocolToolchainEntry(legacy) = %+v", got)
	}
	if got := ToolchainEntryFromProtocol(wsproto.LockToolchainEntry{}); got.Archives != nil || got.Integrities != nil {
		t.Fatalf("ToolchainEntryFromProtocol(empty) = %+v, want nil maps", got)
	}
}
