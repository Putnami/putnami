package lockfile

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLockFileV4AgentArtifactRoundTrip(t *testing.T) {
	lf := NewLockFile()
	lf.SetAgentArtifact("@putnami/agent-workflows", AgentArtifactLockEntry{
		Version:      "1.0.0",
		Integrity:    strings.Repeat("1", 64),
		ManifestHash: strings.Repeat("2", 64),
		Source:       "https://put.putnami.dev/agents/download?channel=1.0.0",
	})

	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLockFile(data)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := got.GetAgentArtifact("@putnami/agent-workflows")
	if !ok || entry.Version != "1.0.0" || entry.Integrity != strings.Repeat("1", 64) || entry.ManifestHash != strings.Repeat("2", 64) {
		t.Fatalf("agent artifact pin did not round-trip: %+v (ok=%v)", entry, ok)
	}
	if got.Version != FormatVersionV4 {
		t.Fatalf("Version = %d, want %d", got.Version, FormatVersionV4)
	}
}

func TestLockFileV3ProjectionDropsV4VocabularyWithoutMutation(t *testing.T) {
	lf := NewLockFile()
	lf.Version = FormatVersionV3
	lf.SetAgentArtifact("workflows", AgentArtifactLockEntry{
		Version:      "1.0.0",
		Integrity:    strings.Repeat("1", 64),
		ManifestHash: strings.Repeat("2", 64),
	})

	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "agentArtifacts") {
		t.Fatalf("v3 projection emitted v4 vocabulary:\n%s", data)
	}
	if _, ok := lf.GetAgentArtifact("workflows"); !ok {
		t.Fatal("v3 projection mutated the caller's agent artifact map")
	}
}

func TestMigrateLockFileToCurrentIsExplicitAndDeep(t *testing.T) {
	legacy, err := ParseLockFile([]byte(`{
  "version": 3,
  "cli": {"version":"1.4.2","integrities":{"linux/amd64":"cli"},"protocolVersion":2},
  "toolchains": {"go":{"version":"1.26.1","integrities":{"linux/amd64":"go"},"source":"https://go.dev/dl/"}},
  "extensions": {"@putnami/go":{"version":"2.0.0","integrities":{"linux/amd64":"ext"}}},
  "templates": {}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Version != FormatVersionV3 || legacy.AgentArtifacts != nil {
		t.Fatalf("ordinary read promoted v3: %+v", legacy)
	}

	migrated, err := MigrateLockFileToCurrent(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != FormatVersionV4 || migrated.AgentArtifacts == nil {
		t.Fatalf("migration result = %+v, want v4 with explicit empty agentArtifacts", migrated)
	}
	if legacy.Version != FormatVersionV3 || legacy.AgentArtifacts != nil {
		t.Fatalf("migration mutated source lock: %+v", legacy)
	}

	migrated.CLI.Integrities["linux/amd64"] = "changed"
	toolchain := migrated.Toolchains["go"]
	toolchain.Integrities["linux/amd64"] = "changed"
	extension := migrated.Extensions["@putnami/go"]
	extension.Integrities["linux/amd64"] = "changed"
	if legacy.CLI.Integrities["linux/amd64"] != "cli" ||
		legacy.Toolchains["go"].Integrities["linux/amd64"] != "go" ||
		legacy.Extensions["@putnami/go"].Integrities["linux/amd64"] != "ext" {
		t.Fatalf("migration result aliases source collections: legacy=%+v migrated=%+v", legacy, migrated)
	}

	first, err := MarshalLockFile(migrated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalLockFile(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("v4 serialization is non-deterministic:\n%s\n%s", first, second)
	}
}

func TestMigrateLockFileToCurrentDoesNotBypassV1Migration(t *testing.T) {
	_, err := MigrateLockFileToCurrent(&LockFile{Version: FormatVersionV1})
	var outdated *OutdatedVersionError
	if !errors.As(err, &outdated) {
		t.Fatalf("MigrateLockFileToCurrent(v1) error = %v, want OutdatedVersionError", err)
	}
}

func TestReadV3DiscardsSmuggledV4Vocabulary(t *testing.T) {
	lock, err := ParseLockFile([]byte(`{
  "version": 3,
  "extensions": {},
  "templates": {},
  "agentArtifacts": {"workflows":{"version":"1","integrity":"x","manifestHash":"y"}}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if lock.AgentArtifacts != nil {
		t.Fatalf("v3 read exposed v4 vocabulary: %+v", lock.AgentArtifacts)
	}
}
