package openapi

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/api"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

const opaqueJSONRequirement = "opaque-json"

// opaqueAudit carries every Go form of a value the provider does not interpret.
type opaqueAudit struct {
	Attributes map[string]any             `json:"attributes" validate:"required"`
	Raw        json.RawMessage            `json:"raw" validate:"required"`
	Value      any                        `json:"value" validate:"required"`
	Trail      []any                      `json:"trail,omitempty"`
	Optional   *json.RawMessage           `json:"optional,omitempty"`
	Frames     map[string]json.RawMessage `json:"frames,omitempty"`
	Labels     *map[string]any            `json:"labels,omitempty"`
}

func strictOpaqueProvider(t *testing.T) (*api.Plugin, *Plugin) {
	t.Helper()
	apiPlugin := api.New(&fakeServer{}, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "audit", Audience: "https://audit.internal"},
	}))
	apiPlugin.Register(api.Endpoint("POST", "/audit").
		Body(api.Type[opaqueAudit]()).
		Returns(api.Type[opaqueAudit]()).
		MayThrow(perrors.CodeNotFound).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	openapiPlugin := NewPlugin(PluginOptions{Title: "Audit", Version: "1.0.0"}).From(apiPlugin)
	// Configure re-reads the serialized document through the strict reader, so
	// success here is the strict validation the issue asks for.
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("a strict provider with opaque JSON fields must publish its contract: %v", err)
	}
	return apiPlugin, openapiPlugin
}

// TestOpenAPI_FirstPartyOpaqueJSONFieldsPassStrictValidation proves the Go
// forms of an uninterpreted value project to the two closed spellings, pass the
// strict reader, and emit a Go client that holds them as raw JSON.
func TestOpenAPI_FirstPartyOpaqueJSONFieldsPassStrictValidation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", opaqueJSONRequirement,
		"a-go-provider-declares-raw-json-maps-of-any-and-the-empty-interface-in-the-closed-opaque-spelling")
	_, openapiPlugin := strictOpaqueProvider(t)
	spec := openapiPlugin.Spec()
	var audit *SchemaObject
	for name, component := range spec.Components.Schemas {
		if _, ok := component.Properties["raw"]; ok {
			schema := spec.Components.Schemas[name]
			audit = &schema
		}
	}
	if audit == nil {
		t.Fatalf("no component carries the audit shape: %v", keys(spec.Components.Schemas))
	}
	opaque := func(name string) {
		t.Helper()
		got := audit.Properties[name]
		if got.OpaqueJSON != clientcontract.OpaqueJSONAny || got.Type != "" || got.Nullable != nil {
			t.Errorf("%s = %+v, want exactly {x-putnami-json: any}", name, got)
		}
	}
	freeForm := func(name string, nullable bool) {
		t.Helper()
		got := audit.Properties[name]
		if got.Type != "object" || got.AdditionalProperties == nil || got.AdditionalProperties.Allowed == nil ||
			!*got.AdditionalProperties.Allowed || got.AdditionalProperties.Schema != nil || got.OpaqueJSON != "" {
			t.Errorf("%s = %+v, want {type: object, additionalProperties: true}", name, got)
		}
		if (got.Nullable != nil && *got.Nullable) != nullable {
			t.Errorf("%s nullable = %v, want %v", name, got.Nullable, nullable)
		}
	}
	opaque("raw")
	opaque("value")
	// A pointer to an opaque value declares nothing more: null is already one
	// of its values.
	opaque("optional")
	freeForm("attributes", false)
	freeForm("frames", false)
	freeForm("labels", true)
	if trail := audit.Properties["trail"]; trail.Type != "array" || trail.Items == nil || trail.Items.OpaqueJSON != clientcontract.OpaqueJSONAny {
		t.Errorf("trail = %+v, want an array of opaque values", trail)
	}

	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	ir, err := api.ReadOpenAPISpec(body)
	if err != nil {
		t.Fatalf("strict reader refused the provider document: %v", err)
	}
	source, err := api.GenerateClientFromIR(ir, api.ClientGenOptions{PackageName: "auditclient", ClientName: "AuditClient"})
	if err != nil {
		t.Fatalf("generate Go client: %v", err)
	}
	for _, want := range []string{
		"Attributes map[string]json.RawMessage `json:\"attributes\"`",
		"Raw json.RawMessage `json:\"raw\"`",
		"Value json.RawMessage `json:\"value\"`",
		"Optional json.RawMessage `json:\"optional,omitempty\"`",
		"Trail *[]json.RawMessage `json:\"trail,omitempty\"`",
		"Frames *map[string]json.RawMessage `json:\"frames,omitempty\"`",
	} {
		if !strings.Contains(strings.Join(strings.Fields(source), " "), want) {
			t.Errorf("generated client is missing %q:\n%s", want, source)
		}
	}
}

