package api

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/app"
	protofeatures "go.putnami.dev/protocol/features"
)

type designFilter struct {
	Cursor string `json:"cursor"`
}

// One Go type reaching an endpoint through two "accepts" roles used to produce
// two edges sharing the (from, to, kind) de-duplication key with a differing
// role property, which the design builder rejects as a conflicting native
// declaration — failing the whole build on a derived, disposable artifact.
func TestContributeDesignSeparatesSchemaRolesSharingOneGoType(t *testing.T) {
	output := t.TempDir()

	apiPlugin := New(&fakeServer{})
	apiPlugin.Register(Endpoint("POST", "/items").
		Query(Type[designFilter]()).
		Body(Type[designFilter]()).
		Returns(Type[[]designFilter]()).
		Document())

	application := app.New("designsvc")
	application.Feature(app.Feature{
		ID:      "items/manage",
		Name:    "Item management",
		Outcome: "Consumers filter items",
		Owner:   "samples",
	})
	application.Use(apiPlugin)

	if err := application.Describe(output, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatalf("read design graph: %v", err)
	}
	graph, err := protofeatures.ParseDesignGraph(data)
	if err != nil {
		t.Fatalf("parse design graph: %v", err)
	}

	roles := map[string]string{}
	for _, node := range graph.Nodes {
		if node.Kind == protofeatures.DesignNodeAPIOperation && node.Properties["operationId"] != "postItems" {
			t.Errorf("api operationId = %q, want postItems", node.Properties["operationId"])
		}
		if node.Kind == protofeatures.DesignNodeAPISchema {
			roles[node.Properties["role"]] = node.ID
		}
	}
	for _, role := range []string{"query", "body", "response"} {
		if roles[role] == "" {
			t.Errorf("no api.schema node for role %q; got %v", role, roles)
		}
	}
	if roles["query"] == roles["body"] {
		t.Errorf("query and body schemas collapsed onto one node %q", roles["query"])
	}
}
