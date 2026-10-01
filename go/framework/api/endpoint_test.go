package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/inject"

	"go.putnami.dev/protocol/features/spectest"
)

// --- EndpointBuilder fluent API tests ---

func TestEndpointBuilder_FluentAPI(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "an-endpoint-carries-its-input-schemas")
	b := Endpoint("POST", "/items").
		Description("Create an item").
		Returns(Type[CreatedItem]()).
		Throws(400, "Validation error", nil).
		Throws(409, "Conflict", nil)

	if b.method != "POST" {
		t.Errorf("method = %q, want %q", b.method, "POST")
	}
	if b.path != "/items" {
		t.Errorf("path = %q, want %q", b.path, "/items")
	}
	if b.description != "Create an item" {
		t.Errorf("description = %q, want %q", b.description, "Create an item")
	}
	if b.returns == nil || b.returns.Name() != "CreatedItem" {
		t.Errorf("returns schema not captured: %v", b.returns)
	}
	if b.returnsStatus != 200 || b.returnsDescription != "Successful response" {
		t.Errorf("Returns default = %d %q, want 200 Successful response", b.returnsStatus, b.returnsDescription)
	}
	if len(b.throws) != 2 {
		t.Errorf("throws count = %d, want 2", len(b.throws))
	}
}

type CreatedItem struct {
	ID string `json:"id"`
}

func TestEndpointBuilder_Response(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "an-endpoint-carries-its-response-declaration")
	b := Endpoint("POST", "/items").
		Returns(Type[CreatedItem]()).
		Response(201, "Created", Type[CreatedItem]()).
		Response(204, "No content", nil)

	if len(b.responses) != 2 {
		t.Fatalf("responses count = %d, want 2", len(b.responses))
	}
	if b.responses[0].Status != 201 || b.responses[0].Description != "Created" {
		t.Errorf("response[0] = %+v", b.responses[0])
	}
	if b.responses[1].Schema != nil {
		t.Errorf("response[1] schema should be nil, got %v", b.responses[1].Schema)
	}
}

func TestEndpointBuilder_ReturnsStatusDeclaresPrimarySuccess(t *testing.T) {
	b := Endpoint("POST", "/items").ReturnsStatus(201, "Created", Type[CreatedItem]())
	if b.returnsStatus != 201 || b.returnsDescription != "Created" || b.returns != Type[CreatedItem]() {
		t.Fatalf("primary response = status %d description %q schema %v", b.returnsStatus, b.returnsDescription, b.returns)
	}
}

func TestEndpointBuilder_ReturnsStatusRejectsNonSuccessStatus(t *testing.T) {
	tests := map[string]int{
		"below success range": 199,
		"redirect":            300,
		"server error":        500,
	}
	for name, status := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				message, ok := recover().(string)
				if !ok || !strings.Contains(message, "status must be between 200 and 299") {
					t.Fatalf("panic = %q, want primary success status validation", message)
				}
			}()
			Endpoint("POST", "/items").ReturnsStatus(status, "Invalid", Type[CreatedItem]())
		})
	}
}

func TestEndpointBuilder_ReturnsStatusRejectsStreamEndpoints(t *testing.T) {
	tests := map[string]func(){
		"outgoing handle": func() {
			Endpoint("GET", "/feed").
				ReturnsStatus(201, "Created", StreamOf[CreatedItem]()).
				Handle(ServerStream(func(_ *ServerStreamContext[CreatedItem]) error { return nil }))
		},
		"incoming handle": func() {
			Endpoint("GET", "/upload").
				Body(StreamOf[CreatedItem]()).
				ReturnsStatus(201, "Created", Type[CreatedItem]()).
				Handle(ClientStream(func(_ *ClientStreamContext[CreatedItem, CreatedItem]) error { return nil }))
		},
		"raw handle": func() {
			Endpoint("GET", "/raw-feed").
				ReturnsStatus(201, "Created", StreamOf[CreatedItem]()).
				HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) })
		},
		"document only": func() {
			Endpoint("GET", "/documented-feed").
				ReturnsStatus(201, "Created", StreamOf[CreatedItem]()).
				Document()
		},
	}

	for name, finalize := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				message, ok := recover().(string)
				if !ok || !strings.Contains(message, "ReturnsStatus() is not supported on stream endpoints") {
					t.Fatalf("panic = %q, want stream incompatibility", message)
				}
			}()
			finalize()
		})
	}
}

