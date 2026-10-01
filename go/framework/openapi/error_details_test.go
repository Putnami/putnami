package openapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

const declaredErrorDetailsRequirement = "declared-error-details"

// deployRejectedCode is a service-owned stable code, not a framework one: the
// provider registers the status it answers with, and the published contract
// reads the same table.
const deployRejectedCode perrors.Code = "deploy_rejected"

type deployRequest struct {
	Projects []string `json:"projects" validate:"required"`
}

type deployAccepted struct {
	ID string `json:"id" validate:"required"`
}

type deployRejection struct {
	Index   int    `json:"index" validate:"required"`
	Project string `json:"project" validate:"required"`
	Error   string `json:"error" validate:"required"`
}

// deployRejected is the structured refusal a deploy answers: the per-project
// rejections, the upstream response body and the retry verdict.
type deployRejected struct {
	Rejections      []deployRejection `json:"rejections" validate:"required"`
	GCPResponseBody string            `json:"gcp_response_body" validate:"required"`
	Retryable       bool              `json:"retryable" validate:"required"`
}

type deployQuota struct {
	Limit int `json:"limit" validate:"required"`
}

// sampleDeployRejection is what the provider sends and what every consumer
// must read back, field for field.
var sampleDeployRejection = deployRejected{
	Rejections:      []deployRejection{{Index: 0, Project: "a", Error: "image not found"}},
	GCPResponseBody: `{"error":{"code":400}}`,
	Retryable:       false,
}

func registerDeployRejectedStatus() {
	perrors.RegisterHTTPStatus(deployRejectedCode, http.StatusBadRequest)
}

func deploysProvider(t *testing.T, server api.Server, endpoint api.EndpointDefinition) *Plugin {
	t.Helper()
	registerDeployRejectedStatus()
	apiPlugin := api.New(server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "deploys", Audience: "https://deploys.internal"},
	}))
	apiPlugin.Register(endpoint)
	openapiPlugin := NewPlugin(PluginOptions{Title: "Deploys", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	// Configure re-reads the serialized document through the strict reader.
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("a provider declaring error details must publish its contract: %v", err)
	}
	return openapiPlugin
}

