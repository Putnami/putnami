package cloudcli

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// TestReadSubcommandsDeclareNoWorkspaceState pins the cloud subcommands that
// read no installed workspace state, so the CLI answers them on a fresh
// checkout without the install and the client restore, and the group flag
// that chooses the credential of workspace commands.
func TestReadSubcommandsDeclareNoWorkspaceState(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "read-commands-start-no-job", "read-subcommands-declare-no-workspace-state")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	manifest, diagnostics := extensionproto.ParseManifest(data)
	if len(diagnostics) != 0 {
		t.Fatalf("strict extension parse diagnostics = %+v", diagnostics)
	}
	group := manifest.CommandGroups["cloud"]
	var none []string
	for name, sub := range group.Subcommands {
		if sub.ReadsNoWorkspaceState() {
			none = append(none, name)
		}
	}
	sort.Strings(none)
	if want := []string{"login", "logout", "status", "whoami"}; !slices.Equal(none, want) {
		t.Fatalf("subcommands with workspaceState none = %v, want %v", none, want)
	}
	flag, ok := group.Flags["credential"]
	if !ok || flag.Type != "string" || !slices.Equal(flag.Choices, []string{"session", "machine"}) {
		t.Fatalf("cloud --credential = %+v, want a string flag with the choices session and machine", flag)
	}
}
