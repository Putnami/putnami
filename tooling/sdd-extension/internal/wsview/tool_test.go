package wsview

import (
	"encoding/json"
	"reflect"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	pctx "go.putnami.dev/sdk/extension/context"
)

// TestFromToolRequestReadsTheWholeMembership pins what the tool wire carries
// that the job wire does not.
//
// A job acts on its own project, so it receives that project's authored facts
// and nobody else's; a tool reports ON other projects, so a sibling's tags,
// publish channels and putnami.json have to travel. Dropping any of them is
// invisible at run time — the answer stays well-formed and gets quietly poorer
// — which is why each is asserted rather than assumed.
func TestFromToolRequestReadsTheWholeMembership(t *testing.T) {
	view := FromToolRequest(proto.ToolCallRequest{
		WorkspaceRoot: "/ws",
		WorkspaceProjects: []proto.ToolProjectRef{
			{
				ID: "/billing", Name: "@acme/billing", SourceName: "billing",
				Version: "2.1.0", Type: "application", Path: "billing",
				Tags: []string{"service"}, Publish: []string{"npm"},
				Extensions: []string{"/tooling/sdd-extension"}, RunsWith: []string{"@acme/db"},
				Dependencies: []string{"/shipping"},
				Config:       json.RawMessage(`{"name":"@acme/billing","featureAuthority":{"owner":"billing/invoice"}}`),
			},
			{ID: "/shipping", Name: "@acme/shipping", Path: "shipping"},
		},
	})
	if view == nil {
		t.Fatal("no view was built")
	}
	if view.Root != "/ws" {
		t.Errorf("root = %q, want /ws", view.Root)
	}
	billing := view.ProjectByID("/billing")
	if billing == nil {
		t.Fatal("the billing project is not in the view")
	}
	for _, check := range []struct {
		what string
		got  any
		want any
	}{
		{"name", billing.Name, "@acme/billing"},
		{"sourceName", billing.SourceName, "billing"},
		{"version", billing.Version, "2.1.0"},
		{"type", billing.Type, "application"},
		{"path", billing.Path, "billing"},
	} {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.what, check.got, check.want)
		}
	}
	if len(billing.Tags) != 1 || billing.Tags[0] != "service" {
		t.Errorf("tags = %v, want the authored tag", billing.Tags)
	}
	if len(billing.Publish) != 1 || billing.Publish[0] != "npm" {
		t.Errorf("publish = %v; spec completeness reads it to decide whether a project is published", billing.Publish)
	}
	if len(billing.Extensions) != 1 || len(billing.RunsWith) != 1 {
		t.Errorf("extensions = %v, runsWith = %v", billing.Extensions, billing.RunsWith)
	}
	if billing.Config == nil || billing.Config.FeatureAuthority == nil ||
		billing.Config.FeatureAuthority.Owner != "billing/invoice" {
		t.Errorf("config = %+v; featureAuthority is the reviewed answer a sibling gives", billing.Config)
	}
	// The resolved edges make this a graph rather than a listing.
	if deps := view.Graph.DependenciesOf("/billing"); len(deps) != 1 || deps[0] != "/shipping" {
		t.Errorf("resolved edges = %v, want the orchestrator's own graph", deps)
	}
	// A project by NAME too: feature ownership matches on name, id and
	// sourceName, and a view indexed on one of the three answers two of them
	// with "not found".
	if view.ProjectByName("@acme/shipping") == nil {
		t.Error("the view is not indexed by resolved name")
	}
}

// TestFromToolRequestMintsAnIDForAnUnidentifiedMember keeps the view readable
// for a producer that publishes a path and no id, using the SAME derivation
// core uses — a different one would make two runs over one tree report
// different owners.
func TestFromToolRequestMintsAnIDForAnUnidentifiedMember(t *testing.T) {
	view := FromToolRequest(proto.ToolCallRequest{
		WorkspaceProjects: []proto.ToolProjectRef{{Name: "lib", Path: "packages/(group)/lib"}},
	})
	if view.ProjectByID("/packages/lib") == nil {
		t.Errorf("no id was minted from the path; projects = %+v", view.Projects[0])
	}
}

// TestToolSelectionRefusesAnUnresolvedCall is the fail-closed marker.
//
// A CLI that publishes no `selection` is one that resolved nothing, and every
// SDD tool would answer such a call perfectly well — for the whole workspace,
// whatever the caller asked for. The boolean is what lets the caller refuse
// instead, so it is asserted in both directions.
func TestToolSelectionRefusesAnUnresolvedCall(t *testing.T) {
	if _, resolved := ToolSelection(proto.ToolCallRequest{}); resolved {
		t.Fatal("a request with no selection reported one; a narrowed call would answer unnarrowed")
	}
	selection, resolved := ToolSelection(proto.ToolCallRequest{
		Selection: &proto.ToolSelection{
			Mode: proto.ToolSelectionModeImpacted, Scoped: true,
			Baseline: "origin/main", BaselineSource: "trunk",
			ProjectIDs: []string{"/billing"}, EmptyImpact: true,
		},
	})
	if !resolved {
		t.Fatal("a request with a selection reported none")
	}
	want := pctx.Selection{
		Mode: pctx.SelectionModeImpacted, Scoped: true,
		Baseline: "origin/main", BaselineSource: "trunk",
		ProjectIDs: []string{"/billing"}, EmptyImpact: true,
	}
	if !reflect.DeepEqual(selection, want) {
		// Compared whole, so a member added to one contract and forgotten here
		// fails rather than silently reading as its zero value.
		t.Errorf("selection = %+v, want %+v", selection, want)
	}
}

// TestToolSelectionCarriesEveryMemberOfTheWireContract keeps the projection
// above from being partial: every member the extension contract declares must
// be read, or a narrowing arrives as a whole-workspace run.
func TestToolSelectionCarriesEveryMemberOfTheWireContract(t *testing.T) {
	wire := proto.ToolSelection{
		Mode: proto.ToolSelectionModeProjects, Scoped: true,
		Baseline: "HEAD", BaselineSource: "explicit",
		ProjectIDs: []string{"/a", "/b"}, EmptyImpact: true,
	}
	selection, _ := ToolSelection(proto.ToolCallRequest{Selection: &wire})

	wireJSON, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal the wire selection: %v", err)
	}
	viewJSON, err := json.Marshal(selection)
	if err != nil {
		t.Fatalf("marshal the projected selection: %v", err)
	}
	if string(wireJSON) != string(viewJSON) {
		t.Errorf("the projection lost or renamed a member:\n  wire: %s\n  view: %s", wireJSON, viewJSON)
	}
}

// TestFromToolRequestToleratesAnUnreadableConfig states the choice: a config
// that will not decode costs the refinements it carried, never the whole
// answer.
func TestFromToolRequestToleratesAnUnreadableConfig(t *testing.T) {
	view := FromToolRequest(proto.ToolCallRequest{
		WorkspaceProjects: []proto.ToolProjectRef{
			{ID: "/billing", Name: "@acme/billing", Path: "billing", Config: json.RawMessage(`"not an object"`)},
		},
	})
	billing := view.ProjectByID("/billing")
	if billing == nil {
		t.Fatal("an unreadable config removed the project from the view")
	}
	if billing.Config != nil {
		t.Errorf("config = %+v, want none", billing.Config)
	}
}
