package consumer

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/client"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
	phttp "go.putnami.dev/http"
)

// connectWireRequest is what one request carried to the provider socket: the
// method identity, the media type and the headers a Connect call must send.
// Credential values are never recorded, only whether one was presented.
type connectWireRequest struct {
	Path            string
	ContentType     string
	ProtocolVersion string
	TimeoutMs       string
	Traceparent     string
	KeyPresented    bool
}

type connectWire struct {
	mu       sync.Mutex
	requests []connectWireRequest
}

func (wire *connectWire) record(request *http.Request) {
	wire.mu.Lock()
	defer wire.mu.Unlock()
	wire.requests = append(wire.requests, connectWireRequest{
		Path:            request.URL.Path,
		ContentType:     request.Header.Get("Content-Type"),
		ProtocolVersion: request.Header.Get("Connect-Protocol-Version"),
		TimeoutMs:       request.Header.Get("Connect-Timeout-Ms"),
		Traceparent:     request.Header.Get("traceparent"),
		KeyPresented:    request.Header.Get(service.CatalogKeyHeader) == service.CatalogAPIKey,
	})
}

func (wire *connectWire) snapshot() []connectWireRequest {
	wire.mu.Lock()
	defer wire.mu.Unlock()
	return slices.Clone(wire.requests)
}

// realConnectProvider starts the sample provider with the Connect wire mounted
// — the ConnectPlugins NewApp installs, configured in the lifecycle order — on
// a real loopback socket, and logs what every request carried.
func realConnectProvider(t *testing.T) (string, *connectWire) {
	t.Helper()
	serverPlugin := phttp.NewServerPlugin(phttp.ServerConfig{})
	serverPlugin.Use(service.IdentityResolver())
	apiPlugin := api.New(serverPlugin, service.ClientContract())
	service.Register(apiPlugin)
	protoPlugin, bridge := service.ConnectPlugins(apiPlugin, serverPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("configure api: %v", err)
	}
	if err := protoPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("configure proto: %v", err)
	}
	if err := bridge.Configure(t.Context(), nil); err != nil {
		t.Fatalf("configure connect bridge: %v", err)
	}
	if err := bridge.Start(t.Context(), nil); err != nil {
		t.Fatalf("start connect bridge: %v", err)
	}
	wire := &connectWire{}
	handler := serverPlugin.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		wire.record(request)
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server.URL, wire
}

// wantQuote is the provider's quote, member for member: the widest unsigned
// 64-bit value, a negative 32-bit value, raw bytes, a list and a map.
var wantQuote = itemsclient.Quote{
	Id:          "1",
	Units:       18446744073709551615,
	Offset:      -7,
	Fingerprint: []byte{0x00, 0xff, 0x80, 0x22},
	Tags:        []string{"a", "b"},
	Labels:      map[string]string{"k": "v"},
}

// assertConnectCall checks the one request a generated call made: the declared
// method identity, the media type of the dispatched encoding, the protocol
// version, the caller's remaining budget and the declared credential.
//
// A unary call always states its remaining budget. A stream with no declared
// duration has no deadline to state, and Connect makes the header optional;
// when one is sent it must still be a positive budget.
func assertConnectCall(t *testing.T, requests []connectWireRequest, path, contentType string, unary bool) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("the provider saw %d requests, want exactly one: %+v", len(requests), requests)
	}
	got := requests[0]
	if got.Path != path || got.ContentType != contentType || got.ProtocolVersion != "1" || !got.KeyPresented {
		t.Fatalf("request = %+v, want %s as %s with Connect-Protocol-Version 1 and the declared key", got, path, contentType)
	}
	if !unary && got.TimeoutMs == "" {
		return
	}
	if budget, err := strconv.Atoi(got.TimeoutMs); err != nil || budget <= 0 {
		t.Fatalf("Connect-Timeout-Ms = %q, want the caller's remaining budget", got.TimeoutMs)
	}
}

// The Go→Go Connect unary cell. The provider declares Connect alone for this
// operation, JSON first; the consumer names neither, and the wire log shows the
// call took exactly that encoding and kept every value exact.
func TestReadQuoteTravelsOverTheFirstDeclaredConnectEncoding(t *testing.T) {
	baseURL, wire := realConnectProvider(t)
	generated := boundClient(t, sampleBinding(baseURL))

	quote, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
	if err != nil {
		t.Fatalf("GetQuotes: %v", err)
	}
	if !reflect.DeepEqual(*quote, wantQuote) {
		t.Fatalf("quote = %+v, want %+v", *quote, wantQuote)
	}
	assertConnectCall(t, wire.snapshot(), "/items.v1.ApiService/GetQuotes", "application/json", true)
}

