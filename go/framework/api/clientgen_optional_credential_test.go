package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// optionalCredentialSpec is the contract of a public package read that answers
// more to an authenticated caller: the `user` credential when the consumer
// holds one, anonymous otherwise (ADR 0002 of go/framework/security).
func optionalCredentialSpec() SpecIR {
	resolveOperation := strictUnaryOperation("/packages/{name}/resolve", []clientcontract.DeclaredError{})
	resolveOperation.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "user", Scopes: []string{"packages:read"}}}},
		{AllOf: []clientcontract.SecurityRequirement{}},
	}}
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "put-server", Audience: "put-server"},
			Credentials: map[string]clientcontract.CredentialProfile{
				"user": {Kind: clientcontract.CredentialForwardedUserToken},
			},
		},
		Schemas: map[string]clientcontract.Schema{
			"Resolved": strictSchemaObject(map[string]clientcontract.Schema{"caller": {Type: "string"}}, "caller"),
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "resolvePackage", OperationID: "resolvePackage", HTTPMethod: "GET", Path: "/packages/{name}/resolve",
			Parameters: []ParameterIR{{Name: "name", Location: "path", Required: true, Schema: clientcontract.Schema{Type: "string"}}},
			Successes:  []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/Resolved"}}}}},
			Client:     resolveOperation,
		}}}},
	}
}

// generatedOptionalCredentialE2E runs inside the throwaway module, in the
// generated package: the emitted client, bound through the real runtime, and
// the Authorization header each call carried, observed by the provider.
const generatedOptionalCredentialE2E = `package packagesclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.putnami.dev/client"
)

func TestTheEmittedClientPresentsTheOptionalCredentialOnlyWhenItHoldsOne(t *testing.T) {
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(` + "`" + `{"caller":"any"}` + "`" + `))
	}))
	defer server.Close()
	bind := func(credentials map[string]client.CredentialBinding) *PackagesClient {
		transport, err := client.NewServiceClientBinding(client.ServiceBinding{
			URL: server.URL, ClientID: "registry.cli", AllowInsecure: true, Credentials: credentials,
		}, serviceDescriptor)
		if err != nil {
			t.Fatal(err)
		}
		return NewPackagesClient(transport)
	}
	forwarding := bind(map[string]client.CredentialBinding{"user": {Source: client.CredentialSourceForwardedUser}})
	unbound := bind(nil)
	var in ResolvePackageInput
	in.Path.Name = "private"
	cases := []struct {
		name     string
		ctx      context.Context
		packages *PackagesClient
		want     string
	}{
		{"the binding holds the credential", client.WithForwardedUserToken(context.Background(), "user-token"), forwarding, "Bearer user-token"},
		{"the binding forwards and the call carries no token", context.Background(), forwarding, ""},
		{"the binding declares no credential", client.WithForwardedUserToken(context.Background(), "user-token"), unbound, ""},
	}
	for _, tc := range cases {
		if _, err := tc.packages.ResolvePackage(tc.ctx, in); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := <-seen; got != tc.want {
			t.Fatalf("%s: the provider received Authorization %q, want %q", tc.name, got, tc.want)
		}
	}
}
`

// TestTheEmittedGoClientPresentsAnOptionalCredentialOnlyWhenItsBindingHoldsOne
// is the Go half of provider contract → generation → compiled client → real
// bound call for an optional credential. The emitted package embeds both
// alternatives in their declared order, and the runtime it compiles against
// presents the credential when the binding satisfies it and calls anonymously
// when it does not — instead of failing with client.credential.
func TestTheEmittedGoClientPresentsAnOptionalCredentialOnlyWhenItsBindingHoldsOne(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "optional-credential",
		"the-emitted-go-client-presents-an-optional-credential-only-when-its-binding-holds-one")
	spec := optionalCredentialSpec()
	source, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName: "packagesclient", ClientName: "PackagesClient",
	})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	// The emitted operation carries the declared alternatives verbatim and in
	// order: an emitter that dropped or reordered the anonymous alternative
	// would change what every consumer presents.
	embedded, err := json.Marshal(spec.Services[0].Methods[0].Client)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, strconv.Quote(string(embedded))) {
		t.Fatalf("the emitted operation is not the contract it was generated from:\n%s", source)
	}
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "optional_credential_e2e_test.go"), []byte(generatedOptionalCredentialE2E), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted client failed against the real runtime: %v\n%s", testErr, output)
	}
}
