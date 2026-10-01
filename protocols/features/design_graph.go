package features

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// DesignGraphCompatibility deliberately prevents the derived design graph from
// being mistaken for a durable protocol. The graph is a disposable build
// projection: native framework declarations remain its source of truth.
const DesignGraphCompatibility = "provisional"

// DesignGraphArtifact is the path, relative to a build output directory, used
// by every framework producer and tooling consumer.
const DesignGraphArtifact = "design/graph.json"

// DesignNodeKind is the bounded vocabulary shared by native framework
// contributors. New kinds may be added while the projection is provisional.
type DesignNodeKind string

// Supported design node kinds for the provisional graph.
//
// DesignNodeDataSchema and DesignNodeDataTable are deliberately distinct: a
// schema node is the datasource namespace a migration writes into, while a
// table node is one relation declared inside it. Sharing one kind would let a
// table named after a schema on the same datasource mint a colliding node ID.
//
// DesignNodeEventOutbox is distinct from DesignNodeEventTopic for the same
// reason a commit is not a delivery: a transactional outbox row is durable the
// moment its writing transaction commits, while the topic is only published
// once a relay claims that row. Folding the two would make the graph claim a
// transport publication the runtime has not performed yet.
//
// DesignNodeClient and DesignNodeTypedClient are likewise distinct: a generated
// client is built from an immutable producer contract, while a typed client is
// hand-written and only declares which producer it calls. Sharing one kind
// would let a hand-written call claim generated-client authority.
//
// DesignNodeProject and DesignNodeCommand are workspace-level technical kinds
// minted by tooling from native workspace declarations (putnami.json), never by
// framework producers: a framework only knows its own project, and a command
// exists only where a project declares a binary entrypoint. They are distinct
// kinds precisely so a project or command can never be promoted to a product
// feature by naming convention — only an authored feature declaration mints a
// DesignNodeFeature.
//
// Config, infra, lifecycle, and test stay separate technical kinds because
// none of them is product intent. Their native declarations are projected
// under an authored feature scope with ordinary contains edges; a name match
// never promotes one into a feature.
const (
	DesignNodeFeature       DesignNodeKind = "feature"
	DesignNodeModule        DesignNodeKind = "module"
	DesignNodeAPIOperation  DesignNodeKind = "api.operation"
	DesignNodeAPISchema     DesignNodeKind = "api.schema"
	DesignNodeService       DesignNodeKind = "service"
	DesignNodeDataSchema    DesignNodeKind = "data.schema"
	DesignNodeDataTable     DesignNodeKind = "data.table"
	DesignNodeDataMigration DesignNodeKind = "data.migration"
	DesignNodeEventTopic    DesignNodeKind = "event.topic"
	DesignNodeEventOutbox   DesignNodeKind = "event.outbox"
	DesignNodeEventHandler  DesignNodeKind = "event.handler"
	DesignNodeClient        DesignNodeKind = "client.generated"
	DesignNodeTypedClient   DesignNodeKind = "client.typed"
	DesignNodeProject       DesignNodeKind = "project"
	DesignNodeCommand       DesignNodeKind = "command"
	DesignNodeConfig        DesignNodeKind = "config"
	DesignNodeInfra         DesignNodeKind = "infra"
	DesignNodeLifecycle     DesignNodeKind = "lifecycle"
	DesignNodeTest          DesignNodeKind = "test"
)

// designNodeKinds is the ordered vocabulary. It is the single list both
// ValidateDesignGraph and OrderedDesignNodeKinds read, so a kind added to the
// constants above but omitted here is rejected on the wire rather than silently
// half-supported.
var designNodeKinds = [...]DesignNodeKind{
	DesignNodeFeature, DesignNodeModule, DesignNodeAPIOperation, DesignNodeAPISchema,
	DesignNodeService, DesignNodeDataSchema, DesignNodeDataTable, DesignNodeDataMigration,
	DesignNodeEventTopic, DesignNodeEventOutbox, DesignNodeEventHandler,
	DesignNodeClient, DesignNodeTypedClient, DesignNodeProject, DesignNodeCommand,
	DesignNodeConfig, DesignNodeInfra, DesignNodeLifecycle, DesignNodeTest,
}

