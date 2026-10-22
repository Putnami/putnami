package workspace

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// v5GoEntryJSON is a valid version 5 go entry for one platform, with extra
// spliced in before the closing brace.
func v5GoEntryJSON(extra string) string {
	return `{"version":"1.26.9","archives":{"darwin/arm64":{"url":"https://go.dev/dl/go1.26.9.darwin-arm64.tar.gz","format":"tar.gz","executable":"go/bin/go"}},"integrities":{"darwin/arm64":"` +
		strings.Repeat("5", 64) + `"}` + extra + `}`
}

func TestLockVersionCompatibilityMatrix(t *testing.T) {
	tests := []struct {
		name string
		data string
		code string
	}{
		{"v2 remains readable", `{"version":2,"extensions":{},"templates":{}}`, ""},
		{"v3 remains readable", `{"version":3,"toolchains":{},"extensions":{},"templates":{}}`, ""},
		{"v3 legacy Node pin remains readable", `{"version":3,"toolchains":{"node":{"version":"22.8.0","integrities":{"linux/amd64":"digest"},"source":"https://nodejs.org/dist/v22.8.0/"}},"extensions":{},"templates":{}}`, ""},
		{"v3 cannot claim v4 field", `{"version":3,"extensions":{},"templates":{},"agentArtifacts":{}}`, "field-version"},
		{"v4 section is optional when empty", `{"version":4,"extensions":{},"templates":{}}`, ""},
		{"v4 remains readable", `{"version":4,"extensions":{},"templates":{},"agentArtifacts":{}}`, ""},
		{"v4 uv pin is accepted", `{"version":4,"toolchains":{"uv":{"version":"0.11.32","integrities":{"darwin/arm64":"ed336d0ba49db8ef89b2b41fffa372ce63bd032f22a56f001c265891aec32829"},"source":"https://github.com/astral-sh/uv/releases/download/0.11.32/"}},"extensions":{},"templates":{},"agentArtifacts":{}}`, ""},
		{"v4 unknown runtime is rejected", `{"version":4,"toolchains":{"deno":{"version":"2.0.0","integrities":{"linux/amd64":"digest"},"source":"https://example.test/"}},"extensions":{},"templates":{},"agentArtifacts":{}}`, "unsupported-toolchain"},
		{"v4 legacy Node pin remains readable", `{"version":4,"toolchains":{"node":{"version":"22.8.0","integrities":{"linux/amd64":"digest"},"source":"https://nodejs.org/dist/v22.8.0/"}},"extensions":{},"templates":{},"agentArtifacts":{}}`, ""},
		{"v4 cannot claim the v5 archives", `{"version":4,"toolchains":{"go":` + v5GoEntryJSON(`,"source":"https://go.dev/dl/"`) + `},"extensions":{},"templates":{}}`, "field-version"},
		{"v5 current", `{"version":5,"extensions":{},"templates":{},"agentArtifacts":{}}`, ""},
		{"v5 go pin with archives is accepted", `{"version":5,"toolchains":{"go":` + v5GoEntryJSON("") + `},"extensions":{},"templates":{}}`, ""},
		{"v5 pins any lock identity", `{"version":5,"toolchains":{"golangci-lint":` + v5GoEntryJSON("") + `},"extensions":{},"templates":{}}`, ""},
		{"v5 refuses an invalid identity", `{"version":5,"toolchains":{"1go":` + v5GoEntryJSON("") + `},"extensions":{},"templates":{}}`, "invalid-value"},
		{"v5 entry cannot carry the v3/v4 source", `{"version":5,"toolchains":{"go":` + v5GoEntryJSON(`,"source":"https://go.dev/dl/"`) + `},"extensions":{},"templates":{}}`, "field-version"},
		{"v5 entry requires archives", `{"version":5,"toolchains":{"go":{"version":"1.26.9","integrities":{"darwin/arm64":"` + strings.Repeat("5", 64) + `"}}},"extensions":{},"templates":{}}`, "required-field"},
		{"v6 is not readable", `{"version":6,"extensions":{},"templates":{}}`, "invalid-lock-version"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lock, diags := ParseAndValidateLock([]byte(tc.data))
			if tc.code == "" {
				if diag.HasErrors(diags) || lock == nil {
					t.Fatalf("valid lock rejected: lock=%v diagnostics=%v", lock, diags)
				}
				return
			}
			if !hasDiagnosticCode(diags, tc.code) {
				t.Fatalf("diagnostics = %v, want code %q", diags, tc.code)
			}
		})
	}
}

