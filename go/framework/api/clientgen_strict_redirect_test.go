package api

import (
	"encoding/json"
	"strings"
	"testing"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// redirectFixtureSpec declares two operations that answer a redirect:
//
//   - openArtifact: a 302 alone, the provider's Location being the answer;
//   - resolveArtifact: the artifact under 200, or a 307 to where it lives.
func redirectFixtureSpec() SpecIR {
	artifact := strictSchemaObject(map[string]clientcontract.Schema{"id": {Type: "string"}}, "id")
	idParameter := []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}}
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "artifacts", Audience: "https://artifacts.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{"Artifact": artifact},
		Services: []ServiceIR{{Methods: []MethodIR{
			{
				Name: "openArtifact", OperationID: "openArtifact", HTTPMethod: "GET", Path: "/artifacts/{id}",
				Parameters: idParameter,
				Successes:  []SuccessIR{{Status: 302}},
				Client:     strictUnaryOperation("/artifacts/{id}", nil),
			},
			{
				Name: "resolveArtifact", OperationID: "resolveArtifact", HTTPMethod: "GET", Path: "/artifacts/{id}/resolve",
				Parameters: idParameter,
				Successes: []SuccessIR{
					{Status: 200, Content: jsonContent(schemaRef("Artifact"))},
					{Status: 307},
				},
				Client: strictUnaryOperation("/artifacts/{id}/resolve", nil),
			},
		}}},
	}
}

// An operation that declares a redirect returns the status the provider
// answered and its Location, beside any body another status declares, and
// travels on REST only: a Connect response carries no redirect.
func TestGenerateStrictClient_RedirectOperationReturnsStatusAndLocation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "redirect-success", "a-redirect-operation-returns-the-answered-status-and-location")
	source, err := GenerateClientFromIR(redirectFixtureSpec(), ClientGenOptions{PackageName: "artifactsclient", ClientName: "ArtifactsClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	for _, want := range []string{
		"func (c *ArtifactsClient) OpenArtifact(ctx context.Context, in OpenArtifactInput) (*OpenArtifactResult, error) {",
		"type OpenArtifactResult struct {",
		"// Location is the target of a 302 redirect, as the provider answered it.",
		"\tLocation string\n",
		"result := &OpenArtifactResult{Status: response.StatusCode}\n\tswitch response.StatusCode {\n\tcase 302:\n\t\tresult.Location = response.Headers.Get(\"Location\")\n\t}",
		`"[{\"status\":302,\"content\":null}]"`,
		"type ResolveArtifactResult struct {",
		"// Location is the target of a 307 redirect, as the provider answered it.",
		"// Body is the body of a 200 answer, nil on any other status.",
		"case 307:\n\t\tresult.Location = response.Headers.Get(\"Location\")",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("emitted client is missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("source:\n%s", source)
	}

	connect := redirectFixtureSpec()
	connect.Services[0].Methods[0].Client.Transports = []clientcontract.Transport{{
		Protocol: clientcontract.TransportConnect, Path: "/artifacts.v1.Artifacts/Open", Encoding: clientcontract.EncodingJSON,
		ProtobufMethod: "/artifacts.v1.Artifacts/Open",
	}}
	_, err = GenerateClientFromIR(connect, ClientGenOptions{PackageName: "artifactsclient", ClientName: "ArtifactsClient"})
	if errors.GetCode(err) != CodeClientGenUnsupportedSemantic || !strings.Contains(err.Error(), "openArtifact") ||
		!strings.Contains(err.Error(), "a Connect response carries no redirect") {
		t.Fatalf("connect redirect = %v, want an unsupported-semantic refusal naming openArtifact", err)
	}
}

// The contract reader keeps a declared redirect as a success and leaves a 304
// to conditional reads.
func TestNeutralSuccessesKeepsRedirectsButNotNotModified(t *testing.T) {
	successes, err := neutralSuccesses(map[string]oaResponse{
		"200": {Description: "ok"},
		"302": {Description: "found"},
		"304": {Description: "not modified"},
		"404": {Description: "missing"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int, len(successes))
	for i, success := range successes {
		got[i] = success.Status
	}
	if len(got) != 2 || got[0] != 200 || got[1] != 302 {
		t.Fatalf("successes = %v, want [200 302]", got)
	}
}

// Beside a raw octet success, a declared redirect stays documentation: the
// octets travel on exactly one success status, so the operation still
// generates, as it did before redirects were successes.
func TestNeutralSuccessesLeavesARedirectBesideOctetsOut(t *testing.T) {
	var responses map[string]oaResponse
	if err := json.Unmarshal([]byte(`{
		"200": {"description": "the blob", "content": {"application/octet-stream": {"schema": {"type": "string", "format": "binary"}, "x-putnami-max-bytes": 4096}}},
		"302": {"description": "where the blob lives"}
	}`), &responses); err != nil {
		t.Fatal(err)
	}
	successes, err := neutralSuccesses(responses, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(successes) != 1 || successes[0].Status != 200 {
		t.Fatalf("successes = %+v, want the 200 octets alone", successes)
	}
}
