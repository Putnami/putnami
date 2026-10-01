package api

import (
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// binarySchema is the one shape a raw octet representation takes.
func binarySchema() *clientcontract.Schema {
	return &clientcontract.Schema{Type: "string", Format: "binary"}
}

// binaryFixtureSpec declares one upload (octets in, document out) and one
// download (document-free in, octets out) so both halves of the emission are
// exercised by the same contract.
func binaryFixtureSpec() SpecIR {
	stored := strictSchemaObject(map[string]clientcontract.Schema{
		"size": {Type: "integer", Format: "int64"},
	}, "size")
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "blobs", Audience: "https://blobs.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{"Stored": stored},
		Services: []ServiceIR{{Methods: []MethodIR{
			{
				Name: "putBlob", OperationID: "putBlob", HTTPMethod: "PUT", Path: "/blobs/{id}",
				Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
				Request: &RequestIR{Required: true, Content: []ContentIR{
					{MediaType: "application/octet-stream", Schema: binarySchema(), MaxBytes: 4096},
				}},
				Successes: []SuccessIR{{Status: 201, Content: []ContentIR{
					{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/Stored"}},
				}}},
				Client: strictUnaryOperation("/blobs/{id}", nil),
			},
			{
				Name: "getBlob", OperationID: "getBlob", HTTPMethod: "GET", Path: "/blobs/{id}",
				Parameters: []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
				Successes: []SuccessIR{{Status: 200, Content: []ContentIR{
					{MediaType: "image/png", Schema: binarySchema(), MaxBytes: 8192},
				}}},
				Client: strictUnaryOperation("/blobs/{id}", []clientcontract.DeclaredError{{Status: 404, Code: "not_found"}}),
			},
		}}},
	}
}

// TestGenerateStrictClient_EmitsRawOctetsWithoutJSONOrBase64 pins the emitted
// shape: a reader in, bounded before the socket; the bytes, the status and the
// content type out; and nowhere a JSON encode or a base64 hop.
func TestGenerateStrictClient_EmitsRawOctetsWithoutJSONOrBase64(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "binary-payloads", "the-emitted-client-carries-octets-without-json-or-base64")
	source, err := GenerateClientFromIR(binaryFixtureSpec(), ClientGenOptions{
		PackageName: "blobsclient", ClientName: "BlobsClient",
	})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	for _, want := range []string{
		"Body io.Reader",
		"client.ReadBoundedBody(in.Body, 4096)",
		`headers.Set("Content-Type", "application/octet-stream")`,
		"client.CallOperationBinary(ctx, c.transport, call, getBlobOperation)",
		"type GetBlobOutput struct",
		"ContentType string",
		"Body []byte",
		"func (c *BlobsClient) GetBlob(ctx context.Context, in GetBlobInput) (*GetBlobOutput, error)",
		"func (c *BlobsClient) PutBlob(ctx context.Context, in PutBlobInput) (*Stored, error)",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("emitted client is missing %q", want)
		}
	}
	for _, forbidden := range []string{"base64", "client.EncodeJSON(in.Body)"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("emitted client re-wraps raw octets: found %q", forbidden)
		}
	}
	// The declared bound reaches the runtime metadata, so the response read is
	// capped by the contract and not only by the resilience policy.
	if !strings.Contains(source, `\"maxBytes\":8192`) {
		t.Errorf("the declared response bound is missing from the embedded operation metadata:\n%s", source)
	}
}

// TestGenerateStrictClient_RefusesRawOctetsWhereTheyCannotTravel collects the
// refusals: a transport that cannot carry octets, a stream, and a success set
// the emitter cannot discriminate.
func TestGenerateStrictClient_RefusesRawOctetsWhereTheyCannotTravel(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "binary-payloads", "raw-octets-are-refused-where-they-cannot-travel-unchanged")
	t.Run("connect transport", func(t *testing.T) {
		spec := binaryFixtureSpec()
		method := &spec.Services[0].Methods[1]
		method.Client.Transports = []clientcontract.Transport{{
			Protocol: clientcontract.TransportConnect, Path: "/blobs.v1.Blobs/Get", Encoding: clientcontract.EncodingProto,
		}}
		_, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"})
		if err == nil || !strings.Contains(err.Error(), "only rest-json carries octets unchanged") {
			t.Fatalf("connect + octets = %v, want an explicit refusal", err)
		}
	})

	t.Run("stream", func(t *testing.T) {
		spec := binaryFixtureSpec()
		method := &spec.Services[0].Methods[1]
		method.Client.Stream = clientcontract.StreamServer
		method.Client.Transports = []clientcontract.Transport{{
			Protocol: clientcontract.TransportSSE, Path: "/blobs/{id}", Encoding: clientcontract.EncodingJSON,
		}}
		_, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"})
		if err == nil || !strings.Contains(err.Error(), "streams carry declared messages, not bodies") {
			t.Fatalf("stream + octets = %v, want an explicit refusal", err)
		}
	})

	t.Run("multiple success variants", func(t *testing.T) {
		spec := binaryFixtureSpec()
		method := &spec.Services[0].Methods[1]
		method.Successes = append(method.Successes, SuccessIR{Status: 204})
		_, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"})
		if err == nil || !strings.Contains(err.Error(), "raw octet payload beside 2 declared success variants") {
			t.Fatalf("multi-success + octets = %v, want an explicit refusal", err)
		}
	})
}