// The Go→Go Connect server-stream cell: envelope frames in the provider's order,
// ended by one EndStreamResponse the runtime turns into a clean completion.
func TestQuoteTicksStreamOverConnect(t *testing.T) {
	baseURL, wire := realConnectProvider(t)
	generated := boundClient(t, sampleBinding(baseURL))

	stream, err := generated.GetQuotesTicks(t.Context(), itemsclient.GetQuotesTicksInput{Path: itemsclient.GetQuotesTicksPath{Id: "1"}})
	if err != nil {
		t.Fatalf("GetQuotesTicks: %v", err)
	}
	sequences := make([]uint64, 0, 3)
	for tick := range stream.Messages() {
		if tick.Id != "1" {
			t.Fatalf("tick = %+v, want quote 1", tick)
		}
		sequences = append(sequences, tick.Sequence)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream terminal: %v", err)
	}
	if !slices.Equal(sequences, []uint64{1, 2, 3}) {
		t.Fatalf("sequences = %v, want [1 2 3]", sequences)
	}
	assertConnectCall(t, wire.snapshot(), "/items.v1.ApiService/GetQuotesTicks", "application/connect+json", false)
}

// A declared error over Connect arrives as the generated type with its stable
// code and its HTTP status, on the unary call and on the stream alike.
func TestQuoteDeclaredErrorArrivesTypedOverConnect(t *testing.T) {
	baseURL, _ := realConnectProvider(t)
	generated := boundClient(t, sampleBinding(baseURL))

	_, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "absent"}})
	var typed *itemsclient.GetQuotesNotFoundError
	if !errors.As(err, &typed) {
		t.Fatalf("unary error = %T %v, want the generated not_found type", err, err)
	}
	if typed.Remote.Code() != "not_found" || typed.Remote.StatusCode != http.StatusNotFound {
		t.Fatalf("unary error = %s/%d, want not_found/404", typed.Remote.Code(), typed.Remote.StatusCode)
	}

	stream, err := generated.GetQuotesTicks(t.Context(), itemsclient.GetQuotesTicksInput{Path: itemsclient.GetQuotesTicksPath{Id: "absent"}})
	if err == nil {
		for range stream.Messages() {
			t.Fatal("a refused stream delivered a message")
		}
		err = stream.Err()
	}
	var typedStream *itemsclient.GetQuotesTicksNotFoundError
	if !errors.As(err, &typedStream) {
		t.Fatalf("stream error = %T %v, want the generated not_found type", err, err)
	}
	if typedStream.Remote.Code() != "not_found" || typedStream.Remote.StatusCode != http.StatusNotFound {
		t.Fatalf("stream error = %s/%d, want not_found/404", typedStream.Remote.Code(), typedStream.Remote.StatusCode)
	}
}

// A credential the provider refuses is refused on both Connect wires, and the
// stream states it before its first tick.
func TestQuoteRefusedCredentialReachesTheConsumerOnBothConnectWires(t *testing.T) {
	baseURL, _ := realConnectProvider(t)
	options := sampleBinding(baseURL)
	binding := options.Services["items"]
	binding.Credentials = map[string]client.CredentialBinding{
		"catalog-key": {Source: client.CredentialSourceStatic, Value: "not-the-catalog-key"},
	}
	options.Services["items"] = binding
	generated := boundClient(t, options)

	_, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unary refusal = %T %v", err, err)
	}

	stream, err := generated.GetQuotesTicks(t.Context(), itemsclient.GetQuotesTicksInput{
		Path: itemsclient.GetQuotesTicksPath{Id: "1"},
	})
	if err != nil {
		if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
			t.Fatalf("stream open refusal = %T %v", err, err)
		}
		return
	}
	defer func() { _ = stream.Close() }()
	for range stream.Messages() {
		t.Fatal("a refused stream delivered a tick")
	}
	if !errors.As(stream.Err(), &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stream terminal = %T %v", stream.Err(), stream.Err())
	}
}

// A Connect operation whose declared credential is not bound never reaches the
// provider: the call fails before dispatch, and the wire log stays empty.
func TestQuoteNeverTravelsAnonymouslyOverConnect(t *testing.T) {
	baseURL, wire := realConnectProvider(t)
	options := sampleBinding(baseURL)
	binding := options.Services["items"]
	binding.Credentials = map[string]client.CredentialBinding{"workload": binding.Credentials["workload"]}
	options.Services["items"] = binding
	generated := boundClient(t, options)

	if _, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}}); err == nil {
		t.Fatal("a call whose declared credential is unbound succeeded")
	}
	if requests := wire.snapshot(); len(requests) != 0 {
		t.Fatalf("the provider saw %d requests from an unbound credential: %+v", len(requests), requests)
	}
}
