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

// A publish channel is contract 12 vocabulary: the packager stamps 12 in both
// package forms, so an older CLI refuses the artifact instead of judging its
// publishing runs by another rule, and the field survives the rewrite. A
// channel list on a task without the registry effect fails packaging and leaves
// the staged manifest as it was.
func TestGateStampsPublishChannels(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "publish-tasks-declare-their-channels", "the-go-packager-stamps-publish-channels")
	for name, form := range map[string]packageForm{"put archive": putArchive, "npm package": npmPackage} {
		dir := stageManifest(t, publishChannelsManifest)
		if err := gateAndStampManifestContract(dir, form); err != nil {
			t.Fatalf("%s: a manifest that declares publish channels must be stamped: %v", name, err)
		}
		loaded, err := proto.LoadManifest(filepath.Join(dir, proto.ManifestFilename))
		if err != nil {
			t.Fatalf("%s: the stamped manifest must load: %v", name, err)
		}
		if loaded.CLIContract != protocolcli.PublishChannelsContract {
			t.Fatalf("%s: cliContract = %d, want %d", name, loaded.CLIContract, protocolcli.PublishChannelsContract)
		}
		if declares := loaded.Tasks["t"].Declares; declares == nil || !slices.Equal(declares.PublishChannels, []string{"npm"}) {
			t.Errorf("%s: declaration %+v, want publishChannels [npm]", name, declares)
		}
	}

	invalid := strings.Replace(publishChannelsManifest, `["network", "registry"]`, `["network"]`, 1)
	if invalid == publishChannelsManifest {
		t.Fatal("the fixture no longer declares the registry effect this test removes")
	}
	dir := stageManifest(t, invalid)
	if err := gateAndStampManifestContract(dir, putArchive); err == nil {
		t.Fatal("publish channels without the registry effect must fail packaging")
	}
	data, err := os.ReadFile(filepath.Join(dir, proto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != invalid {
		t.Fatalf("a refused manifest must stay unstamped, got:\n%s", data)
	}
}
