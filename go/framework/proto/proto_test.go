package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/api"
	"go.putnami.dev/proto/internal/dupfixture"
	clientcontract "go.putnami.dev/protocol/clientcontract"

	"go.putnami.dev/protocol/features/spectest"
)

// DupKind shares its simple name with dupfixture.DupKind but lives in this
// (test) package with a different field, so the generator must emit two distinct
// proto messages for them rather than clobbering one.
type DupKind struct {
	FromProto string `json:"fromProto"`
}

type protoCollideLocalBody struct {
	Item DupKind `json:"item"`
}

type protoCollideDupBody struct {
	Item dupfixture.DupKind `json:"item"`
}

type ProtoUserParams struct {
	ID string `json:"id" validate:"required,uuid"`
}

type ProtoListUsersQuery struct {
	Page  int `json:"page" validate:"min=1"`
	Limit int `json:"limit" validate:"min=1,max=100"`
}

type ProtoUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type ProtoCreateUserBody struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

// ProtoNode is self-referential, used to exercise the cycle-breaking placeholder.
type ProtoNode struct {
	ID       string      `json:"id"`
	Children []ProtoNode `json:"children"`
}

func TestGenerate_RPCNames(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "the-protobuf-rpc-name-is-derived-the-same-way")
	cases := []struct {
		method, path string
		want         string
	}{
		{"GET", "/users", "ListUsers"},
		{"GET", "/users/{id}", "GetUsers"},
		{"POST", "/users", "CreateUsers"},
		{"PUT", "/users/{id}", "UpdateUsers"},
		{"DELETE", "/users/{id}", "DeleteUsers"},
		{"GET", "/admin/projects/{projectId}", "GetAdminProjects"},
	}
	for _, c := range cases {
		got := clientcontract.RPCName(c.method, c.path)
		if got != c.want {
			t.Errorf("%s %s → %s, want %s", c.method, c.path, got, c.want)
		}
	}
}

func TestGenerate_DocumentShape(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{
			Method:        "GET",
			Path:          "/users",
			Description:   "List users",
			QuerySchema:   typeOf[ProtoListUsersQuery](),
			ReturnsSchema: typeOf[ProtoUser](),
		},
		{
			Method:        "POST",
			Path:          "/users",
			BodySchema:    typeOf[ProtoCreateUserBody](),
			ReturnsSchema: typeOf[ProtoUser](),
		},
		{
			Method:        "GET",
			Path:          "/users/{id}",
			ParamsSchema:  typeOf[ProtoUserParams](),
			ReturnsSchema: typeOf[ProtoUser](),
		},
	}

	doc := Generate(routes, Options{PackageName: "test.v1"})

	if doc.PackageName != "test.v1" {
		t.Errorf("package name = %q, want test.v1", doc.PackageName)
	}
	if got := len(doc.Service.RPCs); got != 3 {
		t.Fatalf("RPC count = %d, want 3", got)
	}
	if doc.Service.RPCs[0].Name != "ListUsers" {
		t.Errorf("rpc[0].Name = %q, want ListUsers", doc.Service.RPCs[0].Name)
	}
	if doc.Service.RPCs[0].Description != "List users" {
		t.Errorf("description not propagated: %q", doc.Service.RPCs[0].Description)
	}

	// Request messages now reference sub-messages per section so colliding field
	// names cannot duplicate.
	listReq := findMessage(t, doc, "ListUsersRequest")
	if !hasField(listReq, "query", "ListUsersQuery") {
		t.Errorf("ListUsersRequest should embed a ListUsersQuery section: %+v", listReq.Fields)
	}
	listQuery := findMessage(t, doc, "ListUsersQuery")
	if !hasField(listQuery, "page", "int64") || !hasField(listQuery, "limit", "int64") {
		t.Errorf("ListUsersQuery missing query fields: %+v", listQuery.Fields)
	}

	getReq := findMessage(t, doc, "GetUsersRequest")
	if !hasField(getReq, "params", "GetUsersParams") {
		t.Errorf("GetUsersRequest should embed a GetUsersParams section: %+v", getReq.Fields)
	}
	getParams := findMessage(t, doc, "GetUsersParams")
	if !hasField(getParams, "id", "string") {
		t.Errorf("GetUsersParams missing id field: %+v", getParams.Fields)
	}

	// Reply messages reference the returned struct via embedded fields.
	listReply := findMessage(t, doc, "ListUsersReply")
	if !hasField(listReply, "id", "string") || !hasField(listReply, "email", "string") {
		t.Errorf("ListUsersReply fields not derived from ProtoUser: %+v", listReply.Fields)
	}
}

func TestClientDescriptorPreservesAssignedWireShape(t *testing.T) {
	doc := Generate([]api.DiscoveredRoute{{
		Method:        "GET",
		Path:          "/users/{id}",
		ParamsSchema:  typeOf[ProtoUserParams](),
		ReturnsSchema: typeOf[ProtoUser](),
	}}, Options{PackageName: "test.v1"})
	descriptor := ClientDescriptor(doc)
	if descriptor.Syntax != "proto3" || descriptor.Package != "test.v1" {
		t.Fatalf("descriptor header = %#v", descriptor)
	}
	if len(descriptor.Services) != 1 || len(descriptor.Services[0].Methods) != 1 {
		t.Fatalf("descriptor services = %#v", descriptor.Services)
	}
	method := descriptor.Services[0].Methods[0]
	if method.Name != "GetUsers" || method.Input != "GetUsersRequest" || method.Output != "GetUsersReply" {
		t.Fatalf("descriptor method = %#v", method)
	}
	params := findClientMessage(t, descriptor.Messages, "GetUsersParams")
	if len(params.Fields) != 1 || params.Fields[0].Number != 1 || params.Fields[0].TypeKind != "scalar" || params.Fields[0].JSONName != "id" {
		t.Fatalf("descriptor params = %#v", params)
	}
}

