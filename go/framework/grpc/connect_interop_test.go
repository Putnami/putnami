package grpc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/api"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/openapi"
	"go.putnami.dev/proto"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
	"go.putnami.dev/protocol/features/spectest"
)

// interopItem carries one value of every shape the two encodings must agree on.
// Its member names are single lowercase words on purpose: the descriptor joins
// to the published schema by jsonName, and a name the proto identifier grammar
// would rewrite has no join.
type interopItem struct {
	ID      string            `json:"id" validate:"required"`
	Count   uint64            `json:"count" validate:"required"`
	Tags    []string          `json:"tags" validate:"required"`
	Labels  map[string]string `json:"labels" validate:"required"`
	Payload []byte            `json:"payload" validate:"required"`
}

// interopNullable declares a member the schema lets be null. proto3 has no
// null, so a Connect client that dispatched on proto could not tell it from an
// absent value — which is why generation refuses that combination.
type interopNullable struct {
	ID   string  `json:"id" validate:"required"`
	Note *string `json:"note"`
}

type interopParams struct {
	ID string `json:"id" validate:"required"`
}

// interopKeyRule is the provider's real authorization: an endpoint that refuses
// a request without the declared api key. It implements phttp.SecurityClaims so
// the contract projection can represent it, which is what lets the operation
// declare the credential a generated client must carry.
type interopKeyRule struct{ key string }

func (rule interopKeyRule) Middleware() phttp.Middleware {
	return func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		if ctx.Request.Header.Get("X-Api-Key") != rule.key {
			return phttp.Unauthorized()
		}
		return next()
	}
}

func (interopKeyRule) SecurityRoles() []string  { return nil }
func (interopKeyRule) SecurityScopes() []string { return nil }

// interopHeaders records what the provider actually received, so the test
// asserts on the wire rather than on the client's intention.
type interopHeaders struct {
	mu      sync.Mutex
	entries []http.Header
}

func (h *interopHeaders) record(header http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, header.Clone())
}

func (h *interopHeaders) snapshot() []http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]http.Header(nil), h.entries...)
}

// interopProvider stands up the real first-party provider — api + proto + the
// Connect bridge + openapi, all four in one process — on a real port, and
// returns its URL and the IR its own published document reads back to.
//
// This is the join the three plugins never had a home for: no module in the
// repository required all of them, so the descriptor was proved against the
// schema in one package and the contract against the corpus in another. Here
// one provider produces all three views and the strict reader accepts them
// together.
func interopProvider(t *testing.T) (string, api.SpecIR, *interopHeaders) {
	t.Helper()
	headers := &interopHeaders{}
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "interop", Audience: "https://interop.internal"},
		Credentials: map[string]clientcontract.CredentialProfile{
			"apiKey": {Kind: clientcontract.CredentialAPIKey, Header: "X-Api-Key"},
		},
	}))
	secured := api.ClientOperationOptions{Security: clientcontract.Security{
		Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "apiKey"}}}},
	}}
	rule := interopKeyRule{key: "interop-key"}
	apiPlugin.Register(api.Endpoint("GET", "/items/{id}").
		Params(api.Type[interopParams]()).
		Returns(api.Type[interopItem]()).
		MayThrow(perrors.CodeNotFound).
		Client(secured).
		Secure(rule).
		Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
			headers.record(ctx.Request.Header)
			return next()
		}).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			if ctx.Params["id"] == "missing" {
				return phttp.ErrorResponse(perrors.NotFound("no such item"))
			}
			return phttp.JSON(interopItem{
				ID: ctx.Params["id"], Count: 18446744073709551615,
				Tags: []string{"a", "b"}, Labels: map[string]string{"k": "v"},
				Payload: []byte{0x00, 0x01, 0xff},
			})
		}))
	apiPlugin.Register(api.Endpoint("GET", "/items/watch").
		Returns(api.StreamOf[interopItem]()).
		Client(secured).
		Secure(rule).
		Handle(api.ServerStream(func(stream *api.ServerStreamContext[interopItem]) error {
			for _, id := range []string{"one", "two"} {
				if err := stream.Send(interopItem{
					ID: id, Count: 7, Tags: []string{}, Labels: map[string]string{}, Payload: []byte{},
				}); err != nil {
					return err
				}
			}
			return nil
		})))
	apiPlugin.Register(api.Endpoint("GET", "/nullable/{id}").
		Params(api.Type[interopParams]()).
		Returns(api.Type[interopNullable]()).
		Client(secured).
		Secure(rule).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			return phttp.JSON(interopNullable{ID: ctx.Params["id"]})
		}))

	protoPlugin := proto.NewPlugin(proto.PluginOptions{PackageName: "interop.v1"}).From(apiPlugin)
	bridge := NewApiBridge(apiPlugin, httpServer, WithPackage("interop.v1"))
	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Interop", Version: "1.0.0"}).From(apiPlugin)

	// The order is the lifecycle order: the api plugin discovers the routes, the
	// proto plugin publishes the descriptor and its route binding, the bridge
	// mounts the URLs and publishes the encodings it serves, and only then does
	// the contract projection read both.
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := protoPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("proto Configure: %v", err)
	}
	if err := bridge.Configure(t.Context(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}
	if err := bridge.Start(t.Context(), nil); err != nil {
		t.Fatalf("bridge Start: %v", err)
	}
	if err := openapiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	document, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("publish the provider document: %v", err)
	}
	ir, err := api.ReadOpenAPISpec(document)
	if err != nil {
		t.Fatalf("the strict reader rejected the provider's own document: %v", err)
	}
	if ir.Contract == nil || ir.Contract.Protobuf == nil {
		t.Fatal("the published contract carries no protobuf descriptor")
	}
	server := httptest.NewServer(httpServer.Handler())
	t.Cleanup(server.Close)
	return server.URL, ir, headers
}

