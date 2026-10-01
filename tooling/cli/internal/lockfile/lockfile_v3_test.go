package lockfile

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func TestLockFileV3RoundTrip(t *testing.T) {
	lf := NewLockFile()
	lf.Version = FormatVersionV3
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
	if got.Version != FormatVersionV3 {
		t.Fatalf("Version = %d, want %d", got.Version, FormatVersionV3)
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

func TestLockFileV2ProjectionDropsV3VocabularyWithoutMutation(t *testing.T) {
	lf := NewLockFile()
	lf.Version = FormatVersionV2
	lf.SetCLI(LockEntry{Version: "1.0.0", ProtocolVersion: protocolcli.ResultProtocolVersion})
	lf.SetToolchain("go", LockEntry{Version: "1.26.1"})

	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "toolchains") || strings.Contains(string(data), "protocolVersion") {
		t.Fatalf("v2 projection emitted v3 vocabulary:\n%s", data)
	}
	if cli, _ := lf.GetCLI(); cli.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Fatal("v2 projection mutated the caller's CLI entry")
	}
	if _, ok := lf.GetToolchain("go"); !ok {
		t.Fatal("v2 projection mutated the caller's toolchain map")
	}
}

func TestLockFileReadWindowAcceptsV2ThroughV4(t *testing.T) {
	for _, version := range []int{FormatVersionV2, FormatVersionV3, FormatVersionV4} {
		data := []byte(fmt.Sprintf(`{"version":%d,"extensions":{},"templates":{}}`, version))
		if _, err := ParseLockFile(data); err != nil {
			t.Errorf("ParseLockFile(v%d): %v", version, err)
		}
	}
	_, err := ParseLockFile([]byte(`{"version":5,"extensions":{},"templates":{}}`))
	var unsupported *UnsupportedVersionError
	if !errors.As(err, &unsupported) || unsupported.Max != FormatVersionV4 {
		t.Fatalf("ParseLockFile(v5) error = %v, want max v4", err)
	}
}
