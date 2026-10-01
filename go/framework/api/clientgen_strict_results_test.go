package api

import (
	"strings"
	"testing"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func jsonContent(schema clientcontract.Schema) []ContentIR {
	return []ContentIR{{MediaType: "application/json", Schema: &schema}}
}

func schemaRef(name string) clientcontract.Schema {
	return clientcontract.Schema{Ref: "#/components/schemas/" + name}
}

// multiSuccessFixtureSpec declares the shapes a provider answers with
// more than one success status, plus a nullable body:
//
//   - createWidget: created or existing, the same body under 200 and 201;
//   - deployWidget: done or accepted, a different body under 200 and 202;
//   - removeWidget: a body under 200, nothing under 204;
//   - touchWidget: a nullable body under 200, nothing under 202;
//   - replaceWidget: created or replaced, nothing under 201 or 204.
func multiSuccessFixtureSpec() SpecIR {
	nullable := true
	widget := strictSchemaObject(map[string]clientcontract.Schema{"id": {Type: "string"}}, "id")
	job := strictSchemaObject(map[string]clientcontract.Schema{"jobId": {Type: "string"}}, "jobId")
	idParameter := []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}}
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "widgets", Audience: "https://widgets.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{"Widget": widget, "Job": job},
		Services: []ServiceIR{{Methods: []MethodIR{
			{
				Name: "createWidget", OperationID: "createWidget", HTTPMethod: "POST", Path: "/widgets",
				Request: &RequestIR{Required: true, Content: jsonContent(schemaRef("Widget"))},
				Successes: []SuccessIR{
					{Status: 200, Content: jsonContent(schemaRef("Widget"))},
					{Status: 201, Content: jsonContent(schemaRef("Widget"))},
				},
				Client: strictUnaryOperation("/widgets", []clientcontract.DeclaredError{{Status: 409, Code: "conflict"}}),
			},
			{
				Name: "deployWidget", OperationID: "deployWidget", HTTPMethod: "POST", Path: "/widgets/{id}/deploy",
				Parameters: idParameter,
				Successes: []SuccessIR{
					{Status: 200, Content: jsonContent(schemaRef("Widget"))},
					{Status: 202, Content: jsonContent(schemaRef("Job"))},
				},
				Client: strictUnaryOperation("/widgets/{id}/deploy", nil),
			},
			{
				Name: "removeWidget", OperationID: "removeWidget", HTTPMethod: "DELETE", Path: "/widgets/{id}",
				Parameters: idParameter,
				Successes: []SuccessIR{
					{Status: 200, Content: jsonContent(schemaRef("Widget"))},
					{Status: 204},
				},
				Client: strictUnaryOperation("/widgets/{id}", nil),
			},
			{
				Name: "touchWidget", OperationID: "touchWidget", HTTPMethod: "PUT", Path: "/widgets/{id}/touch",
				Parameters: idParameter,
				Successes: []SuccessIR{
					{Status: 200, Content: jsonContent(clientcontract.Schema{Ref: "#/components/schemas/Widget", Nullable: &nullable})},
					{Status: 202},
				},
				Client: strictUnaryOperation("/widgets/{id}/touch", nil),
			},
			{
				Name: "replaceWidget", OperationID: "replaceWidget", HTTPMethod: "PUT", Path: "/widgets/{id}",
				Parameters: idParameter,
				Request:    &RequestIR{Required: true, Content: jsonContent(schemaRef("Widget"))},
				Successes:  []SuccessIR{{Status: 201}, {Status: 204}},
				Client:     strictUnaryOperation("/widgets/{id}", nil),
			},
		}}},
	}
}

// An operation with several success statuses returns the status the provider
// answered beside the body that status declares: one Body when every
// body-carrying status declares the same schema, one Body<status> otherwise,
// and nil where the answered status declares none.
func TestGenerateStrictClient_EmitsOneResultPerMultiSuccessOperation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "multiple-success-statuses", "a-multi-success-operation-returns-the-answered-status-and-its-declared-body")
	source, err := GenerateClientFromIR(multiSuccessFixtureSpec(), ClientGenOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	for _, want := range []string{
		// Same body, two statuses: one field, one decode for both.
		"func (c *WidgetsClient) CreateWidget(ctx context.Context, in CreateWidgetInput) (*CreateWidgetResult, error) {",
		"type CreateWidgetResult struct {",
		"provider answered, 200 or 201, and the body that status declares.",
		"response, err := client.CallOperationResponse(ctx, c.transport, call, createWidgetOperation)",
		"if err != nil {\n\t\treturn nil, decodeCreateWidgetError(err)\n\t}",
		"case 200, 201:",
		"decoded, decodeErr := client.DecodeResponse[Widget](c.transport, response, createWidgetOperation)",
		"result.Body = &decoded",
		// A different body per status: one field per status.
		"type DeployWidgetResult struct {",
		"Body200 *Widget",
		"Body202 *Job",
		"client.DecodeResponse[Job](c.transport, response, deployWidgetOperation)",
		// A body or nothing: one field, nil on the bodyless status.
		"type RemoveWidgetResult struct {",
		"// Body is the body of a 200 answer, nil on any other status.",
		// A nullable body decodes a JSON null to nil.
		"client.DecodeResponse[*Widget](c.transport, response, touchWidgetOperation)",
		"result.Body = decoded",
		"A null body is nil as well.",
		// No body on any status: the status is the whole answer.
		"func (c *WidgetsClient) ReplaceWidget(ctx context.Context, in ReplaceWidgetInput) (*ReplaceWidgetResult, error) {",
		"provider answered, 201 or 204. No status declares a body.",
		"type ReplaceWidgetResult struct {\n\t// Status is the declared success status the provider answered.\n\tStatus int\n}",
		"result := &ReplaceWidgetResult{Status: response.StatusCode}\n\treturn result, nil",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("emitted client is missing %q", want)
		}
	}
	if strings.Contains(source, "Body201") || strings.Contains(source, "Body204") {
		t.Errorf("a status with the shared schema or no body got its own field:\n%s", source)
	}
	again, err := GenerateClientFromIR(multiSuccessFixtureSpec(), ClientGenOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"})
	if err != nil || again != source {
		t.Fatalf("generating the same contract twice differs (err=%v)", err)
	}
	if t.Failed() {
		t.Logf("source:\n%s", source)
	}
}

