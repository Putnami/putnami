package extension

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// moduleGraphManifest is a workspace adapter that attributes edges to
// module-graph, stamped with the contract that member requires. It lives
// outside fixtures/valid for the reason buildGraphManifest does.
const moduleGraphManifest = "testdata/module-graph.extension.json"

// moduleGraphEdges needs dependencySources: core asks for provenance only from
// an adapter that declares it, so a module-graph source alone would never be
// asked for.
func TestModuleGraphEdges_RequireDependencySources(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "module-graph-edges-are-declared-by-the-adapter", "module-graph-edges-require-dependency-sources")
	m := loadFixtureManifest(t, moduleGraphManifest)
	if diags := FullValidateManifest(m); len(diagCodes(diags)) != 0 {
		t.Fatalf("the valid module-graph fixture reports %v", diags)
	}
	m.Workspace.DependencySources = false
	diags := ValidateWorkspaceAdapter(m)
	if len(diags) != 1 || diags[0].Code != "module-graph-without-dependency-sources" || diags[0].Field != "workspace.moduleGraphEdges" {
		t.Fatalf("diagnostics = %v, want one module-graph-without-dependency-sources at workspace.moduleGraphEdges", diags)
	}
}

// The member raises the floor to contract 31, its absence does not, and a
// manifest stamped below 31 is refused with the reason.
func TestModuleGraphEdges_RequireContract31(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "module-graph-edges-are-declared-by-the-adapter", "module-graph-edges-require-contract-31")
	path := moduleGraphManifest
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := RequiredCLIContract(m); got != protocolcli.ModuleGraphEdgesContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.ModuleGraphEdgesContract)
	}
	if protocolcli.LatestContract < protocolcli.ModuleGraphEdgesContract {
		t.Fatalf("LatestContract %d does not read contract %d", protocolcli.LatestContract, protocolcli.ModuleGraphEdgesContract)
	}
	m.Workspace.ModuleGraphEdges = false
	if got := RequiredCLIContract(m); got >= protocolcli.ModuleGraphEdgesContract {
		t.Fatalf("without moduleGraphEdges RequiredCLIContract = %d, want below %d", got, protocolcli.ModuleGraphEdgesContract)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["cliContract"] = protocolcli.SelectionFreeTaskContract
	below, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := NegotiateManifest(path, below); err == nil || m != nil {
		t.Fatal("a manifest with module-graph edges stamped 30 loaded")
	} else if !strings.Contains(err.Error(), "requires 31 for its module-graph edges") {
		t.Fatalf("the refusal must name the module-graph requirement: %v", err)
	}
}
