package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	protofeatures "go.putnami.dev/protocol/features"
)

const (
	introspectProducerProject = "cloud/identity"
	introspectProducerFeature = "identity/opaque-tokens"
	introspectClientNodeID    = "client.typed:cloud/identity:introspectauth"
)

// introspectionClientStub stands in for Cloud's hand-written introspection
// client: it holds real runtime configuration (a bearer token, an environment
// value, a deployment base URL) and declares only producer identity.
type introspectionClientStub struct {
	design    TypedClientDesign
	baseURL   string
	token     string
	tokenEnv  string
	configure int
	start     int
	stop      int
}

func (*introspectionClientStub) Name() string { return "introspectauth" }

func (c *introspectionClientStub) TypedClientDesign() TypedClientDesign { return c.design }

type introspectionLifecycleClient struct{ introspectionClientStub }

func (c *introspectionLifecycleClient) Configure(context.Context, *Module) error {
	c.configure++
	return nil
}
func (c *introspectionLifecycleClient) Start(context.Context, *Module) error { c.start++; return nil }
func (c *introspectionLifecycleClient) Stop(context.Context, *Module) error  { c.stop++; return nil }

// designOperationsPlugin mimics the api plugin's operation nodes so an
// in-project producer and its typed consumers can be exercised in one graph
// without app depending on the api framework.
type designOperationsPlugin struct{ operations []TypedClientOperation }

func (*designOperationsPlugin) Name() string { return "design-operations" }

func (p *designOperationsPlugin) ContributeDesign(builder *DesignBuilder) error {
	for _, operation := range p.operations {
		id := "api.operation:" + operation.Method + ":" + operation.Path
		if err := builder.AddNode(protofeatures.DesignNode{
			ID: id, Kind: protofeatures.DesignNodeAPIOperation, Name: operation.Method + " " + operation.Path,
			Properties: map[string]string{
				"method": operation.Method, "path": operation.Path, "operationId": operation.OperationID,
			},
		}); err != nil {
			return err
		}
		if err := builder.RelateFromModule(id, protofeatures.DesignEdgeExposes, protofeatures.DesignAuthorityExact); err != nil {
			return err
		}
	}
	return nil
}

func introspectOperations() []TypedClientOperation {
	return []TypedClientOperation{
		{OperationID: "revokeToken", Method: "POST", Path: "/v1/tokens/revoke"},
		{OperationID: "introspectToken", Method: "POST", Path: "/v1/introspect"},
	}
}

func consumerModule(name string, feature string, compose func(*Module)) *Module {
	module := NewModule(name).Feature(Feature{
		ID: feature, Name: name, Outcome: "consumes opaque-token introspection", Owner: "cloud",
	})
	compose(module)
	return module
}

// describeGraph runs the real describe pipeline and returns the published graph
// together with its exact bytes, so redaction can be asserted on the artifact.
func describeGraph(t *testing.T, application *Application) (*protofeatures.DesignGraph, []byte) {
	t.Helper()
	output := t.TempDir()
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
	return graph, data
}

func edgesInto(graph *protofeatures.DesignGraph, to string, kind protofeatures.DesignEdgeKind) []protofeatures.DesignEdge {
	var result []protofeatures.DesignEdge
	for _, edge := range graph.Edges {
		if edge.To == to && edge.Kind == kind {
			result = append(result, edge)
		}
	}
	return result
}

func nodesOfKind(graph *protofeatures.DesignGraph, kind protofeatures.DesignNodeKind) []protofeatures.DesignNode {
	var result []protofeatures.DesignNode
	for _, node := range graph.Nodes {
		if node.Kind == kind {
			result = append(result, node)
		}
	}
	return result
}