// Generation refuses a multi-success operation only where its result cannot
// travel: over Connect, whose response carries one status, and beside a raw
// octet response, whose single payload names no status. Octets in the request
// leave the JSON answers to the ordinary result.
func TestGenerateStrictClient_RefusesMultiSuccessOnlyWhereTheResultCannotTravel(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "multiple-success-statuses", "a-multi-success-operation-is-refused-only-where-its-result-cannot-travel")
	generate := func(spec SpecIR) (string, error) {
		return GenerateClientFromIR(spec, ClientGenOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"})
	}

	connect := multiSuccessFixtureSpec()
	connect.Services[0].Methods[1].Client.Transports = []clientcontract.Transport{{
		Protocol: clientcontract.TransportConnect, Path: "/widgets.v1.Widgets/Deploy", Encoding: clientcontract.EncodingJSON,
		ProtobufMethod: "/widgets.v1.Widgets/Deploy",
	}}
	_, err := generate(connect)
	if errors.GetCode(err) != CodeClientGenUnsupportedSemantic || !strings.Contains(err.Error(), "deployWidget") ||
		!strings.Contains(err.Error(), "a Connect response carries one success status") {
		t.Fatalf("connect multi-success = %v, want an unsupported-semantic refusal naming deployWidget", err)
	}

	headers := multiSuccessFixtureSpec()
	headers.Services[0].Methods[0].Successes[1].Headers = []ResponseHeaderIR{{Name: "Location", Required: true, Schema: clientcontract.Schema{Type: "string"}}}
	if _, err := generate(headers); err == nil || !strings.Contains(err.Error(), "typed response headers on success 201") {
		t.Fatalf("typed headers on a multi-success operation = %v, want a refusal naming the status", err)
	}

	octetResponse := binaryFixtureSpec()
	octetResponse.Services[0].Methods[1].Successes = append(octetResponse.Services[0].Methods[1].Successes, SuccessIR{Status: 204})
	_, err = GenerateClientFromIR(octetResponse, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"})
	if errors.GetCode(err) != CodeClientGenUnsupportedSemantic || !strings.Contains(err.Error(), "raw octet payload beside 2 declared success variants") {
		t.Fatalf("octet response + multi-success = %v, want an unsupported-semantic refusal", err)
	}

	octetRequest := binaryFixtureSpec()
	upload := &octetRequest.Services[0].Methods[0]
	upload.Successes = append([]SuccessIR{{Status: 200, Content: jsonContent(schemaRef("Stored"))}}, upload.Successes...)
	source, err := GenerateClientFromIR(octetRequest, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"})
	if err != nil {
		t.Fatalf("octet request + JSON multi-success: %v", err)
	}
	for _, want := range []string{
		"func (c *BlobsClient) PutBlob(ctx context.Context, in PutBlobInput) (*PutBlobResult, error) {",
		"client.ReadBoundedBody(in.Body, 4096)",
		"client.CallOperationResponse(ctx, c.transport, call, putBlobOperation)",
		"case 200, 201:",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("octet upload with two JSON answers is missing %q\n%s", want, source)
		}
	}
}

// The result type is a generated symbol like any other: a schema that already
// owns the name fails generation instead of being silently shadowed.
func TestGenerateStrictClient_ResultTypeClaimsItsSymbol(t *testing.T) {
	spec := multiSuccessFixtureSpec()
	spec.Schemas["CreateWidgetResult"] = strictSchemaObject(map[string]clientcontract.Schema{"id": {Type: "string"}}, "id")
	_, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "widgetsclient", ClientName: "WidgetsClient"})
	if errors.GetCode(err) != CodeClientGenCollision || !strings.Contains(err.Error(), "CreateWidgetResult") {
		t.Fatalf("result symbol collision = %v, want %s", err, CodeClientGenCollision)
	}
}
