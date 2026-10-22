package extension

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// provisionedToolchainManifest declares one runtime toolchain, at contract,
// whose candidates end with the provisioned source for an identity the
// manifest answers for.
func provisionedToolchainManifest(name string, contract int) string {
	return fmt.Sprintf(`{
		"name": %q,
		"cliContract": %d,
		"commands": {"build": {"run": [{"id": "b", "task": "build"}]}},
		"tasks": {"build": {"kind": "command", "command": "{extensionRuntime}", "toolchains": ["compiler"]}},
		"runtime": {
			"executable": "bin/compiler-ext",
			"resolves": ["compiler"],
			"toolchains": {"compiler": {"lock": "compiler",
				"candidates": [{"from": "path", "path": "compiler"}, {"from": "provisioned"}],
				"probe": {"args": ["--version"], "expect": "{version}"}}}
		}
	}`, name, contract)
}

// Discovery refuses a manifest whose runtime toolchain declares a provisioned
// candidate below contract 13, with the reason, and loads it at 13 with the
// candidate as declared.
func TestDiscovery_AManifestDeclaringAProvisionedCandidateRequiresContract13(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "below"), provisionedToolchainManifest("@acme/below", protocolcli.PublishChannelsContract))
	writeExtensionManifest(t, filepath.Join(dir, "at"), provisionedToolchainManifest("@acme/at", protocolcli.ProvisionedToolchainContract))
	result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"below", "at"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason.Error(), "requires 13 for its provisioned toolchain candidate") {
		t.Fatalf("skipped = %v, want the contract-12 manifest refused for its provisioned candidate", result.Skipped)
	}
	if FindExtensionByName(result.Extensions, "@acme/below") != nil {
		t.Fatal("the contract-12 manifest loaded")
	}
	loaded := FindExtensionByName(result.Extensions, "@acme/at")
	if loaded == nil {
		t.Fatalf("loaded %d extensions, want the contract-13 one", len(result.Extensions))
	}
	if loaded.Runtime == nil {
		t.Fatal("the loaded extension lost its runtime")
	}
	candidates := loaded.Runtime.Toolchains["compiler"].Candidates
	if len(candidates) != 2 || candidates[1] != (extensionproto.RuntimeToolchainCandidate{From: extensionproto.RuntimeToolchainCandidateProvisioned}) {
		t.Fatalf("loaded candidates = %+v, want a bare provisioned candidate last", candidates)
	}
}
