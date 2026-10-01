package workspace

import (
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

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
		{"v4 current", `{"version":4,"extensions":{},"templates":{},"agentArtifacts":{}}`, ""},
		{"v4 legacy Node pin remains readable", `{"version":4,"toolchains":{"node":{"version":"22.8.0","integrities":{"linux/amd64":"digest"},"source":"https://nodejs.org/dist/v22.8.0/"}},"extensions":{},"templates":{},"agentArtifacts":{}}`, ""},
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
		Toolchains: map[string]LockToolchainEntry{
			"go": {Version: "1.26.1", Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)}, Source: "https://go.dev/dl/"},
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
	if migrated.Version != LockVersion || migrated.AgentArtifacts == nil {
		t.Fatalf("migrated lock = %+v, want v%d with an explicit empty agentArtifacts section", migrated, LockVersion)
	}
	if legacy.Version != LockVersion-1 || legacy.AgentArtifacts != nil {
		t.Fatalf("migration mutated its input: %+v", legacy)
	}

	// Every nested collection must be detached, not merely the top-level map.
	migrated.Extensions["z"] = LockEntry{Version: "changed"}
	migrated.Toolchains["go"] = LockToolchainEntry{Version: "changed"}
	migrated.CLI.Integrities["linux/amd64"] = "changed"
	if migrated.CLI.ProtocolVersion == legacy.CLI.ProtocolVersion {
		t.Fatal("migration result aliases input CLI protocol version")
	}
	*migrated.CLI.ProtocolVersion = 3
	if legacy.Extensions["z"].Version != "2" || legacy.Toolchains["go"].Version != "1.26.1" || legacy.CLI.Integrities["linux/amd64"] != "cli-hash" || *legacy.CLI.ProtocolVersion != 2 {
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