// OrderedDesignNodeKinds returns every supported node kind in declaration
// order. Projections over the graph use it to prove they handle the whole
// vocabulary instead of hardcoding a subset that silently rots as the
// provisional graph grows.
func OrderedDesignNodeKinds() []DesignNodeKind {
	return append([]DesignNodeKind(nil), designNodeKinds[:]...)
}

// DesignEdgeKind describes one useful design relationship. Edges point in the
// direction a reader naturally follows from product intent to implementation.
type DesignEdgeKind string

// Supported design edge kinds for the provisional graph.
//
// DesignEdgeDependsOn carries one direct project-to-project workspace
// dependency. It is deliberately distinct from DesignEdgeInjects (a DI
// relationship between services) and never transitive: an indirect dependency
// is absent rather than invented.
const (
	DesignEdgeImplementedBy DesignEdgeKind = "implementedBy"
	DesignEdgeContains      DesignEdgeKind = "contains"
	DesignEdgeExposes       DesignEdgeKind = "exposes"
	DesignEdgeInjects       DesignEdgeKind = "injects"
	DesignEdgeAccepts       DesignEdgeKind = "accepts"
	DesignEdgeReturns       DesignEdgeKind = "returns"
	DesignEdgeReads         DesignEdgeKind = "reads"
	DesignEdgeWrites        DesignEdgeKind = "writes"
	DesignEdgeEnqueues      DesignEdgeKind = "enqueues"
	DesignEdgePublishes     DesignEdgeKind = "publishes"
	DesignEdgeSubscribes    DesignEdgeKind = "subscribes"
	DesignEdgeGeneratedFrom DesignEdgeKind = "generatedFrom"
	DesignEdgeCalls         DesignEdgeKind = "calls"
	DesignEdgeDependsOn     DesignEdgeKind = "dependsOn"
)

// DesignAuthority makes inference visible instead of presenting every edge as
// equally certain.
type DesignAuthority string

// Supported authority levels, from native fact to explicit inference.
//
// DesignAuthorityUnmodeled is the weakest level and is not a guess: the
// framework holds a native declaration that the relationship exists but cannot
// prove its producer lineage exactly, and refuses to invent the missing half.
// It is deliberately distinct from DesignAuthorityHeuristic, which claims an
// inference the framework did make.
const (
	DesignAuthorityExact     DesignAuthority = "exact"
	DesignAuthorityDerived   DesignAuthority = "derived"
	DesignAuthorityHeuristic DesignAuthority = "heuristic"
	DesignAuthorityUnmodeled DesignAuthority = "currently-unmodeled"
)

// designAuthorities is the ordered vocabulary, strongest first. It is the
// single list ValidateDesignGraph and OrderedDesignAuthorities read, so an
// authority added to the constants above but omitted here is rejected on the
// wire rather than silently half-supported.
var designAuthorities = [...]DesignAuthority{
	DesignAuthorityExact, DesignAuthorityDerived, DesignAuthorityHeuristic, DesignAuthorityUnmodeled,
}

// OrderedDesignAuthorities returns every supported authority strongest first.
// Projections that weaken an authority along a path derive their ladder from
// this list instead of hardcoding one, so a new level cannot be silently
// promoted to exact by an unaware consumer.
func OrderedDesignAuthorities() []DesignAuthority {
	return append([]DesignAuthority(nil), designAuthorities[:]...)
}

// DesignGraph is the compact, framework-neutral build projection consumed by
// features inspect. It is intentionally not a versioned authored document.
type DesignGraph struct {
	// Compatibility identifies the provisional graph contract understood by consumers.
	Compatibility string `json:"compatibility"`
	// Project is the semantic identity of the project represented by the graph.
	Project string `json:"project"`
	// Nodes contains every product-intent and technical-surface vertex.
	Nodes []DesignNode `json:"nodes"`
	// Edges contains the directed relationships between Nodes.
	Edges []DesignEdge `json:"edges"`
}

