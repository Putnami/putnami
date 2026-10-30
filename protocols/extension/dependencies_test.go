package extension

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

func TestIsRegistryName(t *testing.T) {
	for name, want := range map[string]bool{
		"@putnami/go":          true,
		"@acme/tool-kit":       true,
		"@acme/tool_kit.v2":    true,
		"@0x/1":                true,
		"@fixture/dependency":  true,
		"putnami":              false,
		"@putnami":             false,
		"@/go":                 false,
		"@putnami/":            false,
		"@putnami/go/sub":      false,
		"@putnami//go":         false,
		"@putnami/go/":         false,
		"@putnami/dev#":        false,
		"@putnami/dev?x=1":     false,
		"@putnami/dev&os=win":  false,
		"@putnami/..":          false,
		"@../..":               false,
		"@putnami/.hidden":     false,
		"@-acme/go":            false,
		"@acme/ go":            false,
		"@acme/go ":            false,
		"@Acme/go":             false,
		"@acme/Go":             false,
		"@acme/gö":             false,
		"@acme/go\x00":         false,
		"@acme/go\n":           false,
		"@acme/go%2e%2e":       false,
		"@acme/go@1.0.0":       false,
		"@putnami/dev:publish": false,
		"/tools/ext":           false,
		"./ext":                false,
		"":                     false,
	} {
		if got := IsRegistryName(name); got != want {
			t.Errorf("IsRegistryName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsDependencyName(t *testing.T) {
	for name, want := range map[string]bool{
		"@putnami/go":   true,
		"local-tool":    true,
		"tool_kit.v2":   true,
		"0x":            true,
		"@putnami/dev#": false,
		"@putnami":      false,
		"Local-Tool":    false,
		".hidden":       false,
		"-tool":         false,
		"a/b":           false,
		"./ext":         false,
		"a b":           false,
		"":              false,
	} {
		if got := IsDependencyName(name); got != want {
			t.Errorf("IsDependencyName(%q) = %v, want %v", name, got, want)
		}
	}
}

// Strict validation refuses a dependency key outside the registry and package
// grammars, at the key that holds it, and the schema states the same grammar
// for the list and the map form.
func TestValidateManifest_DependencyNameFollowsTheGrammar(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "strict-manifests-and-a-fixture-corpus", "each-counter-example-reports-its-exact-codes")
	m := &Manifest{
		Name:     "@test/extension",
		Commands: map[string]CommandDefinition{"lint": {}},
		ExtensionDeps: Dependencies{List: map[string]string{
			"@putnami/dev": "", "local-tool": "^1.0.0", "@putnami/dev#": "", "@acme/ go": "*",
		}},
	}
	var got []string
	for _, d := range ValidateManifest(m) {
		if d.Code == "invalid-extension-dependency" {
			got = append(got, d.Field)
		}
	}
	want := []string{"extensionDependencies.@acme/ go", "extensionDependencies.@putnami/dev#"}
	if !slices.Equal(got, want) {
		t.Fatalf("invalid-extension-dependency fields = %v, want %v", got, want)
	}
	m = loadFixtureManifest(t, "fixtures/valid/minimal.json")
	m.ExtensionDeps = Dependencies{List: map[string]string{"@putnami/dev": "", "local-tool": "", "tool_kit.v2": "^1"}}
	if diags := ValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("well-formed dependency names should pass, got %v", diags)
	}

	data, err := os.ReadFile("schemas/extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			ExtensionDependencies struct {
				OneOf []struct {
					Items struct {
						Pattern string `json:"pattern"`
					} `json:"items"`
					PropertyNames struct {
						Pattern string `json:"pattern"`
					} `json:"propertyNames"`
				} `json:"oneOf"`
			} `json:"extensionDependencies"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	forms := schema.Properties.ExtensionDependencies.OneOf
	if len(forms) != 2 {
		t.Fatalf("extensionDependencies has %d forms, want 2", len(forms))
	}
	for field, pattern := range map[string]string{
		"items.pattern":         forms[0].Items.Pattern,
		"propertyNames.pattern": forms[1].PropertyNames.Pattern,
	} {
		re, err := regexp.Compile(pattern)
		if err != nil || pattern == "" {
			t.Fatalf("schema extensionDependencies %s = %q: %v", field, pattern, err)
		}
		for name, want := range map[string]bool{
			"@putnami/dev": true, "local-tool": true, "@putnami/dev#": false, "@acme/ go": false, "@putnami/..": false, "Local": false,
		} {
			if re.MatchString(name) != want {
				t.Errorf("schema extensionDependencies %s = %q: MatchString(%q) = %v, want %v", field, pattern, name, !want, want)
			}
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
	spectest.Proves(t, "extension/manifest-contract", "a-registry-extension-dependency-requires-rung-8", "extension-dependencies-require-rung-8")
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
	spectest.Proves(t, "extension/manifest-contract", "a-registry-extension-dependency-requires-rung-8", "extension-dependencies-require-rung-8")
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
