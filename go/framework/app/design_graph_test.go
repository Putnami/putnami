package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/config"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	protofeatures "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/infra"
)

type nativeDesignOptions struct {
	Enabled bool `json:"enabled"`
}

var nativeDesignConfig = config.Config[nativeDesignOptions]("tasks")

// nativeCompositionDesignPlugin deliberately does not implement
// DesignContributor. Every node it emits comes from a bounded native seam.
type nativeCompositionDesignPlugin struct{}

type duplicateConfigDesignPluginA struct{}
type duplicateConfigDesignPluginB struct{}

func (*duplicateConfigDesignPluginA) Name() string { return "duplicate-config-a" }
func (*duplicateConfigDesignPluginB) Name() string { return "duplicate-config-b" }
func (*duplicateConfigDesignPluginA) ConfigDefinitions() []config.Descriptor {
	return []config.Descriptor{nativeDesignConfig.Descriptor()}
}
func (*duplicateConfigDesignPluginB) ConfigDefinitions() []config.Descriptor {
	return []config.Descriptor{nativeDesignConfig.Descriptor()}
}

func (*nativeCompositionDesignPlugin) Name() string { return "native-composition" }
func (*nativeCompositionDesignPlugin) ConfigDefinitions() []config.Descriptor {
	return []config.Descriptor{nativeDesignConfig.Descriptor()}
}
func (*nativeCompositionDesignPlugin) DesignInfraRequirements() []DesignInfraRequirement {
	return []DesignInfraRequirement{{
		Name: "tasks", Kind: protocaps.InfraKindDatabase,
		Provenance: &protofeatures.DesignProvenance{Path: "test/native.go", Symbol: "TasksDatabase"},
	}}
}
func (*nativeCompositionDesignPlugin) DesignTests() []DesignTest {
	return []DesignTest{{
		Name: "tasks/create", Kind: DesignTestIntegration,
		Proves: []DesignTestProof{{
			Feature: "tasks/manage", Requirement: "creation", Check: "task-create-flow",
		}},
		Provenance: &protofeatures.DesignProvenance{Path: "test/native_test.go", Symbol: "TestCreateTask"},
	}}
}
func (*nativeCompositionDesignPlugin) Configure(context.Context, *Module) error { return nil }
func (*nativeCompositionDesignPlugin) Start(context.Context, *Module) error     { return nil }
func (*nativeCompositionDesignPlugin) Stop(context.Context, *Module) error      { return nil }

func TestNativeCompositionDesignMatchesSharedGoTypeScriptSemantics(t *testing.T) {
	output := t.TempDir()
	application := New("tasks")
	tasks := NewModule("tasks").Feature(Feature{
		ID: "tasks/manage", Name: "Task management",
		Outcome: "Users can manage tasks", Owner: "samples",
	})
	tasks.Use(&nativeCompositionDesignPlugin{})
	application.Use(tasks)
	if err := application.Describe(output, []string{describerNameDesign}); err != nil {
		t.Fatalf("describe native composition: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatal(err)
	}
	graph, err := protofeatures.ParseDesignGraph(data)
	if err != nil {
		t.Fatal(err)
	}
	configNode := findProtocolDesignNode(graph.Nodes, "config:tasks")
	configEdge := findProtocolDesignEdge(graph.Edges, "config:tasks")
	if configNode == nil || configNode.Properties["path"] != "tasks" || configNode.Provenance != nil || configEdge == nil ||
		configEdge.Provenance == nil || !strings.HasSuffix(configEdge.Provenance.Path, "design_graph_test.go") ||
		!strings.Contains(configEdge.Provenance.Symbol, "ConfigDefinitions") {
		t.Fatalf("native config registration or exact provenance not projected: %#v", configNode)
	}
	infraNode := findProtocolDesignNode(graph.Nodes, "infra:database:tasks")
	infraEdge := findProtocolDesignEdge(graph.Edges, "infra:database:tasks")
	if infraNode == nil || infraNode.Provenance != nil || infraEdge == nil || infraEdge.Provenance == nil || infraEdge.Provenance.Path != "test/native.go" || infraEdge.Provenance.Symbol != "TasksDatabase" {
		t.Fatalf("bounded infra declaration provenance not preserved: %#v", infraNode)
	}
	testNode := findProtocolDesignNode(graph.Nodes, "test:integration:tasks/create")
	testEdge := findProtocolDesignEdge(graph.Edges, "test:integration:tasks/create")
	if testNode == nil || testNode.Provenance != nil || testEdge == nil || testEdge.Provenance == nil || testEdge.Provenance.Path != "test/native_test.go" || testEdge.Provenance.Symbol != "TestCreateTask" {
		t.Fatalf("bounded test declaration provenance not preserved: %#v", testNode)
	}
	// Source languages use different declaration paths. Provenance is asserted
	// separately; remove only that field before comparing shared semantics.
	for index := range graph.Nodes {
		graph.Nodes[index].Provenance = nil
	}
	for index := range graph.Edges {
		graph.Edges[index].Provenance = nil
	}
	got, err := protofeatures.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "protocols", "features", "fixtures", "equivalence", "go-typescript-design.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("native Go design semantics differ from shared fixture\n%s", got)
	}

}

