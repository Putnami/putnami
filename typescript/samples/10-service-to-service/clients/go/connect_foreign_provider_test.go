package itemsclient_test

// TS→Go Connect cells of the cross-language interop matrix: the real TypeScript provider runs in
// its own process (the D0.5 harness of foreign_provider_test.go), and the Go
// client generated from its contract calls the two quotes operations the
// provider declares Connect-only. A recording reverse proxy in front of the
// provider shows the wire each generated call really took.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"

	itemsclient "go.putnami.dev/examples/ts-items-client"
)

// connectWireRequest is what one request carried to the provider. Credential
// values are never recorded, only whether one was presented.
type connectWireRequest struct {
	Path            string
	ContentType     string
	ProtocolVersion string
	TimeoutMs       string
	KeyPresented    bool
}

type connectWire struct {
	mu       sync.Mutex
	requests []connectWireRequest
}

func (wire *connectWire) reset() {
	wire.mu.Lock()
	defer wire.mu.Unlock()
	wire.requests = nil
}

func (wire *connectWire) snapshot() []connectWireRequest {
	wire.mu.Lock()
	defer wire.mu.Unlock()
	return slices.Clone(wire.requests)
}

// assertOneCall checks the single request a generated call made: the declared
// method identity, the media type of the dispatched encoding, the protocol
// version, the caller's remaining budget and the declared credential.
//
// A unary call always states its remaining budget. A stream with no declared
// duration has no deadline to state, and Connect makes the header optional;
// when one is sent it must still be a positive budget.
func (wire *connectWire) assertOneCall(t *testing.T, path, contentType string, unary bool) {
	t.Helper()
	requests := wire.snapshot()
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

// recordConnectWire puts a recording reverse proxy in front of the provider. It
// forwards every byte unchanged and flushes each streamed frame as it arrives.
func recordConnectWire(t *testing.T, providerURL string) (string, *connectWire) {
	t.Helper()
	target, err := url.Parse(providerURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	wire := &connectWire{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		wire.mu.Lock()
		wire.requests = append(wire.requests, connectWireRequest{
			Path:            request.URL.Path,
			ContentType:     request.Header.Get("Content-Type"),
			ProtocolVersion: request.Header.Get("Connect-Protocol-Version"),
			TimeoutMs:       request.Header.Get("Connect-Timeout-Ms"),
			KeyPresented:    request.Header.Get("X-Catalog-Key") == catalogAPIKey,
		})
		wire.mu.Unlock()
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server.URL, wire
}

// The TypeScript provider declares Connect alone for the quotes operations,
// protobuf before JSON. The consumer names neither: the committed Go client
// takes the first declared encoding.
func TestTypeScriptProviderAnswersTheGeneratedGoClientOverConnect(t *testing.T) {
	providerURL := startForeignProvider(t)
	wireURL, wire := recordConnectWire(t, providerURL)
	generated := boundClientWithKey(t, wireURL, catalogAPIKey)

	t.Run("a unary call travels as protobuf, the first declared encoding", func(t *testing.T) {
		wire.reset()
		quote, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
		if err != nil {
			t.Fatalf("GetQuotes: %v", err)
		}
		// 2^53 - 1 is the widest integer a TypeScript provider holds exactly; the
		// negative 32-bit value exercises the sign extension the wire requires.
		if quote.Id != "1" || quote.Units != 9007199254740991 || quote.Offset != -7 || !slices.Equal(quote.Tags, []string{"a", "b"}) {
			t.Fatalf("quote = %+v", *quote)
		}
		wire.assertOneCall(t, "/catalog.items.v1.QuotesService/GetQuotesById", "application/proto", true)
	})

	t.Run("a server stream travels as protobuf envelopes ended by one terminal", func(t *testing.T) {
		wire.reset()
		stream, err := generated.GetQuotesTicks(t.Context(), itemsclient.GetQuotesTicksInput{Path: itemsclient.GetQuotesTicksPath{Id: "1"}})
		if err != nil {
			t.Fatalf("GetQuotesTicks: %v", err)
		}
		sequences := make([]int64, 0, 3)
		for tick := range stream.Messages() {
			if tick.Id != "1" {
				t.Fatalf("tick = %+v, want quote 1", tick)
			}
			sequences = append(sequences, tick.Sequence)
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("stream terminal: %v", err)
		}
		if !slices.Equal(sequences, []int64{1, 2, 3}) {
			t.Fatalf("sequences = %v, want [1 2 3]", sequences)
		}
		wire.assertOneCall(t, "/catalog.items.v1.QuotesService/ListQuotesByIdTicks", "application/connect+proto", false)
	})

	t.Run("the declared error arrives typed with its declared details", func(t *testing.T) {
		_, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "absent"}})
		var typed *itemsclient.GetQuotesNotFoundError
		if !errors.As(err, &typed) {
			t.Fatalf("error = %T %v, want the generated not_found type", err, err)
		}
		if typed.Remote.Code() != "not_found" || typed.Remote.StatusCode != http.StatusNotFound {
			t.Fatalf("error = %s/%d, want not_found/404", typed.Remote.Code(), typed.Remote.StatusCode)
		}
		want := itemsclient.GetQuotesNotFoundErrorPayload{Id: "absent", Resource: "quote"}
		if typed.Payload == nil || *typed.Payload != want {
			t.Fatalf("details = %+v, want %+v", typed.Payload, want)
		}
	})
}