// TestOpenAPI_MayThrowDetailsPublishesTheDetailsSchemaForThatCodeOnly pins the
// projection: the declared type becomes the error's x-putnami-client schema —
// the details body, never the envelope — on that code alone, beside a retry
// classification declared separately, at the status the provider answers with.
// The documented response names `details`, because the first-party envelope
// forbids an undeclared member, and the bytes do not depend on declaration
// order.
func TestOpenAPI_MayThrowDetailsPublishesTheDetailsSchemaForThatCodeOnly(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", declaredErrorDetailsRequirement,
		"the-contract-publishes-the-declared-details-schema-for-that-code-only")
	declare := func(forward bool) api.EndpointDefinition {
		endpoint := api.Endpoint("POST", "/deploys").
			Body(api.Type[deployRequest]()).
			Returns(api.Type[deployAccepted]())
		if forward {
			endpoint = endpoint.
				MayThrowDetails(deployRejectedCode, api.Type[deployRejected]()).
				MayThrowDetails(perrors.CodeInvalidArg, api.Type[deployQuota]()).
				MayThrowDetails(perrors.CodeValidation, api.Type[deployQuota]()).
				MayThrow(perrors.CodeNotFound).
				MayThrowWith(deployRejectedCode, api.ErrorOptions{Retryable: false})
		} else {
			endpoint = endpoint.
				MayThrow(perrors.CodeNotFound).
				MayThrowWith(deployRejectedCode, api.ErrorOptions{Retryable: false}).
				MayThrowDetails(perrors.CodeValidation, api.Type[deployQuota]()).
				MayThrowDetails(perrors.CodeInvalidArg, api.Type[deployQuota]()).
				MayThrowDetails(deployRejectedCode, api.Type[deployRejected]())
		}
		return endpoint.Document()
	}
	forward := deploysProvider(t, &fakeServer{}, declare(true))
	reverse := deploysProvider(t, &fakeServer{}, declare(false))
	forwardJSON, err := forward.OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	reverseJSON, err := reverse.OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(forwardJSON, reverseJSON) {
		t.Fatalf("the published document depends on declaration order\nforward:\n%s\nreverse:\n%s", forwardJSON, reverseJSON)
	}

	spec := forward.Spec()
	operation := spec.Paths["/deploys"]["post"]
	byCode := map[string]clientcontract.DeclaredError{}
	for _, declared := range operation.ClientContract.Errors {
		byCode[declared.Code] = declared
	}
	component := func(schema *clientcontract.Schema) SchemaObject {
		t.Helper()
		if schema == nil || !strings.HasPrefix(schema.Ref, "#/components/schemas/") {
			t.Fatalf("details schema = %#v, want a reference to the declared type", schema)
		}
		resolved, ok := spec.Components.Schemas[strings.TrimPrefix(schema.Ref, "#/components/schemas/")]
		if !ok {
			t.Fatalf("details schema %q names no component", schema.Ref)
		}
		return resolved
	}

	rejected, ok := byCode[string(deployRejectedCode)]
	if !ok || rejected.Status != http.StatusBadRequest {
		t.Fatalf("deploy_rejected = %#v, want status 400 from the registered status table", rejected)
	}
	if rejected.Retryable == nil || *rejected.Retryable {
		t.Fatalf("the MayThrowWith classification was lost beside the details: %#v", rejected.Retryable)
	}
	details := component(rejected.Schema)
	for _, property := range []string{"rejections", "gcp_response_body", "retryable"} {
		if _, ok := details.Properties[property]; !ok {
			t.Errorf("details schema lacks %q: %#v", property, details.Properties)
		}
	}
	// ADR 0006: the schema is the details body, never the envelope carrying it.
	for _, envelope := range []string{"code", "message"} {
		if _, ok := details.Properties[envelope]; ok {
			t.Errorf("details schema declares the envelope member %q", envelope)
		}
	}
	if quota := component(byCode[string(perrors.CodeInvalidArg)].Schema); quota.Properties["limit"].Type != "integer" {
		t.Fatalf("invalid_argument details = %#v", quota)
	}
	for _, bare := range []string{string(perrors.CodeNotFound), string(perrors.CodeBadRequest), string(perrors.CodeInternalServer)} {
		declared, ok := byCode[bare]
		if !ok {
			t.Fatalf("%s is missing from the contract: %#v", bare, operation.ClientContract.Errors)
		}
		if declared.Schema != nil {
			t.Errorf("%s declares no details but publishes a schema: %#v", bare, declared.Schema)
		}
	}

	envelope := operation.Responses["400"].Content["application/json"].Schema
	if envelope == nil || envelope.AdditionalProperties == nil || envelope.AdditionalProperties.Allowed == nil || *envelope.AdditionalProperties.Allowed {
		t.Fatalf("400 envelope is no longer closed: %#v", envelope)
	}
	// Three codes answer 400 with two distinct types: one variant per type, in
	// stable-code order — deploy_rejected, then invalid_argument, whose type
	// validation shares — whatever the type names. The variants are anyOf:
	// oneOf would refuse a body two overlapping variants both admit, and the
	// envelope's code already tells the errors apart. The TypeScript
	// projection orders and combines them the same way.
	documented, ok := envelope.Properties["details"]
	if !ok || len(documented.OneOf) != 0 {
		t.Fatalf("400 envelope documents details = %#v, want anyOf the declared types", documented)
	}
	variants := make([]string, 0, len(documented.AnyOf))
	for _, variant := range documented.AnyOf {
		variants = append(variants, variant.Ref)
	}
	if want := []string{schemaRef("deployRejected"), schemaRef("deployQuota")}; !slices.Equal(variants, want) {
		t.Fatalf("400 details variants = %q, want %q", variants, want)
	}
	if !bytes.Contains(forwardJSON, []byte(`"anyOf"`)) || bytes.Contains(forwardJSON, []byte(`"oneOf"`)) {
		t.Fatalf("the published document does not document details with anyOf:\n%s", forwardJSON)
	}
	if _, ok := operation.Responses["404"].Content["application/json"].Schema.Properties["details"]; ok {
		t.Fatal("404 declares no details but documents them")
	}

	if _, err := api.ReadOpenAPISpec(forwardJSON); err != nil {
		t.Fatalf("strict reader refused the published details: %v", err)
	}
}