func findClientMessage(t *testing.T, messages []clientcontract.ProtobufMessage, name string) clientcontract.ProtobufMessage {
	t.Helper()
	for _, message := range messages {
		if message.Name == name {
			return message
		}
	}
	t.Fatalf("client protobuf message %q not found", name)
	return clientcontract.ProtobufMessage{}
}

func TestGenerate_RenderedContent(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{
			Method:        "POST",
			Path:          "/items",
			BodySchema:    typeOf[ProtoCreateUserBody](),
			ReturnsSchema: typeOf[ProtoUser](),
		},
	}
	doc := Generate(routes, Options{PackageName: "test.v1", GoPackage: "example.com/test/v1"})

	expectedSubstrs := []string{
		`syntax = "proto3";`,
		`package test.v1;`,
		`option go_package = "example.com/test/v1";`,
		`message CreateItemsBody {`,
		`  string name = 1;`,
		`  string email = 2;`,
		`message CreateItemsRequest {`,
		`  CreateItemsBody body = 1;`,
		`message CreateItemsReply {`,
		`service ApiService {`,
		`  rpc CreateItems(CreateItemsRequest) returns (CreateItemsReply);`,
	}
	for _, s := range expectedSubstrs {
		if !strings.Contains(doc.Content, s) {
			t.Errorf("rendered content missing %q\n--- output:\n%s", s, doc.Content)
		}
	}
}

func TestGenerate_StreamRPCs(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{
			Method:        "GET",
			Path:          "/chat",
			StreamMode:    api.StreamModeBidirectional,
			BodySchema:    typeOf[ProtoCreateUserBody](),
			ReturnsSchema: typeOf[ProtoUser](),
		},
		{
			Method:        "GET",
			Path:          "/notifications",
			StreamMode:    api.StreamModeServer,
			ReturnsSchema: typeOf[ProtoUser](),
		},
	}

	doc := Generate(routes, Options{PackageName: "test.v1"})

	if !doc.Service.RPCs[0].ClientStreaming || !doc.Service.RPCs[0].ServerStreaming {
		t.Errorf("chat rpc streaming flags = (%v, %v), want bidirectional",
			doc.Service.RPCs[0].ClientStreaming, doc.Service.RPCs[0].ServerStreaming)
	}
	if doc.Service.RPCs[1].ClientStreaming || !doc.Service.RPCs[1].ServerStreaming {
		t.Errorf("notifications rpc streaming flags = (%v, %v), want server-stream",
			doc.Service.RPCs[1].ClientStreaming, doc.Service.RPCs[1].ServerStreaming)
	}
	for _, want := range []string{
		`rpc ListChat(stream ListChatRequest) returns (stream ListChatReply);`,
		`rpc ListNotifications(ListNotificationsRequest) returns (stream ListNotificationsReply);`,
	} {
		if !strings.Contains(doc.Content, want) {
			t.Errorf("rendered content missing %q\n--- output:\n%s", want, doc.Content)
		}
	}
}

func TestGenerate_NestedSectionsAvoidFieldCollision(t *testing.T) {
	// PUT /users/{id} with a body that ALSO has an `id` field used to flatten into a
	// duplicate-field message. With nested sections each `id` lives on a different
	// sub-message and there is no collision.
	type collideBody struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	routes := []api.DiscoveredRoute{
		{
			Method:        "PUT",
			Path:          "/users/{id}",
			ParamsSchema:  typeOf[ProtoUserParams](),
			BodySchema:    typeOf[collideBody](),
			ReturnsSchema: typeOf[ProtoUser](),
		},
	}
	doc := Generate(routes, Options{PackageName: "test.v1"})

	req := findMessage(t, doc, "UpdateUsersRequest")
	if !hasField(req, "params", "UpdateUsersParams") || !hasField(req, "body", "UpdateUsersBody") {
		t.Errorf("UpdateUsersRequest should embed both Params and Body sections: %+v", req.Fields)
	}
	if seen := countField(req, "id"); seen != 0 {
		t.Errorf("UpdateUsersRequest should not contain a flat id field, got %d", seen)
	}

	params := findMessage(t, doc, "UpdateUsersParams")
	if !hasField(params, "id", "string") {
		t.Errorf("UpdateUsersParams missing id: %+v", params.Fields)
	}
	body := findMessage(t, doc, "UpdateUsersBody")
	if !hasField(body, "id", "string") || !hasField(body, "name", "string") {
		t.Errorf("UpdateUsersBody missing fields: %+v", body.Fields)
	}
}

func countField(m Message, name string) int {
	n := 0
	for _, f := range m.Fields {
		if f.Name == name {
			n++
		}
	}
	return n
}

func TestGenerate_EmptyReturnsProducesEmptyReply(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{Method: "DELETE", Path: "/items/{id}", ParamsSchema: typeOf[ProtoUserParams]()},
	}
	doc := Generate(routes, Options{})
	reply := findMessage(t, doc, "DeleteItemsReply")
	if len(reply.Fields) != 0 {
		t.Errorf("empty Returns should yield zero-field reply, got %+v", reply.Fields)
	}
}