// A real provider with all three plugins mounted publishes a document whose
// descriptor, schema and IR the strict reader accepts together — the parity the
// module boundaries kept in two halves.
func TestConnectInterop_TheThreeViewsAgreeOnOneProvider(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "one-provider-publishes-a-descriptor-schema-and-ir-that-agree")
	_, ir, _ := interopProvider(t)
	transports := map[string][]clientcontract.Transport{}
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			if method.Client != nil {
				transports[method.OperationID] = method.Client.Transports
			}
		}
	}
	unary, declared := transports["getItems_Id"]
	if !declared {
		t.Fatalf("the document declares no getItems operation: %v", transports)
	}
	if len(unary) != 3 || unary[0].Protocol != clientcontract.TransportRESTJSON {
		t.Fatalf("unary transports = %#v, want rest-json then connect json and proto", unary)
	}
	if unary[1].Encoding != clientcontract.EncodingJSON || unary[2].Encoding != clientcontract.EncodingProto {
		t.Fatalf("connect encodings = %#v, want json then proto", unary[1:])
	}
	if unary[1].Path != unary[1].ProtobufMethod || !strings.HasPrefix(unary[1].Path, "/interop.v1.ApiService/") {
		t.Errorf("connect path = %q, want the declared method identity", unary[1].Path)
	}
	stream := transports["getItems_Watch"]
	if len(stream) != 4 || stream[0].Protocol != clientcontract.TransportSSE || stream[2].Protocol != clientcontract.TransportConnect {
		t.Fatalf("stream transports = %#v, want sse, websocket, then connect json and proto", stream)
	}
	// The reply message of a streamed method is declared as streaming in the
	// descriptor, not inferred by the client.
	for _, service := range ir.Contract.Protobuf.Services {
		for _, method := range service.Methods {
			if strings.HasSuffix(stream[2].ProtobufMethod, "/"+method.Name) && !method.ServerStreaming {
				t.Errorf("descriptor method %s is not declared serverStreaming", method.Name)
			}
		}
	}
}

// Generation refuses the one combination whose two encodings would disagree
// about a value the caller can observe.
func TestConnectInterop_GenerationRefusesANullableMemberOverProto(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "the-proto-encoding-is-refused-where-it-would-lose-a-declared-null")
	_, ir, _ := interopProvider(t)
	reordered := reorderTransports(t, ir, clientcontract.TransportConnect, clientcontract.EncodingProto)
	_, err := api.GenerateClientFromIR(reordered, api.ClientGenOptions{PackageName: "x", ClientName: "X"})
	if err == nil {
		t.Fatal("generation accepted a nullable member over the proto encoding")
	}
	if !strings.Contains(err.Error(), "proto3 carries no null") {
		t.Fatalf("error = %v, want the named refusal", err)
	}
}