// DesignNode represents either product intent or one native technical surface.
// Properties contain kind-specific scalar facts such as an HTTP method or a
// feature outcome; keeping these open is appropriate while the graph evolves.
type DesignNode struct {
	// ID is the stable graph-local identity referenced by edge endpoints.
	ID string `json:"id"`
	// Kind classifies the product or technical surface represented by the node.
	Kind DesignNodeKind `json:"kind"`
	// Name is the concise human-readable label for the node.
	Name string `json:"name"`
	// Properties carries kind-specific scalar facts about the node.
	Properties map[string]string `json:"properties,omitempty"`
	// Provenance locates the native declaration that produced the node.
	Provenance *DesignProvenance `json:"provenance,omitempty"`
}

// DesignEdge connects two nodes and records how strongly the framework can
// prove that relationship.
type DesignEdge struct {
	// From is the ID of the relationship's source node.
	From string `json:"from"`
	// To is the ID of the relationship's destination node.
	To string `json:"to"`
	// Kind classifies the relationship between From and To.
	Kind DesignEdgeKind `json:"kind"`
	// Authority records how strongly the framework can prove the relationship.
	Authority DesignAuthority `json:"authority"`
	// Properties carries relationship-specific scalar facts.
	Properties map[string]string `json:"properties,omitempty"`
	// Provenance locates the native declaration that produced the edge.
	Provenance *DesignProvenance `json:"provenance,omitempty"`
}

// DesignProvenance locates the native declaration that produced a node or
// edge. Paths are project-relative slash paths when a producer can resolve
// them; Symbol and Line remain optional.
type DesignProvenance struct {
	// Path is the project-relative slash path of the native declaration.
	Path string `json:"path"`
	// Line is the optional one-based source line of the native declaration.
	Line int `json:"line,omitempty"`
	// Symbol is the optional declaration name within Path.
	Symbol string `json:"symbol,omitempty"`
}

var validDesignNodeKinds = func() map[DesignNodeKind]bool {
	valid := make(map[DesignNodeKind]bool, len(designNodeKinds))
	for _, kind := range designNodeKinds {
		valid[kind] = true
	}
	return valid
}()

var validDesignEdgeKinds = map[DesignEdgeKind]bool{
	DesignEdgeImplementedBy: true, DesignEdgeContains: true, DesignEdgeExposes: true,
	DesignEdgeInjects: true, DesignEdgeAccepts: true, DesignEdgeReturns: true,
	DesignEdgeReads: true, DesignEdgeWrites: true, DesignEdgeEnqueues: true,
	DesignEdgePublishes: true, DesignEdgeSubscribes: true, DesignEdgeGeneratedFrom: true,
	DesignEdgeCalls: true, DesignEdgeDependsOn: true,
}

var validDesignAuthorities = func() map[DesignAuthority]bool {
	valid := make(map[DesignAuthority]bool, len(designAuthorities))
	for _, authority := range designAuthorities {
		valid[authority] = true
	}
	return valid
}()

