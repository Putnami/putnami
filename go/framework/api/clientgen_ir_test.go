package api

import (
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"

	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generateFromFixture reads a shared OpenAPI fixture, builds the IR, and emits a
// Go client — the full Phase-2b pipeline end to end.
func generateFromFixture(t *testing.T, name string, opts ClientGenOptions) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sharedOpenAPIDir(), name+".openapi.json"))
	if err != nil {
		t.Skipf("shared fixtures not available (%v)", err)
	}
	spec, err := ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("ReadOpenAPISpec(%s): %v", name, err)
	}
	src, err := GenerateClientFromIR(spec, opts)
	if err != nil {
		t.Fatalf("GenerateClientFromIR(%s): %v", name, err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse:\n%s\n--- error: %v", src, err)
	}
	return src
}

func TestGenerateClientFromIR_RequiresPackageName(t *testing.T) {
	if _, err := GenerateClientFromIR(SpecIR{}, ClientGenOptions{}); err == nil {
		t.Fatal("expected an error when PackageName is empty")
	}
}

func TestGenerateClientFromIR_FirstPartyNeverFallsBackToLegacyFieldTokens(t *testing.T) {
	spec := SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "items", Audience: "https://items.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
	}
	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "items", ClientName: "ItemsClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR error = %v", err)
	}
	for _, want := range []string{"client.MustServiceDescriptor", "RegisterItemsClient", "client.NewServiceClient"} {
		if !strings.Contains(source, want) {
			t.Errorf("strict source missing %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, "map[string]any") {
		t.Fatalf("strict source contains untyped value fallback:\n%s", source)
	}
}

func TestGenerateClientFromIR_RefNamedTypes(t *testing.T) {
	src := generateFromFixture(t, "ref-named-types", ClientGenOptions{PackageName: "ssclient"})

	wants := []string{
		`package ssclient`,
		// Shared models emitted once as top-level structs.
		`type Model1 struct {`,
		`type Model2 struct {`,
		`Address *Model2 ` + "`json:\"address,omitempty\"`",
		`Tags []Model2 ` + "`json:\"tags,omitempty\"`",
		`City string ` + "`json:\"city\"`",
		// POST /users → CreateUsers, body+response are the shared Model1.
		`func (c *Client) CreateUsers(ctx context.Context, in CreateUsersInput) (*Model1, error) {`,
		`Body Model1 ` + "`json:\"body\"`",
		// GET /users/{id} → GetUsers, path param + Model1 response.
		`func (c *Client) GetUsers(ctx context.Context, in GetUsersInput) (*Model1, error) {`,
		`url.PathEscape(fmt.Sprint(in.Params.Id))`,
	}
	for _, w := range wants {
		if !containsNormalized(src, w) {
			t.Errorf("generated source missing %q\n--- output:\n%s", w, src)
		}
	}
	// The named-type response must NOT produce a per-operation GetUsersOutput
	// struct — it references the shared model directly.
	if strings.Contains(src, "type CreateUsersOutput struct") || strings.Contains(src, "type GetUsersOutput struct") {
		t.Errorf("named response should reference the shared model, not a per-op Output struct:\n%s", src)
	}
}

func TestGenerateClientFromIR_ArraysAndDegrade(t *testing.T) {
	src := generateFromFixture(t, "arrays-and-degrade", ClientGenOptions{PackageName: "items"})
	wants := []string{
		`Tags []string ` + "`json:\"tags\"`",
		`Counts []int64 ` + "`json:\"counts,omitempty\"`",
		`Meta map[string]any ` + "`json:\"meta,omitempty\"`",
		`Rows []map[string]any ` + "`json:\"rows,omitempty\"`",
	}
	for _, w := range wants {
		if !containsNormalized(src, w) {
			t.Errorf("generated source missing %q\n--- output:\n%s", w, src)
		}
	}
}

func TestGenerateClientFromIR_OperationsSurface(t *testing.T) {
	src := generateFromFixture(t, "operations", ClientGenOptions{PackageName: "ops", ClientName: "OpsClient"})
	wants := []string{
		`type OpsClient struct {`,
		// GET /users → ListUsers with a query param.
		`func (c *OpsClient) ListUsers(ctx context.Context, in ListUsersInput) (*ListUsersOutput, error) {`,
		`Page *int64 ` + "`json:\"page,omitempty\"`",
		`if in.Query.Page != nil {`,
		`query.Set("page", fmt.Sprint(*in.Query.Page))`,
		// POST /users → CreateUsers with an inline body, void return.
		`func (c *OpsClient) CreateUsers(ctx context.Context, in CreateUsersInput) error {`,
		`Email *string ` + "`json:\"email,omitempty\"`",
		// GET /orders/{id} → GetOrders, void return (204).
		`func (c *OpsClient) GetOrders(ctx context.Context, in GetOrdersInput) error {`,
		`url.PathEscape(fmt.Sprint(in.Params.Id))`,
	}
	for _, w := range wants {
		if !containsNormalized(src, w) {
			t.Errorf("generated source missing %q\n--- output:\n%s", w, src)
		}
	}
}

func TestGenerateClientFromIR_ArrayQueryParamUsesAdd(t *testing.T) {
	// An array query param must be serialized one value per element (query.Add),
	// not as fmt.Sprint of the whole slice ("[a b]") which the server rejects.
	spec := SpecIR{
		Transport: "http",
		Services: []ServiceIR{{
			Name:      "SearchService",
			ClassName: "SearchClient",
			Methods: []MethodIR{{
				Name:       "search",
				HTTPMethod: "GET",
				Path:       "/search",
				Query:      []FieldIR{{Name: "tags", GoType: "string", Array: true}},
			}},
		}},
	}
	src, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "search"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse:\n%s\n--- error: %v", src, err)
	}
	if !containsNormalized(src, `query.Add("tags", fmt.Sprint(v))`) {
		t.Errorf("expected per-element query.Add for an array query param\n--- output:\n%s", src)
	}
	if containsNormalized(src, `query.Set("tags", fmt.Sprint(in.Query.Tags))`) {
		t.Errorf("array query param must not be serialized with query.Set of the whole slice\n--- output:\n%s", src)
	}
}