func TestEndpointBuilder_MayThrow(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "an-endpoint-carries-its-error-declarations")
	b := Endpoint("GET", "/items/{id}").
		MayThrow(perrors.CodeNotFound, perrors.CodeConflict)

	if len(b.errorCodes) != 2 {
		t.Fatalf("errorCodes count = %d, want 2", len(b.errorCodes))
	}
	if b.errorCodes[0] != perrors.CodeNotFound || b.errorCodes[1] != perrors.CodeConflict {
		t.Errorf("errorCodes = %v", b.errorCodes)
	}
}

func TestEndpointBuilder_MayThrowWithPreservesExplicitRetryability(t *testing.T) {
	b := Endpoint("GET", "/items/{id}").
		MayThrowWith(perrors.CodeNotFound, ErrorOptions{Retryable: false}).
		MayThrowWith(perrors.CodeUnavailable, ErrorOptions{Retryable: true})

	if policy, ok := b.errorPolicies[perrors.CodeNotFound]; !ok || policy.Retryable {
		t.Fatalf("not-found retryability = %#v, %v", policy, ok)
	}
	if policy, ok := b.errorPolicies[perrors.CodeUnavailable]; !ok || !policy.Retryable {
		t.Fatalf("unavailable retryability = %#v, %v", policy, ok)
	}
}

type deployRejectionDetails struct {
	Project string `json:"project"`
}

// MayThrowDetails declares the code and its details type and nothing else: a
// details declaration must not invent a retry verdict, and the discovered route
// keeps its own copy of what was declared.
func TestEndpointBuilder_MayThrowDetailsDeclaresTheCodeAndItsDetailsWithoutARetryVerdict(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-error-details", "an-endpoint-declares-a-typed-details-body-per-error-code")
	const rejected perrors.Code = "deploy_rejected"
	b := Endpoint("POST", "/deploys").
		MayThrowDetails(rejected, Type[deployRejectionDetails]()).
		MayThrowWith(perrors.CodeConflict, ErrorOptions{Retryable: false})

	if len(b.errorCodes) != 2 || b.errorCodes[0] != rejected {
		t.Fatalf("errorCodes = %v, want the details code declared first", b.errorCodes)
	}
	if got := b.errorDetails[rejected]; got != Type[deployRejectionDetails]() {
		t.Fatalf("details type = %v", got)
	}
	if _, classified := b.errorPolicies[rejected]; classified {
		t.Fatal("MayThrowDetails declared a retry classification")
	}

	route := toDiscoveredRoute(b.Document(), "/deploys")
	if got := route.Responses.ErrorDetails[rejected]; got != Type[deployRejectionDetails]() {
		t.Fatalf("discovered details = %v", got)
	}
	if _, declared := route.Responses.ErrorDetails[perrors.CodeConflict]; declared {
		t.Fatal("a code declared without details gained a details type")
	}
	if _, classified := route.Responses.ErrorRetryability[rejected]; classified {
		t.Fatal("discovery gave the details code a retry classification")
	}
	b.errorDetails[rejected] = Type[CreatedItem]()
	if got := route.Responses.ErrorDetails[rejected]; got != Type[deployRejectionDetails]() {
		t.Fatalf("the discovered route aliases the builder: %v", got)
	}

	defer func() {
		recovered := recover()
		if message, ok := recovered.(string); !ok || !strings.Contains(message, "MayThrowDetails") {
			t.Fatalf("panic = %v, want a nil-details refusal", recovered)
		}
	}()
	Endpoint("GET", "/x").MayThrowDetails(perrors.CodeNotFound, nil)
}

// Every endpoint answers http.bad_request and http.internal_server, and the
// framework writes their details: a request validation failure carries its
// field errors there. A declared type for either would tell generated clients
// to decode every validation failure against it, so the builder refuses both
// and leaves the endpoint as it was.
func TestEndpointBuilder_MayThrowDetailsRefusesTheImplicitCodes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-error-details", "the-implicit-codes-refuse-a-declared-details-body")
	for _, code := range []perrors.Code{perrors.CodeBadRequest, perrors.CodeInternalServer} {
		t.Run(string(code), func(t *testing.T) {
			b := Endpoint("POST", "/deploys")
			defer func() {
				recovered := recover()
				message, ok := recovered.(string)
				if !ok || !strings.Contains(message, "MayThrowDetails") || !strings.Contains(message, string(code)) ||
					!strings.Contains(message, "framework-owned implicit error") {
					t.Fatalf("panic = %v, want an implicit-code refusal naming %q", recovered, code)
				}
				if len(b.errorCodes) != 0 || len(b.errorDetails) != 0 {
					t.Fatalf("the refused declaration was recorded: codes %v, details %v", b.errorCodes, b.errorDetails)
				}
			}()
			b.MayThrowDetails(code, Type[deployRejectionDetails]())
		})
	}
}

