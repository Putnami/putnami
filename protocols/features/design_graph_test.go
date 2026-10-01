package features

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestDesignGraphCanonicalGoldenAndRoundTrip(t *testing.T) {
	graph := &DesignGraph{
		Compatibility: DesignGraphCompatibility,
		Project:       "samples/tasks",
		Nodes: []DesignNode{
			{ID: "module:tasks", Kind: DesignNodeModule, Name: "tasks"},
			{ID: "feature:tasks/manage", Kind: DesignNodeFeature, Name: "Task management", Properties: map[string]string{
				"owner": "samples", "outcome": "Users can create, list, update and complete tasks",
			}, Provenance: &DesignProvenance{Path: "src/tasks/tasks.module.ts", Line: 4, Symbol: "tasks"}},
			{ID: "api.operation:POST:/api/tasks", Kind: DesignNodeAPIOperation, Name: "POST /api/tasks", Properties: map[string]string{
				"method": "POST", "path": "/api/tasks", "operationId": "createTask",
			}},
		},
		Edges: []DesignEdge{
			{From: "module:tasks", To: "api.operation:POST:/api/tasks", Kind: DesignEdgeExposes, Authority: DesignAuthorityExact},
			{From: "feature:tasks/manage", To: "module:tasks", Kind: DesignEdgeImplementedBy, Authority: DesignAuthorityExact},
		},
	}

	before := *graph
	before.Nodes = append([]DesignNode(nil), graph.Nodes...)
	before.Edges = append([]DesignEdge(nil), graph.Edges...)
	data, err := MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("fixtures", "design", "tasks.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("canonical graph differs\n%s", data)
	}
	if !reflect.DeepEqual(*graph, before) {
		t.Fatal("canonicalization mutated caller graph")
	}
	parsed, err := ParseDesignGraph(data)
	if err != nil {
		t.Fatal(err)
	}
	again, err := MarshalDesignGraph(parsed)
	if err != nil || !bytes.Equal(again, data) {
		t.Fatalf("round trip changed bytes: %v", err)
	}
}

func TestDesignGraphRejectsDanglingAndAmbiguousRelationships(t *testing.T) {
	base := DesignGraph{
		Compatibility: DesignGraphCompatibility,
		Project:       "samples/tasks",
		Nodes:         []DesignNode{{ID: "feature:tasks/manage", Kind: DesignNodeFeature, Name: "Task management"}},
	}
	base.Edges = []DesignEdge{{From: "feature:tasks/manage", To: "module:missing", Kind: DesignEdgeImplementedBy, Authority: DesignAuthorityExact}}
	if err := ValidateDesignGraph(&base); err == nil {
		t.Fatal("dangling edge accepted")
	}
	base.Edges = nil
	base.Nodes = append(base.Nodes, base.Nodes[0])
	if err := ValidateDesignGraph(&base); err == nil {
		t.Fatal("duplicate node accepted")
	}
}

// A hand-written typed client and a generated client are separate kinds so a
// hand-written call can never be read as generated-client authority, and
// currently-unmodeled is a first-class authority rather than an unknown value a
// consumer may round up to exact.
func TestDesignGraphAcceptsTypedClientsAndUnmodeledAuthority(t *testing.T) {
	graph := DesignGraph{
		Compatibility: DesignGraphCompatibility,
		Project:       "cloud/identity",
		Nodes: []DesignNode{
			{ID: "module:control-plane", Kind: DesignNodeModule, Name: "control-plane"},
			{ID: "client.typed:cloud/identity:introspectauth", Kind: DesignNodeTypedClient, Name: "introspectauth", Properties: map[string]string{
				"producer": "cloud/identity", "language": "go", "operations": "POST /v1/introspect",
			}},
		},
		Edges: []DesignEdge{
			{From: "module:control-plane", To: "client.typed:cloud/identity:introspectauth", Kind: DesignEdgeCalls, Authority: DesignAuthorityUnmodeled},
		},
	}
	if err := ValidateDesignGraph(&graph); err != nil {
		t.Fatalf("typed client with unmodeled authority rejected: %v", err)
	}
	data, err := MarshalDesignGraph(&graph)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again, err := MarshalDesignGraph(&graph)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("canonical marshaling is not deterministic: %v", err)
	}
	parsed, err := ParseDesignGraph(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Edges[0].Authority != DesignAuthorityUnmodeled {
		t.Fatalf("authority = %q, want %q", parsed.Edges[0].Authority, DesignAuthorityUnmodeled)
	}

	graph.Nodes[1].Kind = "client.handwritten"
	if err := ValidateDesignGraph(&graph); err == nil {
		t.Fatal("out-of-vocabulary node kind accepted")
	}
	graph.Nodes[1].Kind = DesignNodeTypedClient
	graph.Edges[0].Authority = "unknown"
	if err := ValidateDesignGraph(&graph); err == nil {
		t.Fatal("out-of-vocabulary authority accepted")
	}
}

