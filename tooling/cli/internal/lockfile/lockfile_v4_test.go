package lockfile

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func TestLockFileV4RoundTripsTheCLIProtocolAndToolchains(t *testing.T) {
	lf := NewLockFile()
	lf.Version = FormatVersionV4
	lf.SetCLI(LockEntry{
		Version:         "1.4.2",
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Integrities:     map[string]string{"linux/amd64": strings.Repeat("a", 64)},
	})
	lf.SetToolchain("go", LockEntry{
		Version:     "1.26.1",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("b", 64)},
		Source:      "https://go.dev/dl/",
	})

	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLockFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != FormatVersionV4 {
		t.Fatalf("Version = %d, want %d", got.Version, FormatVersionV4)
	}
	cli, _ := got.GetCLI()
	if cli.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("cli.protocolVersion = %d, want %d", cli.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	goRuntime, ok := got.GetToolchain("go")
	if !ok || goRuntime.Version != "1.26.1" || goRuntime.Source == "" {
		t.Errorf("go toolchain = %+v, %v", goRuntime, ok)
	}
}

func TestLockFileV4AgentArtifactRoundTrip(t *testing.T) {
	lf := NewLockFile()
	lf.Version = FormatVersionV4
	lf.AgentArtifacts = map[string]AgentArtifactLockEntry{"@putnami/agent-workflows": {
		Version:      "1.0.0",
		Integrity:    strings.Repeat("1", 64),
		ManifestHash: strings.Repeat("2", 64),
		Source:       "https://put.putnami.dev/agents/download?channel=1.0.0",
	}}

	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLockFile(data)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := got.AgentArtifacts["@putnami/agent-workflows"]
	if !ok || entry.Version != "1.0.0" || entry.Integrity != strings.Repeat("1", 64) || entry.ManifestHash != strings.Repeat("2", 64) {
		t.Fatalf("agent artifact pin did not round-trip: %+v (ok=%v)", entry, ok)
	}
	if got.Version != FormatVersionV4 {
		t.Fatalf("Version = %d, want %d", got.Version, FormatVersionV4)
	}
}

func TestMigrateLockFileToCurrentIsExplicitAndDeep(t *testing.T) {
	legacy, err := ParseLockFile([]byte(`{
  "version": 4,
  "cli": {"version":"1.4.2","integrities":{"linux/amd64":"cli"},"protocolVersion":2},
  "toolchains": {},
  "extensions": {"@putnami/go":{"version":"2.0.0","integrities":{"linux/amd64":"ext"}}},
  "templates": {}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Version != FormatVersionV4 {
		t.Fatalf("ordinary read promoted v4: %+v", legacy)
	}

	migrated, err := MigrateLockFileToCurrent(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != FormatVersionV5 || migrated.AgentArtifacts == nil || migrated.Toolchains == nil {
		t.Fatalf("migration result = %+v, want v5 with explicit empty dimensions", migrated)
	}
	if legacy.Version != FormatVersionV4 {
		t.Fatalf("migration mutated source lock: %+v", legacy)
	}

	migrated.CLI.Integrities["linux/amd64"] = "changed"
	extension := migrated.Extensions["@putnami/go"]
	extension.Integrities["linux/amd64"] = "changed"
	if legacy.CLI.Integrities["linux/amd64"] != "cli" ||
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
		t.Fatalf("v5 serialization is non-deterministic:\n%s\n%s", first, second)
	}
}

func TestMigrateLockFileToCurrentRefusesALockBelowTheFloor(t *testing.T) {
	_, err := MigrateLockFileToCurrent(&LockFile{Version: MinSupportedVersion - 1})
	var outdated *OutdatedVersionError
	if !errors.As(err, &outdated) {
		t.Fatalf("MigrateLockFileToCurrent(below the floor) error = %v, want OutdatedVersionError", err)
	}
}