func TestGenerate_SkipsDocumentOnlyRoutes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "document-only", "a-document-only-route-is-absent-from-the-protobuf-service")
	// Doc-only routes are intentionally not bridged to Connect-style RPCs
	// (the gRPC bridge skips them too), so emitting an RPC for one would
	// document an unreachable endpoint. Two routes share (method, path) here
	// — the handler-bound one and the doc-only one — to also catch a future
	// regression where the generator forgets to skip and produces duplicate
	// RPC names.
	routes := []api.DiscoveredRoute{
		{
			Method:        "GET",
			Path:          "/users",
			ReturnsSchema: typeOf[ProtoUser](),
		},
		{
			Method:        "GET",
			Path:          "/users",
			ReturnsSchema: typeOf[ProtoUser](),
			DocumentOnly:  true,
		},
	}
	doc := Generate(routes, Options{})
	if got := len(doc.Service.RPCs); got != 1 {
		names := make([]string, 0, len(doc.Service.RPCs))
		for _, r := range doc.Service.RPCs {
			names = append(names, r.Name)
		}
		t.Fatalf("RPC count = %d (%v), want 1 (doc-only must be skipped)", got, names)
	}
}

// A route an external authority owns speaks the standard's wire format, not a
// proto3 message: the descriptor declares no RPC for it, and its first-party
// neighbor keeps its own.
func TestGenerate_SkipsRoutesAnExternalAuthorityOwns(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "external-contract-operations", "an-external-route-is-absent-from-the-protobuf-service")
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/v2/_putnami/capabilities", ReturnsSchema: typeOf[ProtoUser]()},
		{
			Method:        "GET",
			Path:          "/v2/{name}/manifests/{reference}",
			ReturnsSchema: typeOf[ProtoUser](),
			Meta: api.EndpointMeta{ClientOptions: &api.ClientOperationOptions{
				External: "OCI Distribution Specification v1.1",
			}},
		},
	}
	doc := Generate(routes, Options{})
	if doc.GenerationErr != nil {
		t.Fatalf("Generate: %v", doc.GenerationErr)
	}
	if len(doc.Service.RPCs) != 1 || doc.Service.RPCs[0].Path != "/v2/_putnami/capabilities" {
		t.Fatalf("RPCs = %+v, want only the first-party route", doc.Service.RPCs)
	}
	if _, bound := doc.RouteMethods()["GET /v2/{name}/manifests/{reference}"]; bound {
		t.Fatal("the external route is bound to a protobuf method")
	}
	if strings.Contains(doc.Content, "Manifests") {
		t.Fatalf("the rendered proto names the external route:\n%s", doc.Content)
	}
}

type protoOpaqueEntry struct {
	Attributes map[string]any `json:"attributes"`
}

type protoOpaqueBody struct {
	Name    string             `json:"name"`
	Entries []protoOpaqueEntry `json:"entries"`
}

type protoRawReply struct {
	Payload *json.RawMessage `json:"payload"`
}

type protoAnyQuery struct {
	Filter any `json:"filter"`
}

// A route that reaches opaque JSON at any depth — a map of the empty interface
// inside a slice of structs, a pointer to json.RawMessage, an empty-interface
// query field — is left out of the descriptor instead of refusing the whole
// provider, and its neighbors keep their RPCs.
func TestGenerate_LeavesRoutesCarryingOpaqueJSONOutOfTheDescriptor(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "opaque-json", "a-route-carrying-opaque-json-keeps-rest-and-is-left-out-of-the-protobuf-descriptor")
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/users", ReturnsSchema: typeOf[ProtoUser]()},
		{Method: "POST", Path: "/audit", BodySchema: typeOf[protoOpaqueBody](), ReturnsSchema: typeOf[ProtoUser]()},
		{Method: "GET", Path: "/raw", ReturnsSchema: typeOf[protoRawReply]()},
		{Method: "GET", Path: "/search", QuerySchema: typeOf[protoAnyQuery](), ReturnsSchema: typeOf[ProtoUser]()},
		{Method: "PUT", Path: "/values", BodySchema: typeOf[[]any]()},
	}
	doc := Generate(routes, Options{})
	if doc.GenerationErr != nil {
		t.Fatalf("an opaque route must not refuse the provider: %v", doc.GenerationErr)
	}
	methods := doc.RouteMethods()
	if _, ok := methods["GET /users"]; !ok || len(methods) != 1 {
		t.Fatalf("route methods = %v, want only GET /users", methods)
	}
	for _, message := range doc.Messages {
		if strings.Contains(message.Name, "Audit") || strings.Contains(message.Name, "Raw") || strings.Contains(message.Name, "protoOpaque") {
			t.Errorf("message %q of an opaque route was published", message.Name)
		}
	}
	if !strings.Contains(doc.Content, "rpc ListUsers") {
		t.Fatalf("the neighboring route lost its RPC:\n%s", doc.Content)
	}
}

