package pkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// hookOnlyDependentManifest declares only a hook and a registry extension
// dependency, the shape of a framework package.
const hookOnlyDependentManifest = `{
	"name": "@putnami/testext",
	"extensionDependencies": ["@putnami/application"],
	"hooks": {"preBuild": {"kind": "command", "command": "echo"}},
	"commands": {}
}`

// A put archive whose manifest declares only hooks and a registry dependency
// takes the dependency stamp without the strict requirement of a command,
// tool or agent content, and loads.
func TestGateStampsAHookOnlyDependentPutArchive(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "the-packager-stamps-a-dependent", "a-hook-only-dependent-is-stamped")
	dir := stageManifest(t, hookOnlyDependentManifest)
	if err := gateAndStampManifestContract(dir, putArchive); err != nil {
		t.Fatalf("a hook-only dependent must take the dependency stamp: %v", err)
	}
	loaded, err := proto.LoadManifest(filepath.Join(dir, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the stamped archive must load: %v", err)
	}
	if loaded.CLIContract != protocolcli.ExtensionDependenciesContract {
		t.Fatalf("cliContract = %d, want %d", loaded.CLIContract, protocolcli.ExtensionDependenciesContract)
	}
}

// A put archive with commands and a registry dependency is stamped with the
// dependency rung after the full validation.
func TestGateStampsADependentWithCommands(t *testing.T) {
	manifest := strings.Replace(conformingManifest, `"version": "1.2.3",`,
		`"version": "1.2.3", "extensionDependencies": {"@putnami/dev": "^1.0.0"},`, 1)
	dir := stageManifest(t, manifest)
	if err := gateAndStampManifestContract(dir, putArchive); err != nil {
		t.Fatal(err)
	}
	if got := stagedContract(t, dir); got != protocolcli.ExtensionDependenciesContract {
		t.Fatalf("cliContract = %d, want %d", got, protocolcli.ExtensionDependenciesContract)
	}
}

// An npm package leaves its extension dependencies out of the stamp: a
// hook-only framework package stays byte-identical and unstamped.
func TestGateLeavesNPMDependenciesOutOfTheStamp(t *testing.T) {
	dir := stageManifest(t, hookOnlyDependentManifest)
	if err := gateAndStampManifestContract(dir, npmPackage); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, proto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != hookOnlyDependentManifest {
		t.Fatalf("an npm hook-only dependent must stay unstamped, got:\n%s", data)
	}

	manifest := strings.Replace(conformingManifest, `"version": "1.2.3",`,
		`"version": "1.2.3", "extensionDependencies": {"@putnami/dev": "^1.0.0"},`, 1)
	dir = stageManifest(t, manifest)
	if err := gateAndStampManifestContract(dir, npmPackage); err != nil {
		t.Fatal(err)
	}
	if got := stagedContract(t, dir); got != protocolcli.CurrentContract {
		t.Fatalf("an npm package is stamped %d, want %d without the dependency rung", got, protocolcli.CurrentContract)
	}
}

// A dependency list that does not decode is gated, so the strict decode
// refuses it instead of the archive shipping unstamped.
func TestGateRefusesAMalformedDependencyList(t *testing.T) {
	dir := stageManifest(t, `{"name": "@putnami/testext", "extensionDependencies": 3, "hooks": {"preBuild": {"kind": "command", "command": "echo"}}}`)
	if err := gateAndStampManifestContract(dir, putArchive); err == nil {
		t.Fatal("a malformed dependency list must fail packaging")
	}
}
