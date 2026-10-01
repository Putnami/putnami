package openapi

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Test types ---

type UserParams struct {
	ID string `json:"id" validate:"required,uuid" description:"User ID"`
}

type ListUsersQuery struct {
	Page  int    `json:"page" validate:"min=1"`
	Limit int    `json:"limit" validate:"min=1,max=100"`
	Sort  string `json:"sort"`
}

type CreateUserBody struct {
	Name  string `json:"name" validate:"required,minlen=2"`
	Email string `json:"email" validate:"required,email"`
	Age   int    `json:"age" validate:"min=0,max=150"`
}

type UserResponse struct {
	ID    string `json:"id" validate:"uuid"`
	Name  string `json:"name"`
	Email string `json:"email" validate:"email"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// --- GenerateSpec Tests ---

func TestGenerateSpecBasic(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:      "GET",
			Path:        "/users",
			Description: "List all users",
			Returns:     reflect.TypeOf([]UserResponse{}),
		},
		{
			Method:      "POST",
			Path:        "/users",
			Description: "Create a new user",
			Body:        reflect.TypeOf(CreateUserBody{}),
			Returns:     reflect.TypeOf(UserResponse{}),
		},
	}

	doc := GenerateSpec(routes, Options{
		Title:   "Test API",
		Version: "1.0.0",
	})

	if doc.OpenAPI != "3.0.3" {
		t.Errorf("expected openapi 3.0.3, got %q", doc.OpenAPI)
	}
	if doc.Info.Title != "Test API" {
		t.Errorf("expected title 'Test API', got %q", doc.Info.Title)
	}
	if doc.Info.Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %q", doc.Info.Version)
	}

	// Should have /users path
	pathItem, ok := doc.Paths["/users"]
	if !ok {
		t.Fatal("expected /users path")
	}

	// Should have GET and POST operations
	if _, ok := pathItem["get"]; !ok {
		t.Error("expected GET operation on /users")
	}
	if _, ok := pathItem["post"]; !ok {
		t.Error("expected POST operation on /users")
	}
}

func TestGenerateSpecDuplicateRoutesResolveDeterministically(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "colliding-routes-are-disambiguated-in-the-openapi-document")
	routesA := []DiscoveredRoute{
		{Method: "GET", Path: "/users", Description: "z route"},
		{Method: "GET", Path: "/users", Description: "a route"},
	}
	routesB := []DiscoveredRoute{
		{Method: "GET", Path: "/users", Description: "a route"},
		{Method: "GET", Path: "/users", Description: "z route"},
	}

	docA := GenerateSpec(routesA, Options{Title: "Test", Version: "1.0.0"})
	docB := GenerateSpec(routesB, Options{Title: "Test", Version: "1.0.0"})
	jsonA, err := docA.JSON()
	if err != nil {
		t.Fatalf("docA.JSON: %v", err)
	}
	jsonB, err := docB.JSON()
	if err != nil {
		t.Fatalf("docB.JSON: %v", err)
	}
	if string(jsonA) != string(jsonB) {
		t.Fatalf("duplicate route output differs by input order:\nA:\n%s\nB:\n%s", jsonA, jsonB)
	}
}

func TestGenerateSpecStreamEndpoint(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:      "GET",
			Path:        "/notifications",
			StreamMode:  "server",
			Description: "Subscribe to notifications",
			Returns:     reflect.TypeOf(UserResponse{}),
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})
	op := doc.Paths["/notifications"]["get"]

	if !strings.Contains(op.Description, "Stream endpoint (WebSocket, SSE)") {
		t.Errorf("description missing stream note: %q", op.Description)
	}
	if _, ok := op.Responses["101"]; !ok {
		t.Fatal("missing 101 WebSocket response")
	}
	resp, ok := op.Responses["200"]
	if !ok {
		t.Fatal("missing 200 SSE response")
	}
	if _, ok := resp.Content["text/event-stream"]; !ok {
		t.Errorf("missing text/event-stream content: %+v", resp.Content)
	}
	if op.RequestBody != nil {
		t.Errorf("stream endpoint should not emit HTTP requestBody, got %+v", op.RequestBody)
	}
}

func TestGenerateSpecPathParams(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:      "GET",
			Path:        "/users/{id}",
			Description: "Get a user by ID",
			Params:      reflect.TypeOf(UserParams{}),
			Returns:     reflect.TypeOf(UserResponse{}),
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})

	pathItem, ok := doc.Paths["/users/{id}"]
	if !ok {
		t.Fatal("expected /users/{id} path")
	}

	op := pathItem["get"]
	if len(op.Parameters) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(op.Parameters))
	}
	param := op.Parameters[0]
	if param.Name != "id" {
		t.Errorf("expected param name 'id', got %q", param.Name)
	}
	if param.In != "path" {
		t.Errorf("expected param in 'path', got %q", param.In)
	}
	if !param.Required {
		t.Error("path params should be required")
	}
	if param.Description != "User ID" {
		t.Errorf("expected description 'User ID', got %q", param.Description)
	}
}

func TestGenerateSpecQueryParams(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method: "GET",
			Path:   "/users",
			Query:  reflect.TypeOf(ListUsersQuery{}),
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})
	op := doc.Paths["/users"]["get"]

	if len(op.Parameters) != 3 {
		t.Fatalf("expected 3 query params, got %d", len(op.Parameters))
	}

	// Check that all are "in: query"
	for _, p := range op.Parameters {
		if p.In != "query" {
			t.Errorf("expected query param, got %q for %q", p.In, p.Name)
		}
	}
}

func TestGenerateSpecRequestBody(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method: "POST",
			Path:   "/users",
			Body:   reflect.TypeOf(CreateUserBody{}),
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})
	op := doc.Paths["/users"]["post"]

	if op.RequestBody == nil {
		t.Fatal("expected request body")
	}
	if !op.RequestBody.Required {
		t.Error("expected required request body")
	}

	content, ok := op.RequestBody.Content["application/json"]
	if !ok {
		t.Fatal("expected application/json content type")
	}
	if content.Schema == nil {
		t.Fatal("expected schema in request body")
	}
	// A named body struct is promoted to a component and referenced via $ref so
	// downstream client generators can name the type.
	if content.Schema.Ref != "#/components/schemas/CreateUserBody" {
		t.Errorf("expected request body $ref to CreateUserBody, got ref=%q type=%q", content.Schema.Ref, content.Schema.Type)
	}
	if doc.Components == nil || doc.Components.Schemas == nil {
		t.Fatal("expected component schemas for the promoted body type")
	}
	body, ok := doc.Components.Schemas["CreateUserBody"]
	if !ok {
		t.Fatalf("expected components.schemas.CreateUserBody, got keys %v", doc.Components.Schemas)
	}
	if body.Type != "object" {
		t.Errorf("expected object schema, got %q", body.Type)
	}
	// Check required fields
	if !contains(body.Required, "name") || !contains(body.Required, "email") {
		t.Errorf("expected name and email as required, got %v", body.Required)
	}
}

func TestGenerateSpecResponses(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:  "GET",
			Path:    "/users/{id}",
			Returns: reflect.TypeOf(UserResponse{}),
			Throws: []ThrowsMeta{
				{Status: 404, Description: "User not found"},
				{Status: 400, Description: "Bad request", Schema: reflect.TypeOf(ErrorResponse{})},
			},
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})
	op := doc.Paths["/users/{id}"]["get"]

	// 200 response
	resp200, ok := op.Responses["200"]
	if !ok {
		t.Fatal("expected 200 response")
	}
	if resp200.Content == nil {
		t.Error("expected content in 200 response")
	}

	// 404 response
	resp404, ok := op.Responses["404"]
	if !ok {
		t.Fatal("expected 404 response")
	}
	if resp404.Description != "User not found" {
		t.Errorf("expected 'User not found', got %q", resp404.Description)
	}

	// 400 response with schema
	resp400, ok := op.Responses["400"]
	if !ok {
		t.Fatal("expected 400 response")
	}
	if resp400.Content == nil {
		t.Error("expected content in 400 response")
	}
}

func TestGenerateSpecDefaultErrorResponses(t *testing.T) {
	routes := []DiscoveredRoute{
		{Method: "GET", Path: "/health"},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})
	op := doc.Paths["/health"]["get"]

	for _, status := range []string{"400", "500"} {
		resp, ok := op.Responses[status]
		if !ok {
			t.Fatalf("missing default %s response", status)
		}
		if resp.Content == nil || resp.Content["application/json"].Schema == nil {
			t.Fatalf("%s response missing JSON error schema: %+v", status, resp)
		}
		schema := resp.Content["application/json"].Schema
		if schema.Properties["error"].Type != "string" || schema.Properties["message"].Type != "string" {
			t.Errorf("%s response schema missing standard error/message fields: %+v", status, schema.Properties)
		}
		if _, ok := schema.Properties["details"]; !ok {
			t.Errorf("%s response schema missing details field", status)
		}
		if got := schema.Properties["details"]; got.OpaqueJSON != "any" || got.Type != "" {
			t.Errorf("%s response details schema = %+v, want the opaque JSON declaration", status, got)
		}
	}
}

func TestGenerateSpecMayThrowErrorCodes(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:     "GET",
			Path:       "/users/{id}",
			ErrorCodes: []perrors.Code{perrors.CodeNotFound, perrors.CodeConflict},
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})
	op := doc.Paths["/users/{id}"]["get"]

	if op.Responses["404"].Description != "Not Found" {
		t.Errorf("404 description = %q, want %q", op.Responses["404"].Description, "Not Found")
	}
	if op.Responses["409"].Description != "Conflict" {
		t.Errorf("409 description = %q, want %q", op.Responses["409"].Description, "Conflict")
	}
}

func TestGenerateSpecExplicitThrowsOverrideGeneratedErrors(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:     "GET",
			Path:       "/users/{id}",
			ErrorCodes: []perrors.Code{perrors.CodeNotFound},
			Throws: []ThrowsMeta{
				{Status: 404, Description: "User not found", Schema: reflect.TypeOf(ErrorResponse{})},
			},
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})
	resp := doc.Paths["/users/{id}"]["get"].Responses["404"]

	if resp.Description != "User not found" {
		t.Errorf("404 description = %q, want %q", resp.Description, "User not found")
	}
	if resp.Content == nil {
		t.Fatal("expected explicit 404 schema content")
	}
}

func TestGenerateSpecSecurity(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:      "DELETE",
			Path:        "/users/{id}",
			Description: "Delete a user",
			Security: &SecurityMeta{
				Roles:  []string{"admin"},
				Scopes: []string{"users:delete"},
			},
		},
	}

	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})

	// Should have security schemes
	if doc.Components == nil || doc.Components.SecuritySchemes == nil {
		t.Fatal("expected security schemes in components")
	}
	if _, ok := doc.Components.SecuritySchemes["bearerAuth"]; !ok {
		t.Error("expected bearerAuth security scheme")
	}

	op := doc.Paths["/users/{id}"]["delete"]
	if len(op.Security) == 0 {
		t.Error("expected security on delete operation")
	}

	// Description should include roles and scopes
	if !strings.Contains(op.Description, "Required roles: admin") {
		t.Errorf("expected roles in description, got %q", op.Description)
	}
	if !strings.Contains(op.Description, "Required scopes: users:delete") {
		t.Errorf("expected scopes in description, got %q", op.Description)
	}
}

func TestGenerateSpecNoSecurity(t *testing.T) {
	routes := []DiscoveredRoute{
		{Method: "GET", Path: "/health"},
	}
	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})

	// No security → no components
	if doc.Components != nil {
		t.Error("expected no components when no security is needed")
	}
}

// --- Schema Generation Tests ---

func TestStructToSchemaTypes(t *testing.T) {
	type AllTypes struct {
		Str     string   `json:"str"`
		Int     int      `json:"int"`
		Float   float64  `json:"float"`
		Bool    bool     `json:"bool"`
		Strings []string `json:"strings"`
	}

	schema := structToSchema(reflect.TypeOf(AllTypes{}))
	if schema.Type != "object" {
		t.Errorf("expected object, got %q", schema.Type)
	}

	checks := map[string]string{
		"str":     "string",
		"int":     "integer",
		"float":   "number",
		"bool":    "boolean",
		"strings": "array",
	}
	for field, expectedType := range checks {
		prop, ok := schema.Properties[field]
		if !ok {
			t.Errorf("missing property %q", field)
			continue
		}
		if prop.Type != expectedType {
			t.Errorf("property %q: expected type %q, got %q", field, expectedType, prop.Type)
		}
	}
}

func TestStructToSchemaPreservesUntaggedJSONFieldName(t *testing.T) {
	type Body struct {
		Count int `validate:"required"`
	}
	generated := structToSchema(reflect.TypeOf(Body{}))
	if _, ok := generated.Properties["Count"]; !ok {
		t.Fatalf("untagged encoding/json field name was changed: %v", generated.Properties)
	}
	if _, ok := generated.Properties["count"]; ok {
		t.Fatalf("schema advertised a field encoding/json does not emit: %v", generated.Properties)
	}
}

func TestStructToSchemaMatchesEncodingJSONForUnexportedEmbedding(t *testing.T) {
	type hidden struct {
		Visible string `json:"visible"`
	}
	type Body struct {
		hidden
		Fallback string `json:"fallback"`
	}
	value := Body{hidden: hidden{Visible: "yes"}, Fallback: "exact"}
	wire, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"visible":"yes","fallback":"exact"}` {
		t.Fatalf("unexpected encoding/json proof: %s", wire)
	}
	generated := structToSchema(reflect.TypeOf(value))
	for _, name := range []string{"visible", "fallback"} {
		if _, ok := generated.Properties[name]; !ok {
			t.Fatalf("schema lost wire field %q: %v", name, generated.Properties)
		}
	}
}