func TestGenerate_DisambiguatesCollidingRPCNames(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "colliding-rpc-names-are-disambiguated")
	// POST /users and POST /users/{id} both reduce to CreateUsers because path
	// params are dropped from the subject. They must get distinct RPC names and
	// both request bodies must survive (the message dedup-by-name previously
	// dropped one).
	type firstBody struct {
		A string `json:"a"`
	}
	type secondBody struct {
		B string `json:"b"`
	}
	routes := []api.DiscoveredRoute{
		{Method: "POST", Path: "/users", BodySchema: typeOf[firstBody]()},
		{Method: "POST", Path: "/users/{id}", ParamsSchema: typeOf[ProtoUserParams](), BodySchema: typeOf[secondBody]()},
	}
	doc := Generate(routes, Options{})

	if got := len(doc.Service.RPCs); got != 2 {
		t.Fatalf("RPC count = %d, want 2", got)
	}
	seen := map[string]int{}
	for _, r := range doc.Service.RPCs {
		seen[r.Name]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("RPC name %q emitted %d times, want unique", name, n)
		}
	}
	if seen["CreateUsers"] != 1 || seen["CreateUsersById"] != 1 {
		t.Errorf("expected CreateUsers + CreateUsersById, got %v", seen)
	}

	first := findMessage(t, doc, "CreateUsersBody")
	if !hasField(first, "a", "string") {
		t.Errorf("first route body dropped: %+v", first.Fields)
	}
	second := findMessage(t, doc, "CreateUsersByIdBody")
	if !hasField(second, "b", "string") {
		t.Errorf("second route body dropped: %+v", second.Fields)
	}

	// No duplicate rpc line in the rendered output (protoc would reject it).
	if n := strings.Count(doc.Content, "rpc CreateUsers("); n != 1 {
		t.Errorf("found %d `rpc CreateUsers(` lines, want 1:\n%s", n, doc.Content)
	}
}

func TestGenerate_DisambiguatesIdenticalRoutesNumerically(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "duplicate-routes-are-disambiguated-numerically")
	// Two genuinely identical (method, path) routes with no path params can't be
	// told apart by folding params, so the second falls back to a stable numeric
	// suffix rather than colliding.
	routes := []api.DiscoveredRoute{
		{Method: "POST", Path: "/jobs", BodySchema: typeOf[ProtoCreateUserBody]()},
		{Method: "POST", Path: "/jobs", BodySchema: typeOf[ProtoCreateUserBody]()},
	}
	doc := Generate(routes, Options{})

	if got := len(doc.Service.RPCs); got != 2 {
		t.Fatalf("RPC count = %d, want 2", got)
	}
	names := map[string]int{}
	for _, r := range doc.Service.RPCs {
		names[r.Name]++
	}
	if names["CreateJobs"] != 1 || names["CreateJobs2"] != 1 {
		t.Errorf("want CreateJobs + CreateJobs2, got %v", names)
	}
}

func TestGenerate_SliceReturnBecomesRepeated(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/users", ReturnsSchema: typeOf[[]ProtoUser]()},
	}
	doc := Generate(routes, Options{})

	reply := findMessage(t, doc, "ListUsersReply")
	var value *Field
	for i := range reply.Fields {
		if reply.Fields[i].Name == "value" {
			value = &reply.Fields[i]
		}
	}
	if value == nil {
		t.Fatalf("reply missing value field: %+v", reply.Fields)
	}
	if !value.Repeated || value.Type != "ProtoUser" {
		t.Errorf("value field = %+v, want repeated ProtoUser", *value)
	}
	findMessage(t, doc, "ProtoUser") // element message emitted
	if !strings.Contains(doc.Content, "repeated ProtoUser value = 1;") {
		t.Errorf("rendered output missing repeated value field:\n%s", doc.Content)
	}
}

func TestGenerate_TimeMapsToWellKnownTimestamp(t *testing.T) {
	type stamped struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
	}
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/events/{id}", ReturnsSchema: typeOf[stamped]()},
	}
	doc := Generate(routes, Options{})

	reply := findMessage(t, doc, "GetEventsReply")
	if !hasField(reply, "createdAt", "google.protobuf.Timestamp") {
		t.Errorf("time.Time not mapped to Timestamp: %+v", reply.Fields)
	}
	for _, m := range doc.Messages {
		if m.Name == "Time" {
			t.Errorf("time.Time should not emit a nested Time message; have %v", messageNames(doc.Messages))
		}
	}
	if len(doc.Imports) != 1 || doc.Imports[0] != "google/protobuf/timestamp.proto" {
		t.Errorf("imports = %v, want [google/protobuf/timestamp.proto]", doc.Imports)
	}
	if !strings.Contains(doc.Content, `import "google/protobuf/timestamp.proto";`) {
		t.Errorf("rendered output missing import line:\n%s", doc.Content)
	}
}

// A struct with no exported field is an empty closed object in JSON, and the
// OpenAPI projection describes it as one. Mapping it to `bytes` made the
// descriptor promise base64 text for a value the provider sends as `{}`, so a
// proto client and a REST client decoded different things from one declaration.
func TestGenerate_ZeroFieldStructIsAnEmptyMessage(t *testing.T) {
	type holder struct {
		ID   string   `json:"id"`
		Meta struct{} `json:"meta"`
	}
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/things/{id}", ReturnsSchema: typeOf[holder]()},
	}
	doc := Generate(routes, Options{})

	reply := findMessage(t, doc, "GetThingsReply")
	var meta Field
	for _, f := range reply.Fields {
		if f.Name == "meta" {
			meta = f
		}
	}
	if meta.Kind != FieldKindMessage {
		t.Fatalf("zero-field struct should map to a message: %+v", reply.Fields)
	}
	empty := findMessage(t, doc, meta.Type)
	if len(empty.Fields) != 0 {
		t.Errorf("message %q should be empty: %+v", meta.Type, empty.Fields)
	}
}

