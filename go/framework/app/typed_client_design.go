package app

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	protofeatures "go.putnami.dev/protocol/features"
)

// typedClientLanguage is stamped by the framework rather than declared: a Go
// contributor is a Go client. Keeping it out of the descriptor keeps the node's
// property values derived from framework facts instead of free author text.
const typedClientLanguage = "go"

// TypedClientOperation is one operation a hand-written client calls on its
// producer. Path is the contract template, not a deployment URL: it is parsed
// and must carry no scheme, host, credentials, query, or fragment, so a base
// URL, host, token, or environment value can never reach the graph through this
// field. See validateTypedClientPath.
type TypedClientOperation struct {
	// OperationID is the producer's canonical operation identity, when the
	// client author knows it. It is optional and never guessed.
	OperationID string
	// Method is the HTTP method, canonicalized to upper case.
	Method string
	// Path is the producer's contract path template, e.g. "/v1/introspect".
	Path string
}

// TypedClientDesign is the build-time-only descriptor a hand-written client
// declares about the producer it calls. It carries declared identity only:
// tokens, bearer headers, request bodies, environment values, base URLs, and
// absolute paths have no field here and must never be smuggled into one.
//
// ProducerFeature is deliberately optional. When the client author cannot name
// the producer feature exactly, the derived consumer call edge is published
// with protofeatures.DesignAuthorityUnmodeled instead of a guessed lineage.
type TypedClientDesign struct {
	// ProducerProject is the semantic project identity that produces the API
	// this client calls. Required; the framework never infers it.
	ProducerProject string
	// ProducerFeature is the producer's feature ID. Optional: absent means the
	// relationship stays explicitly unmodeled.
	ProducerFeature string
	// Client is the hand-written client's stable name. Required.
	Client string
	// Operations is the client's declared operation table. It may be empty when
	// the author declares only the producer relationship.
	Operations []TypedClientOperation
}

type ownedTypedClient struct {
	owner  *Module
	client TypedClientContributor
}

// collectTypedClients returns every (owning module, typed client) pair in the
// tree. It deliberately does NOT reuse collectWithOwner: that helper
// de-duplicates by pointer identity, which is right for lifecycle capabilities
// but would silently drop every consumer after the first when one shared client
// instance is composed into several modules — exactly the relationship this
// projection exists to record.
func collectTypedClients(root *Module) []ownedTypedClient {
	var result []ownedTypedClient
	target := collectTarget[TypedClientContributor]()
	for _, module := range root.CollectModules() {
		for _, plugin := range module.plugins {
			if client, ok := plugin.(TypedClientContributor); ok {
				result = append(result, ownedTypedClient{owner: module, client: client})
			}
		}
		for _, contributed := range module.contributions {
			if !contributed.target.AssignableTo(target) {
				continue
			}
			if client, ok := contributed.value.(TypedClientContributor); ok {
				result = append(result, ownedTypedClient{owner: module, client: client})
			}
		}
	}
	return result
}

// contributeTypedClientDesign projects every typed client composed under a
// feature scope. The node is producer-scoped and folded across consumers; each
// consuming module contributes its own `calls` edge.
func (builder *designGraphBuilder) contributeTypedClientDesign(root *Module) error {
	for _, owned := range collectTypedClients(root) {
		moduleID := builder.moduleIDs[owned.owner]
		featureID := builder.featureIDs[owned.owner]
		if moduleID == "" || featureID == "" {
			continue
		}
		design, err := normalizeTypedClientDesign(owned.client.Name(), owned.client.TypedClientDesign())
		if err != nil {
			return err
		}
		nodeID := typedClientNodeID(design)
		if err := builder.addNode(typedClientNode(nodeID, design)); err != nil {
			return err
		}
		// The consumer's lineage is complete only when the descriptor names both
		// halves of it. A missing producer feature stays visibly unmodeled rather
		// than being reconstructed from the producer project.
		authority := protofeatures.DesignAuthorityExact
		if design.ProducerFeature == "" {
			authority = protofeatures.DesignAuthorityUnmodeled
		}
		if err := builder.addEdge(protofeatures.DesignEdge{
			From: moduleID, To: nodeID, Kind: protofeatures.DesignEdgeCalls, Authority: authority,
		}); err != nil {
			return err
		}
		// Operation nodes exist only in the producer project's own graph. Decide
		// on the declared producer project, which is known before any contributor
		// runs: keying on "is the node already in the builder" would make the
		// artifact depend on contributor order, and would make every cross-project
		// consumer spam finish()'s dropped-relationship warning. Cross-project
		// consumers carry the same operations as bounded node properties instead.
		if design.ProducerProject != builder.graph.Project {
			continue
		}
		for _, operation := range design.Operations {
			edge := protofeatures.DesignEdge{
				From:      nodeID,
				To:        "api.operation:" + operation.Method + ":" + operation.Path,
				Kind:      protofeatures.DesignEdgeCalls,
				Authority: protofeatures.DesignAuthorityExact,
			}
			if operation.OperationID != "" {
				edge.Properties = map[string]string{"operationId": operation.OperationID}
			}
			if err := builder.addEdge(edge); err != nil {
				return err
			}
		}
	}
	return nil
}