// TestTypedClientDerivesOneCallEdgePerComposingModule is the acceptance fixture:
// one producer feature with API operations plus three consumer modules sharing a
// single hand-written client instance. Pointer-identity de-duplication would
// keep only the first consumer, so this pins all three.
func TestTypedClientDerivesOneCallEdgePerComposingModule(t *testing.T) {
	client := &introspectionClientStub{design: TypedClientDesign{
		ProducerProject: introspectProducerProject,
		ProducerFeature: introspectProducerFeature,
		Client:          "introspectauth",
		Operations:      introspectOperations(),
	}}

	application := New(introspectProducerProject)
	application.Use(NewModule("opaque-tokens").
		Feature(Feature{ID: introspectProducerFeature, Name: "Opaque tokens", Outcome: "Tokens can be introspected", Owner: "cloud"}).
		Use(&designOperationsPlugin{operations: introspectOperations()}))
	for _, consumer := range []struct{ name, feature string }{
		{"identity", "identity/sessions"},
		{"control-plane", "identity/control-plane"},
		{"distribution", "identity/distribution"},
	} {
		name, feature := consumer.name, consumer.feature
		application.Use(consumerModule(name, feature, func(module *Module) {
			// The first two consumers compose the client with Use; the third
			// contributes the same instance. Both are real composition.
			if name == "distribution" {
				Contribute[TypedClientContributor](module, client)
				return
			}
			module.Use(client)
		}))
	}

	graph, _ := describeGraph(t, application)

	typed := nodesOfKind(graph, protofeatures.DesignNodeTypedClient)
	if len(typed) != 1 || typed[0].ID != introspectClientNodeID {
		t.Fatalf("typed client nodes = %+v, want exactly %q", typed, introspectClientNodeID)
	}
	if generated := nodesOfKind(graph, protofeatures.DesignNodeClient); len(generated) != 0 {
		t.Fatalf("hand-written client was published as a generated client: %+v", generated)
	}

	callEdges := edgesInto(graph, introspectClientNodeID, protofeatures.DesignEdgeCalls)
	consumers := make([]string, 0, len(callEdges))
	for _, edge := range callEdges {
		if edge.Authority != protofeatures.DesignAuthorityExact {
			t.Errorf("edge %s authority = %q, want exact when producer project and feature are both declared", edge.From, edge.Authority)
		}
		consumers = append(consumers, edge.From)
	}
	sort.Strings(consumers)
	want := []string{
		"module:cloud/identity/control-plane",
		"module:cloud/identity/distribution",
		"module:cloud/identity/identity",
	}
	if !slices.Equal(consumers, want) {
		t.Fatalf("consumer call edges = %v, want one per composing module %v", consumers, want)
	}

	var operations []string
	for _, edge := range graph.Edges {
		if edge.From == introspectClientNodeID && edge.Kind == protofeatures.DesignEdgeCalls {
			if edge.Authority != protofeatures.DesignAuthorityExact {
				t.Errorf("in-project operation edge authority = %q, want exact", edge.Authority)
			}
			operations = append(operations, edge.To)
		}
	}
	sort.Strings(operations)
	wantOperations := []string{"api.operation:POST:/v1/introspect", "api.operation:POST:/v1/tokens/revoke"}
	if !slices.Equal(operations, wantOperations) {
		t.Fatalf("in-project operation edges = %v, want %v", operations, wantOperations)
	}
	if got := typed[0].Properties["operations"]; got != "POST /v1/introspect, POST /v1/tokens/revoke" {
		t.Fatalf("operation property = %q, want the sorted canonical list", got)
	}
}

// TestTypedClientWithoutProducerFeatureStaysUnmodeled pins the refusal to guess:
// the relationship is published, its lineage is not invented.
func TestTypedClientWithoutProducerFeatureStaysUnmodeled(t *testing.T) {
	client := &introspectionClientStub{design: TypedClientDesign{
		ProducerProject: introspectProducerProject,
		Client:          "introspectauth",
		Operations:      introspectOperations(),
	}}

	application := New("cloud/distribution")
	application.Use(consumerModule("edge", "distribution/edge", func(module *Module) { module.Use(client) }))

	graph, data := describeGraph(t, application)

	edges := edgesInto(graph, introspectClientNodeID, protofeatures.DesignEdgeCalls)
	if len(edges) != 1 || edges[0].Authority != protofeatures.DesignAuthorityUnmodeled {
		t.Fatalf("call edges = %+v, want one %q edge", edges, protofeatures.DesignAuthorityUnmodeled)
	}
	typed := nodesOfKind(graph, protofeatures.DesignNodeTypedClient)
	if len(typed) != 1 {
		t.Fatalf("typed client nodes = %+v, want exactly one", typed)
	}
	if feature, present := typed[0].Properties["feature"]; present {
		t.Fatalf("absent producer feature was guessed as %q", feature)
	}
	if strings.Contains(string(data), introspectProducerFeature) {
		t.Fatalf("undeclared producer feature leaked into the artifact:\n%s", data)
	}
	// The producer's operations live in the producer's own graph. A cross-project
	// consumer must carry them as node properties instead of minting edges that
	// finish() then drops with a warning on every build.
	for _, edge := range graph.Edges {
		if edge.From == introspectClientNodeID {
			t.Errorf("cross-project consumer emitted operation edge %s -> %s", edge.From, edge.To)
		}
	}
	if got := typed[0].Properties["operations"]; got != "POST /v1/introspect, POST /v1/tokens/revoke" {
		t.Fatalf("cross-project operation property = %q, want the sorted canonical list", got)
	}
}

