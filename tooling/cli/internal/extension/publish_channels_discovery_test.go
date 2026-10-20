package extension

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// publishChannelsManifest declares one publish task, at contract, that lists
// the channels it serves in authored order.
func publishChannelsManifest(name string, contract int) string {
	return fmt.Sprintf(`{
		"name": %q,
		"cliContract": %d,
		"commands": {"publish": {"run": [{"id": "ship", "task": "ship"}]}},
		"tasks": {"ship": {"kind": "command", "command": "echo", "timeoutMs": -1, "cache": false,
			"declares": {"effects": ["network", "registry"], "publishChannels": ["npm", "docker"]}}}
	}`, name, contract)
}

// Discovery refuses a manifest whose task declares publish channels below
// contract 12, with the reason, and loads it at 12 with the channels the task
// declares.
func TestDiscovery_AManifestDeclaringPublishChannelsRequiresContract12(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publish-tasks-declare-their-channels", "a-manifest-declaring-publish-channels-requires-contract-12")
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "below"), publishChannelsManifest("@acme/below", protocolcli.CommandKindContract))
	writeExtensionManifest(t, filepath.Join(dir, "at"), publishChannelsManifest("@acme/at", protocolcli.PublishChannelsContract))
	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"below", "at"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason.Error(), "requires 12 for its publish channels") {
		t.Fatalf("skipped = %v, want the contract-11 manifest refused for its publish channels", result.Skipped)
	}
	if FindExtensionByName(result.Extensions, "@acme/below") != nil {
		t.Fatal("the contract-11 manifest loaded")
	}
	loaded := FindExtensionByName(result.Extensions, "@acme/at")
	if loaded == nil {
		t.Fatalf("loaded %d extensions, want the contract-12 one", len(result.Extensions))
	}
	declares := loaded.Tasks["ship"].Declares
	if declares == nil {
		t.Fatal("the loaded task lost its declaration")
	}
	if got := slices.Sorted(slices.Values(declares.PublishChannels)); !slices.Equal(got, []string{"docker", "npm"}) {
		t.Fatalf("loaded publishChannels = %v, want docker and npm", declares.PublishChannels)
	}
}