func TestStructToSchemaInvalidTagFallsBackToGoFieldName(t *testing.T) {
	invalidTag := reflect.StructTag("json:" + strconv.Quote("bad\\name"))
	dynamicType := reflect.StructOf([]reflect.StructField{{
		Name: "Fallback",
		Type: reflect.TypeOf(""),
		Tag:  invalidTag,
	}})
	value := reflect.New(dynamicType).Elem()
	value.Field(0).SetString("exact")
	wire, err := json.Marshal(value.Interface())
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"Fallback":"exact"}` {
		t.Fatalf("encoding/json invalid tag fallback = %s", wire)
	}
	generated := structToSchema(dynamicType)
	if _, ok := generated.Properties["Fallback"]; !ok {
		t.Fatalf("schema lost encoding/json fallback field: %v", generated.Properties)
	}
	if _, ok := generated.Properties[`bad\name`]; ok {
		t.Fatalf("schema retained invalid JSON tag name: %v", generated.Properties)
	}
}

func TestStructToSchemaOmitsDashJSONFields(t *testing.T) {
	type WithHidden struct {
		Visible string `json:"visible"`
		Hidden  string `json:"-"`
		NoName  string `json:",omitempty"`
	}
	schema := structToSchema(reflect.TypeOf(WithHidden{}))
	if _, ok := schema.Properties["visible"]; !ok {
		t.Error("visible must be present")
	}
	if _, ok := schema.Properties["hidden"]; ok {
		t.Error(`json:"-" field must be omitted from the schema`)
	}
	// json:",omitempty" carries no name override, so encoding/json uses the
	// exact Go field name — it must not be dropped or case-normalized.
	if _, ok := schema.Properties["NoName"]; !ok {
		t.Errorf(`json:",omitempty" should keep the field name; got props %v`, schema.Properties)
	}
}

func TestStructToSchemaPromotesEmbeddedFields(t *testing.T) {
	type Base struct {
		ID        string `json:"id"`
		CreatedAt string `json:"createdAt"`
	}
	type Resource struct {
		Base
		Name string `json:"name"`
	}
	schema := structToSchema(reflect.TypeOf(Resource{}))
	for _, want := range []string{"id", "createdAt", "name"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("embedded field %q should be promoted to the parent; got props %v", want, schema.Properties)
		}
	}
	if _, ok := schema.Properties["base"]; ok {
		t.Error("an untagged embedded struct must be flattened, not nested as its own property")
	}
}

func TestStructToSchemaShallowerEmbeddedFieldWins(t *testing.T) {
	// encoding/json: a shallower field wins a JSON-name collision over a deeper
	// one, regardless of embed declaration order. The deeper embed (Mid→Deep,
	// depth 2) is declared BEFORE the shallower one (Shallow, depth 1); the
	// depth-1 string field must win, so the property type is string, not integer.
	type DeepN struct {
		Name int `json:"name"`
	}
	type MidN struct {
		DeepN
	}
	type ShallowN struct {
		Name string `json:"name"`
	}
	type OuterN struct {
		MidN
		ShallowN
	}
	schema := structToSchema(reflect.TypeOf(OuterN{}))
	prop, ok := schema.Properties["name"]
	if !ok {
		t.Fatalf("expected a promoted 'name' property; got %v", schema.Properties)
	}
	if prop.Type != "string" {
		t.Errorf("shallower ShallowN.Name (string) must win over deeper DeepN.Name (int); got type %q", prop.Type)
	}
}

func TestStructToSchemaDropsSameDepthAmbiguousField(t *testing.T) {
	// encoding/json: two fields resolving to the same JSON name at the SAME depth
	// (here both at depth 1, both untagged so both fall back to the field name)
	// are ambiguous and dropped from the wire entirely — the schema must not
	// advertise a property that never serializes. (Two same-depth *tagged* fields
	// are dropped the same way; the untagged form is used here so `go vet`'s
	// jsontag analyzer does not flag a deliberately duplicated tag.)
	type AmbA struct {
		Shared string
		AOnly  string `json:"aOnly"`
	}
	type AmbB struct {
		Shared string
		BOnly  string `json:"bOnly"`
	}
	type AmbCollision struct {
		AmbA
		AmbB
	}
	schema := structToSchema(reflect.TypeOf(AmbCollision{}))
	for _, want := range []string{"aOnly", "bOnly"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("non-conflicting field %q should be present; got %v", want, schema.Properties)
		}
	}
	// Only the two non-conflicting fields survive; the ambiguous Shared field
	// (under whatever name the field-name fallback produces) must be dropped.
	if len(schema.Properties) != 2 {
		t.Errorf("same-depth ambiguous field must be dropped, leaving only aOnly+bOnly; got %v", schema.Properties)
	}
}

func TestSchemaGenComponentNameDisambiguatesAcrossPackages(t *testing.T) {
	// A locally-declared "Time" and stdlib time.Time share a name but differ by
	// package; they must not collapse into one component slot.
	type Time struct{ X int }
	g := newSchemaGen(nil)
	local := g.componentName(reflect.TypeOf(Time{}))
	stdlib := g.componentName(reflect.TypeOf(time.Time{}))
	if local == stdlib {
		t.Errorf("same-named types from different packages must get distinct component names, both = %q", local)
	}
	if again := g.componentName(reflect.TypeOf(Time{})); again != local {
		t.Errorf("component name must be stable: %q then %q", local, again)
	}
}

func TestStructToSchemaFormats(t *testing.T) {
	schema := structToSchema(reflect.TypeOf(CreateUserBody{}))

	emailProp := schema.Properties["email"]
	if emailProp.Format != "email" {
		t.Errorf("expected email format, got %q", emailProp.Format)
	}
}

func TestStructToSchemaURLFormat(t *testing.T) {
	type LinkBody struct {
		Homepage string `json:"homepage" validate:"url"`
	}

	schema := structToSchema(reflect.TypeOf(LinkBody{}))

	prop := schema.Properties["homepage"]
	if prop.Format != "uri" {
		t.Errorf("expected url constraint to map to format %q, got %q", "uri", prop.Format)
	}
}

func TestStructToSchemaRequired(t *testing.T) {
	schema := structToSchema(reflect.TypeOf(CreateUserBody{}))

	if !contains(schema.Required, "name") {
		t.Error("expected 'name' in required")
	}
	if !contains(schema.Required, "email") {
		t.Error("expected 'email' in required")
	}
	// age is not required
	if contains(schema.Required, "age") {
		t.Error("'age' should not be required")
	}
}

// --- Operation ID Tests ---

func TestGenerateOperationID(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "stable-operation-identity", "the-openapi-operation-id-is-derived-the-same-way")
	tests := []struct {
		method, path, expected string
	}{
		{"GET", "/users", "getUsers"},
		{"POST", "/users", "postUsers"},
		{"GET", "/users/{id}", "getUsers_Id"},
		{"DELETE", "/users/{id}/posts/{postId}", "deleteUsers_Id_Posts_PostId"},
		{"GET", "/", "get"},
	}

	for _, tt := range tests {
		got := generateOperationID(tt.method, tt.path)
		if got != tt.expected {
			t.Errorf("operationId(%s %s) = %q, expected %q", tt.method, tt.path, got, tt.expected)
		}
	}
}

// --- Path Conversion Tests ---

func TestFrameworkPathToOpenAPI(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"/users/{id}", "/users/{id}"},
		{"/users/[id]", "/users/{id}"},
		{"/users/[id]/posts/[postId]", "/users/{id}/posts/{postId}"},
		{"/users", "/users"},
	}

	for _, tt := range tests {
		got := frameworkPathToOpenAPI(tt.input)
		if got != tt.expected {
			t.Errorf("frameworkPathToOpenAPI(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

// --- JSON Serialization Test ---

func TestDocumentJSON(t *testing.T) {
	routes := []DiscoveredRoute{
		{
			Method:      "GET",
			Path:        "/users",
			Description: "List users",
			Returns:     reflect.TypeOf([]UserResponse{}),
		},
	}

	doc := GenerateSpec(routes, Options{
		Title:       "My API",
		Version:     "2.0.0",
		Description: "A test API",
		Servers:     []Server{{URL: "https://api.example.com"}},
	})

	data, err := doc.JSON()
	if err != nil {
		t.Fatal(err)
	}

	// Parse back and verify structure
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed["openapi"] != "3.0.3" {
		t.Error("expected openapi: 3.0.3")
	}

	info := parsed["info"].(map[string]any)
	if info["title"] != "My API" {
		t.Errorf("expected title 'My API', got %v", info["title"])
	}

	servers := parsed["servers"].([]any)
	if len(servers) != 1 {
		t.Errorf("expected 1 server, got %d", len(servers))
	}
}

// --- Plugin Tests ---

func TestPluginAddRouteAndSpec(t *testing.T) {
	plugin := NewPlugin(PluginOptions{
		Title:   "Test API",
		Version: "1.0.0",
	})

	plugin.AddRoute(DiscoveredRoute{
		Method:      "GET",
		Path:        "/users",
		Description: "List users",
	})
	plugin.AddRoute(DiscoveredRoute{
		Method:      "POST",
		Path:        "/users",
		Description: "Create user",
		Body:        reflect.TypeOf(CreateUserBody{}),
	})

	// Spec should be nil before warmup
	if plugin.Spec() != nil {
		t.Error("spec should be nil before warmup")
	}

	// Configure generates the spec
	if err := plugin.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	spec := plugin.Spec()
	if spec == nil {
		t.Fatal("spec should be generated after warmup")
	}

	if spec.Info.Title != "Test API" {
		t.Errorf("expected title 'Test API', got %q", spec.Info.Title)
	}

	if len(spec.Paths) != 1 { // /users has both GET and POST
		t.Errorf("expected 1 path, got %d", len(spec.Paths))
	}

	pathItem := spec.Paths["/users"]
	if len(pathItem) != 2 {
		t.Errorf("expected 2 operations on /users, got %d", len(pathItem))
	}
}

func TestPluginDefaults(t *testing.T) {
	plugin := NewPlugin(PluginOptions{})

	if plugin.opts.Route != "/_/openapi.json" {
		t.Errorf("expected default route, got %q", plugin.opts.Route)
	}
	if plugin.opts.Title != "API" {
		t.Errorf("expected default title, got %q", plugin.opts.Title)
	}
	if plugin.opts.Version != "1.0.0" {
		t.Errorf("expected default version, got %q", plugin.opts.Version)
	}
}

// --- helpers ---

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// --- self-referential types must not recurse without bound ---

type Category struct {
	Name     string     `json:"name"`
	Children []Category `json:"children"`
}

func TestGenerateSpecRecursiveType(t *testing.T) {
	routes := []DiscoveredRoute{
		{Method: "POST", Path: "/categories", Body: reflect.TypeOf(Category{})},
	}
	// Previously a self-referential model stack-overflowed during generation.
	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})

	if doc.Components == nil || doc.Components.Schemas == nil {
		t.Fatal("expected a component schema for the recursive type")
	}
	cat, ok := doc.Components.Schemas["Category"]
	if !ok {
		t.Fatalf("expected components.schemas.Category, got keys %v", doc.Components.Schemas)
	}
	// The self-reference (children -> []Category) is emitted as a $ref back to
	// the component rather than inlined recursively.
	children := cat.Properties["children"]
	if children.Type != "array" || children.Items == nil {
		t.Fatalf("expected children to be an array, got %+v", children)
	}
	if children.Items.Ref != "#/components/schemas/Category" {
		t.Errorf("expected children.items.$ref to Category, got ref=%q type=%q", children.Items.Ref, children.Items.Type)
	}
	// The request body references the component too.
	body := doc.Paths["/categories"]["post"].RequestBody
	if body == nil || body.Content["application/json"].Schema.Ref != "#/components/schemas/Category" {
		t.Errorf("expected request body $ref to Category, got %+v", body)
	}
}

// --- well-known stdlib types map to correct schemas ---

func TestSchemaWellKnownTypes(t *testing.T) {
	type Event struct {
		At    time.Time     `json:"at"`
		AtPtr *time.Time    `json:"atPtr"`
		Blob  []byte        `json:"blob"`
		TTL   time.Duration `json:"ttl"`
	}
	schema := structToSchema(reflect.TypeOf(Event{}))
	want := map[string][2]string{ // field -> {type, format}
		"at":    {"string", "date-time"},
		"atPtr": {"string", "date-time"},
		"blob":  {"string", "byte"},
		"ttl":   {"integer", "int64"},
	}
	for f, w := range want {
		p := schema.Properties[f]
		if p.Type != w[0] || p.Format != w[1] {
			t.Errorf("field %q: got {type:%q format:%q}, want {type:%q format:%q}", f, p.Type, p.Format, w[0], w[1])
		}
	}
}

// --- validate constraints surface in the generated schema ---

func TestSchemaValidateConstraints(t *testing.T) {
	type Body struct {
		Age  int    `json:"age" validate:"min=0,max=150"`
		Name string `json:"name" validate:"minlen=2,maxlen=32"`
		Code string `json:"code" validate:"pattern=^[A-Z]+$"`
		Tier string `json:"tier" validate:"oneof=free|pro|max"`
	}
	schema := structToSchema(reflect.TypeOf(Body{}))

	age := schema.Properties["age"]
	if age.Minimum == nil || age.Minimum.String() != "0" || age.Maximum == nil || age.Maximum.String() != "150" {
		t.Errorf("age min/max: got %v/%v", age.Minimum, age.Maximum)
	}
	name := schema.Properties["name"]
	if name.MinLength == nil || *name.MinLength != 2 || name.MaxLength == nil || *name.MaxLength != 32 {
		t.Errorf("name minlen/maxlen: got %v/%v", name.MinLength, name.MaxLength)
	}
	if got := schema.Properties["code"].Pattern; got != "^[A-Z]+$" {
		t.Errorf("code pattern: got %q", got)
	}
	if got := schema.Properties["tier"].Enum; !reflect.DeepEqual(got, []string{"free", "pro", "max"}) {
		t.Errorf("tier enum: got %v", got)
	}
}

func TestSchemaDefaultsPreserveDeclaredJSONTypes(t *testing.T) {
	type Body struct {
		Label    string  `json:"label" default:"ready"`
		Enabled  bool    `json:"enabled" default:"false"`
		Wide     int64   `json:"wide" default:"9007199254740993"`
		Nullable *string `json:"nullable" default:"fallback"`
		Required int64   `json:"required" default:"7" validate:"required"`
	}

	generated := structToSchema(reflect.TypeOf(Body{}))
	want := map[string]string{
		"label":    `"ready"`,
		"enabled":  "false",
		"wide":     "9007199254740993",
		"nullable": `"fallback"`,
		"required": "7",
	}
	for name, expected := range want {
		if got := string(generated.Properties[name].Default); got != expected {
			t.Errorf("%s default = %q, want %q", name, got, expected)
		}
	}
	if !slices.Contains(generated.Required, "required") {
		t.Fatalf("required field with a default lost required presence: %v", generated.Required)
	}

	encoded, err := json.Marshal(generated)
	if err != nil {
		t.Fatalf("marshal schema defaults: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"wide":{"type":"integer","format":"int64","default":9007199254740993}`)) {
		t.Fatalf("wide default lost its exact numeric JSON token: %s", encoded)
	}
	neutral := schemaObjectToClient(&generated)
	if got := string(neutral.Properties["wide"].Default); got != "9007199254740993" {
		t.Fatalf("neutral client schema default = %q, want exact wide integer", got)
	}
}