// TestOpenAPI_AThrowsStatusSharedByCodesWithDeclaredDetailsKeepsEachCodesSchema
// composes MayThrowDetails with a Throws at a status several declared codes
// share. Throws only documents that status: it neither leaves a blank code in
// the contract nor becomes a details schema, so each code keeps exactly what it
// declared — its details type, or nothing.
func TestOpenAPI_AThrowsStatusSharedByCodesWithDeclaredDetailsKeepsEachCodesSchema(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", declaredErrorDetailsRequirement,
		"a-throws-status-shared-by-codes-with-declared-details-keeps-each-codes-schema")
	openapiPlugin := deploysProvider(t, &fakeServer{}, api.Endpoint("POST", "/deploys").
		Body(api.Type[deployRequest]()).
		Returns(api.Type[deployAccepted]()).
		MayThrowDetails(deployRejectedCode, api.Type[deployRejected]()).
		MayThrow(perrors.CodeValidation).
		Throws(http.StatusBadRequest, "Rejected", api.Type[apiThrownNotFound]()).
		Document())
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.ReadOpenAPISpec(raw); err != nil {
		t.Fatalf("strict reader refused the published contract: %v", err)
	}
	operation := openapiPlugin.Spec().Paths["/deploys"]["post"]
	atBadRequest := 0
	for _, declared := range operation.ClientContract.Errors {
		if declared.Code == "" {
			t.Fatalf("Throws(400) left an undiscriminated error: %#v", operation.ClientContract.Errors)
		}
		if declared.Status == http.StatusBadRequest {
			atBadRequest++
		}
		if declared.Code == string(deployRejectedCode) {
			if declared.Schema == nil || declared.Schema.Ref == "" {
				t.Fatalf("deploy_rejected lost its declared details: %#v", declared.Schema)
			}
			continue
		}
		if declared.Schema != nil {
			t.Errorf("%s declares no details but publishes a schema: %#v", declared.Code, declared.Schema)
		}
	}
	if atBadRequest != 3 {
		t.Fatalf("400 carries %d declared codes, want http.bad_request, validation and deploy_rejected", atBadRequest)
	}
	// Throws keeps documenting the whole 400 response body.
	thrown := operation.Responses["400"].Content["application/json"].Schema
	if thrown == nil || thrown.Ref == "" {
		t.Fatalf("400 response lost the declared Throws schema: %#v", thrown)
	}
}

// generatedDetailsE2E runs inside the throwaway module, in the generated
// package, against the real provider the parent test serves.
const generatedDetailsE2E = `package deploysclient

import (
	"context"
	"errors"
	"os"
	"testing"

	"go.putnami.dev/client"
)

func TestTheEmittedClientDecodesTheDeclaredDetails(t *testing.T) {
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: os.Getenv("DEPLOYS_PROVIDER_URL"), ClientID: "deploys.consumer", AllowInsecure: true,
	}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	deploys := NewDeploysClient(transport)
	var in CreateDeploysInput
	in.Body = DeployRequest{Projects: []string{"a"}}
	_, err = deploys.CreateDeploys(context.Background(), in)
	var rejected *CreateDeploysDeployRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("declared error = %T %v, want *CreateDeploysDeployRejectedError", err, err)
	}
	if rejected.Remote.StatusCode != 400 || rejected.Remote.Code() != "deploy_rejected" {
		t.Fatalf("typed error = %d/%q, want 400/deploy_rejected", rejected.Remote.StatusCode, rejected.Remote.Code())
	}
	payload := rejected.Payload
	if payload == nil {
		t.Fatal("the declared details did not reach the typed error")
	}
	if len(payload.Rejections) != 1 || payload.Rejections[0].Index != 0 || payload.Rejections[0].Project != "a" ||
		payload.Rejections[0].Error != "image not found" {
		t.Fatalf("rejections = %+v", payload.Rejections)
	}
	if payload.GcpResponseBody != ` + "`" + `{"error":{"code":400}}` + "`" + ` || payload.Retryable {
		t.Fatalf("details = %+v", payload)
	}
}
`

