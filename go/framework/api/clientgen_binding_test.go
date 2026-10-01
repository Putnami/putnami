package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func explicitBindingSpec() SpecIR {
	attempts, timeout := 2, 5_000
	notRetryable := false
	operation := strictUnaryOperation("/config/resolve", []clientcontract.DeclaredError{{
		Status: 404, Code: "config.missing", Retryable: &notRetryable,
		Schema: &clientcontract.Schema{Ref: "#/components/schemas/Missing"},
	}})
	operation.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
		AllOf: []clientcontract.SecurityRequirement{{Profile: "bootstrap"}},
	}}}
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "config", Audience: "urn:config"},
			Credentials: map[string]clientcontract.CredentialProfile{
				"bootstrap": {Kind: clientcontract.CredentialAPIKey, Header: "X-Bootstrap-Key"},
			},
			Defaults: &clientcontract.Defaults{Resilience: &clientcontract.ResiliencePolicy{
				TimeoutMs: &timeout, Retry: &clientcontract.RetryPolicy{MaxAttempts: &attempts},
			}},
		},
		Schemas: map[string]clientcontract.Schema{
			"Query": strictSchemaObject(map[string]clientcontract.Schema{
				"mode": {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"ok"`), json.RawMessage(`"retry"`), json.RawMessage(`"exhausted"`), json.RawMessage(`"invalid"`), json.RawMessage(`"missing"`)}},
			}, "mode"),
			"Config":  strictSchemaObject(map[string]clientcontract.Schema{"value": {Type: "string"}}, "value"),
			"Missing": strictSchemaObject(map[string]clientcontract.Schema{"name": {Type: "string"}}, "name"),
		},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "resolveConfig", OperationID: "resolveConfig", HTTPMethod: "POST", Path: "/config/resolve",
			Request:   &RequestIR{Required: true, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/Query"}}}},
			Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "application/json", Schema: &clientcontract.Schema{Ref: "#/components/schemas/Config"}}}}},
			Client:    operation,
		}}}},
	}
}

func TestGeneratedExplicitBindingConstructorNamesAreDeterministicAndReserved(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "explicit-client-binding", "the-binding-constructor-is-deterministic-and-its-symbol-is-reserved")
	for _, name := range []string{"", "ConfigClient"} {
		t.Run(name, func(t *testing.T) {
			clientName := name
			if clientName == "" {
				clientName = "Client"
			}
			opts := ClientGenOptions{PackageName: "configclient", ClientName: name}
			source, err := GenerateClientFromIR(explicitBindingSpec(), opts)
			if err != nil {
				t.Fatal(err)
			}
			constructor := "New" + clientName + "Binding"
			if !strings.Contains(source, "func "+constructor+"(binding client.ServiceBinding) (*"+clientName+", error)") {
				t.Fatalf("missing public binding constructor %s", constructor)
			}
			second, err := GenerateClientFromIR(explicitBindingSpec(), opts)
			if err != nil || second != source {
				t.Fatalf("generation is not deterministic: %v", err)
			}
			spec := explicitBindingSpec()
			spec.Schemas[constructor] = clientcontract.Schema{Type: "string"}
			_, err = GenerateClientFromIR(spec, opts)
			if !errors.Is(err, CodeClientGenCollision) || !strings.Contains(err.Error(), constructor) {
				t.Fatalf("constructor collision = %v", err)
			}
		})
	}
}

// This external package cannot access the generated descriptor. The only
// construction API it uses is the public binding constructor, without DI.
const generatedExplicitBindingE2E = `package configclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	configclient "generated.example/usersclient"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
)

func TestExplicitBindingPreservesTheProviderContractWithoutDI(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/config/resolve" || r.Header.Get("X-Bootstrap-Key") != "bootstrap-key" || r.Header.Get("X-Client-Id") != "config-source" {
			t.Errorf("unexpected bound request: %s %s %v", r.Method, r.URL.Path, r.Header)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var query struct { Mode string }
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls[query.Mode]++
		attempt := calls[query.Mode]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case query.Mode == "exhausted" || query.Mode == "retry" && attempt == 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case query.Mode == "missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(` + "`" + `{"code":"config.missing","message":"private provider detail","details":{"name":"setting"}}` + "`" + `))
		case query.Mode == "invalid":
			_, _ = w.Write([]byte(` + "`" + `{"value":42}` + "`" + `))
		default:
			_, _ = w.Write([]byte(` + "`" + `{"value":"loaded"}` + "`" + `))
		}
	}))
	defer server.Close()
	binding := client.ServiceBinding{
		URL: server.URL, ClientID: "config-source", AllowInsecure: true,
		Credentials: map[string]client.CredentialBinding{"bootstrap": {Source: client.CredentialSourceStatic, Value: "bootstrap-key"}},
	}
	for _, invalid := range []client.ServiceBinding{{}, {URL: "http://config.example.com", ClientID: "config-source"}, {URL: server.URL, AllowInsecure: true}} {
		if c, err := configclient.NewConfigClientBinding(invalid); c != nil || !perrors.Is(err, client.CodeClientConfig) {
			t.Fatalf("invalid binding returned client=%v err=%v", c, err)
		}
	}
	bound, err := configclient.NewConfigClientBinding(binding)
	if err != nil { t.Fatal(err) }
	// The registry snapshots the binding, so a later caller-owned map edit
	// cannot replace the credential of a client already constructed.
	delete(binding.Credentials, "bootstrap")
	unbound, err := configclient.NewConfigClientBinding(binding)
	if err != nil { t.Fatal(err) }
	input := func(mode string) configclient.ResolveConfigInput {
		var in configclient.ResolveConfigInput
		in.Body.Mode = configclient.QueryMode(mode)
		return in
	}
	if _, err := unbound.ResolveConfig(context.Background(), input("ok")); !perrors.Is(err, client.CodeClientCredential) {
		t.Fatalf("missing credential did not fail closed: %v", err)
	}
	if _, err := bound.ResolveConfig(context.Background(), input("undeclared")); !perrors.Is(err, client.CodeClientRequest) {
		t.Fatalf("invalid request did not fail before dispatch: %v", err)
	}
	mu.Lock()
	if len(calls) != 0 { t.Errorf("refused calls reached provider: %v", calls) }
	mu.Unlock()
	for _, mode := range []string{"ok", "retry"} {
		out, err := bound.ResolveConfig(context.Background(), input(mode))
		if err != nil || out == nil || out.Value != "loaded" { t.Fatalf("%s: out=%v err=%v", mode, out, err) }
	}
	if _, err := bound.ResolveConfig(context.Background(), input("exhausted")); err == nil { t.Fatal("exhausted retry succeeded") }
	if _, err := bound.ResolveConfig(context.Background(), input("invalid")); !perrors.Is(err, client.CodeClientResponse) {
		t.Fatalf("invalid response did not fail validation: %v", err)
	}
	_, err = bound.ResolveConfig(context.Background(), input("missing"))
	var missing *configclient.ResolveConfigConfigMissingError
	if !errors.As(err, &missing) || missing.Payload == nil || missing.Payload.Name != "setting" || missing.Remote.Service() != "config" || missing.Remote.Operation() != "resolveConfig" || missing.Remote.Message != "" {
		t.Fatalf("declared typed error was not preserved: %#v (%v)", missing, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for mode, want := range map[string]int{"ok": 1, "retry": 2, "exhausted": 2, "invalid": 1, "missing": 1} {
		if calls[mode] != want { t.Errorf("%s dispatched %d times, want %d", mode, calls[mode], want) }
	}
}
`

func TestGeneratedExplicitBindingCallsTheProviderWithoutDI(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "explicit-client-binding", "the-emitted-binding-client-preserves-auth-policy-schemas-and-errors-without-di")
	source, err := GenerateClientFromIR(explicitBindingSpec(), ClientGenOptions{PackageName: "configclient", ClientName: "ConfigClient"})
	if err != nil {
		t.Fatal(err)
	}
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "binding_e2e_test.go"), []byte(generatedExplicitBindingE2E), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the emitted binding client failed against the real runtime: %v\n%s", err, output)
	}
}
