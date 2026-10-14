package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func TestIsRegistryName(t *testing.T) {
	for name, want := range map[string]bool{
		"@putnami/go":     true,
		"@acme/tool-kit":  true,
		"putnami":         false,
		"@putnami":        false,
		"@/go":            false,
		"@putnami/":       false,
		"@putnami/go/sub": false,
		"/tools/ext":      false,
		"./ext":           false,
		"":                false,
	} {
		if got := IsRegistryName(name); got != want {
			t.Errorf("IsRegistryName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRegistryDependenciesAreSortedRegistryNames(t *testing.T) {
	m := &Manifest{ExtensionDeps: Dependencies{List: map[string]string{
		"@putnami/web": "", "local-tool": "", "@acme/b": "^1.0.0", "@acme/a": "",
	}}}
	got := RegistryDependencies(m)
	want := []string{"@acme/a", "@acme/b", "@putnami/web"}
	if !slices.Equal(got, want) {
		t.Fatalf("RegistryDependencies = %v, want %v", got, want)
	}
	if !NamesRegistryDependency(m) {
		t.Fatal("NamesRegistryDependency = false, want true")
	}
	if NamesRegistryDependency(&Manifest{ExtensionDeps: Dependencies{List: map[string]string{"local-tool": ""}}}) {
		t.Fatal("a non-registry name is not a registry dependency")
	}
}

// The dependency rung is part of the stamp, not of the origin-free load floor.
func TestRequiredContractsSplitTheDependencyRung(t *testing.T) {
	m := &Manifest{
		Commands:      map[string]CommandDefinition{"lint": {}},
		ExtensionDeps: Dependencies{List: map[string]string{"@putnami/dev": ""}},
	}
	if got := RequiredCLIContract(m); got != protocolcli.ExtensionDependenciesContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.ExtensionDependenciesContract)
	}
	if got := RequiredLoadContract(m); got != protocolcli.CurrentContract {
		t.Fatalf("RequiredLoadContract = %d, want %d", got, protocolcli.CurrentContract)
	}
	m.ExtensionDeps = Dependencies{List: map[string]string{"local-tool": ""}}
	if got := RequiredCLIContract(m); got != protocolcli.CurrentContract {
		t.Fatalf("a non-registry dependency requires %d, want %d", got, protocolcli.CurrentContract)
	}
}

func writeDependentManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ManifestFilename)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A dependent stamped below rung 8 still negotiates, and the origin-aware
// check refuses it; a stamp of 8 passes both; a stamp above the latest
// contract is refused as written for a newer putnami.
func TestCheckDependencyContract(t *testing.T) {
	const manifest = `{"cliContract": %d, "extensionDependencies": ["@putnami/dev"], "commands": {"lint": {}}}`

	path := writeDependentManifest(t, fmt.Sprintf(manifest, protocolcli.ReleaseBaselineInputContract))
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("the load floor leaves the dependency rung out: %v", err)
	}
	err = CheckDependencyContract(path, m)
	if err == nil || !strings.Contains(err.Error(), "requires 8 for its extensionDependencies") {
		t.Fatalf("CheckDependencyContract = %v, want the rung-8 refusal", err)
	}

	path = writeDependentManifest(t, fmt.Sprintf(manifest, protocolcli.ExtensionDependenciesContract))
	m, err = LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDependencyContract(path, m); err != nil {
		t.Fatalf("a rung-8 stamp passes: %v", err)
	}

	path = writeDependentManifest(t, fmt.Sprintf(manifest, protocolcli.LatestContract+1))
	if _, err := LoadManifest(path); err == nil || !strings.Contains(err.Error(), "requires a newer putnami") {
		t.Fatalf("LoadManifest = %v, want the newer-putnami refusal", err)
	}
}

// A hook-only dependent, such as a published npm framework package, loads
// unstamped: the negotiation leaves it outside the ladder.
func TestHookOnlyDependentLoadsUnstamped(t *testing.T) {
	path := writeDependentManifest(t, `{"extensionDependencies": ["@putnami/application"], "hooks": {"preBuild": {"kind": "command", "command": "echo"}}, "commands": {}}`)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("an unstamped hook-only dependent must load: %v", err)
	}
	if m.CLIContract != 0 {
		t.Fatalf("cliContract = %d, want 0", m.CLIContract)
	}
}
