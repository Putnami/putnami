package pkg

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// publishChannelsManifest declares a publish task that lists the channels it
// serves: the field contract 12 adds.
const publishChannelsManifest = `{
	"name": "@putnami/testchannels",
	"commands": {"publish": {"run": [{"id": "p", "task": "t"}]}},
	"tasks": {
		"t": {"kind": "command", "command": "echo", "timeoutMs": -1, "cache": false, "declares": {"effects": ["network", "registry"], "publishChannels": ["npm"]}}
	}
}`

// A publish channel is contract 12 vocabulary for the npm packager too: the
// package is stamped 12 with the field intact, and a channel list on a task
// without the registry effect fails the package job unstamped, the same
// verdicts as the Go packager.
func TestCopyHookFiles_StampsPublishChannels(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "publish-tasks-declare-their-channels", "the-npm-packager-stamps-publish-channels")
	projDir := stageSourceManifest(t, publishChannelsManifest)
	outDir := t.TempDir()
	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("a manifest that declares publish channels must be packaged: %v", err)
	}
	loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the packaged manifest must load: %v", err)
	}
	if loaded.CLIContract != protocolcli.PublishChannelsContract {
		t.Fatalf("cliContract = %d, want %d", loaded.CLIContract, protocolcli.PublishChannelsContract)
	}
	if declares := loaded.Tasks["t"].Declares; declares == nil || !slices.Equal(declares.PublishChannels, []string{"npm"}) {
		t.Errorf("declaration %+v, want publishChannels [npm]", declares)
	}

	invalid := strings.Replace(publishChannelsManifest, `["network", "registry"]`, `["network"]`, 1)
	if invalid == publishChannelsManifest {
		t.Fatal("the fixture no longer declares the registry effect this test removes")
	}
	projDir = stageSourceManifest(t, invalid)
	outDir = t.TempDir()
	if err := copyHookFiles(projDir, outDir, "1.0.0"); err == nil {
		t.Fatal("publish channels without the registry effect must fail packaging")
	}
	data, _ := os.ReadFile(filepath.Join(outDir, proto.ManifestFilename))
	if strings.Contains(string(data), "cliContract") {
		t.Fatal("a refused manifest must not carry the cliContract stamp")
	}
}