// The end of the chain: a real Go provider, its own published contract, three
// clients generated from it, compiled in a throwaway module, and run against
// the live server on a real port. Nothing here is scripted on either side.
func TestConnectInterop_GeneratedGoClientsCallTheRealProvider(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "a-generated-go-client-calls-a-real-go-provider-over-connect")
	if testing.Short() {
		t.Skip("compiling a module is not a short test")
	}
	url, ir, headers := interopProvider(t)
	moduleDir := t.TempDir()
	variants := []struct {
		pkg      string
		protocol clientcontract.TransportProtocol
		encoding clientcontract.Encoding
	}{
		{"restclient", clientcontract.TransportRESTJSON, clientcontract.EncodingJSON},
		{"connectjson", clientcontract.TransportConnect, clientcontract.EncodingJSON},
		{"connectproto", clientcontract.TransportConnect, clientcontract.EncodingProto},
	}
	for _, variant := range variants {
		spec := reorderTransports(t, dropNullableOperation(ir), variant.protocol, variant.encoding)
		source, err := api.GenerateClientFromIR(spec, api.ClientGenOptions{
			PackageName: variant.pkg, ClientName: "InteropClient",
		})
		if err != nil {
			t.Fatalf("generate the %s client: %v", variant.pkg, err)
		}
		packageDir := filepath.Join(moduleDir, variant.pkg)
		if err := os.MkdirAll(packageDir, 0o750); err != nil {
			t.Fatalf("create %s: %v", packageDir, err)
		}
		if err := os.WriteFile(filepath.Join(packageDir, "client.gen.go"), []byte(source), 0o600); err != nil {
			t.Fatalf("write the %s client: %v", variant.pkg, err)
		}
	}
	writeInteropModule(t, moduleDir)

	command := exec.Command("go", "run", ".", url)
	command.Dir = moduleDir
	// GOPROXY=off proves the module resolves from the replaced workspace
	// sources and the local cache, so the proof never depends on a fetch.
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=mod", "GOWORK=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated consumer failed: %v\n%s", err, output)
	}
	var report interopReport
	if decodeErr := json.Unmarshal(interopReportJSON(t, output), &report); decodeErr != nil {
		t.Fatalf("consumer output is not the expected report: %v\n%s", decodeErr, output)
	}

	const wantItem = `{"count":18446744073709551615,"id":"i-1","labels":{"k":"v"},"payload":"AAH/","tags":["a","b"]}`
	for name, got := range map[string]string{
		"rest-json":     report.RestItem,
		"connect+json":  report.ConnectJSONItem,
		"connect+proto": report.ConnectProtoItem,
	} {
		if got != wantItem {
			t.Errorf("%s item = %s\nwant %s", name, got, wantItem)
		}
	}
	for name, got := range map[string][]string{
		"rest-json":     report.RestStream,
		"connect+json":  report.ConnectJSONStream,
		"connect+proto": report.ConnectProtoStream,
	} {
		if strings.Join(got, ",") != "one,two" {
			t.Errorf("%s stream = %v, want [one two]", name, got)
		}
	}
	for name, got := range map[string]interopFailure{
		"rest-json":     report.RestError,
		"connect+json":  report.ConnectJSONError,
		"connect+proto": report.ConnectProtoError,
	} {
		if got.Code != string(perrors.CodeNotFound) || got.Status != http.StatusNotFound {
			t.Errorf("%s declared error = %#v, want %s/404", name, got, perrors.CodeNotFound)
		}
	}

	// What the provider actually received: the declared api key, the client
	// identity, and — on the Connect calls only — the caller's own deadline.
	seen := headers.snapshot()
	if len(seen) == 0 {
		t.Fatal("the provider recorded no request")
	}
	connectDeadlines := 0
	for index, header := range seen {
		if header.Get("X-Api-Key") != "interop-key" {
			t.Errorf("request %d carried api key %q, want the declared credential", index, header.Get("X-Api-Key"))
		}
		if header.Get("X-Client-Id") != "interop.consumer" {
			t.Errorf("request %d carried client id %q", index, header.Get("X-Client-Id"))
		}
		if raw := header.Get(connectwire.TimeoutHeader); raw != "" {
			value, convErr := strconv.Atoi(raw)
			if convErr != nil || value <= 0 || value > 30001 {
				t.Errorf("request %d carried %s = %q, want the caller's remaining budget", index, connectwire.TimeoutHeader, raw)
			}
			connectDeadlines++
		}
	}
	if connectDeadlines == 0 {
		t.Error("no request carried Connect-Timeout-Ms; the caller's deadline never reached the provider")
	}
}

type interopFailure struct {
	Code   string `json:"code"`
	Status int    `json:"status"`
}