func TestMarshalLockProjectsVersionVocabularyWithoutMutation(t *testing.T) {
	protocolVersion := 2
	lock := &Lock{
		Version:        2,
		CLI:            &LockCLIEntry{Version: "1", ProtocolVersion: &protocolVersion},
		Toolchains:     map[string]LockToolchainEntry{"go": {Version: "1.26.1"}},
		Extensions:     map[string]LockEntry{},
		Templates:      map[string]LockEntry{},
		AgentArtifacts: map[string]LockAgentArtifactEntry{"workflows": {Version: "1"}},
	}
	data, err := MarshalLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "toolchains") || strings.Contains(string(data), "protocolVersion") || strings.Contains(string(data), "agentArtifacts") {
		t.Fatalf("v2 serialization emitted newer vocabulary:\n%s", data)
	}
	if lock.CLI.ProtocolVersion == nil || lock.Toolchains == nil || lock.AgentArtifacts == nil {
		t.Fatalf("version projection mutated its input: %+v", lock)
	}
}

func TestParseLockRejectsTrailingDocument(t *testing.T) {
	_, diags := ParseLock([]byte(`{"version":4,"extensions":{},"templates":{}} {}`))
	if !hasDiagnosticCode(diags, "parse-error") {
		t.Fatalf("trailing lock document was accepted: %v", diags)
	}
}

func TestMigrateLockToCurrentIsExplicitDeepAndDeterministic(t *testing.T) {
	protocolVersion := 2
	legacy := &Lock{
		Version: LockVersion - 1,
		CLI: &LockCLIEntry{
			Version:         "1.4.2",
			ProtocolVersion: &protocolVersion,
			Integrities:     map[string]string{"linux/amd64": "cli-hash"},
		},
		Extensions: map[string]LockEntry{
			"z": {Version: "2", Integrities: map[string]string{"linux/amd64": "z"}},
			"a": {Version: "1"},
		},
		Templates: map[string]LockEntry{"template": {Version: "1"}},
	}

	migrated, diags := MigrateLockToCurrent(legacy)
	if diag.HasErrors(diags) {
		t.Fatalf("migration failed: %v", diags)
	}
	if migrated.Version != LockVersion || migrated.AgentArtifacts == nil || migrated.Toolchains == nil {
		t.Fatalf("migrated lock = %+v, want v%d with explicit empty agentArtifacts and toolchains sections", migrated, LockVersion)
	}
	if legacy.Version != LockVersion-1 || legacy.AgentArtifacts != nil || legacy.Toolchains != nil {
		t.Fatalf("migration mutated its input: %+v", legacy)
	}

	// Every nested collection must be detached, not merely the top-level map.
	migrated.Extensions["z"] = LockEntry{Version: "changed"}
	migrated.CLI.Integrities["linux/amd64"] = "changed"
	if migrated.CLI.ProtocolVersion == legacy.CLI.ProtocolVersion {
		t.Fatal("migration result aliases input CLI protocol version")
	}
	*migrated.CLI.ProtocolVersion = 3
	if legacy.Extensions["z"].Version != "2" || legacy.CLI.Integrities["linux/amd64"] != "cli-hash" || *legacy.CLI.ProtocolVersion != 2 {
		t.Fatalf("migration result aliases input collections: legacy=%+v migrated=%+v", legacy, migrated)
	}

	first, err := MarshalLock(migrated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalLock(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("canonical lock serialization is non-deterministic:\n%s\n%s", first, second)
	}
	if !strings.HasSuffix(string(first), "\n") || strings.HasSuffix(string(first), "\n\n") {
		t.Fatalf("canonical lock must carry exactly one trailing newline: %q", first)
	}
}

// A version 3 or 4 toolchain pin records no archives, and only the extension
// that answers for its identity can supply them: the migration refuses with
// one error per entry, in identity order, instead of writing a version 5 lock
// whose entries name no archive.
func TestMigrateLockToCurrentRefusesToolchainPinsWithoutArchives(t *testing.T) {
	for _, version := range []int{3, 4} {
		legacy := &Lock{
			Version: version,
			Toolchains: map[string]LockToolchainEntry{
				"go":  {Version: "1.26.1", Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)}, Source: "https://go.dev/dl/"},
				"bun": {Version: "1.3.14", Integrities: map[string]string{"linux/amd64": strings.Repeat("b", 64)}, Source: "https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/"},
			},
			Extensions: map[string]LockEntry{},
			Templates:  map[string]LockEntry{},
		}
		migrated, diags := MigrateLockToCurrent(legacy)
		if migrated != nil {
			t.Fatalf("v%d: migration wrote a lock whose toolchain pins name no archive: %+v", version, migrated)
		}
		var fields []string
		for _, d := range diag.Errors(diags) {
			if d.Code != "needs-resolution" {
				t.Fatalf("v%d: unexpected diagnostic %v", version, d)
			}
			fields = append(fields, d.Field)
		}
		if want := []string{"toolchains.bun", "toolchains.go"}; !reflect.DeepEqual(fields, want) {
			t.Fatalf("v%d: needs-resolution fields = %v, want %v", version, fields, want)
		}
		if legacy.Version != version || legacy.Toolchains["go"].Source != "https://go.dev/dl/" {
			t.Fatalf("v%d: migration mutated its input: %+v", version, legacy)
		}
	}
}