func TestGenerateClientFromIR_DetectsMethodNameCollision(t *testing.T) {
	// Two GET operations under /users with distinct path params both derive
	// "GetUsers" — the generator must reject the collision.
	spec := SpecIR{
		Transport: "http",
		Services: []ServiceIR{{
			Name:      "UsersService",
			ClassName: "UsersClient",
			Methods: []MethodIR{
				{Name: "getById", HTTPMethod: "GET", Path: "/users/{id}", Params: []FieldIR{{Name: "id", GoType: "string"}}},
				{Name: "getByName", HTTPMethod: "GET", Path: "/users/{name}", Params: []FieldIR{{Name: "name", GoType: "string"}}},
			},
		}},
	}
	_, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "x"})
	if err == nil {
		t.Fatal("expected a collision error")
	}
	for _, want := range []string{"/users/{id}", "/users/{name}", "GetUsers"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error %q missing %q", err.Error(), want)
		}
	}
}

func TestGenerateClientFromIR_HelperNameCollidesWithSharedModel(t *testing.T) {
	// A shared model may legitimately be named exactly like a generated per-operation
	// helper. POST /items derives method "CreateItems", whose input wrapper is
	// "CreateItemsInput" — the same name as the model below. Shared models are declared
	// first, so the input wrapper must take a suffixed name. Otherwise declareInputStruct
	// silently no-ops (first-writer-wins) and CreateItems references in.Body on the model,
	// which has no Body field: the source parses but does not compile.
	spec := SpecIR{
		Transport: "http",
		NamedTypes: map[string][]FieldIR{
			"Item":             {{Name: "id", GoType: "string"}},
			"CreateItemsInput": {{Name: "foo", GoType: "string"}},
		},
		Services: []ServiceIR{{
			Name:      "ItemsService",
			ClassName: "ItemsClient",
			Methods: []MethodIR{{
				Name:         "create",
				HTTPMethod:   "POST",
				Path:         "/items",
				BodyType:     "Item",
				ResponseType: "Item",
			}},
		}},
	}
	src, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "items"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse:\n%s\n--- error: %v", src, err)
	}

	// The shared model keeps its name and its own field.
	if !containsNormalized(src, "type CreateItemsInput struct {") || !containsNormalized(src, "Foo string") {
		t.Errorf("shared model CreateItemsInput must survive with its own field\n--- output:\n%s", src)
	}
	// The input wrapper is disambiguated to CreateItemsInput2, and the method signature
	// references it — not the colliding model. (Pre-fix this was CreateItemsInput.)
	if !containsNormalized(src, "func (c *Client) CreateItems(ctx context.Context, in CreateItemsInput2) (*Item, error) {") {
		t.Errorf("CreateItems input must be the disambiguated wrapper, not the colliding model\n--- output:\n%s", src)
	}
	// The disambiguated wrapper carries the Body section the method marshals via in.Body.
	if !containsNormalized(src, "type CreateItemsInput2 struct {") || !containsNormalized(src, "Body Item") {
		t.Errorf("disambiguated input wrapper must nest the Body section\n--- output:\n%s", src)
	}
}