func TestGenerate_TypeMappingContract(t *testing.T) {
	type Inner struct {
		Label string `json:"label"`
	}
	type AllKinds struct {
		S      string            `json:"s"`
		B      bool              `json:"b"`
		I      int               `json:"i"`
		I32    int32             `json:"i32"`
		I64    int64             `json:"i64"`
		U      uint              `json:"u"`
		U32    uint32            `json:"u32"`
		U64    uint64            `json:"u64"`
		F32    float32           `json:"f32"`
		F64    float64           `json:"f64"`
		Nested Inner             `json:"nested"`
		List   []string          `json:"list"`
		M      map[string]string `json:"m"`
		D      time.Duration     `json:"d"`
	}
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/all/{id}", ReturnsSchema: typeOf[AllKinds]()},
	}
	doc := Generate(routes, Options{})

	reply := findMessage(t, doc, "GetAllReply")
	want := map[string]string{
		"s": "string", "b": "bool",
		// Go int and uint are 64-bit values in the published JSON schema, so the
		// descriptor states the same width. Narrowing them to 32 bits was a
		// transport side effect, never a declaration (D0.2).
		"i": "int64", "i32": "int32", "i64": "int64",
		"u": "uint64", "u32": "uint32", "u64": "uint64",
		"f32": "float", "f64": "double",
		"nested": "Inner", "list": "string", "m": "map",
		"d": "google.protobuf.Duration",
	}
	for name, ty := range want {
		if !hasField(reply, name, ty) {
			t.Errorf("field %q: want type %q; fields=%+v", name, ty, reply.Fields)
		}
	}
	for _, f := range reply.Fields {
		if f.Name == "list" && !f.Repeated {
			t.Errorf("list field should be repeated: %+v", f)
		}
		if f.Name != "m" {
			continue
		}
		if f.Kind != FieldKindMap || f.Map == nil {
			t.Fatalf("map field should carry map metadata: %+v", f)
		}
		if f.Map.KeyType != "string" || f.Map.ValueKind != FieldKindScalar || f.Map.ValueType != "string" {
			t.Errorf("map metadata = %+v, want string → scalar string", *f.Map)
		}
		if !strings.Contains(doc.Content, "map<string, string> m = 13;") {
			t.Errorf("rendered proto should declare the map field:\n%s", doc.Content)
		}
	}
	inner := findMessage(t, doc, "Inner")
	if !hasField(inner, "label", "string") {
		t.Errorf("Inner message missing label: %+v", inner.Fields)
	}
}

func TestGenerate_SelfReferentialStructBreaksCycle(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/nodes/{id}", ReturnsSchema: typeOf[ProtoNode]()},
	}
	doc := Generate(routes, Options{})

	node := findMessage(t, doc, "ProtoNode")
	if !hasField(node, "id", "string") {
		t.Errorf("ProtoNode missing id: %+v", node.Fields)
	}
	var children *Field
	for i := range node.Fields {
		if node.Fields[i].Name == "children" {
			children = &node.Fields[i]
		}
	}
	if children == nil || !children.Repeated || children.Type != "ProtoNode" {
		t.Fatalf("children should be repeated ProtoNode (cycle broken): %+v", node.Fields)
	}
}

func TestGenerate_AnonymousStructGetsSynthesizedName(t *testing.T) {
	type holder struct {
		Inner struct {
			A string `json:"a"`
		} `json:"inner"`
	}
	routes := []api.DiscoveredRoute{
		{Method: "GET", Path: "/x/{id}", ReturnsSchema: typeOf[holder]()},
	}
	doc := Generate(routes, Options{})

	reply := findMessage(t, doc, "GetXReply")
	var innerType string
	for _, f := range reply.Fields {
		if f.Name == "inner" {
			innerType = f.Type
		}
	}
	if !strings.HasPrefix(innerType, "Anon_") {
		t.Fatalf("inner field type = %q, want synthesized Anon_ name; fields=%+v", innerType, reply.Fields)
	}
	anon := findMessage(t, doc, innerType)
	if !hasField(anon, "a", "string") {
		t.Errorf("anonymous message missing field a: %+v", anon.Fields)
	}
}

func TestGenerate_SanitizesInvalidFieldIdentifier(t *testing.T) {
	type weird struct {
		Field string `json:"my field"` // space is not a valid proto identifier char
	}
	routes := []api.DiscoveredRoute{
		{Method: "POST", Path: "/x", BodySchema: typeOf[weird]()},
	}
	doc := Generate(routes, Options{})

	body := findMessage(t, doc, "CreateXBody")
	if !hasField(body, "my_field", "string") {
		t.Errorf("invalid identifier not sanitized: %+v", body.Fields)
	}
	if strings.Contains(doc.Content, "my field") {
		t.Errorf("rendered proto leaked an invalid identifier:\n%s", doc.Content)
	}
}

func TestRender_MultiLineDescriptionStaysCommented(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{
			Method:      "POST",
			Path:        "/x",
			Description: "line1\nmalicious = injected",
			BodySchema:  typeOf[ProtoCreateUserBody](),
		},
	}
	doc := Generate(routes, Options{})

	if !strings.Contains(doc.Content, "  // line1\n") {
		t.Errorf("first description line not commented:\n%s", doc.Content)
	}
	if !strings.Contains(doc.Content, "  // malicious = injected\n") {
		t.Errorf("second description line not commented:\n%s", doc.Content)
	}
	for _, raw := range strings.Split(doc.Content, "\n") {
		if strings.TrimSpace(raw) == "malicious = injected" {
			t.Errorf("injected token escaped its comment:\n%s", doc.Content)
		}
	}
}