func TestMigrateLockToCurrentDetachesToolchainArchives(t *testing.T) {
	current := &Lock{
		Version:    LockVersion,
		Toolchains: map[string]LockToolchainEntry{"go": validV5GoEntry()},
		Extensions: map[string]LockEntry{},
		Templates:  map[string]LockEntry{},
	}
	migrated, diags := MigrateLockToCurrent(current)
	if diag.HasErrors(diags) {
		t.Fatalf("migration of a current lock failed: %v", diags)
	}
	migrated.Toolchains["go"].Archives["darwin/arm64"] = LockToolchainArchive{URL: "https://changed.test/"}
	migrated.Toolchains["go"].Integrities["darwin/arm64"] = "changed"
	if current.Toolchains["go"].Archives["darwin/arm64"].URL != "https://go.dev/dl/go1.26.9.darwin-arm64.tar.gz" ||
		current.Toolchains["go"].Integrities["darwin/arm64"] != strings.Repeat("5", 64) {
		t.Fatalf("migration result aliases the input's toolchain maps: %+v", current.Toolchains["go"])
	}
}

func TestValidateLockAgentArtifactIntegrityAndDiagnosticOrder(t *testing.T) {
	lock := &Lock{
		Version:    LockVersion,
		Extensions: map[string]LockEntry{},
		Templates:  map[string]LockEntry{},
		AgentArtifacts: map[string]LockAgentArtifactEntry{
			"z": {Version: "", Integrity: "bad", ManifestHash: "bad"},
			"a": {Version: "", Integrity: "bad", ManifestHash: "bad"},
		},
	}
	canonical := ValidateLock(lock)
	if !diag.HasErrors(canonical) {
		t.Fatal("invalid agent artifact pins were accepted")
	}
	for i := 0; i < 100; i++ {
		if got := ValidateLock(lock); !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d produced non-deterministic diagnostics:\nwant %v\ngot %v", i, canonical, got)
		}
	}
	if !strings.HasPrefix(canonical[0].Field, "agentArtifacts.a.") {
		t.Fatalf("agent artifact diagnostics are not sorted by artifact name: %v", canonical)
	}
}

// A source workspace builds its own CLI, so its entry carries the reserved
// source sentinel and nothing else. The two shapes reject each other's fields:
// a published pin without a version is unreproducible, and a sentinel WITH one
// claims a published artifact the workspace does not have.
func TestValidateLockCLISourceWorkspaceSentinel(t *testing.T) {
	base := func(cli *LockCLIEntry) *Lock {
		return &Lock{
			Version:    LockVersion,
			CLI:        cli,
			Extensions: map[string]LockEntry{},
			Templates:  map[string]LockEntry{},
		}
	}
	protocolVersion := 2

	for _, tc := range []struct {
		name      string
		cli       *LockCLIEntry
		wantCode  string
		wantField string
	}{
		{name: "sentinel alone is valid", cli: &LockCLIEntry{Source: LockCLISourceWorkspace}},
		{name: "published pin is valid", cli: &LockCLIEntry{Version: "1.4.2", Source: "https://put.putnami.dev/cli"}},
		{
			name: "sentinel with a version", cli: &LockCLIEntry{Version: "1.4.2", Source: LockCLISourceWorkspace},
			wantCode: "conflicting-field", wantField: "cli.version",
		},
		{
			name: "sentinel with integrities",
			cli: &LockCLIEntry{
				Source:      LockCLISourceWorkspace,
				Integrities: map[string]string{"linux/amd64": "sha256:aaaa"},
			},
			wantCode: "conflicting-field", wantField: "cli.integrities",
		},
		{
			name:     "sentinel with a protocol version",
			cli:      &LockCLIEntry{Source: LockCLISourceWorkspace, ProtocolVersion: &protocolVersion},
			wantCode: "conflicting-field", wantField: "cli.protocolVersion",
		},
		{
			name: "published pin without a version", cli: &LockCLIEntry{Source: "https://put.putnami.dev/cli"},
			wantCode: "required-field", wantField: "cli.version",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateLock(base(tc.cli))
			if tc.wantCode == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("valid CLI entry rejected: %v", diags)
				}
				return
			}
			for _, d := range diags {
				if d.Code == tc.wantCode && d.Field == tc.wantField {
					return
				}
			}
			t.Fatalf("want %s on %s, got %v", tc.wantCode, tc.wantField, diags)
		})
	}
}