// The optional-fields requirement (moved here from go/typed-service-clients by
// decision 4 of the spec rollout) governs the contract path: a field the
// OpenAPI document leaves out of `required` is generated as a pointer or an
// omitted value, and an unset optional query parameter is left out of the
// request URL instead of being sent empty.
func TestGenerateClientFromIR_OptionalQueryParamIsOmittedWhenUnset(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "optional-fields", "an-unset-optional-query-param-is-left-out-of-the-url")
	spec := SpecIR{
		Transport: "http",
		Services: []ServiceIR{{
			Name:      "UserService",
			ClassName: "UserClient",
			Methods: []MethodIR{{
				Name:       "listUsers",
				HTTPMethod: "GET",
				Path:       "/users",
				Query: []FieldIR{
					{Name: "limit", GoType: "int"},
					{Name: "sort", GoType: "string", Optional: true},
				},
			}},
		}},
	}
	src, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "users"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse:\n%s\n--- error: %v", src, err)
	}

	// The required parameter is set unconditionally…
	if !containsNormalized(src, `query.Set("limit"`) {
		t.Fatalf("required param limit is never set\n--- output:\n%s", src)
	}
	// …the optional one only behind a presence check, so an unset value never
	// reaches the URL as an empty string.
	idx := strings.Index(src, `query.Set("sort"`)
	if idx < 0 {
		t.Fatalf("optional param sort is never emitted\n--- output:\n%s", src)
	}
	tail := src[:idx]
	lastIf := strings.LastIndex(tail, "if ")
	lastSet := strings.LastIndex(tail, `query.Set("limit"`)
	if lastIf < lastSet {
		t.Errorf("optional param sort is set unconditionally\n--- output:\n%s", src)
	}
}

