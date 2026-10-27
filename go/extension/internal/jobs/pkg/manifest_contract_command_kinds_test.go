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

// A command kind is contract 11 vocabulary: the packager stamps 11 in both
// package forms, so an older CLI refuses the artifact instead of planning the
// command as a plain one, and the fields survive the rewrite. A kind outside
// the vocabulary fails packaging and leaves the staged manifest as it was.
func TestGateStampsACommandKind(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "first-party-packagers-stamp-and-gate-the-contract", "the-go-extension-stamps-the-rung-its-vocabulary-requires")
	for name, form := range map[string]packageForm{"put archive": putArchive, "npm package": npmPackage} {
		dir := stageManifest(t, commandKindManifest)
		if err := gateAndStampManifestContract(dir, form); err != nil {
			t.Fatalf("%s: a manifest that declares a command kind must be stamped: %v", name, err)
		}
		loaded, err := proto.LoadManifest(filepath.Join(dir, proto.ManifestFilename))
		if err != nil {
			t.Fatalf("%s: the stamped manifest must load: %v", name, err)
		}
		if loaded.CLIContract != protocolcli.CommandKindContract {
			t.Fatalf("%s: cliContract = %d, want %d", name, loaded.CLIContract, protocolcli.CommandKindContract)
		}
		traits, batch := loaded.Commands["check"].Traits, loaded.Tasks["t"].Batchable
		if traits == nil || traits.Kind != proto.CommandKindVerification || traits.Workload != proto.WorkloadOnce {
			t.Errorf("%s: command traits %+v, want the verification kind and the once workload", name, traits)
		}
		if batch == nil || batch.Kind != proto.CommandKindVerification || batch.Phase != "check" {
			t.Errorf("%s: batchable %+v, want the verification kind and the check phase", name, batch)
		}
	}

	invalid := strings.Replace(commandKindManifest, `"kind": "verification", "workload"`, `"kind": "report", "workload"`, 1)
	if invalid == commandKindManifest {
		t.Fatal("the fixture no longer declares the command kind this test replaces")
	}
	dir := stageManifest(t, invalid)
	if err := gateAndStampManifestContract(dir, putArchive); err == nil {
		t.Fatal("an unknown command kind must fail packaging")
	}
	data, err := os.ReadFile(filepath.Join(dir, proto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != invalid {
		t.Fatalf("a refused manifest must stay unstamped, got:\n%s", data)
	}
}
