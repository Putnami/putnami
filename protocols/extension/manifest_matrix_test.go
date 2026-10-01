package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// The cliContract compatibility matrix, executable.
//
// doc/04-validation.md states the ladder as a table; B6c pinned its rows for a
// manifest whose surface is COMMANDS. The surface kind is not incidental to the
// ladder, though — it is what decides whether the ladder applies at all
// (DeclaresContractSurface), and that predicate counts command groups and MCP
// tools too. A manifest exposing only agent-facing tools, unstamped, is the case
// where getting it wrong is quietest: nothing in a build log mentions tools.
//
// So this is the full cross product of {absent, one behind, current, latest,
// one ahead of latest} × {commands, command groups, tools, agent content, none},
// in one table, next to the doc it mirrors. Ratcheting a row means editing this
// table and that table together.
//
// The latest contract is additive: a manifest needs it only when it declares
// agent content, so the floor a stamp must reach depends on the surface, while
// the ceiling does not.

// matrixManifest renders a manifest carrying exactly one kind of contract
// surface at a given declared contract. Contract 0 is written as an ABSENT
// field, which is what a pre-registry manifest actually looks like on disk.
func matrixManifest(contract int, surface string) string {
	stamp := ""
	if contract > 0 {
		stamp = fmt.Sprintf("\n  \"cliContract\": %d,", contract)
	}
	return fmt.Sprintf(`{
  "name": "@putnami/matrix",%s
  %s
}`, stamp, surface)
}

const (
	commandsSurface = `"commands": {"deploy": {"run": [{"id": "d", "task": "t"}]}},
  "tasks": {"t": {"kind": "command", "command": "echo"}}`

	commandGroupsSurface = `"commandGroups": {"cloud": {"subcommands": {"login": {"command": "deploy"}}}},
  "commands": {"deploy": {"run": [{"id": "d", "task": "t"}]}},
  "tasks": {"t": {"kind": "command", "command": "echo"}}`

	toolsSurface = `"tools": {
    "putnami.search": {
      "description": "Search the index.",
      "inputSchema": {"type": "object"},
      "annotations": {"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true},
      "_meta": {"putnami.dev/contract": {"access": "read", "readOnly": true, "supportsDryRun": false}},
      "command": "search"
    }
  }`

	hookOnlySurface = `"hooks": {"preBuild": {"kind": "command", "command": "echo"}},
  "commands": {}`

	agentContentSurface = `"agentContent": {
    "path": "agent-content",
    "manifestSha256": "3f6c9a1d2b7e4f8a0c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3"
  }`
)

func writeMatrixManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ManifestFilename)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadManifest_CompatibilityMatrix pins every cell of the ladder × surface
// cross product. The expectations are stated as "loads" plus, for a rejection,
// the substrings the error must carry: a rejection a user cannot act on is a
// different failure from a rejection, and the remediation text is the only part
// of it they see.
func TestLoadManifest_CompatibilityMatrix(t *testing.T) {
	current := protocolcli.CurrentContract
	latest := protocolcli.LatestContract
	repackage := func(required int) []string {
		return []string{
			fmt.Sprintf("requires %d", required),
			"re-package the extension",
			"putnami extensions update",
		}
	}
	tooNew := []string{"requires a newer putnami"}

	surfaces := []struct {
		name     string
		body     string
		governs  bool // does the contract ladder apply to this manifest at all?
		required int  // the lowest stamp that covers the surface's vocabulary
	}{
		{name: "commands", body: commandsSurface, governs: true, required: current},
		{name: "commandGroups", body: commandGroupsSurface, governs: true, required: current},
		{name: "tools", body: toolsSurface, governs: true, required: current},
		{name: "agentContent", body: agentContentSurface, governs: true, required: protocolcli.AgentContentContract},
		{name: "hook-only", body: hookOnlySurface, governs: false, required: current},
	}
	contracts := []struct {
		name  string
		value int
	}{
		{name: "absent", value: 0},
		{name: "one behind", value: current - 1},
		{name: "current", value: current},
		{name: "latest", value: latest},
		{name: "one ahead", value: latest + 1},
	}

	for _, surface := range surfaces {
		for _, contract := range contracts {
			if contract.value < 0 {
				continue // no contract below 0 exists to test
			}
			t.Run(surface.name+"/"+contract.name, func(t *testing.T) {
				path := writeMatrixManifest(t, matrixManifest(contract.value, surface.body))
				m, err := LoadManifest(path)

				// A future contract is never half-interpreted, and that verdict
				// is reached BEFORE the surface question: a hook-only manifest
				// stamped one ahead was written against rules this build does
				// not have, whatever it happens to declare today.
				if contract.value > latest {
					assertRejected(t, m, err, tooNew)
					return
				}
				// Below the contract its vocabulary requires, the surface decides.
				// A manifest the contract governs must carry the stamp it earned;
				// one it does not govern is outside the ladder, and the packager
				// never stamps it, so demanding a stamp would reject exactly the
				// manifests packaging refuses to produce.
				if contract.value < surface.required && surface.governs {
					assertRejected(t, m, err, repackage(surface.required))
					return
				}
				if err != nil {
					t.Fatalf("manifest must load: %v", err)
				}
				if m == nil {
					t.Fatal("a loaded manifest must not be nil")
				}
				if got := DeclaresContractSurface(m); got != surface.governs {
					t.Errorf("DeclaresContractSurface = %v, want %v — the loader's exemption "+
						"and the packager's must ask the same question", got, surface.governs)
				}
				if got := RequiredCLIContract(m); got != surface.required {
					t.Errorf("RequiredCLIContract = %d, want %d", got, surface.required)
				}
			})
		}
	}
}

func assertRejected(t *testing.T, m *Manifest, err error, want []string) {
	t.Helper()
	if err == nil {
		t.Fatal("manifest must not load")
	}
	if m != nil {
		t.Error("a rejected manifest must not be returned; a caller could use it")
	}
	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error missing %q: %v", fragment, err)
		}
	}
}

// TestDeclaresContractSurface_CountsEverySurfaceKind is the predicate the whole
// matrix pivots on, checked directly. A regression that stopped counting tools
// would make every unstamped tool-only manifest load — silently handing agents
// capabilities from an extension whose contract was never verified — and the
// matrix above would still pass its commands rows.
func TestDeclaresContractSurface_CountsEverySurfaceKind(t *testing.T) {
	cases := map[string]bool{
		commandsSurface:      true,
		commandGroupsSurface: true,
		toolsSurface:         true,
		agentContentSurface:  true,
		hookOnlySurface:      false,
	}
	for surface, want := range cases {
		path := writeMatrixManifest(t, matrixManifest(protocolcli.LatestContract, surface))
		m, err := LoadManifest(path)
		if err != nil {
			t.Fatalf("load %s: %v", surface, err)
		}
		if got := DeclaresContractSurface(m); got != want {
			t.Errorf("DeclaresContractSurface = %v, want %v for:\n%s", got, want, surface)
		}
	}
}