// ValidateDesignGraph rejects ambiguity and dangling relationships before a
// producer publishes the projection.
func ValidateDesignGraph(graph *DesignGraph) error {
	if graph == nil {
		return fmt.Errorf("design graph is required")
	}
	if graph.Compatibility != DesignGraphCompatibility {
		return fmt.Errorf("design graph compatibility %q is unsupported", graph.Compatibility)
	}
	if strings.TrimSpace(graph.Project) == "" {
		return fmt.Errorf("design graph project is required")
	}
	nodes := make(map[string]struct{}, len(graph.Nodes))
	for i, node := range graph.Nodes {
		if strings.TrimSpace(node.ID) == "" {
			return fmt.Errorf("nodes[%d].id is required", i)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return fmt.Errorf("duplicate design node %q", node.ID)
		}
		nodes[node.ID] = struct{}{}
		if !validDesignNodeKinds[node.Kind] {
			return fmt.Errorf("nodes[%d].kind %q is unsupported", i, node.Kind)
		}
		if strings.TrimSpace(node.Name) == "" {
			return fmt.Errorf("nodes[%d].name is required", i)
		}
		if err := validateDesignProvenance(node.Provenance); err != nil {
			return fmt.Errorf("nodes[%d].provenance: %w", i, err)
		}
	}
	edges := make(map[string]struct{}, len(graph.Edges))
	for i, edge := range graph.Edges {
		if _, ok := nodes[edge.From]; !ok {
			return fmt.Errorf("edges[%d].from references missing node %q", i, edge.From)
		}
		if _, ok := nodes[edge.To]; !ok {
			return fmt.Errorf("edges[%d].to references missing node %q", i, edge.To)
		}
		if !validDesignEdgeKinds[edge.Kind] {
			return fmt.Errorf("edges[%d].kind %q is unsupported", i, edge.Kind)
		}
		if !validDesignAuthorities[edge.Authority] {
			return fmt.Errorf("edges[%d].authority %q is unsupported", i, edge.Authority)
		}
		key := edge.From + "\x00" + edge.To + "\x00" + string(edge.Kind)
		if _, duplicate := edges[key]; duplicate {
			return fmt.Errorf("duplicate design edge %q -> %q (%s)", edge.From, edge.To, edge.Kind)
		}
		edges[key] = struct{}{}
		if err := validateDesignProvenance(edge.Provenance); err != nil {
			return fmt.Errorf("edges[%d].provenance: %w", i, err)
		}
	}
	return nil
}

func validateDesignProvenance(provenance *DesignProvenance) error {
	if provenance == nil {
		return nil
	}
	if strings.TrimSpace(provenance.Path) == "" {
		return fmt.Errorf("path is required")
	}
	if provenance.Line < 0 {
		return fmt.Errorf("line must not be negative")
	}
	return nil
}

// CanonicalDesignGraph returns a deeply copied graph with deterministic node
// and edge ordering, without mutating caller-owned slices or maps.
func CanonicalDesignGraph(input *DesignGraph) *DesignGraph {
	if input == nil {
		return nil
	}
	out := *input
	out.Nodes = append([]DesignNode(nil), input.Nodes...)
	for i := range out.Nodes {
		out.Nodes[i].Properties = cloneStringMap(out.Nodes[i].Properties)
		if out.Nodes[i].Provenance != nil {
			copy := *out.Nodes[i].Provenance
			out.Nodes[i].Provenance = &copy
		}
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	out.Edges = append([]DesignEdge(nil), input.Edges...)
	for i := range out.Edges {
		out.Edges[i].Properties = cloneStringMap(out.Edges[i].Properties)
		if out.Edges[i].Provenance != nil {
			copy := *out.Edges[i].Provenance
			out.Edges[i].Provenance = &copy
		}
	}
	sort.Slice(out.Edges, func(i, j int) bool {
		left, right := out.Edges[i], out.Edges[j]
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Authority < right.Authority
	})
	return &out
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// MarshalDesignGraph validates and serializes deterministic two-space-indented
// JSON with one trailing newline.
func MarshalDesignGraph(graph *DesignGraph) ([]byte, error) {
	if err := ValidateDesignGraph(graph); err != nil {
		return nil, err
	}
	return marshalCanonical(CanonicalDesignGraph(graph))
}

// ParseDesignGraph strictly reads one derived graph artifact.
func ParseDesignGraph(data []byte) (*DesignGraph, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var graph DesignGraph
	if err := decoder.Decode(&graph); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, err
	}
	if err := ValidateDesignGraph(&graph); err != nil {
		return nil, err
	}
	return &graph, nil
}