func TestEndpointBuilder_ThrowsAll(t *testing.T) {
	authErrors := []ResponseMeta{
		{Status: 401, Description: "Unauthorized"},
		{Status: 403, Description: "Forbidden"},
	}
	b := Endpoint("GET", "/secure").
		ThrowsAll(authErrors...).
		Throws(404, "Not found", nil)

	if len(b.throws) != 3 {
		t.Fatalf("throws count = %d, want 3", len(b.throws))
	}
	statuses := []int{b.throws[0].Status, b.throws[1].Status, b.throws[2].Status}
	want := []int{401, 403, 404}
	for i, s := range statuses {
		if s != want[i] {
			t.Errorf("throws[%d].Status = %d, want %d", i, s, want[i])
		}
	}
}

func TestEndpointBuilder_Inject(t *testing.T) {
	token := inject.Named[string]("myService")
	b := Endpoint("GET", "/test").Inject("svc", token)
	if len(b.injectTokens) != 1 {
		t.Errorf("injectTokens count = %d, want 1", len(b.injectTokens))
	}
}

type mockRule struct{}

func (mockRule) Middleware() phttp.Middleware {
	return func(_ *phttp.Context, next func() *phttp.Response) *phttp.Response { return next() }
}

func TestEndpoint_InjectOnStreamPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when .Inject() is used on a stream endpoint")
		}
	}()
	Endpoint("GET", "/feed").
		Inject("svc", inject.Named[string]("svc")).
		Returns(StreamOf[CreatedItem]()).
		Handle(ServerStream(func(_ *ServerStreamContext[CreatedItem]) error { return nil }))
}

func TestEndpointBuilder_Secure(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "an-endpoint-carries-its-security-rule")
	rule := mockRule{}
	b := Endpoint("GET", "/admin").Secure(rule)
	if b.security == nil {
		t.Error("security should not be nil")
	}
	if b.securityMeta == nil {
		t.Error("securityMeta should not be nil for OpenAPI propagation")
	}
}

func TestEndpointDefinition_MethodAndPath(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "an-endpoint-carries-its-method-and-path")
	def := Endpoint("DELETE", "/items/{id}").
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.NoContent() })

	if def.Method() != "DELETE" {
		t.Errorf("Method() = %q, want %q", def.Method(), "DELETE")
	}
	if def.Path() != "/items/{id}" {
		t.Errorf("Path() = %q, want %q", def.Path(), "/items/{id}")
	}
}

// --- BuildHandler tests ---

func TestEndpoint_HandleRaw_InvalidType(t *testing.T) {
	b := &EndpointBuilder{handler: "not a handler", injectTokens: make(map[string]inject.Token)}
	def := EndpointDefinition{builder: b, raw: true}
	handler := def.BuildHandler()

	req := httptest.NewRequest("GET", "/", nil)
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 500 {
		t.Errorf("expected 500 for invalid raw handler type, got %d", resp.Status)
	}
}

func TestEndpoint_HandleTyped_InvalidType(t *testing.T) {
	b := &EndpointBuilder{handler: "not a handler", injectTokens: make(map[string]inject.Token)}
	def := EndpointDefinition{builder: b, raw: false}
	handler := def.BuildHandler()

	req := httptest.NewRequest("GET", "/", nil)
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 500 {
		t.Errorf("expected 500 for invalid typed handler type, got %d", resp.Status)
	}
}

func TestEndpoint_HandleRaw_WithMiddleware(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "single-declaration", "an-endpoint-carries-its-middleware")
	called := false
	ep := Endpoint("GET", "/test").
		Use(func(_ *phttp.Context, next func() *phttp.Response) *phttp.Response {
			called = true
			return next()
		}).
		HandleRaw(func(_ *phttp.Context) *phttp.Response {
			return phttp.JSON("ok")
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("GET", "/test", nil)
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))

	if !called {
		t.Error("middleware should have been called")
	}
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

