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

// commandKindManifest declares a command kind and workload, and a batchable
// task's kind and phase: the fields contract 11 adds.
const commandKindManifest = `{
	"name": "@putnami/testkinds",
	"commands": {
		"check": {"traits": {"kind": "verification", "workload": "once"}, "run": [{"id": "c", "task": "t"}]}
	},
	"tasks": {
		"t": {"kind": "command", "command": "echo", "timeoutMs": -1, "batchable": {"tool": "echo", "kind": "verification", "phase": "check"}}
	}
}`

// A command kind is contract 11 vocabulary for the npm packager too: the
// package is stamped 11 with the fields intact, and a kind outside the
// vocabulary fails the package job unstamped, the same verdicts as the Go
// packager.
func TestCopyHookFiles_StampsACommandKind(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "first-party-packagers-stamp-and-gate-the-contract", "the-typescript-extension-stamps-the-rung-its-vocabulary-requires")
	projDir := stageSourceManifest(t, commandKindManifest)
	outDir := t.TempDir()
	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("a manifest that declares a command kind must be packaged: %v", err)
	}
	loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the packaged manifest must load: %v", err)
	}
	if loaded.CLIContract != protocolcli.CommandKindContract {
		t.Fatalf("cliContract = %d, want %d", loaded.CLIContract, protocolcli.CommandKindContract)
	}
	traits, batch := loaded.Commands["check"].Traits, loaded.Tasks["t"].Batchable
	if traits == nil || traits.Kind != proto.CommandKindVerification || traits.Workload != proto.WorkloadOnce {
		t.Errorf("command traits %+v, want the verification kind and the once workload", traits)
	}
	if batch == nil || batch.Kind != proto.CommandKindVerification || batch.Phase != "check" {
		t.Errorf("batchable %+v, want the verification kind and the check phase", batch)
	}

	invalid := strings.Replace(commandKindManifest, `"kind": "verification", "workload"`, `"kind": "report", "workload"`, 1)
	if invalid == commandKindManifest {
		t.Fatal("the fixture no longer declares the command kind this test replaces")
	}
	projDir = stageSourceManifest(t, invalid)
	outDir = t.TempDir()
	if err := copyHookFiles(projDir, outDir, "1.0.0"); err == nil {
		t.Fatal("an unknown command kind must fail packaging")
	}
	data, _ := os.ReadFile(filepath.Join(outDir, proto.ManifestFilename))
	if strings.Contains(string(data), "cliContract") {
		t.Fatal("a refused manifest must not carry the cliContract stamp")
	}
}