// Both ordered vocabularies must list every declared constant: the CLI derives
// its exhaustiveness gate and its authority ladder from them, so a constant
// missing here silently drops out of both.
func TestOrderedDesignVocabulariesCoverEveryConstant(t *testing.T) {
	kinds := OrderedDesignNodeKinds()
	for _, kind := range []DesignNodeKind{
		DesignNodeFeature, DesignNodeModule, DesignNodeAPIOperation, DesignNodeAPISchema,
		DesignNodeService, DesignNodeDataSchema, DesignNodeDataTable, DesignNodeDataMigration,
		DesignNodeEventTopic, DesignNodeEventOutbox, DesignNodeEventHandler, DesignNodeClient, DesignNodeTypedClient,
		DesignNodeProject, DesignNodeCommand, DesignNodeConfig, DesignNodeInfra,
		DesignNodeLifecycle, DesignNodeTest,
	} {
		if !slices.Contains(kinds, kind) {
			t.Errorf("node kind %q is declared but not in the ordered vocabulary", kind)
		}
	}
	authorities := OrderedDesignAuthorities()
	want := []DesignAuthority{
		DesignAuthorityExact, DesignAuthorityDerived, DesignAuthorityHeuristic, DesignAuthorityUnmodeled,
	}
	if !slices.Equal(authorities, want) {
		t.Fatalf("OrderedDesignAuthorities() = %v, want strongest-first %v", authorities, want)
	}
	for _, authority := range authorities {
		if !validDesignAuthorities[authority] {
			t.Errorf("ordered authority %q is not accepted by validation", authority)
		}
	}
	if len(validDesignAuthorities) != len(authorities) {
		t.Errorf("validation accepts %d authorities, ordered vocabulary has %d", len(validDesignAuthorities), len(authorities))
	}
	authorities[0] = "mutated"
	if OrderedDesignAuthorities()[0] != DesignAuthorityExact {
		t.Error("OrderedDesignAuthorities returned an aliased slice")
	}
}

func TestDesignGraphAcceptsNativeCompositionFactsAsTechnicalNodes(t *testing.T) {
	graph := DesignGraph{
		Compatibility: DesignGraphCompatibility,
		Project:       "samples/tasks",
		Nodes: []DesignNode{
			{ID: "module:tasks", Kind: DesignNodeModule, Name: "tasks"},
			{ID: "config:tasks", Kind: DesignNodeConfig, Name: "tasks", Properties: map[string]string{"path": "tasks"}},
			{ID: "infra:database:tasks", Kind: DesignNodeInfra, Name: "tasks", Properties: map[string]string{"kind": "database"}},
			{ID: "lifecycle:start:module:tasks", Kind: DesignNodeLifecycle, Name: "start", Properties: map[string]string{"phase": "start"}},
			{ID: "test:integration:tasks/create", Kind: DesignNodeTest, Name: "tasks/create", Properties: map[string]string{"kind": "integration"}},
		},
		Edges: []DesignEdge{
			{From: "module:tasks", To: "config:tasks", Kind: DesignEdgeContains, Authority: DesignAuthorityExact},
			{From: "module:tasks", To: "infra:database:tasks", Kind: DesignEdgeContains, Authority: DesignAuthorityExact},
			{From: "module:tasks", To: "lifecycle:start:module:tasks", Kind: DesignEdgeContains, Authority: DesignAuthorityExact},
			{From: "module:tasks", To: "test:integration:tasks/create", Kind: DesignEdgeContains, Authority: DesignAuthorityExact},
		},
	}
	if err := ValidateDesignGraph(&graph); err != nil {
		t.Fatalf("native composition facts rejected: %v", err)
	}
}

// A schema is the datasource namespace a migration writes into; a table is one
// relation inside it. They must stay separate kinds so a table named after a
// schema on the same datasource cannot mint a colliding node ID.
func TestDesignGraphSeparatesSchemaAndTableNodes(t *testing.T) {
	graph := DesignGraph{
		Compatibility: DesignGraphCompatibility,
		Project:       "samples/tasks",
		Nodes: []DesignNode{
			{ID: "data.schema:default:audit", Kind: DesignNodeDataSchema, Name: "audit", Properties: map[string]string{
				"datasource": "default", "engine": "postgres", "schema": "audit",
			}},
			{ID: "data.table:default:audit", Kind: DesignNodeDataTable, Name: "audit", Properties: map[string]string{
				"datasource": "default", "columns": "id,message",
			}},
		},
	}
	if err := ValidateDesignGraph(&graph); err != nil {
		t.Fatalf("same-named schema and table rejected: %v", err)
	}
}