// TestEndpoint_CacheAppliesCacheControlMiddleware asserts that .Cache() applies
// enforcing middleware (parity with .Cors()/.RateLimit()) rather than only
// recording documentation metadata — the Cache-Control header must reach the
// response.
func TestEndpoint_CacheAppliesCacheControlMiddleware(t *testing.T) {
	ep := Endpoint("GET", "/cached").
		Cache(phttp.CacheOptions{MaxAge: 120}).
		HandleRaw(func(_ *phttp.Context) *phttp.Response {
			return phttp.JSON("ok")
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("GET", "/cached", nil)
	w := httptest.NewRecorder()
	resp := handler(phttp.NewContext(w, req))

	if resp.Status != 200 {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=120" {
		t.Errorf("Cache-Control = %q, want %q (.Cache() must enforce, not no-op)", got, "public, max-age=120")
	}
}

func TestEndpoint_HandleRaw_WithoutMiddleware(t *testing.T) {
	ep := Endpoint("GET", "/test").
		HandleRaw(func(_ *phttp.Context) *phttp.Response {
			return phttp.JSON("ok")
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("GET", "/test", nil)
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

// --- Validation tests ---

type CreateUserInput struct {
	Name  string `json:"name" validate:"required,minlen=2"`
	Email string `json:"email" validate:"required,email"`
}

func TestEndpoint_BodyValidation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "validation-pipeline", "the-request-body-is-validated")
	ep := Endpoint("POST", "/users").
		Body(Type[CreateUserInput]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSONStatus(201, ctx.ValidatedBody)
		})

	handler := ep.BuildHandler()

	req := httptest.NewRequest("POST", "/users", strings.NewReader(`{"name":"Alice","email":"alice@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	if got := handler(phttp.NewContext(httptest.NewRecorder(), req)).Status; got != 201 {
		t.Errorf("valid body → got %d, want 201", got)
	}

	req = httptest.NewRequest("POST", "/users", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if got := handler(phttp.NewContext(httptest.NewRecorder(), req)).Status; got != 400 {
		t.Errorf("invalid body → got %d, want 400", got)
	}

	// The stage's product is what the handler reads, so assert that too: a field
	// the schema does not declare must not survive into ValidatedBody. Handing
	// the handler the raw decoded map instead would keep both status assertions
	// above green while opening a mass-assignment path.
	req = httptest.NewRequest("POST", "/users",
		strings.NewReader(`{"name":"Alice","email":"alice@example.com","isAdmin":true}`))
	req.Header.Set("Content-Type", "application/json")
	body, err := handler(phttp.NewContext(httptest.NewRecorder(), req)).BodyBytes()
	if err != nil {
		t.Fatalf("reading echoed body: %v", err)
	}
	if strings.Contains(string(body), "isAdmin") {
		t.Errorf("ValidatedBody carried an undeclared field: %s", body)
	}
}

func TestEndpoint_FirstPartyBodyPreservesExactJSONValuesAndRootShapes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "json-body-fidelity", "first-party-json-bodies-preserve-presence-null-root-shapes-and-exact-integers")
	spectest.Proves(t, "go/api-contracts", "json-body-fidelity", "invalid-first-party-json-bodies-are-rejected-before-dispatch")
	t.Run("untagged fields use exact encoding/json names", func(t *testing.T) {
		type input struct {
			Count int `validate:"required"`
		}
		var received input
		definition := Endpoint("POST", "/untagged").Body(Type[input]()).Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			var err error
			received, err = phttp.BodyAs[input](ctx)
			if err != nil {
				return phttp.InternalError("decode failed")
			}
			return phttp.JSON(received)
		})
		definition.builder.firstParty = true
		handler := definition.BuildHandler()
		request := httptest.NewRequest("POST", "/untagged", strings.NewReader(`{"Count":0}`))
		request.Header.Set("Content-Type", "application/json")
		response := handler(phttp.NewContext(httptest.NewRecorder(), request))
		body, err := response.BodyBytes()
		if err != nil {
			t.Fatal(err)
		}
		if response.Status != 200 || string(body) != `{"Count":0}` {
			t.Fatalf("untagged provider round trip status=%d body=%s", response.Status, body)
		}
	})

	t.Run("object presence, null, and wide integers", func(t *testing.T) {
		type input struct {
			Enabled         bool    `json:"enabled" validate:"required"`
			Count           int64   `json:"count" validate:"required"`
			Label           string  `json:"label" validate:"required"`
			Wide            uint64  `json:"wide" validate:"required,min=9007199254740993,max=9007199254740993"`
			Nullable        *string `json:"nullable" validate:"required"`
			Defaulted       int64   `json:"defaulted" default:"9007199254740993"`
			RequiredDefault int64   `json:"requiredDefault" default:"7" validate:"required"`
		}
		var received input
		definition := Endpoint("POST", "/values").Body(Type[input]()).Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			var err error
			received, err = phttp.BodyAs[input](ctx)
			if err != nil {
				return phttp.InternalError("decode failed")
			}
			return phttp.NoContent()
		})
		definition.builder.firstParty = true
		handler := definition.BuildHandler()

		request := httptest.NewRequest("POST", "/values", strings.NewReader(`{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null,"requiredDefault":0}`))
		request.Header.Set("Content-Type", "application/json")
		if response := handler(phttp.NewContext(httptest.NewRecorder(), request)); response.Status != 204 {
			t.Fatalf("valid exact body status = %d, want 204", response.Status)
		}
		if received.Enabled || received.Count != 0 || received.Label != "" || received.Wide != 9007199254740993 || received.Nullable != nil ||
			received.Defaulted != 9007199254740993 || received.RequiredDefault != 0 {
			t.Fatalf("decoded body = %#v", received)
		}

		for name, body := range map[string]string{
			"missing required zero":    `{"enabled":false,"label":"","wide":9007199254740993,"nullable":null,"requiredDefault":0}`,
			"missing required default": `{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null}`,
			"wide constraint":          `{"enabled":false,"count":0,"label":"","wide":9007199254740994,"nullable":null,"requiredDefault":0}`,
			"non-nullable null":        `{"enabled":false,"count":null,"label":"","wide":9007199254740993,"nullable":null,"requiredDefault":0}`,
		} {
			t.Run(name, func(t *testing.T) {
				request := httptest.NewRequest("POST", "/values", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				if response := handler(phttp.NewContext(httptest.NewRecorder(), request)); response.Status != 400 {
					t.Fatalf("invalid body status = %d, want 400", response.Status)
				}
			})
		}
	})

	t.Run("array root", func(t *testing.T) {
		var received []int64
		definition := Endpoint("POST", "/values").Body(Type[[]int64]()).Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			var err error
			received, err = phttp.BodyAs[[]int64](ctx)
			if err != nil {
				return phttp.InternalError("decode failed")
			}
			return phttp.NoContent()
		})
		definition.builder.firstParty = true
		request := httptest.NewRequest("POST", "/values", strings.NewReader(`[0,9007199254740993,9223372036854775807]`))
		request.Header.Set("Content-Type", "application/json")
		if response := definition.BuildHandler()(phttp.NewContext(httptest.NewRecorder(), request)); response.Status != 204 {
			t.Fatalf("array body status = %d, want 204", response.Status)
		}
		if len(received) != 3 || received[1] != 9007199254740993 || received[2] != 9223372036854775807 {
			t.Fatalf("array body = %#v", received)
		}
	})

	t.Run("nested constraints and defaults", func(t *testing.T) {
		type child struct {
			Count int64  `json:"count" validate:"required,min=9007199254740993"`
			Label string `json:"label" default:"ready"`
		}
		type input struct {
			Nested child            `json:"nested" validate:"required"`
			Rows   []child          `json:"rows" validate:"required"`
			ByName map[string]child `json:"byName" validate:"required"`
		}
		var received input
		definition := Endpoint("POST", "/nested").Body(Type[input]()).Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			var err error
			received, err = phttp.BodyAs[input](ctx)
			if err != nil {
				return phttp.InternalError("decode failed")
			}
			return phttp.NoContent()
		})
		definition.builder.firstParty = true
		handler := definition.BuildHandler()

		valid := `{"nested":{"count":9007199254740993},"rows":[{"count":9007199254740993}],"byName":{"primary":{"count":9007199254740993}}}`
		request := httptest.NewRequest("POST", "/nested", strings.NewReader(valid))
		request.Header.Set("Content-Type", "application/json")
		if response := handler(phttp.NewContext(httptest.NewRecorder(), request)); response.Status != 204 {
			t.Fatalf("valid nested body status = %d, want 204", response.Status)
		}
		if received.Nested.Label != "ready" || received.Rows[0].Label != "ready" || received.ByName["primary"].Label != "ready" {
			t.Fatalf("handler did not receive recursive defaults: %#v", received)
		}

		for name, body := range map[string]string{
			"nested required absent": `{"nested":{},"rows":[],"byName":{}}`,
			"nested scalar null":     `{"nested":{"count":null},"rows":[],"byName":{}}`,
			"slice item invalid":     `{"nested":{"count":9007199254740993},"rows":[{}],"byName":{}}`,
			"map value invalid":      `{"nested":{"count":9007199254740993},"rows":[],"byName":{"primary":{}}}`,
		} {
			t.Run(name, func(t *testing.T) {
				request := httptest.NewRequest("POST", "/nested", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				if response := handler(phttp.NewContext(httptest.NewRecorder(), request)); response.Status != 400 {
					t.Fatalf("invalid nested body status = %d, want 400", response.Status)
				}
			})
		}
	})

	t.Run("primitive and nullable roots", func(t *testing.T) {
		var received uint64
		definition := Endpoint("POST", "/value").Body(Type[uint64]()).Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			var err error
			received, err = phttp.BodyAs[uint64](ctx)
			if err != nil {
				return phttp.InternalError("decode failed")
			}
			return phttp.NoContent()
		})
		definition.builder.firstParty = true
		request := httptest.NewRequest("POST", "/value", strings.NewReader(`18446744073709551615`))
		request.Header.Set("Content-Type", "application/json")
		if response := definition.BuildHandler()(phttp.NewContext(httptest.NewRecorder(), request)); response.Status != 204 || received != ^uint64(0) {
			t.Fatalf("primitive body status=%d value=%d", response.Status, received)
		}

		var nullable *string
		nullDefinition := Endpoint("POST", "/nullable").Body(Type[*string]()).Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			var err error
			nullable, err = phttp.BodyAs[*string](ctx)
			if err != nil {
				return phttp.InternalError("decode failed")
			}
			return phttp.NoContent()
		})
		nullDefinition.builder.firstParty = true
		nullRequest := httptest.NewRequest("POST", "/nullable", strings.NewReader(`null`))
		nullRequest.Header.Set("Content-Type", "application/json")
		if response := nullDefinition.BuildHandler()(phttp.NewContext(httptest.NewRecorder(), nullRequest)); response.Status != 204 || nullable != nil {
			t.Fatalf("nullable body status=%d value=%v", response.Status, nullable)
		}
		absentRequest := httptest.NewRequest("POST", "/nullable", nil)
		absentRequest.Header.Set("Content-Type", "application/json")
		if response := nullDefinition.BuildHandler()(phttp.NewContext(httptest.NewRecorder(), absentRequest)); response.Status != 400 {
			t.Fatalf("absent body status=%d, want 400", response.Status)
		}
	})
}

// TestEndpoint_BodyValidatedForEveryWritingMethod pins the positive half of the
// body-methods contract. The skipped-for-GET/DELETE checks below prove the
// exclusion; nothing proved the inclusion, so dropping a method from the gate
// would silently accept a schema-violating body on that verb.
func TestEndpoint_BodyValidatedForEveryWritingMethod(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "body-methods", "body-validation-runs-for-post-put-and-patch")

	for _, method := range []string{"POST", "PUT", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			ep := Endpoint(method, "/users").
				Body(Type[CreateUserInput]()).
				Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
					return phttp.JSONStatus(201, ctx.ValidatedBody)
				})
			handler := ep.BuildHandler()

			req := httptest.NewRequest(method, "/users", strings.NewReader(`{"name":"A","email":"not-an-email"}`))
			req.Header.Set("Content-Type", "application/json")
			if got := handler(phttp.NewContext(httptest.NewRecorder(), req)).Status; got != 400 {
				t.Errorf("%s with a schema-violating body → %d, want 400", method, got)
			}

			req = httptest.NewRequest(method, "/users", strings.NewReader(`{"name":"Alice","email":"alice@example.com"}`))
			req.Header.Set("Content-Type", "application/json")
			if got := handler(phttp.NewContext(httptest.NewRecorder(), req)).Status; got != 201 {
				t.Errorf("%s with a valid body → %d, want 201", method, got)
			}
		})
	}
}

func TestEndpoint_InvalidJSONBody(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "validation-pipeline", "an-undecodable-body-answers-400")
	type Input struct {
		Name string `json:"name" validate:"required"`
	}
	ep := Endpoint("POST", "/items").
		Body(Type[Input]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSONStatus(201, ctx.ValidatedBody)
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("POST", "/items", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 400 {
		t.Errorf("expected 400 for invalid JSON, got %d", resp.Status)
	}
}

func TestEndpoint_BodySkippedForGET(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "body-methods", "a-declared-body-schema-does-not-reject-a-get")
	type Input struct {
		Name string `json:"name" validate:"required"`
	}
	ep := Endpoint("GET", "/items").
		Body(Type[Input]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			if ctx.ValidatedBody != nil {
				return phttp.InternalError("body should be nil for GET")
			}
			return phttp.JSON("ok")
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("GET", "/items", nil)
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200 (body skipped for GET), got %d", resp.Status)
	}
}

type IDParams struct {
	ID string `json:"id" validate:"required"`
}

func TestEndpoint_ParamsValidation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "validation-pipeline", "path-parameters-are-validated")
	ep := Endpoint("GET", "/users/{id}").
		Params(Type[IDParams]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(ctx.ValidatedParams)
		})

	handler := ep.BuildHandler()

	req := httptest.NewRequest("GET", "/users/42", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	ctx.Params = map[string]string{"id": "42"}
	if got := handler(ctx).Status; got != 200 {
		t.Errorf("valid params → got %d, want 200", got)
	}

	req = httptest.NewRequest("GET", "/users/", nil)
	ctx = phttp.NewContext(httptest.NewRecorder(), req)
	ctx.Params = map[string]string{}
	if got := handler(ctx).Status; got != 400 {
		t.Errorf("missing params → got %d, want 400", got)
	}
}

type SearchQuery struct {
	Q    string `json:"q" validate:"required"`
	Page int    `json:"page"`
}

func TestEndpoint_QueryValidation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "validation-pipeline", "query-parameters-are-validated")
	ep := Endpoint("GET", "/search").
		Query(Type[SearchQuery]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(ctx.ValidatedQuery)
		})

	handler := ep.BuildHandler()

	req := httptest.NewRequest("GET", "/search?q=hello&page=2", nil)
	if got := handler(phttp.NewContext(httptest.NewRecorder(), req)).Status; got != 200 {
		t.Errorf("valid query → got %d, want 200", got)
	}

	req = httptest.NewRequest("GET", "/search", nil)
	if got := handler(phttp.NewContext(httptest.NewRecorder(), req)).Status; got != 400 {
		t.Errorf("missing query → got %d, want 400", got)
	}
}

func TestEndpoint_QueryMultipleValues(t *testing.T) {
	type TagsQuery struct {
		Tags []string `json:"tags"`
	}
	ep := Endpoint("GET", "/items").
		Query(Type[TagsQuery]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(ctx.ValidatedQuery)
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("GET", "/items?tags=a&tags=b", nil)
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

// --- Catch-all path parameter tests ---

type ModuleBlobsParams struct {
	Module string `json:"module" validate:"required"`
}

type ModuleVersionParams struct {
	Module      string `json:"module" validate:"required"`
	VersionFile string `json:"versionfile" validate:"required"`
}

func TestEndpoint_CatchAllParamAtEnd(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "catch-all-paths", "a-catch-all-parameter-at-the-end-matches-joined-segments")
	ep := Endpoint("GET", "/files/{path...}").
		Params(Type[struct {
			Path string `json:"path" validate:"required"`
		}]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(ctx.ValidatedParams)
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("GET", "/files/docs/readme.txt", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	ctx.Params = map[string]string{"path": "docs/readme.txt"}
	resp := handler(ctx)
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

func TestEndpoint_CatchAllParamInMiddle(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "catch-all-paths", "a-catch-all-parameter-in-the-middle-matches-joined-segments")
	ep := Endpoint("POST", "/{module...}/-/blobs/upload").
		Params(Type[ModuleBlobsParams]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(ctx.ValidatedParams)
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("POST", "/go.putnami.dev/protocol/diagnostic/-/blobs/upload", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	ctx.Params = map[string]string{"module": "go.putnami.dev/protocol/diagnostic"}
	resp := handler(ctx)
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

func TestEndpoint_CatchAllParamInMiddleWithTrailingParam(t *testing.T) {
	ep := Endpoint("GET", "/{module...}/@v/{versionfile}").
		Params(Type[ModuleVersionParams]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(ctx.ValidatedParams)
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("GET", "/go.putnami.dev/protocol/diagnostic/@v/v0.0.0.info", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	ctx.Params = map[string]string{"module": "go.putnami.dev/protocol/diagnostic", "versionfile": "v0.0.0.info"}
	resp := handler(ctx)
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

func TestEndpoint_TypedHandlerWithMiddleware(t *testing.T) {
	mwCalled := false
	ep := Endpoint("POST", "/items").
		Use(func(_ *phttp.Context, next func() *phttp.Response) *phttp.Response {
			mwCalled = true
			return next()
		}).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response {
			return phttp.JSONStatus(201, map[string]string{"ok": "true"})
		})

	handler := ep.BuildHandler()
	req := httptest.NewRequest("POST", "/items", nil)
	resp := handler(phttp.NewContext(httptest.NewRecorder(), req))

	if !mwCalled {
		t.Error("middleware should have been called")
	}
	if resp.Status != 201 {
		t.Errorf("expected 201, got %d", resp.Status)
	}
}

// renderBody serializes a response body the way the transport would, so a test
// can assert on what a caller actually receives rather than on an internal.
func renderBody(t *testing.T, resp *phttp.Response) string {
	t.Helper()
	data, err := resp.BodyBytes()
	if err != nil {
		t.Fatalf("rendering the response body: %v", err)
	}
	return string(data)
}

// The pipeline validates path parameters, then query parameters, then the body.
// The order is observable: a request that violates all three answers with the
// path-parameter failure alone, so a caller fixes the outermost fault first
// instead of chasing errors the later stages never reached.
func TestEndpoint_ValidationRunsParamsThenQueryThenBody(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "validation-pipeline", "the-stages-run-path-then-query-then-body")
	type params struct {
		ID string `json:"id" validate:"required,uuid"`
	}
	type query struct {
		Page string `json:"page" validate:"required"`
	}
	type body struct {
		Name string `json:"name" validate:"required"`
	}

	ep := Endpoint("POST", "/users/{id}").
		Params(Type[params]()).
		Query(Type[query]()).
		Body(Type[body]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON("ok") })

	handler := ep.BuildHandler()
	req := httptest.NewRequest("POST", "/users/not-a-uuid", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	ctx.Params = map[string]string{"id": "not-a-uuid"}

	resp := handler(ctx)
	if resp.Status != 400 {
		t.Fatalf("expected 400 for a failing path parameter, got %d", resp.Status)
	}
	rendered := renderBody(t, resp)
	if !strings.Contains(rendered, "params.id") {
		t.Errorf("expected the path-parameter failure to name its field, got %s", rendered)
	}
	if strings.Contains(rendered, "query.page") || strings.Contains(rendered, "body.name") {
		t.Errorf("query and body must not be validated once path parameters failed, got %s", rendered)
	}
}

// A declared body schema gates on the method, so a DELETE that legitimately
// carries no body reaches its handler instead of being rejected as malformed.
func TestEndpoint_BodySkippedForDELETE(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "body-methods", "a-declared-body-schema-does-not-reject-a-delete")
	type Input struct {
		Name string `json:"name" validate:"required"`
	}
	ep := Endpoint("DELETE", "/items").
		Body(Type[Input]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			if ctx.ValidatedBody != nil {
				return phttp.InternalError("body should be nil for DELETE")
			}
			return phttp.JSON("ok")
		})

	handler := ep.BuildHandler()
	resp := handler(phttp.NewContext(httptest.NewRecorder(), httptest.NewRequest("DELETE", "/items", nil)))
	if resp.Status != 200 {
		t.Errorf("expected 200 (body skipped for DELETE), got %d", resp.Status)
	}
}

// A dependency that cannot be resolved is the service's fault, not the caller's:
// it answers a generic 500 and the underlying resolution error stays out of the
// response, so an unregistered token cannot disclose the container's wiring.
func TestEndpoint_DependencyResolutionFailureAnswersGeneric500(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "validation-pipeline", "a-dependency-resolution-failure-answers-a-generic-500")
	token := inject.Named[string]("never-registered-service")
	ep := Endpoint("GET", "/needs-di").
		Inject("svc", token).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON("handler must not run")
		})

	handler := ep.BuildHandler()
	resp := handler(phttp.NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/needs-di", nil)))

	if resp.Status != 500 {
		t.Fatalf("expected 500 when a dependency cannot be resolved, got %d", resp.Status)
	}
	rendered := renderBody(t, resp)
	if strings.Contains(rendered, "never-registered-service") {
		t.Errorf("the resolution failure must not leak the token, got %s", rendered)
	}
	if strings.Contains(rendered, "handler must not run") {
		t.Errorf("the handler must not run once a dependency failed to resolve, got %s", rendered)
	}
}

// A DELETE that does send its declared body has it decoded and validated, like
// any other method that carries one. Publishing a DELETE request schema and then
// leaving the payload unchecked would hand the handler unvalidated input.
func TestEndpoint_DeclaredDeleteBodyIsValidatedWhenTheRequestCarriesOne(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "body-methods", "a-declared-delete-body-is-validated-when-the-request-carries-one")
	type Input struct {
		Reason string `json:"reason" validate:"required"`
	}
	var seen any
	ep := Endpoint("DELETE", "/items").
		Body(Type[Input]()).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			seen = ctx.ValidatedBody
			return phttp.JSON("ok")
		})
	handler := ep.BuildHandler()

	valid := httptest.NewRequest("DELETE", "/items", strings.NewReader(`{"reason":"retired"}`))
	valid.Header.Set("Content-Type", "application/json")
	if resp := handler(phttp.NewContext(httptest.NewRecorder(), valid)); resp.Status != 200 {
		t.Fatalf("valid DELETE body status = %d, want 200", resp.Status)
	}
	if seen == nil {
		t.Fatal("handler received no validated DELETE body")
	}

	invalid := httptest.NewRequest("DELETE", "/items", strings.NewReader(`{}`))
	invalid.Header.Set("Content-Type", "application/json")
	if resp := handler(phttp.NewContext(httptest.NewRecorder(), invalid)); resp.Status != 400 {
		t.Fatalf("DELETE body violating its schema status = %d, want 400", resp.Status)
	}
}
