package api

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// opaqueFixtureSpec is one operation whose body and reply carry every opaque
// form: a free-form object, required and optional opaque values, an array of
// them, and a component that is itself opaque.
func opaqueFixtureSpec() SpecIR {
	open := true
	audit := strictSchemaObject(map[string]clientcontract.Schema{
		"attributes": {Type: "object", AdditionalProperties: &clientcontract.AdditionalProperties{Allowed: &open}},
		"value":      {OpaqueJSON: clientcontract.OpaqueJSONAny},
		"optional":   {OpaqueJSON: clientcontract.OpaqueJSONAny, Description: "absent and null stay two values"},
		"trail":      {Type: "array", Items: &clientcontract.Schema{OpaqueJSON: clientcontract.OpaqueJSONAny}},
		"snapshot":   {Ref: "#/components/schemas/Snapshot"},
	}, "attributes", "value")
	reference := &clientcontract.Schema{Ref: "#/components/schemas/Audit"}
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "audit", Audience: "https://audit.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{
			"Audit":    audit,
			"Snapshot": {OpaqueJSON: clientcontract.OpaqueJSONAny},
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "echoAudit", OperationID: "echoAudit", HTTPMethod: "POST", Path: "/audit",
			Request:   &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: reference}}},
			Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: reference}}}},
			Client:    strictUnaryOperation("/audit", []clientcontract.DeclaredError{}),
		}}}},
	}
}

func TestGenerateStrictClient_CarriesOpaqueJSONAsRawBytes(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "opaque-json", "the-emitted-go-client-holds-opaque-json-as-raw-bytes")
	source, err := GenerateClientFromIR(opaqueFixtureSpec(), ClientGenOptions{PackageName: "auditclient", ClientName: "AuditClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", source, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, source)
	}
	normalized := strings.Join(strings.Fields(source), " ")
	for _, want := range []string{
		// An alias keeps json.RawMessage's methods; a defined type would
		// marshal the bytes as base64.
		"type Snapshot = json.RawMessage",
		"Attributes map[string]json.RawMessage `json:\"attributes\"`",
		"Value json.RawMessage `json:\"value\"`",
		"Optional json.RawMessage `json:\"optional,omitempty\"`",
		"Snapshot Snapshot `json:\"snapshot,omitempty\"`",
		"Trail *[]json.RawMessage `json:\"trail,omitempty\"`",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("generated client is missing %q:\n%s", want, source)
		}
	}
}

func TestGenerateStrictClient_RefusesOpaqueJSONWhereItHasNoForm(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "opaque-json", "an-opaque-value-is-refused-as-a-parameter-and-on-a-connect-dispatch")
	open := true
	tests := []struct {
		name   string
		mutate func(*SpecIR)
		want   string
	}{
		{name: "path parameter", mutate: func(spec *SpecIR) {
			method := &spec.Services[0].Methods[0]
			method.Path = "/audit/{id}"
			method.Client.Transports[0].Path = "/audit/{id}"
			method.Parameters = []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{OpaqueJSON: clientcontract.OpaqueJSONAny}}}
		}, want: "not representable as a parameter"},
		// A hand-written or TypeScript-provider spec can reach the value through
		// a component reference; the refusal is the same.
		{name: "path parameter through a reference", mutate: func(spec *SpecIR) {
			method := &spec.Services[0].Methods[0]
			method.Path = "/audit/{id}"
			method.Client.Transports[0].Path = "/audit/{id}"
			method.Parameters = []ParameterIR{{Name: "id", Location: "path", Required: true, Schema: clientcontract.Schema{Ref: "#/components/schemas/Snapshot"}}}
		}, want: "not representable as a parameter"},
		{name: "query array of referenced values", mutate: func(spec *SpecIR) {
			spec.Services[0].Methods[0].Parameters = []ParameterIR{{Name: "tag", Location: "query", Schema: clientcontract.Schema{
				Type: "array", Items: &clientcontract.Schema{Ref: "#/components/schemas/Snapshot"},
			}}}
		}, want: "not representable as a parameter"},
		{name: "query parameter referencing an array of opaque values", mutate: func(spec *SpecIR) {
			spec.Schemas["Trail"] = clientcontract.Schema{Type: "array", Items: &clientcontract.Schema{OpaqueJSON: clientcontract.OpaqueJSONAny}}
			spec.Services[0].Methods[0].Parameters = []ParameterIR{{Name: "trail", Location: "query", Schema: clientcontract.Schema{Ref: "#/components/schemas/Trail"}}}
		}, want: "not representable as a parameter"},
		{name: "connect dispatch", mutate: func(spec *SpecIR) {
			spec.Services[0].Methods[0].Client.Transports = []clientcontract.Transport{{
				Protocol: clientcontract.TransportConnect, Path: "/audit.v1.ApiService/EchoAudit",
				Encoding: clientcontract.EncodingJSON, ProtobufMethod: "/audit.v1.ApiService/EchoAudit",
			}}
		}, want: "opaque JSON value"},
		{name: "named properties beside open members", mutate: func(spec *SpecIR) {
			spec.Schemas["Audit"] = clientcontract.Schema{
				Type:                 "object",
				Properties:           map[string]clientcontract.Schema{"id": {Type: "string"}},
				AdditionalProperties: &clientcontract.AdditionalProperties{Allowed: &open},
			}
		}, want: "open object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := opaqueFixtureSpec()
			tc.mutate(&spec)
			err := strictGenerationError(t, spec)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			// An inline and a referenced opaque parameter carry one code.
			if strings.Contains(tc.want, "parameter") && errors.GetCode(err) != CodeClientGenUnsupportedSemantic {
				t.Fatalf("code = %s, want %s", errors.GetCode(err), CodeClientGenUnsupportedSemantic)
			}
		})
	}
}