// TestGenerate_DistinctSameSimpleNameTypesDoNotClobber asserts that two structs
// sharing a simple name but living in different packages each get their own
// proto message (the second disambiguated with a numeric suffix), instead of the
// second silently overwriting the first and producing a wrong wire schema.
func TestGenerate_DistinctSameSimpleNameTypesDoNotClobber(t *testing.T) {
	routes := []api.DiscoveredRoute{
		{
			Method:     "POST",
			Path:       "/local",
			BodySchema: typeOf[protoCollideLocalBody](),
		},
		{
			Method:     "POST",
			Path:       "/dup",
			BodySchema: typeOf[protoCollideDupBody](),
		},
	}

	doc := Generate(routes, Options{PackageName: "test.v1"})

	// Both DupKind types must be present as distinct messages with their own
	// fields — proof that neither clobbered the other.
	first := findMessage(t, doc, "DupKind")
	second := findMessage(t, doc, "DupKind2")

	fromProto := hasField(first, "fromProto", "string")
	fromDup := hasField(second, "fromDup", "string")
	// Disambiguation does not order the two types, so accept either assignment as
	// long as both distinct field sets survive on distinct messages.
	if !((fromProto && fromDup) ||
		(hasField(first, "fromDup", "string") && hasField(second, "fromProto", "string"))) {
		t.Errorf("expected DupKind and DupKind2 to carry the two distinct field sets; got %+v and %+v",
			first.Fields, second.Fields)
	}
}

func TestUniqueMessageName(t *testing.T) {
	g := &generator{messages: map[string]Message{}}
	if got := g.uniqueMessageName("User"); got != "User" {
		t.Fatalf("first uniqueMessageName(User) = %q, want User", got)
	}
	g.messages["User"] = Message{Name: "User"}
	if got := g.uniqueMessageName("User"); got != "User2" {
		t.Fatalf("collision uniqueMessageName(User) = %q, want User2", got)
	}
	g.messages["User2"] = Message{Name: "User2"}
	if got := g.uniqueMessageName("User"); got != "User3" {
		t.Fatalf("second collision uniqueMessageName(User) = %q, want User3", got)
	}
}

// --- Helpers ---

func typeOf[T any]() reflect.Type {
	return reflect.TypeFor[T]()
}

func findMessage(t *testing.T, doc Document, name string) Message {
	t.Helper()
	for _, m := range doc.Messages {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("message %q not found; have %v", name, messageNames(doc.Messages))
	return Message{}
}

func messageNames(ms []Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func hasField(m Message, name, ty string) bool {
	for _, f := range m.Fields {
		if f.Name == name && f.Type == ty {
			return true
		}
	}
	return false
}

// --- Descriptor fidelity: the .proto and the JSON schema describe one shape ---

type protoPresenceBody struct {
	// Required and non-nullable: proto3 implicit presence carries it.
	ID string `json:"id" validate:"required"`
	// Absent from `required`: the JSON schema lets it be omitted.
	Note string `json:"note"`
	// Nullable: the JSON schema lets it be null.
	Owner *string `json:"owner" validate:"required"`
	// Repeated and map values are absent-as-empty on both wires.
	Tags   []string          `json:"tags" validate:"required"`
	Labels map[string]string `json:"labels" validate:"required"`
	// []byte is base64 JSON text, not a list of numbers.
	Payload []byte `json:"payload" validate:"required"`
}

func fieldNamed(t *testing.T, m Message, name string) Field {
	t.Helper()
	for _, f := range m.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("field %q not found in %q: %+v", name, m.Name, m.Fields)
	return Field{}
}

// Proto3 presence is a declaration, not a transport detail: the descriptor marks
// exactly the fields the published JSON schema lets a client omit or send as
// null. Without it a Connect client cannot tell an omitted value from a zero one
// while a REST client can, so one declaration meant two things.
func TestGenerate_PresenceFollowsTheDeclaredSchema(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "proto3-presence-follows-the-published-schema")
	doc := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/things", BodySchema: typeOf[protoPresenceBody]()},
	}, Options{PackageName: "test.v1"})
	if doc.GenerationErr != nil {
		t.Fatalf("Generate: %v", doc.GenerationErr)
	}
	body := findMessage(t, doc, "CreateThingsBody")

	for name, want := range map[string]bool{
		"id": false, "note": true, "owner": true,
		"tags": false, "labels": false, "payload": false,
	} {
		if got := fieldNamed(t, body, name).Optional; got != want {
			t.Errorf("field %q optional = %v, want %v", name, got, want)
		}
	}
	if payload := fieldNamed(t, body, "payload"); payload.Type != "bytes" || payload.Repeated {
		t.Errorf("[]byte should be a single bytes field: %+v", payload)
	}
	labels := fieldNamed(t, body, "labels")
	if labels.Kind != FieldKindMap || labels.Map == nil || labels.Map.ValueType != "string" {
		t.Fatalf("map field lost its key/value shape: %+v", labels)
	}
	if !strings.Contains(doc.Content, "optional string note = 2;") {
		t.Errorf("rendered proto should declare explicit presence:\n%s", doc.Content)
	}
}

// A request section that is not a struct used to be dropped whole, so the
// Connect method published an empty request message while the endpoint still
// required the body it declared.
func TestGenerate_NonStructRequestSectionKeepsTheDeclaredRoot(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "a-non-struct-request-root-keeps-its-declaration")
	type item struct {
		Label string `json:"label" validate:"required"`
	}
	doc := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/items", BodySchema: typeOf[[]item]()},
		{Method: "PUT", Path: "/counters", BodySchema: typeOf[map[string]int]()},
	}, Options{PackageName: "test.v1"})
	if doc.GenerationErr != nil {
		t.Fatalf("Generate: %v", doc.GenerationErr)
	}

	list := fieldNamed(t, findMessage(t, doc, "CreateItemsBody"), "value")
	if list.Type != "item" || !list.Repeated || list.Kind != FieldKindMessage {
		t.Errorf("[]item body lost its element type: %+v", list)
	}
	counters := fieldNamed(t, findMessage(t, doc, "UpdateCountersBody"), "value")
	if counters.Kind != FieldKindMap || counters.Map == nil || counters.Map.ValueType != "int64" {
		t.Errorf("map body lost its value type: %+v", counters)
	}
}