func TestGenerateClientFromIR_EmbedsFeatureTraceWhenDesignIsDeclared(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "producer-attribution", "a-feature-trace-is-embedded-when-the-design-declares-one")
	spec := SpecIR{
		Transport: "http",
		Services: []ServiceIR{{
			Name:      "UsersService",
			ClassName: "UsersClient",
			Methods: []MethodIR{{
				Name:        "listUsers",
				OperationID: "getUsers",
				HTTPMethod:  "GET",
				Path:        "/users",
			}},
		}},
	}
	src, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName: "myclient",
		ClientName:  "MyClient",
		Design: &ClientDesignOptions{
			SpecHash: "spec-123",
			Operations: []ClientOperationProducer{
				{Method: "GET", Path: "/users", ProducerProject: "example/provider", ProducerFeature: "users/manage"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The Go method symbol is ListUsers; the canonical operation id is getUsers.
	// The descriptor and the trace key must use the canonical one.
	for _, want := range []string{
		`{OperationID: "getUsers", Method: "GET", Path: "/users", ProducerProject: "example/provider", ProducerFeature: "users/manage"},`,
		`FeatureTrace: MyClientDesign.Trace("getUsers")`,
		`func (c *MyClient) ListUsers(`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated design missing %q:\n%s", want, src)
		}
	}
}

// One generated client routinely spans several producer features. Each operation
// must carry its own, and an operation the attribution table does not name must
// stay unattributed rather than inherit a neighbor's feature.
func TestGenerateClientFromIR_AttributesEachOperationToItsOwnProducer(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "producer-attribution", "each-operation-records-its-producing-project-and-feature")
	spec := SpecIR{
		Transport: "http",
		Services: []ServiceIR{{
			Name:      "PlatformService",
			ClassName: "PlatformClient",
			Methods: []MethodIR{
				{Name: "cliUsage", OperationID: "getV1_Operator_Cli-usage", HTTPMethod: "GET", Path: "/v1/operator/cli-usage"},
				{Name: "invoices", OperationID: "getV1_Billing_Invoices", HTTPMethod: "GET", Path: "/v1/billing/invoices"},
				{Name: "health", OperationID: "getV1_Health", HTTPMethod: "GET", Path: "/v1/health"},
			},
		}},
	}
	src, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName: "platformclient",
		ClientName:  "PlatformClient",
		Design: &ClientDesignOptions{
			SpecHash: "spec-123",
			Operations: []ClientOperationProducer{
				{
					Method: "GET", Path: "/v1/operator/cli-usage",
					ProducerProject: "acme-platform", ProducerFeature: "platform/operator-cli-usage",
				},
				{
					Method: "GET", Path: "/v1/billing/invoices",
					ProducerProject: "acme-platform", ProducerFeature: "platform/billing",
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{OperationID: "getV1_Operator_Cli-usage", Method: "GET", Path: "/v1/operator/cli-usage", ProducerProject: "acme-platform", ProducerFeature: "platform/operator-cli-usage"},`,
		`{OperationID: "getV1_Billing_Invoices", Method: "GET", Path: "/v1/billing/invoices", ProducerProject: "acme-platform", ProducerFeature: "platform/billing"},`,
		`{OperationID: "getV1_Health", Method: "GET", Path: "/v1/health"},`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated design missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, `Path: "/v1/health", ProducerProject`) {
		t.Errorf("unattributed operation inherited a neighbor's producer:\n%s", src)
	}
}

// The canonical operation id keeps punctuation the Go symbol normalizer cannot:
// the descriptor and the trace key must not silently become the symbol.
func TestGenerateClientFromIR_PreservesCanonicalOperationIDAcrossSymbolNormalization(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "a-generated-descriptor-survives-symbol-normalization")
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "a-generated-client-preserves-the-canonical-operation-id")
	spec := SpecIR{
		Transport: "http",
		Services: []ServiceIR{{
			Name:      "PlatformService",
			ClassName: "PlatformClient",
			Methods: []MethodIR{{
				Name:        "cliUsage",
				OperationID: "getV1_Operator_Cli-usage",
				HTTPMethod:  "GET",
				Path:        "/v1/operator/cli-usage",
			}},
		}},
	}
	src, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName: "platformclient",
		ClientName:  "PlatformClient",
		Design:      &ClientDesignOptions{SpecHash: "spec-123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, `FeatureTrace: PlatformClientDesign.Trace("getV1_Operator_Cli-usage")`) {
		t.Errorf("canonical operation id was normalized away:\n%s", src)
	}
	if !strings.Contains(src, `func (c *PlatformClient) ListV1OperatorCliUsage(`) {
		t.Errorf("expected a punctuation-free Go method symbol:\n%s", src)
	}
}

func TestGenerateClientFromIR_CatchAllParamSubstitution(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "catch-all-paths", "a-generated-client-substitutes-the-joined-value-without-re-escaping")
	// Catch-all params must substitute without per-segment escaping so the
	// generated URL keeps the captured slashes intact.
	spec := SpecIR{
		Transport: "http",
		Services: []ServiceIR{{
			Name:      "BlobsService",
			ClassName: "BlobsClient",
			Methods: []MethodIR{{
				Name:        "uploadBlob",
				OperationID: "postModuleBlobsUpload",
				HTTPMethod:  "POST",
				Path:        "/{module...}/-/blobs/upload",
				Params:      []FieldIR{{Name: "module", GoType: "string"}},
			}},
		}},
	}
	src, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "myclient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse:\n%s\n--- error: %v", src, err)
	}

	wantSub := `strings.Replace(path, "{module...}", fmt.Sprint(in.Params.Module), 1)`
	if !strings.Contains(src, wantSub) {
		t.Errorf("expected catch-all substitution without url.PathEscape:\n--- want substring:\n%s\n--- got:\n%s", wantSub, src)
	}
	// Per-segment escaping (url.PathEscape) must NOT be applied to catch-all params.
	if strings.Contains(src, `url.PathEscape(fmt.Sprint(in.Params.Module))`) {
		t.Errorf("catch-all param must not be url.PathEscape'd:\n%s", src)
	}
}