// generatedOpaqueE2E runs inside the throwaway module, in the generated
// package: a provider that answers with the exact bytes it received, and the
// emitted client bound through the real runtime on both directions.
const generatedOpaqueE2E = `package auditclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.putnami.dev/client"
)

func TestTheEmittedClientKeepsOpaqueJSONBytes(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(received)
	}))
	defer server.Close()
	transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL: server.URL, ClientID: "consumer", AllowInsecure: true}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	audits := NewAuditClient(transport)

	// Unsorted keys, an integer past uint64, a decimal past float64 and a
	// number past float64's range: every one of them is lost by a decode into
	// any, and every one must come back as the same bytes.
	trail := []json.RawMessage{json.RawMessage(` + "`" + `1e400` + "`" + `), json.RawMessage(` + "`" + `"s"` + "`" + `), json.RawMessage(` + "`" + `null` + "`" + `)}
	sent := Audit{
		Attributes: map[string]json.RawMessage{
			"z": json.RawMessage(` + "`" + `{"b":1,"a":[18446744073709551616,0.1000000000000000055511151231257827]}` + "`" + `),
			"n": json.RawMessage(` + "`" + `null` + "`" + `),
		},
		Value:    json.RawMessage(` + "`" + `{"z":true,"a":"x","m":{}}` + "`" + `),
		Optional: json.RawMessage(` + "`" + `null` + "`" + `),
		Snapshot: Snapshot(` + "`" + `[{"k":"v"},-0.0]` + "`" + `),
		Trail:    &trail,
	}
	echoed, err := audits.EchoAudit(context.Background(), EchoAuditInput{Body: sent})
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2][]byte{
		"value":        {sent.Value, echoed.Value},
		"attributes.z": {sent.Attributes["z"], echoed.Attributes["z"]},
		"attributes.n": {sent.Attributes["n"], echoed.Attributes["n"]},
		"optional":     {sent.Optional, echoed.Optional},
		"snapshot":     {sent.Snapshot, echoed.Snapshot},
	} {
		if !bytes.Equal(pair[0], pair[1]) {
			t.Errorf("%s: sent %s, got back %s", name, pair[0], pair[1])
		}
	}
	if echoed.Trail == nil || len(*echoed.Trail) != 3 || string((*echoed.Trail)[0]) != "1e400" || string((*echoed.Trail)[2]) != "null" {
		t.Errorf("trail = %v, want the three values as sent", echoed.Trail)
	}
	if !strings.Contains(string(received), ` + "`" + `"optional":null` + "`" + `) {
		t.Errorf("an explicit null was not sent: %s", received)
	}

	// An absent optional member stays absent: it is not sent, and it does not
	// come back as null.
	echoed, err = audits.EchoAudit(context.Background(), EchoAuditInput{Body: Audit{
		Attributes: map[string]json.RawMessage{}, Value: json.RawMessage(` + "`" + `0` + "`" + `),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(received), ` + "`" + `"optional"` + "`" + `) || echoed.Optional != nil {
		t.Errorf("an absent optional member crossed the wire: sent %s, got %q", received, echoed.Optional)
	}
}
`

// TestTheEmittedGoClientKeepsOpaqueJSONBytesThroughTheRealRuntime is the Go
// half of declaration → generation → compiled client → real bound call for an
// opaque value: the runtime validates the request and the reply against the
// published schema and hands the provider's bytes back unchanged.
func TestTheEmittedGoClientKeepsOpaqueJSONBytesThroughTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "opaque-json", "the-emitted-go-client-carries-opaque-json-bytes-through-the-real-runtime")
	source, err := GenerateClientFromIR(opaqueFixtureSpec(), ClientGenOptions{PackageName: "auditclient", ClientName: "AuditClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "opaque_e2e_test.go"), []byte(generatedOpaqueE2E), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client lost opaque JSON against the real runtime: %v\n%s\n--- source:\n%s", testErr, output, source)
	}
}