// The sentinel must survive a round trip through the canonical writer unchanged.
// Without `omitempty` on Version the writer would add `"version": ""`, so merely
// re-serializing a committed lock would dirty it — and a lock that a routine
// command rewrites is one nobody can review.
func TestMarshalLockKeepsSourceWorkspaceEntryExact(t *testing.T) {
	lock := &Lock{
		Version:    LockVersion,
		CLI:        &LockCLIEntry{Source: LockCLISourceWorkspace},
		Extensions: map[string]LockEntry{},
		Templates:  map[string]LockEntry{},
	}
	data, err := MarshalLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"version": ""`) {
		t.Fatalf("canonical lock invented an empty CLI version:\n%s", data)
	}
	parsed, diags := ParseAndValidateLock(data)
	if diag.HasErrors(diags) {
		t.Fatalf("round-tripped sentinel rejected: %v", diags)
	}
	if !parsed.CLI.IsWorkspaceSource() {
		t.Fatalf("round trip lost the source sentinel: %+v", parsed.CLI)
	}
}

// validV5GoEntry is a valid version 5 go entry for two platforms. Each call
// returns fresh maps, so a test may edit its copy.
func validV5GoEntry() LockToolchainEntry {
	return LockToolchainEntry{
		Version: "1.26.9",
		Archives: map[string]LockToolchainArchive{
			"darwin/arm64": {URL: "https://go.dev/dl/go1.26.9.darwin-arm64.tar.gz", Format: LockArchiveFormatTarGz, Executable: "go/bin/go"},
			"linux/amd64":  {URL: "https://go.dev/dl/go1.26.9.linux-amd64.tar.gz", Format: LockArchiveFormatTarGz, Executable: "go/bin/go"},
		},
		Integrities: map[string]string{
			"darwin/arm64": strings.Repeat("5", 64),
			"linux/amd64":  strings.Repeat("6", 64),
		},
	}
}

// Each version 5 entry rule, by the diagnostic it raises. ValidateLock and the
// toolchain-resolve answer share validateLockToolchainPin, so this table binds
// both.
func TestValidateLockVersion5ToolchainEntryRules(t *testing.T) {
	editArchive := func(edit func(*LockToolchainArchive)) func(*LockToolchainEntry) {
		return func(e *LockToolchainEntry) {
			archive := e.Archives["darwin/arm64"]
			edit(&archive)
			e.Archives["darwin/arm64"] = archive
		}
	}
	const archive = "toolchains.go.archives.darwin/arm64"
	cases := []struct {
		name  string
		edit  func(*LockToolchainEntry)
		code  string
		field string
	}{
		{"valid", func(*LockToolchainEntry) {}, "", ""},
		{"http url", editArchive(func(a *LockToolchainArchive) { a.URL = "http://mirror.test/go.tar.gz" }), "", ""},
		{"zip format", editArchive(func(a *LockToolchainArchive) { a.Format = LockArchiveFormatZip }), "", ""},
		{"dot-prefixed segments", editArchive(func(a *LockToolchainArchive) { a.Executable = ".tool/..bin/go" }), "", ""},
		{"single segment executable", editArchive(func(a *LockToolchainArchive) { a.Executable = "go" }), "", ""},
		{"missing version", func(e *LockToolchainEntry) { e.Version = " " }, "required-field", "toolchains.go.version"},
		{"v3/v4 source", func(e *LockToolchainEntry) { e.Source = "https://go.dev/dl/" }, "field-version", "toolchains.go.source"},
		{"no integrities", func(e *LockToolchainEntry) { e.Integrities = nil }, "required-field", "toolchains.go.integrities"},
		{"too many platforms", func(e *LockToolchainEntry) {
			for i := 0; i < 32; i++ {
				platform := fmt.Sprintf("os%d/arch", i)
				e.Integrities[platform] = strings.Repeat("7", 64)
				e.Archives[platform] = e.Archives["darwin/arm64"]
			}
		}, "limit-exceeded", "toolchains.go.integrities"},
		{"platform key", func(e *LockToolchainEntry) {
			e.Integrities["Darwin/ARM64"] = strings.Repeat("7", 64)
			e.Archives["Darwin/ARM64"] = e.Archives["darwin/arm64"]
		}, "invalid-value", "toolchains.go.integrities.Darwin/ARM64"},
		{"prefixed integrity", func(e *LockToolchainEntry) { e.Integrities["darwin/arm64"] = "sha256:" + strings.Repeat("5", 64) }, "invalid-integrity", "toolchains.go.integrities.darwin/arm64"},
		{"uppercase integrity", func(e *LockToolchainEntry) { e.Integrities["darwin/arm64"] = strings.Repeat("A", 64) }, "invalid-integrity", "toolchains.go.integrities.darwin/arm64"},
		{"no archives", func(e *LockToolchainEntry) { e.Archives = nil }, "required-field", "toolchains.go.archives"},
		{"archive without integrity", func(e *LockToolchainEntry) {
			e.Archives["windows/amd64"] = LockToolchainArchive{URL: "https://go.dev/dl/go1.26.9.windows-amd64.zip", Format: LockArchiveFormatZip, Executable: "go/bin/go.exe"}
		}, "required-field", "toolchains.go.integrities.windows/amd64"},
		{"integrity without archive", func(e *LockToolchainEntry) { delete(e.Archives, "linux/amd64") }, "required-field", "toolchains.go.archives.linux/amd64"},
		{"missing url", editArchive(func(a *LockToolchainArchive) { a.URL = "" }), "required-field", archive + ".url"},
		{"ftp url", editArchive(func(a *LockToolchainArchive) { a.URL = "ftp://go.dev/go.tar.gz" }), "invalid-value", archive + ".url"},
		{"relative url", editArchive(func(a *LockToolchainArchive) { a.URL = "/dl/go.tar.gz" }), "invalid-value", archive + ".url"},
		{"url without host", editArchive(func(a *LockToolchainArchive) { a.URL = "https:///dl/go.tar.gz" }), "invalid-value", archive + ".url"},
		{"url with user information", editArchive(func(a *LockToolchainArchive) { a.URL = "https://token@go.dev/dl/go.tar.gz" }), "invalid-value", archive + ".url"},
		{"oversized url", editArchive(func(a *LockToolchainArchive) { a.URL = "https://go.dev/" + strings.Repeat("a", 2048) }), "invalid-value", archive + ".url"},
		{"missing format", editArchive(func(a *LockToolchainArchive) { a.Format = "" }), "required-field", archive + ".format"},
		{"unknown format", editArchive(func(a *LockToolchainArchive) { a.Format = "tgz" }), "invalid-value", archive + ".format"},
		{"missing executable", editArchive(func(a *LockToolchainArchive) { a.Executable = "" }), "required-field", archive + ".executable"},
		{"escaping executable", editArchive(func(a *LockToolchainArchive) { a.Executable = "go/../../bin/go" }), "invalid-path", archive + ".executable"},
		{"absolute executable", editArchive(func(a *LockToolchainArchive) { a.Executable = "/usr/local/go/bin/go" }), "invalid-path", archive + ".executable"},
		{"backslash executable", editArchive(func(a *LockToolchainArchive) { a.Executable = `go\bin\go.exe` }), "invalid-path", archive + ".executable"},
		{"drive-letter executable", editArchive(func(a *LockToolchainArchive) { a.Executable = "C:/go/bin/go.exe" }), "invalid-path", archive + ".executable"},
		{"empty segment", editArchive(func(a *LockToolchainArchive) { a.Executable = "go//go" }), "invalid-path", archive + ".executable"},
		{"dot segment", editArchive(func(a *LockToolchainArchive) { a.Executable = "go/./go" }), "invalid-path", archive + ".executable"},
		{"oversized executable", editArchive(func(a *LockToolchainArchive) { a.Executable = strings.Repeat("a", 513) }), "invalid-path", archive + ".executable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := validV5GoEntry()
			tc.edit(&entry)
			diags := ValidateLock(&Lock{
				Version:    5,
				Toolchains: map[string]LockToolchainEntry{"go": entry},
				Extensions: map[string]LockEntry{},
				Templates:  map[string]LockEntry{},
			})
			if tc.code == "" {
				if len(diags) != 0 {
					t.Fatalf("valid entry rejected: %v", diags)
				}
				return
			}
			for _, d := range diags {
				if d.Code == tc.code && d.Field == tc.field {
					return
				}
			}
			t.Fatalf("want %s on %s, got %v", tc.code, tc.field, diags)
		})
	}
}

func TestValidateLockToolchainDiagnosticsAreDeterministic(t *testing.T) {
	broken := func() LockToolchainEntry {
		entry := validV5GoEntry()
		entry.Source = "https://go.dev/dl/"
		entry.Integrities["linux/amd64"] = "bad"
		entry.Archives["windows/amd64"] = LockToolchainArchive{URL: "ftp://x", Format: "pkg", Executable: "../x"}
		return entry
	}
	lock := &Lock{
		Version:    5,
		Toolchains: map[string]LockToolchainEntry{"zig": broken(), "go": broken(), "bun": broken()},
		Extensions: map[string]LockEntry{},
		Templates:  map[string]LockEntry{},
	}
	canonical := ValidateLock(lock)
	for i := 0; i < 100; i++ {
		if got := ValidateLock(lock); !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d produced non-deterministic diagnostics:\nwant %v\ngot %v", i, canonical, got)
		}
	}
	if !strings.HasPrefix(canonical[0].Field, "toolchains.bun.") || !strings.HasPrefix(canonical[len(canonical)-1].Field, "toolchains.zig.") {
		t.Fatalf("toolchain diagnostics are not in identity order: %v", canonical)
	}
}

// MarshalLock writes each toolchain entry in its lock version's vocabulary,
// whatever the in-memory value carries, and never edits the caller's value.
func TestMarshalLockProjectsToolchainVocabularyWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		version int
		keep    string
		drop    string
	}{
		{4, `"source"`, `"archives"`},
		{5, `"archives"`, `"source"`},
	} {
		entry := validV5GoEntry()
		entry.Source = "https://go.dev/dl/"
		lock := &Lock{
			Version:    tc.version,
			Toolchains: map[string]LockToolchainEntry{"go": entry},
			Extensions: map[string]LockEntry{},
			Templates:  map[string]LockEntry{},
		}
		data, err := MarshalLock(lock)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), tc.keep) || strings.Contains(string(data), tc.drop) {
			t.Fatalf("v%d serialization must keep %s and drop %s:\n%s", tc.version, tc.keep, tc.drop, data)
		}
		if lock.Toolchains["go"].Source == "" || lock.Toolchains["go"].Archives == nil {
			t.Fatalf("v%d projection mutated its input: %+v", tc.version, lock.Toolchains["go"])
		}
		if _, diags := ParseAndValidateLock(data); diag.HasErrors(diags) {
			t.Fatalf("v%d projection is not a valid lock: %v", tc.version, diags)
		}
	}
}

// lockedGoSHA256 mirrors putnamiw's locked_go_sha256 awk program: it enters the
// go entry of the toolchains section and keeps the last line that starts with
// eight spaces and `"<platform>": ` as that platform's digest, taking the
// line's second whitespace-separated field without its quotes and comma.
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

// The member order of a toolchain entry is a contract with putnamiw, which
// reads the committed lock without a JSON parser: an archives map writes the
// same eight-space platform lines as the integrities map, and the reader keeps
// the last one. Archives must therefore be written before integrities.
func TestMarshalLockWritesIntegritiesAfterArchives(t *testing.T) {
	bun := validV5GoEntry()
	bun.Version = "1.3.14"
	lock := &Lock{
		Version:    5,
		Toolchains: map[string]LockToolchainEntry{"go": validV5GoEntry(), "bun": bun},
		Extensions: map[string]LockEntry{},
		Templates:  map[string]LockEntry{},
	}
	data, err := MarshalLock(lock)
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
	for platform, want := range validV5GoEntry().Integrities {
		version, digest := lockedGoSHA256(text, platform)
		if version != "1.26.9" || digest != want {
			t.Errorf("putnamiw reads go %q digest %q for %s, want 1.26.9 and %q", version, digest, platform, want)
		}
	}
}