// TestTypedClientCrossProjectConsumerDropsNoRelationship pins the same decision
// at the seam that reports it: finish() must have nothing to warn about.
func TestTypedClientCrossProjectConsumerDropsNoRelationship(t *testing.T) {
	client := &introspectionClientStub{design: TypedClientDesign{
		ProducerProject: introspectProducerProject,
		ProducerFeature: introspectProducerFeature,
		Client:          "introspectauth",
		Operations:      introspectOperations(),
	}}
	application := New("cloud/distribution")
	application.Use(consumerModule("edge", "distribution/edge", func(module *Module) { module.Use(client) }))

	builder := newDesignGraphBuilder(application.name)
	if err := builder.collectModule(application.Module, nil, ""); err != nil {
		t.Fatalf("collect modules: %v", err)
	}
	if err := builder.contributeTypedClientDesign(application.Module); err != nil {
		t.Fatalf("contribute typed clients: %v", err)
	}
	if _, dropped := builder.finish(); len(dropped) != 0 {
		t.Fatalf("cross-project consumer dropped relationships: %v", dropped)
	}
}

// TestTypedClientPublishesOnlyBoundedDeclaredProperties is the redaction gate:
// the client's runtime configuration never reaches the build artifact.
func TestTypedClientPublishesOnlyBoundedDeclaredProperties(t *testing.T) {
	client := &introspectionClientStub{
		design: TypedClientDesign{
			ProducerProject: introspectProducerProject,
			ProducerFeature: introspectProducerFeature,
			Client:          "introspectauth",
			Operations:      introspectOperations(),
		},
		baseURL:  "https://identity.internal.example.com",
		token:    "Bearer pk_live_super_secret_value",
		tokenEnv: "CLOUD_IDENTITY_INTROSPECTION_TOKEN",
	}

	application := New("cloud/distribution")
	application.Use(consumerModule("edge", "distribution/edge", func(module *Module) { module.Use(client) }))

	graph, data := describeGraph(t, application)
	for _, secret := range []string{client.baseURL, client.token, client.tokenEnv, "Bearer", "identity.internal"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("design graph leaked %q:\n%s", secret, data)
		}
	}
	typed := nodesOfKind(graph, protofeatures.DesignNodeTypedClient)
	if len(typed) != 1 {
		t.Fatalf("typed client nodes = %+v, want exactly one", typed)
	}
	keys := make([]string, 0, len(typed[0].Properties))
	for key := range typed[0].Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"feature", "language", "operations", "producer"}; !slices.Equal(keys, want) {
		t.Fatalf("node property keys = %v, want exactly the bounded set %v", keys, want)
	}
	if typed[0].Properties["language"] != "go" {
		t.Errorf("language = %q, want the framework-stamped go", typed[0].Properties["language"])
	}
}

// TestTypedClientDesignIsRuntimeInert pins criterion 6: composing a contributor
// changes no lifecycle ordering and never reads build metadata at runtime.
func TestTypedClientDesignIsRuntimeInert(t *testing.T) {
	client := &introspectionLifecycleClient{}
	client.design = TypedClientDesign{
		ProducerProject: introspectProducerProject, Client: "introspectauth",
	}
	other := &runtimeEvidencePlugin{}

	application := New("runtime")
	application.Run(func(context.Context) error { return nil })
	application.Use(client)
	application.Use(other)
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := application.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.configure != 1 || client.start != 1 || client.stop != 1 {
		t.Fatalf("typed client lifecycle counts = %d/%d/%d", client.configure, client.start, client.stop)
	}
	if other.configured != 1 || other.started != 1 || other.stopped != 1 {
		t.Fatalf("neighboring plugin lifecycle counts = %d/%d/%d", other.configured, other.started, other.stopped)
	}
}