// A transactional outbox row is durable at commit time; the topic is published
// only once a relay claims that row. The vocabulary must therefore accept both
// relationships from one producer without collapsing them, and a kind added to
// the constants but omitted from designNodeKinds must stay rejected on the wire.
func TestDesignGraphSeparatesOutboxEnqueueFromTransportPublication(t *testing.T) {
	graph := DesignGraph{
		Compatibility: DesignGraphCompatibility,
		Project:       "samples/identity",
		Nodes: []DesignNode{
			{ID: "module:samples/identity/sessions", Kind: DesignNodeModule, Name: "sessions"},
			{ID: "event.outbox:identity", Kind: DesignNodeEventOutbox, Name: "identity", Properties: map[string]string{
				"table": "auth.event_outbox", "datasource": "identity",
			}},
			{ID: "event.topic:identity.session.revoked", Kind: DesignNodeEventTopic, Name: "identity.session.revoked"},
			{ID: "event.handler:src/events/revoked.on.ts:identity.session.revoked", Kind: DesignNodeEventHandler, Name: "src/events/revoked.on.ts"},
		},
		Edges: []DesignEdge{
			{From: "module:samples/identity/sessions", To: "event.outbox:identity", Kind: DesignEdgeEnqueues, Authority: DesignAuthorityExact},
			{From: "event.outbox:identity", To: "event.topic:identity.session.revoked", Kind: DesignEdgePublishes, Authority: DesignAuthorityExact},
			{From: "module:samples/identity/sessions", To: "event.topic:identity.session.revoked", Kind: DesignEdgePublishes, Authority: DesignAuthorityDerived},
			{From: "event.handler:src/events/revoked.on.ts:identity.session.revoked", To: "event.topic:identity.session.revoked", Kind: DesignEdgeSubscribes, Authority: DesignAuthorityExact},
		},
	}
	if err := ValidateDesignGraph(&graph); err != nil {
		t.Fatalf("outbox enqueue and transport publication rejected: %v", err)
	}

	ordered := OrderedDesignNodeKinds()
	if !slices.Contains(ordered, DesignNodeEventOutbox) {
		t.Fatalf("event.outbox is missing from the ordered vocabulary: %v", ordered)
	}
	for _, kind := range ordered {
		if !validDesignNodeKinds[kind] {
			t.Fatalf("ordered node kind %q is not accepted on the wire", kind)
		}
	}
}

// Projects and commands are workspace-level technical kinds minted by tooling
// from native workspace declarations. They must be accepted on the wire as
// their own kinds — never represented as features — and a project dependency
// must stay a direct dependsOn edge rather than an invented DI relationship.
func TestDesignGraphAcceptsProjectAndCommandNodes(t *testing.T) {
	graph := DesignGraph{
		Compatibility: DesignGraphCompatibility,
		Project:       "samples/tasks",
		Nodes: []DesignNode{
			{ID: "project:samples/tasks", Kind: DesignNodeProject, Name: "samples/tasks", Properties: map[string]string{
				"type": "application", "path": "typescript/samples/13-fullstack-app",
			}, Provenance: &DesignProvenance{Path: "putnami.json"}},
			{ID: "project:@putnami/utils", Kind: DesignNodeProject, Name: "@putnami/utils"},
			{ID: "command:tasks-admin", Kind: DesignNodeCommand, Name: "tasks-admin", Properties: map[string]string{
				"entry": "src/bin/admin.ts",
			}, Provenance: &DesignProvenance{Path: "putnami.json"}},
			{ID: "module:tasks", Kind: DesignNodeModule, Name: "tasks"},
		},
		Edges: []DesignEdge{
			{From: "project:samples/tasks", To: "module:tasks", Kind: DesignEdgeContains, Authority: DesignAuthorityExact},
			{From: "project:samples/tasks", To: "command:tasks-admin", Kind: DesignEdgeExposes, Authority: DesignAuthorityExact},
			{From: "project:samples/tasks", To: "project:@putnami/utils", Kind: DesignEdgeDependsOn, Authority: DesignAuthorityExact},
		},
	}
	if err := ValidateDesignGraph(&graph); err != nil {
		t.Fatalf("project and command nodes rejected: %v", err)
	}

	unknownEdge := graph
	unknownEdge.Edges = append(append([]DesignEdge(nil), graph.Edges...), DesignEdge{
		From: "project:@putnami/utils", To: "module:tasks", Kind: "provides", Authority: DesignAuthorityExact,
	})
	if err := ValidateDesignGraph(&unknownEdge); err == nil {
		t.Fatal("unknown edge kind accepted between workspace nodes")
	}
}