// An embedded struct is flattened by encoding/json and by the JSON schema, so
// the descriptor flattens it too. Nesting it here described a message shape no
// payload ever had.
func TestGenerate_EmbeddedFieldsAreFlattenedLikeTheJSONSchema(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "embedded-struct-fields-are-flattened-like-json")
	type audit struct {
		CreatedBy string `json:"createdBy" validate:"required"`
	}
	type record struct {
		audit
		ID     string `json:"id" validate:"required"`
		Hidden string `json:"-"`
	}
	doc := Generate([]api.DiscoveredRoute{
		{Method: "GET", Path: "/records/{id}", ReturnsSchema: typeOf[record]()},
	}, Options{PackageName: "test.v1"})
	if doc.GenerationErr != nil {
		t.Fatalf("Generate: %v", doc.GenerationErr)
	}
	reply := findMessage(t, doc, "GetRecordsReply")
	if len(reply.Fields) != 2 {
		t.Fatalf("reply fields = %+v, want createdBy and id only", reply.Fields)
	}
	if !hasField(reply, "createdBy", "string") || !hasField(reply, "id", "string") {
		t.Errorf("embedded field was not promoted: %+v", reply.Fields)
	}
}

// A declaration proto3 cannot carry is refused. Degrading it to `bytes` (the
// previous behavior for maps and every unhandled kind) published a field a
// Connect client would decode as base64 while a REST client decoded an object.
func TestGenerate_RefusesADeclarationProto3CannotCarry(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "a-declaration-proto3-cannot-carry-is-refused")
	type intKeyed struct {
		Counts map[int]string `json:"counts" validate:"required"`
	}
	type nestedList struct {
		Grid [][]string `json:"grid" validate:"required"`
	}
	type mapOfLists struct {
		Groups map[string][]string `json:"groups" validate:"required"`
	}
	for name, testCase := range map[string]struct {
		schema reflect.Type
		want   string
	}{
		"non-string map key": {typeOf[intKeyed](), "map key is a string on both transports"},
		"nested list":        {typeOf[nestedList](), "no repeated repeated field"},
		"map of lists":       {typeOf[mapOfLists](), "map values cannot be repeated"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := Generate([]api.DiscoveredRoute{
				{Method: "POST", Path: "/things", BodySchema: testCase.schema},
			}, Options{PackageName: "test.v1"})
			if doc.GenerationErr == nil {
				t.Fatalf("Generate accepted %s: %s", name, doc.Content)
			}
			if !strings.Contains(doc.GenerationErr.Error(), testCase.want) {
				t.Errorf("error = %v, want it to name %q", doc.GenerationErr, testCase.want)
			}
			if doc.Content != "" {
				t.Errorf("a refused document must not render:\n%s", doc.Content)
			}
		})
	}
}

// The route binding is the only join between a published route and the method
// identity a Connect URL carries. Two routes that reduce to the same base name
// must bind to the two distinct methods the descriptor declares.
func TestDocument_RouteMethodsBindEveryPublishedRoute(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "every-published-route-binds-to-one-protobuf-method")
	doc := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/users"},
		{Method: "POST", Path: "/users/{id}"},
		{Method: "GET", Path: "/docs", DocumentOnly: true},
	}, Options{PackageName: "test.v1"})
	if doc.GenerationErr != nil {
		t.Fatalf("Generate: %v", doc.GenerationErr)
	}
	methods := doc.RouteMethods()
	want := map[string]string{
		"POST /users":      "/test.v1.ApiService/CreateUsers",
		"POST /users/{id}": "/test.v1.ApiService/CreateUsersById",
	}
	if len(methods) != len(want) {
		t.Fatalf("route methods = %v, want exactly %v", methods, want)
	}
	for route, identity := range want {
		if methods[route] != identity {
			t.Errorf("route %q bound to %q, want %q", route, methods[route], identity)
		}
	}
}

// ClientDescriptor is what a Connect client decodes with: every wire fact the
// document carries has to survive the projection into the shared contract.
func TestClientDescriptorCarriesPresenceMapsAndKinds(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "the-shared-descriptor-carries-every-wire-fact")
	doc := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/things", BodySchema: typeOf[protoPresenceBody]()},
	}, Options{PackageName: "test.v1"})
	if doc.GenerationErr != nil {
		t.Fatalf("Generate: %v", doc.GenerationErr)
	}
	descriptor := ClientDescriptor(doc)

	var body *clientcontract.ProtobufMessage
	for i := range descriptor.Messages {
		if descriptor.Messages[i].Name == "CreateThingsBody" {
			body = &descriptor.Messages[i]
		}
	}
	if body == nil {
		t.Fatalf("descriptor lost the body message: %+v", descriptor.Messages)
	}
	byName := map[string]clientcontract.ProtobufField{}
	for _, field := range body.Fields {
		byName[field.Name] = field
	}
	if got := byName["note"]; !got.Optional || got.TypeKind != "scalar" {
		t.Errorf("note field lost presence or kind: %+v", got)
	}
	if got := byName["payload"]; got.Type != "bytes" || got.TypeKind != "scalar" {
		t.Errorf("payload field lost its bytes shape: %+v", got)
	}
	labels := byName["labels"]
	if labels.TypeKind != "map" || labels.Map == nil {
		t.Fatalf("labels field lost its map metadata: %+v", labels)
	}
	if labels.Map.KeyType != "string" || labels.Map.ValueKind != "scalar" || labels.Map.ValueType != "string" {
		t.Errorf("map metadata = %+v, want string → scalar string", *labels.Map)
	}
	if got := byName["tags"]; !got.Repeated || got.Optional {
		t.Errorf("repeated field must not carry presence: %+v", got)
	}
}