// TestTypedClientDescriptorErrorsAreActionable pins that an incomplete
// descriptor fails the build with a fixable message instead of publishing an
// unmodeled edge that hides the mistake.
func TestTypedClientDescriptorErrorsAreActionable(t *testing.T) {
	for name, testCase := range map[string]struct {
		design TypedClientDesign
		want   string
	}{
		"missing producer project": {
			design: TypedClientDesign{Client: "introspectauth"},
			want:   "ProducerProject is required",
		},
		"missing client name": {
			design: TypedClientDesign{ProducerProject: introspectProducerProject},
			want:   "Client is required",
		},
		"missing method": {
			design: TypedClientDesign{
				ProducerProject: introspectProducerProject, Client: "introspectauth",
				Operations: []TypedClientOperation{{Path: "/v1/introspect"}},
			},
			want: "Operations[0].Method is required",
		},
		"deployment url instead of a contract path": {
			design: TypedClientDesign{
				ProducerProject: introspectProducerProject, Client: "introspectauth",
				Operations: []TypedClientOperation{{Method: "POST", Path: "https://identity.example.com/v1/introspect"}},
			},
			want: "never a deployment URL",
		},
		// A scheme-relative URL starts with "/", so a leading-slash check alone
		// would publish this host into the graph.
		"scheme-relative deployment url": {
			design: TypedClientDesign{
				ProducerProject: introspectProducerProject, Client: "introspectauth",
				Operations: []TypedClientOperation{{Method: "POST", Path: "//identity.internal.example.com/v1/introspect"}},
			},
			want: "scheme-relative URL carrying a host",
		},
		"credentials in the path": {
			design: TypedClientDesign{
				ProducerProject: introspectProducerProject, Client: "introspectauth",
				Operations: []TypedClientOperation{{Method: "POST", Path: "//user:secret@identity.example.com/v1/introspect"}},
			},
			want: "scheme-relative URL carrying a host",
		},
		"query string that can smuggle a token": {
			design: TypedClientDesign{
				ProducerProject: introspectProducerProject, Client: "introspectauth",
				Operations: []TypedClientOperation{{Method: "POST", Path: "/v1/introspect?access_token=s3cret"}},
			},
			want: "must not carry a query string",
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := &introspectionClientStub{design: testCase.design}
			application := New("cloud/distribution")
			application.Use(consumerModule("edge", "distribution/edge", func(module *Module) { module.Use(client) }))
			err := application.Describe(t.TempDir(), []string{describerNameDesign})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("describe error = %v, want one containing %q", err, testCase.want)
			}
		})
	}
}

// TestTypedClientPathRejectsEveryHostBearingForm pins the boundary directly, so
// a future tightening cannot quietly start rejecting router template syntax and
// a future loosening cannot let a host back in. The describe-level table above
// proves the rule is wired; this proves the rule itself.
func TestTypedClientPathRejectsEveryHostBearingForm(t *testing.T) {
	for _, path := range []string{
		"/v1/introspect",
		"/v1/[tokenId]/revoke",
		"/v1/{tokenId}/revoke",
		"/v1/:tokenId/revoke",
		"/",
	} {
		if err := validateTypedClientPath(path); err != nil {
			t.Errorf("contract template %q was rejected: %v", path, err)
		}
	}
	for _, path := range []string{
		"",
		"v1/introspect",
		"https://identity.example.com/v1/introspect",
		"http://identity.example.com/v1/introspect",
		"//identity.internal.example.com/v1/introspect",
		"//user:secret@identity.example.com/v1/introspect",
		`/\identity.example.com/v1/introspect`,
		"/v1/introspect?access_token=s3cret",
		"/v1/introspect?",
		"/v1/introspect#fragment",
	} {
		if err := validateTypedClientPath(path); err == nil {
			t.Errorf("path %q was accepted as a contract template", path)
		}
	}
}

// TestTypedClientOutsideFeatureScopeIsNotPublished keeps the projection scoped
// to declared product intent, exactly like every other native contributor.
func TestTypedClientOutsideFeatureScopeIsNotPublished(t *testing.T) {
	client := &introspectionClientStub{design: TypedClientDesign{
		ProducerProject: introspectProducerProject, Client: "introspectauth",
	}}
	application := New("cloud/distribution")
	application.Use(NewModule("featureless").Use(client))
	application.Use(consumerModule("edge", "distribution/edge", func(*Module) {}))

	graph, _ := describeGraph(t, application)
	if typed := nodesOfKind(graph, protofeatures.DesignNodeTypedClient); len(typed) != 0 {
		t.Fatalf("client outside a feature scope was published: %+v", typed)
	}
}