func TestSchemaInvalidDefaultFailsSerialization(t *testing.T) {
	type Body struct {
		Count int64 `json:"count" default:"private-not-a-number"`
	}

	document := GenerateSpec([]DiscoveredRoute{{Method: "POST", Path: "/items", Body: reflect.TypeOf(Body{})}}, Options{})
	_, err := json.Marshal(document)
	if err == nil {
		t.Fatal("invalid numeric default serialized instead of failing explicitly")
	}
	if strings.Contains(err.Error(), "private-not-a-number") {
		t.Fatalf("generation error exposed the authored default: %v", err)
	}
}

// --- every named struct is promoted so nested objects stay named ---

type promoChild struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type promoParent struct {
	Name  string       `json:"name"`
	Child promoChild   `json:"child"`
	Kids  []promoChild `json:"kids"`
}

// A nested named struct must surface as a $ref to a component (not an anonymous
// inline object), both when embedded directly and as an array element. This is
// the exact degradation the issue describes: downstream generators cannot name an
// inline object, so nested models collapse to Record<string, unknown>/map[string]any.
func TestGenerateSpecPromotesNestedNamedStructs(t *testing.T) {
	routes := []DiscoveredRoute{
		{Method: "GET", Path: "/parents", Returns: reflect.TypeOf(promoParent{})},
	}
	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})

	// The response body references the parent component rather than inlining it.
	resp := doc.Paths["/parents"]["get"].Responses["200"]
	respSchema := resp.Content["application/json"].Schema
	if respSchema == nil || respSchema.Ref != "#/components/schemas/promoParent" {
		t.Fatalf("expected response $ref to promoParent, got %+v", respSchema)
	}

	if doc.Components == nil || doc.Components.Schemas == nil {
		t.Fatal("expected component schemas for promoted types")
	}
	parent, ok := doc.Components.Schemas["promoParent"]
	if !ok {
		t.Fatalf("expected components.schemas.promoParent, got keys %v", doc.Components.Schemas)
	}
	// Direct nested struct → $ref to the child component.
	child := parent.Properties["child"]
	if child.Ref != "#/components/schemas/promoChild" {
		t.Errorf("expected child.$ref to promoChild, got ref=%q type=%q", child.Ref, child.Type)
	}
	// Array of nested struct → items.$ref to the child component (NOT map/object).
	kids := parent.Properties["kids"]
	if kids.Type != "array" || kids.Items == nil {
		t.Fatalf("expected kids to be an array, got %+v", kids)
	}
	if kids.Items.Ref != "#/components/schemas/promoChild" {
		t.Errorf("expected kids.items.$ref to promoChild, got ref=%q type=%q", kids.Items.Ref, kids.Items.Type)
	}
	// The child component itself exists and carries its real properties.
	childComp, ok := doc.Components.Schemas["promoChild"]
	if !ok {
		t.Fatalf("expected components.schemas.promoChild, got keys %v", doc.Components.Schemas)
	}
	if childComp.Type != "object" || childComp.Properties["label"].Type != "string" {
		t.Errorf("promoChild component missing expected properties: %+v", childComp)
	}
}