func typedClientNodeID(design TypedClientDesign) string {
	return "client.typed:" + design.ProducerProject + ":" + design.Client
}

// typedClientNode emits the bounded property key set: producer, language, the
// producer feature when it is declared, and the canonical operation list. A
// missing producer feature omits the key entirely — writing a placeholder there
// would read as a feature ID to every consumer of the graph.
func typedClientNode(nodeID string, design TypedClientDesign) protofeatures.DesignNode {
	properties := map[string]string{
		"producer": design.ProducerProject,
		"language": typedClientLanguage,
	}
	if design.ProducerFeature != "" {
		properties["feature"] = design.ProducerFeature
	}
	if operations := typedClientOperationList(design.Operations); operations != "" {
		properties["operations"] = operations
	}
	return protofeatures.DesignNode{
		ID: nodeID, Kind: protofeatures.DesignNodeTypedClient, Name: design.Client, Properties: properties,
	}
}

// typedClientOperationList renders the declared operations in the canonical
// "METHOD path" form, sorted, so the node property is order-independent.
func typedClientOperationList(operations []TypedClientOperation) string {
	labels := make([]string, 0, len(operations))
	for _, operation := range operations {
		labels = append(labels, operation.Method+" "+operation.Path)
	}
	sort.Strings(labels)
	return strings.Join(labels, ", ")
}

// validateTypedClientPath enforces that a declared operation path is the
// producer's contract template and nothing else.
//
// A leading-slash check alone is not enough: "//identity.internal.example.com/v1"
// is a scheme-relative URL that carries a host while still starting with "/",
// so it would publish a deployment address into the graph. Parsing and
// requiring an empty scheme, host, and userinfo closes that off structurally
// rather than by pattern-matching the shapes we happened to think of.
//
// Query and fragment are rejected for the same reason the descriptor has no
// header or body field: "/v1/introspect?access_token=..." is exactly the
// credential leak the bounded property set exists to prevent. Router template
// syntax ("[id]", "{id}", ":id") parses with an empty host and stays valid.
func validateTypedClientPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf(`must be the producer's contract template starting with "/", never a deployment URL`)
	}
	// The parse below already rejects this via the host check; leading "//" is
	// called out first only because "you wrote one slash too many" is a far more
	// actionable message than "must not carry a host".
	if strings.HasPrefix(path, "//") {
		return fmt.Errorf(`must not start with "//": that is a scheme-relative URL carrying a host, not a contract template`)
	}
	// url.Parse keeps a backslash in Path, but browsers and some proxies read
	// "/\host" as scheme-relative. The protocol's own path rules already forbid
	// backslashes, so reject it rather than rely on one parser's reading.
	if strings.Contains(path, `\`) {
		return fmt.Errorf("must not contain a backslash")
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("must be a parseable contract template: %w", err)
	}
	if parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil {
		return fmt.Errorf("must not carry a scheme, host, or credentials; it is a contract template, never a deployment URL")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("must not carry a query string, which can smuggle a token or environment value into the graph")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("must not carry a fragment")
	}
	return nil
}

// normalizeTypedClientDesign trims and canonicalizes a descriptor. An
// incomplete descriptor is an author error with an actionable message, not an
// unmodeled edge: only a missing producer feature is a legitimate "not modeled
// yet" answer.
func normalizeTypedClientDesign(plugin string, design TypedClientDesign) (TypedClientDesign, error) {
	normalized := TypedClientDesign{
		ProducerProject: strings.TrimSpace(design.ProducerProject),
		ProducerFeature: strings.TrimSpace(design.ProducerFeature),
		Client:          strings.TrimSpace(design.Client),
	}
	if normalized.ProducerProject == "" {
		return TypedClientDesign{}, fmt.Errorf(
			"typed client design from plugin %q: ProducerProject is required; name the project that produces the API this client calls (the framework never infers it from a URL, package, or import)", plugin)
	}
	if normalized.Client == "" {
		return TypedClientDesign{}, fmt.Errorf(
			"typed client design from plugin %q: Client is required; give the hand-written client a stable name so its graph identity survives refactors", plugin)
	}
	normalized.Operations = make([]TypedClientOperation, 0, len(design.Operations))
	for index, operation := range design.Operations {
		method := strings.ToUpper(strings.TrimSpace(operation.Method))
		path := strings.TrimSpace(operation.Path)
		if method == "" {
			return TypedClientDesign{}, fmt.Errorf(
				"typed client design from plugin %q: Operations[%d].Method is required", plugin, index)
		}
		if err := validateTypedClientPath(path); err != nil {
			return TypedClientDesign{}, fmt.Errorf(
				"typed client design from plugin %q: Operations[%d].Path %q %w", plugin, index, path, err)
		}
		normalized.Operations = append(normalized.Operations, TypedClientOperation{
			OperationID: strings.TrimSpace(operation.OperationID), Method: method, Path: path,
		})
	}
	return normalized, nil
}
