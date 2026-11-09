package extension

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// buildGraphManifest is a workspace adapter that attributes edges to
// build-graph, stamped with the contract that member requires. It lives
// outside fixtures/valid, whose manifests every additive-rung test reads as
// using no rung above agent content.
const buildGraphManifest = "testdata/build-graph.extension.json"

// buildGraphEdges needs dependencySources: core asks for provenance only from
// an adapter that declares it, so a build-graph source alone would never be
// asked for.
func TestBuildGraphEdges_RequireDependencySources(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "build-graph-edges-are-declared-by-the-adapter", "build-graph-edges-require-dependency-sources")
	m := loadFixtureManifest(t, buildGraphManifest)
	if diags := FullValidateManifest(m); len(diagCodes(diags)) != 0 {
		t.Fatalf("the valid build-graph fixture reports %v", diags)
	}
	m.Workspace.DependencySources = false
	diags := ValidateWorkspaceAdapter(m)
	if len(diags) != 1 || diags[0].Code != "build-graph-without-dependency-sources" || diags[0].Field != "workspace.buildGraphEdges" {
		t.Fatalf("diagnostics = %v, want one build-graph-without-dependency-sources at workspace.buildGraphEdges", diags)
	}
}

// The member raises the floor to contract 21, its absence does not, and a
// manifest stamped below 21 is refused with the reason.
func TestBuildGraphEdges_RequireContract21(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "build-graph-edges-are-declared-by-the-adapter", "build-graph-edges-require-contract-21")
	path := buildGraphManifest
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := RequiredCLIContract(m); got != protocolcli.BuildGraphEdgesContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.BuildGraphEdgesContract)
	}
	if protocolcli.LatestContract < protocolcli.BuildGraphEdgesContract {
		t.Fatalf("LatestContract %d does not read contract %d", protocolcli.LatestContract, protocolcli.BuildGraphEdgesContract)
	}
	m.Workspace.BuildGraphEdges = false
	if got := RequiredCLIContract(m); got >= protocolcli.BuildGraphEdgesContract {
		t.Fatalf("without buildGraphEdges RequiredCLIContract = %d, want below %d", got, protocolcli.BuildGraphEdgesContract)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["cliContract"] = protocolcli.UserScopeSkillsContract
	below, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := NegotiateManifest(path, below); err == nil || m != nil {
		t.Fatal("a manifest with build-graph edges stamped 20 loaded")
	} else if !strings.Contains(err.Error(), "requires 21 for its build-graph edges") {
		t.Fatalf("the refusal must name the build-graph requirement: %v", err)
	}
}