// TestOpenAPI_AGoProviderDeclaredDetailsArriveTypedAtTheEmittedGoClient is the
// whole chain on a real socket: the provider declares a custom code with
// MayThrowDetails and answers it with errors.Any("details", …); the published
// document is read by the strict reader, emitted as a Go client, compiled in a
// throwaway module against this repository's runtime, and that compiled client
// calls the provider. The typed error for the code carries the details, field
// for field.
func TestOpenAPI_AGoProviderDeclaredDetailsArriveTypedAtTheEmittedGoClient(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", declaredErrorDetailsRequirement,
		"the-emitted-go-client-decodes-declared-details-from-a-real-provider")
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	openapiPlugin := deploysProvider(t, httpServer, api.Endpoint("POST", "/deploys").
		Body(api.Type[deployRequest]()).
		Returns(api.Type[deployAccepted]()).
		MayThrowDetails(deployRejectedCode, api.Type[deployRejected]()).
		Handle(func(_ *phttp.EndpointContext) *phttp.Response {
			return phttp.ErrorResponse(perrors.User(deployRejectedCode, "deploy rejected",
				perrors.Any("details", sampleDeployRejection)))
		}))
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("strict reader refused the provider document: %v", err)
	}
	server := httptest.NewServer(httpServer.Handler())
	t.Cleanup(server.Close)

	// The runtime the emitted client wraps: the declared details reach
	// RemoteError.Payload as the provider wrote them.
	var operation client.Operation
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			if method.OperationID == "postDeploys" && method.Client != nil {
				operation = client.Operation{ID: method.OperationID, Contract: *method.Client}
			}
		}
	}
	if operation.ID == "" {
		t.Fatal("the provider document declares no postDeploys operation")
	}
	bound, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: server.URL, ClientID: "deploys.consumer", AllowInsecure: true,
	}, client.ServiceDescriptor{Contract: *ir.Contract, Schemas: ir.Schemas})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(deployRequest{Projects: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call[deployAccepted](t.Context(), bound, &client.Request{
		Method: http.MethodPost, Path: "/deploys", Body: body,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, operation)
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.Code() != string(deployRejectedCode) || remote.StatusCode != http.StatusBadRequest {
		t.Fatalf("declared provider error = %T %v, want a typed deploy_rejected at 400", err, err)
	}
	var received deployRejected
	if err := json.Unmarshal(remote.Payload, &received); err != nil {
		t.Fatalf("payload %s: %v", remote.Payload, err)
	}
	if want, _ := json.Marshal(sampleDeployRejection); !bytes.Equal(mustMarshal(t, received), want) {
		t.Fatalf("payload = %s, want %s", remote.Payload, want)
	}

	source, err := api.GenerateClientFromIR(ir, api.ClientGenOptions{PackageName: "deploysclient", ClientName: "DeploysClient"})
	if err != nil {
		t.Fatalf("generate the Go client: %v", err)
	}
	moduleDir := t.TempDir()
	writeEmittedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "details_e2e_test.go"), []byte(generatedDetailsE2E), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	// GOPROXY=off keeps the run on this checkout's sources and the local cache.
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off",
		"DEPLOYS_PROVIDER_URL="+server.URL)
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client did not decode the declared details: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

var emittedModuleLine = regexp.MustCompile(`(?m)^module\s+\S+`)
var emittedRelativeReplace = regexp.MustCompile(`(?m)^replace\s+(\S+)\s+=>\s+(\.\S+)`)

// writeEmittedClientModule materializes a module whose dependency closure is
// the client runtime's own: go/framework/client's go.mod with every relative
// replace rewritten to this checkout, plus the replace for the runtime itself.
func writeEmittedClientModule(t *testing.T, moduleDir, source string) {
	t.Helper()
	clientDir, err := filepath.Abs(filepath.Join("..", "client"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(clientDir, "go.mod"))
	if err != nil {
		t.Fatalf("read client go.mod: %v", err)
	}
	rewritten := emittedModuleLine.ReplaceAllString(string(manifest), "module generated.example/deploysclient")
	rewritten = emittedRelativeReplace.ReplaceAllStringFunc(rewritten, func(match string) string {
		parts := emittedRelativeReplace.FindStringSubmatch(match)
		return "replace " + parts[1] + " => " + filepath.Clean(filepath.Join(clientDir, parts[2]))
	})
	rewritten = strings.TrimRight(rewritten, "\n") +
		"\n\nrequire go.putnami.dev/client v0.0.0\n\nreplace go.putnami.dev/client => " + clientDir + "\n"
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(clientDir, "go.sum"))
	if err != nil {
		t.Fatalf("read client go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.sum"), sum, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "client.gen.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}