// TestOpenAPI_ErrorEnvelopeNeverPublishesAnEmptySchema walks every error
// response the framework adds, for a third-party and a first-party provider,
// and refuses any schema node that declares nothing. The envelope's details
// member is the one that used to be `{}`.
func TestOpenAPI_ErrorEnvelopeNeverPublishesAnEmptySchema(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", opaqueJSONRequirement, "the-framework-error-envelope-never-publishes-an-empty-schema")
	for _, firstParty := range []bool{false, true} {
		t.Run("firstParty="+strconv.FormatBool(firstParty), func(t *testing.T) {
			var options []api.Option
			if firstParty {
				options = append(options, api.WithClientService(api.ClientServiceOptions{
					Service: clientcontract.Service{ID: "errors", Audience: "https://errors.internal"},
				}))
			}
			apiPlugin := api.New(&fakeServer{}, options...)
			apiPlugin.Register(api.Endpoint("GET", "/things/{id}").
				Params(api.Type[apiUserParams]()).
				Returns(api.Type[apiUser]()).
				MayThrow(perrors.CodeNotFound).
				MayThrow(perrors.CodeUnavailable).
				Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
			apiPlugin.Register(api.Endpoint("POST", "/blobs").
				Body(api.Binary("application/octet-stream", 1024)).
				Returns(api.Binary("application/octet-stream", 1024)).
				Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
			if err := apiPlugin.Configure(context.Background(), nil); err != nil {
				t.Fatalf("api Configure: %v", err)
			}
			openapiPlugin := NewPlugin(PluginOptions{Title: "Errors", Version: "1.0.0"}).From(apiPlugin)
			if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
				t.Fatalf("openapi Configure: %v", err)
			}
			raw, err := json.Marshal(openapiPlugin.Spec())
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Paths map[string]map[string]struct {
					Responses map[string]struct {
						Content map[string]struct {
							Schema any `json:"schema"`
						} `json:"content"`
					} `json:"responses"`
				} `json:"paths"`
			}
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			errorResponses, details := 0, 0
			for path, item := range document.Paths {
				for method, operation := range item {
					for status, response := range operation.Responses {
						code, err := strconv.Atoi(status)
						if err != nil || code < 400 {
							continue
						}
						for mediaType, content := range response.Content {
							errorResponses++
							scope := strings.ToUpper(method) + " " + path + " " + status + " " + mediaType
							walkDeclaredSchema(t, scope, content.Schema, func(name string) {
								if name == "details" {
									details++
								}
							})
						}
					}
				}
			}
			if errorResponses == 0 {
				t.Fatal("no error response was published, so this guard would pass vacuously")
			}
			if !firstParty && details == 0 {
				t.Fatal("the third-party envelope no longer declares details, so this guard would pass vacuously")
			}
		})
	}
}

// walkDeclaredSchema fails on any schema node without type, $ref, oneOf or
// x-putnami-json, and reports every property name it visits.
func walkDeclaredSchema(t *testing.T, scope string, node any, property func(string)) {
	t.Helper()
	schema, ok := node.(map[string]any)
	if !ok {
		t.Errorf("%s: schema is %T, want an object", scope, node)
		return
	}
	declared := false
	for _, keyword := range []string{"type", "$ref", "oneOf", clientcontract.OpaqueJSONKey} {
		if _, present := schema[keyword]; present {
			declared = true
		}
	}
	if !declared {
		t.Errorf("%s: schema %v declares nothing; an empty schema is not a declaration", scope, schema)
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			property(name)
			walkDeclaredSchema(t, scope+"."+name, properties[name], property)
		}
	}
	if items, ok := schema["items"]; ok {
		walkDeclaredSchema(t, scope+"[]", items, property)
	}
	if additional, ok := schema["additionalProperties"].(map[string]any); ok {
		walkDeclaredSchema(t, scope+".*", additional, property)
	}
	if variants, ok := schema["oneOf"].([]any); ok {
		for i, variant := range variants {
			walkDeclaredSchema(t, scope+".oneOf["+strconv.Itoa(i)+"]", variant, property)
		}
	}
}