// A named struct reused across multiple operations must resolve to ONE shared
// component (dedup), not a duplicated or renamed schema per operation.
func TestGenerateSpecDedupsSharedNamedStruct(t *testing.T) {
	routes := []DiscoveredRoute{
		{Method: "GET", Path: "/a", Returns: reflect.TypeOf(promoChild{})},
		{Method: "GET", Path: "/b", Returns: reflect.TypeOf(promoChild{})},
	}
	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})

	if doc.Components == nil || doc.Components.Schemas == nil {
		t.Fatal("expected component schemas")
	}
	count := 0
	for name := range doc.Components.Schemas {
		if strings.HasPrefix(name, "promoChild") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one promoChild component (dedup), got %d in %v", count, doc.Components.Schemas)
	}
	for _, path := range []string{"/a", "/b"} {
		got := doc.Paths[path]["get"].Responses["200"].Content["application/json"].Schema
		if got == nil || got.Ref != "#/components/schemas/promoChild" {
			t.Errorf("%s response should $ref the shared promoChild, got %+v", path, got)
		}
	}
}

// An anonymous struct has no type name to promote, so it must stay inline (a
// concrete object schema, never a $ref).
func TestGenerateSpecAnonymousStructStaysInline(t *testing.T) {
	anon := reflect.TypeFor[struct {
		X int `json:"x"`
	}]()
	routes := []DiscoveredRoute{
		{Method: "GET", Path: "/anon", Returns: anon},
	}
	doc := GenerateSpec(routes, Options{Title: "Test", Version: "1.0.0"})

	schema := doc.Paths["/anon"]["get"].Responses["200"].Content["application/json"].Schema
	if schema == nil {
		t.Fatal("expected an inline schema for the anonymous struct")
	}
	if schema.Ref != "" {
		t.Errorf("anonymous struct must not be promoted to a $ref, got %q", schema.Ref)
	}
	if schema.Type != "object" || schema.Properties["x"].Type != "integer" {
		t.Errorf("expected inline object with x:integer, got %+v", schema)
	}
}
