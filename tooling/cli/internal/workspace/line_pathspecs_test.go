package workspace

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// A client's line adds its provider's contract only when the contract lies
// outside the line: a contract inside it is already one of the line's paths.
func TestLinePathspecs_AddsOnlyAContractOutsideTheLine(t *testing.T) {
	spectest.Proves(t, "cli/version-from-git", "a-client-line-follows-its-contract",
		"a-contract-inside-the-line-adds-no-pathspec")
	binding := func(contract string) *GeneratedClientBinding {
		return &GeneratedClientBinding{ServiceID: "s", Language: "ts", ContractPath: contract}
	}
	ws := &Workspace{Projects: []*Project{
		{ID: "/ts/inside", Path: "ts/inside", Line: "ts", GeneratedClient: binding("ts/provider/schema/openapi.json")},
		{ID: "/ts/outside", Path: "ts/outside", Line: "ts", GeneratedClient: binding("go/provider/schema/openapi.json")},
		{ID: "/ts/again", Path: "ts/again", Line: "ts", GeneratedClient: binding("go/provider/schema/openapi.json")},
		{ID: "/ts/unresolved", Path: "ts/unresolved", Line: "ts", GeneratedClient: binding("")},
		{ID: "/ts/prefix", Path: "ts/prefix", Line: "ts", GeneratedClient: binding("tsx/provider/schema/openapi.json")},
		{ID: "/go/client", Path: "go/client", Line: "go", GeneratedClient: binding("rust/provider/schema/openapi.json")},
		{ID: "/ts/plain", Path: "ts/plain", Line: "ts"},
	}}

	want := []string{"ts", "go/provider/schema/openapi.json", "tsx/provider/schema/openapi.json"}
	if got := LinePathspecs(ws, "ts"); !slices.Equal(got, want) {
		t.Errorf("LinePathspecs(ts) = %v, want %v", got, want)
	}
	if got := LinePathspecs(ws, ""); got != nil {
		t.Errorf("LinePathspecs(root) = %v, want none: the root line covers the tree", got)
	}
	if got := LinePathspecs(nil, "ts"); !slices.Equal(got, []string{"ts"}) {
		t.Errorf("LinePathspecs(nil, ts) = %v, want [ts]", got)
	}
}