// The route walker produces no oneof today: a Go declaration has no proto-shaped
// oneof, and a first-party tagged union carries a JSON discriminator a proto3
// oneof does not (ADR 0002). The Document type and the renderer still carry the
// shape, because the descriptor a Connect client decodes with does, so this
// locks in how a oneof renders and projects for whoever supplies one.
func TestRenderAndProject_CarryADeclaredOneOf(t *testing.T) {
	doc := Document{
		PackageName: "test.v1",
		Service:     Service{Name: "ApiService", RPCs: []RPC{{Name: "GetOwner", RequestType: "GetOwnerRequest", ReplyType: "Owner"}}},
		Messages: []Message{
			{Name: "GetOwnerRequest"},
			{Name: "Owner", OneOfs: []string{"holder"}, Fields: []Field{
				{Name: "id", Type: "string", Kind: FieldKindScalar, Number: 1},
				{Name: "user_id", Type: "string", Kind: FieldKindScalar, Number: 2, OneOf: "holder"},
				{Name: "service_id", Type: "string", Kind: FieldKindScalar, Number: 3, OneOf: "holder"},
			}},
		},
	}
	rendered := render(doc, "")
	for _, want := range []string{
		"  string id = 1;\n",
		"  oneof holder {\n",
		"    string user_id = 2;\n",
		"    string service_id = 3;\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered proto missing %q:\n%s", want, rendered)
		}
	}

	descriptor := ClientDescriptor(doc)
	owner := descriptor.Messages[1]
	if len(owner.OneOfs) != 1 || owner.OneOfs[0] != "holder" {
		t.Fatalf("descriptor lost the oneof group: %+v", owner)
	}
	if owner.Fields[1].OneOf != "holder" || owner.Fields[2].OneOf != "holder" {
		t.Errorf("descriptor lost the oneof membership: %+v", owner.Fields)
	}
}

// A Go kind with no proto3 form must stop generation rather than publish a field
// whose content no client can decode.
func TestGenerate_RefusesAKindWithNoProto3Form(t *testing.T) {
	type holder struct {
		Signal chan int `json:"signal"`
	}
	doc := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/things", BodySchema: typeOf[holder]()},
	}, Options{})
	if doc.GenerationErr == nil || !strings.Contains(doc.GenerationErr.Error(), "has no proto3 representation") {
		t.Fatalf("GenerationErr = %v, want a refusal naming the kind", doc.GenerationErr)
	}
}

// A map of maps has no proto3 form either: map values cannot be maps.
func TestGenerate_RefusesAMapOfMaps(t *testing.T) {
	type holder struct {
		Nested map[string]map[string]string `json:"nested"`
	}
	doc := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/things", BodySchema: typeOf[holder]()},
	}, Options{})
	if doc.GenerationErr == nil || !strings.Contains(doc.GenerationErr.Error(), "map values cannot be maps") {
		t.Fatalf("GenerationErr = %v, want a map-of-maps refusal", doc.GenerationErr)
	}
}

// A pointer root can be absent, so the wrapper field carries presence; a list of
// maps at the root is the repeated-map proto3 forbids.
func TestGenerate_RootValueFieldFollowsTheDeclaredRoot(t *testing.T) {
	type item struct {
		Label string `json:"label" validate:"required"`
	}
	optional := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/things", BodySchema: typeOf[*string]()},
	}, Options{})
	if optional.GenerationErr != nil {
		t.Fatalf("Generate: %v", optional.GenerationErr)
	}
	value := fieldNamed(t, findMessage(t, optional, "CreateThingsBody"), "value")
	if value.Type != "string" || !value.Optional {
		t.Errorf("a pointer root must carry presence: %+v", value)
	}

	repeatedMap := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/things", BodySchema: typeOf[[]map[string]item]()},
	}, Options{})
	if repeatedMap.GenerationErr == nil || !strings.Contains(repeatedMap.GenerationErr.Error(), "repeated map") {
		t.Fatalf("GenerationErr = %v, want a repeated-map refusal", repeatedMap.GenerationErr)
	}
}

// Every projected field states its kind. A request message's section fields
// (params / query / body) reference sub-messages, and leaving their kind empty
// published a descriptor the strict reader refuses outright — for every route
// that declares params, a query or a body, which is nearly every route.
func TestClientDescriptorStatesTheKindOfEveryField(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "connect-descriptor-parity", "every-projected-field-states-its-kind")
	type params struct {
		ID string `json:"id" validate:"required"`
	}
	doc := Generate([]api.DiscoveredRoute{
		{Method: "POST", Path: "/things/{id}", ParamsSchema: typeOf[params](), BodySchema: typeOf[protoPresenceBody]()},
	}, Options{PackageName: "test.v1"})
	if doc.GenerationErr != nil {
		t.Fatalf("Generate: %v", doc.GenerationErr)
	}
	for _, message := range ClientDescriptor(doc).Messages {
		for _, field := range message.Fields {
			if field.TypeKind == "" {
				t.Errorf("message %q field %q states no kind: %+v", message.Name, field.Name, field)
			}
		}
	}
}