type interopReport struct {
	RestItem           string         `json:"restItem"`
	ConnectJSONItem    string         `json:"connectJsonItem"`
	ConnectProtoItem   string         `json:"connectProtoItem"`
	RestStream         []string       `json:"restStream"`
	ConnectJSONStream  []string       `json:"connectJsonStream"`
	ConnectProtoStream []string       `json:"connectProtoStream"`
	RestError          interopFailure `json:"restError"`
	ConnectJSONError   interopFailure `json:"connectJsonError"`
	ConnectProtoError  interopFailure `json:"connectProtoError"`
}

// interopReportJSON keeps the last line of the consumer's output, so the
// framework's own startup logs never break the assertion.
func interopReportJSON(t *testing.T, output []byte) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if strings.HasPrefix(line, "{") && strings.Contains(line, `"restItem"`) {
			return []byte(line)
		}
	}
	t.Fatalf("the consumer printed no report:\n%s", output)
	return nil
}

// reorderTransports moves the named transport to the front of every operation
// that declares it. The provider declares REST first; a consumer proving the
// Connect path states the opposite preference over the same declaration, which
// is what the declared order means.
func reorderTransports(t *testing.T, ir api.SpecIR, protocol clientcontract.TransportProtocol, encoding clientcontract.Encoding) api.SpecIR {
	t.Helper()
	encoded, err := json.Marshal(ir)
	if err != nil {
		t.Fatalf("clone the IR: %v", err)
	}
	var clone api.SpecIR
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatalf("clone the IR: %v", err)
	}
	for serviceIndex := range clone.Services {
		for methodIndex := range clone.Services[serviceIndex].Methods {
			method := &clone.Services[serviceIndex].Methods[methodIndex]
			if method.Client == nil {
				continue
			}
			var first []clientcontract.Transport
			var rest []clientcontract.Transport
			for _, transport := range method.Client.Transports {
				if transport.Protocol == protocol && transport.Encoding == encoding {
					first = append(first, transport)
					continue
				}
				rest = append(rest, transport)
			}
			method.Client.Transports = append(append([]clientcontract.Transport(nil), first...), rest...)
		}
	}
	return clone
}

// dropNullableOperation removes the operation that exists only to prove the
// proto refusal, so the three compiled variants share one source shape.
func dropNullableOperation(ir api.SpecIR) api.SpecIR {
	filtered := ir
	filtered.Services = nil
	for _, service := range ir.Services {
		kept := service
		kept.Methods = nil
		for _, method := range service.Methods {
			if strings.HasPrefix(method.Path, "/nullable") {
				continue
			}
			kept.Methods = append(kept.Methods, method)
		}
		filtered.Services = append(filtered.Services, kept)
	}
	return filtered
}

var interopModuleLine = regexp.MustCompile(`(?m)^module\s+\S+`)
var interopRelativeReplace = regexp.MustCompile(`(?m)^replace\s+(\S+)\s+=>\s+(\.\S+)`)

// writeInteropModule materializes a module whose dependency closure is the
// client runtime's own, with every relative replace rewritten to this
// checkout's absolute path.
func writeInteropModule(t *testing.T, moduleDir string) {
	t.Helper()
	clientDir := filepath.Join(interopRepoRoot(t), "go", "framework", "client")
	body, err := os.ReadFile(filepath.Join(clientDir, "go.mod"))
	if err != nil {
		t.Fatalf("read the client go.mod: %v", err)
	}
	rewritten := interopModuleLine.ReplaceAllString(string(body), "module generated.example/interop")
	rewritten = interopRelativeReplace.ReplaceAllStringFunc(rewritten, func(match string) string {
		parts := interopRelativeReplace.FindStringSubmatch(match)
		return "replace " + parts[1] + " => " + filepath.Clean(filepath.Join(clientDir, parts[2]))
	})
	rewritten = strings.TrimRight(rewritten, "\n") +
		"\n\nrequire go.putnami.dev/client v0.0.0\n\nreplace go.putnami.dev/client => " + clientDir + "\n"
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte(rewritten), 0o600); err != nil {
		t.Fatalf("write the module go.mod: %v", err)
	}
	sum, err := os.ReadFile(filepath.Join(clientDir, "go.sum"))
	if err != nil {
		t.Fatalf("read the client go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.sum"), sum, 0o600); err != nil {
		t.Fatalf("write the module go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "main.go"), []byte(interopConsumerSource), 0o600); err != nil {
		t.Fatalf("write the consumer: %v", err)
	}
}

func interopRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "putnami.workspace.json")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no putnami.workspace.json above %s", dir)
		}
		dir = parent
	}
}