// TestDesignTestProvesPropertyIsDeterministicAndClosed pins the spec-check
// binding projection: a partial proof is an authoring error rather
// than a guess, duplicates collapse, and the property is sorted so both
// language producers emit identical bytes.
func TestDesignTestProvesPropertyIsDeterministicAndClosed(t *testing.T) {
	proves, err := designTestProvesProperty([]DesignTestProof{
		{Feature: "tasks/manage", Requirement: "creation", Check: "task-create-flow"},
		{Feature: "tasks/manage", Requirement: "archive", Check: "task-archive-flow"},
		{Feature: "tasks/manage", Requirement: "creation", Check: "task-create-flow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if proves != "tasks/manage#archive#task-archive-flow tasks/manage#creation#task-create-flow" {
		t.Fatalf("proves = %q, want the sorted de-duplicated encoding", proves)
	}

	if _, err := designTestProvesProperty([]DesignTestProof{{Feature: "tasks/manage", Check: "task-create-flow"}}); err == nil {
		t.Fatal("a proof without a requirement was accepted")
	}
	if proves, err := designTestProvesProperty(nil); err != nil || proves != "" {
		t.Fatalf("no proofs must project to no property (got %q, %v)", proves, err)
	}
}

func TestNativeConfigDuplicatesKeepSemanticNodeAndCanonicalEdgeProvenance(t *testing.T) {
	output := t.TempDir()
	application := New("tasks")
	tasks := NewModule("tasks").Feature(Feature{
		ID: "tasks/manage", Name: "Task management", Outcome: "Users can manage tasks", Owner: "samples",
	})
	tasks.Use(&duplicateConfigDesignPluginB{})
	tasks.Use(&duplicateConfigDesignPluginA{})
	application.Use(tasks)
	if err := application.Describe(output, []string{describerNameDesign}); err != nil {
		t.Fatalf("describe duplicate config registrations: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatal(err)
	}
	graph, err := protofeatures.ParseDesignGraph(data)
	if err != nil {
		t.Fatal(err)
	}
	node := findProtocolDesignNode(graph.Nodes, "config:tasks")
	edge := findProtocolDesignEdge(graph.Edges, "config:tasks")
	if node == nil || node.Provenance != nil || edge == nil || edge.Provenance == nil {
		t.Fatalf("duplicate config projection = node %#v edge %#v", node, edge)
	}
	if !strings.Contains(edge.Provenance.Symbol, "duplicateConfigDesignPluginA") {
		t.Fatalf("edge provenance is not canonical: %#v", edge.Provenance)
	}
}

func findProtocolDesignNode(nodes []protofeatures.DesignNode, id string) *protofeatures.DesignNode {
	for index := range nodes {
		if nodes[index].ID == id {
			return &nodes[index]
		}
	}
	return nil
}

func findProtocolDesignEdge(edges []protofeatures.DesignEdge, to string) *protofeatures.DesignEdge {
	for index := range edges {
		if edges[index].From == "module:tasks/tasks" && edges[index].To == to && edges[index].Kind == protofeatures.DesignEdgeContains {
			return &edges[index]
		}
	}
	return nil
}

type designGraphTestPlugin struct{}

func (*designGraphTestPlugin) Name() string { return "design-test" }

func (*designGraphTestPlugin) ContributeDesign(builder *DesignBuilder) error {
	const serviceID = "service:task-store"
	if err := builder.AddNode(protofeatures.DesignNode{
		ID: serviceID, Kind: protofeatures.DesignNodeService, Name: "task-store",
	}); err != nil {
		return err
	}
	return builder.RelateFromModule(serviceID, protofeatures.DesignEdgeInjects, protofeatures.DesignAuthorityExact)
}

func TestApplicationDescribeConvergesDesignGraphInOnePass(t *testing.T) {
	output := t.TempDir()
	graphPath := filepath.Join(output, filepath.FromSlash(protofeatures.DesignGraphArtifact))

	withFeature := New("tasks")
	withFeature.Feature(Feature{
		ID:      "tasks/manage",
		Name:    "Task management",
		Outcome: "Users can manage tasks",
		Owner:   "samples",
	})
	withFeature.Use(&designGraphTestPlugin{})
	if err := withFeature.Describe(output, nil); err != nil {
		t.Fatalf("Describe with feature: %v", err)
	}

	data, err := os.ReadFile(graphPath)
	if err != nil {
		t.Fatalf("read design graph: %v", err)
	}
	graph, err := protofeatures.ParseDesignGraph(data)
	if err != nil {
		t.Fatalf("parse design graph: %v", err)
	}
	if graph.Project != "tasks" {
		t.Fatalf("project = %q, want tasks", graph.Project)
	}
	if len(graph.Nodes) != 3 || len(graph.Edges) != 2 {
		t.Fatalf("graph = %d nodes, %d edges; want 3 nodes, 2 edges", len(graph.Nodes), len(graph.Edges))
	}

	withoutFeature := New("tasks")
	if err := withoutFeature.Describe(output, nil); err != nil {
		t.Fatalf("Describe without feature: %v", err)
	}
	if _, err := os.Stat(graphPath); !os.IsNotExist(err) {
		t.Fatalf("stale design graph was not removed: %v", err)
	}
}

type designMigrationSource struct {
	namespace string
}

func (s designMigrationSource) Kind() migration.Kind { return migration.KindSQL }
func (s designMigrationSource) Namespace() string    { return s.namespace }

// designSchemaSource is the migration.SchemaContributor half of the seam; the
// bare designMigrationSource above deliberately stays outside that interface.
type designSchemaSource struct {
	designMigrationSource
	databases []infra.Database
}

func (s designSchemaSource) InfraDatabases() []infra.Database { return s.databases }

type designMigrationPlugin struct {
	sources []migration.Source
}

func (*designMigrationPlugin) Name() string { return "design-migrations" }
func (p *designMigrationPlugin) MigrationSources() []migration.Source {
	return p.sources
}

// describeDesignNodes runs a one-feature application through the real describe
// pipeline and returns its graph nodes keyed by ID.
func describeDesignNodes(t *testing.T, sources ...migration.Source) map[string]protofeatures.DesignNode {
	t.Helper()
	output := t.TempDir()
	application := New("tasks")
	application.Feature(Feature{
		ID: "tasks/manage", Name: "Task management",
		Outcome: "Users can manage tasks", Owner: "samples",
	})
	application.Use(&designMigrationPlugin{sources: sources})
	if err := application.Describe(output, []string{describerNameDesign}); err != nil {
		t.Fatalf("describe: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatalf("read design graph: %v", err)
	}
	graph, err := protofeatures.ParseDesignGraph(data)
	if err != nil {
		t.Fatalf("parse design graph: %v", err)
	}
	nodes := make(map[string]protofeatures.DesignNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	return nodes
}

func TestDesignGraphProjectsMigrationSourcesWithoutSchemaContribution(t *testing.T) {
	nodes := describeDesignNodes(t, designMigrationSource{namespace: "iam"})
	if _, ok := nodes["data.migration:sql:iam"]; !ok {
		t.Fatalf("migration node is missing: %#v", nodes)
	}
	for id, node := range nodes {
		if node.Kind == protofeatures.DesignNodeDataSchema {
			t.Fatalf("source without SchemaContributor produced schema node %q", id)
		}
	}
}

func TestDesignGraphProjectsDatabasesWithoutNamedSchemas(t *testing.T) {
	nodes := describeDesignNodes(t, designSchemaSource{
		designMigrationSource: designMigrationSource{namespace: "iam"},
		databases:             []infra.Database{{Name: "default", Engine: infra.EnginePostgres}},
	})
	node, ok := nodes["data.schema:default"]
	if !ok {
		t.Fatalf("unnamed-schema database node is missing: %#v", nodes)
	}
	if node.Name != "default" {
		t.Fatalf("node name = %q, want the datasource name", node.Name)
	}
	if _, present := node.Properties["schema"]; present {
		t.Fatalf("database without named schemas set a schema property: %#v", node.Properties)
	}
}

func TestDesignGraphProjectsEveryDeclaredDatabaseAndSchema(t *testing.T) {
	nodes := describeDesignNodes(t, designSchemaSource{
		designMigrationSource: designMigrationSource{namespace: "iam"},
		databases: []infra.Database{
			{Name: "default", Engine: infra.EnginePostgres, Schemas: []string{"public", "audit"}},
			{Name: "reporting", Engine: infra.EnginePostgres, Schemas: []string{"public"}},
		},
	})
	for _, id := range []string{
		"data.schema:default:public",
		"data.schema:default:audit",
		"data.schema:reporting:public",
	} {
		if _, ok := nodes[id]; !ok {
			t.Fatalf("schema node %q is missing: %#v", id, nodes)
		}
	}
}

func TestNormalizeCallerDesignPathConvergesTrimpathAndAbsoluteForms(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "internal", "feature.go")
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatalf("mkdir source directory: %v", err)
	}
	if err := os.WriteFile(source, []byte("package internal\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	for _, input := range []string{
		source,
		filepath.Join("internal", "feature.go"),
		filepath.Join("example.com", "service", "internal", "feature.go"),
	} {
		if got := normalizeCallerDesignPath(input, root); got != "internal/feature.go" {
			t.Errorf("normalizeCallerDesignPath(%q) = %q, want internal/feature.go", input, got)
		}
	}
	if got := normalizeCallerDesignPath(filepath.Join(t.TempDir(), "feature.go"), root); got != "" {
		t.Errorf("outside absolute path = %q, want empty provenance", got)
	}
	if got := normalizeCallerDesignPath("example.com/service/missing.go", root); got != "" {
		t.Errorf("missing trimpath source = %q, want empty provenance", got)
	}
}
