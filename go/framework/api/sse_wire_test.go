package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// sseWireFixture is protocols/clientcontract/fixtures/sse/wire.json.
type sseWireFixture struct {
	Header        string `json:"header"`
	Token         string `json:"token"`
	CompleteFrame string `json:"completeFrame"`
	Negotiation   []struct {
		Lines      []string `json:"lines"`
		Negotiates bool     `json:"negotiates"`
	} `json:"negotiation"`
	Provider []struct {
		Name                      string   `json:"name"`
		RouteDeclaresContinuation bool     `json:"routeDeclaresContinuation"`
		Request                   []string `json:"request"`
		Admission                 string   `json:"admission"`
		Handler                   string   `json:"handler"`
		Expect                    struct {
			Acknowledges bool   `json:"acknowledges"`
			Terminal     string `json:"terminal"`
		} `json:"expect"`
	} `json:"provider"`
}

func loadSSEWireFixture(t *testing.T) sseWireFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../protocols/clientcontract/fixtures/sse/wire.json")
	if err != nil {
		t.Fatalf("read wire fixture: %v", err)
	}
	var fixture sseWireFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode wire fixture: %v", err)
	}
	return fixture
}

type sseLogFrame struct {
	Cursor string `json:"cursor"`
	Line   string `json:"line"`
}

type sseLogQuery struct {
	Run    string `json:"run" validate:"required"`
	Cursor string `json:"cursor"`
}

type sseCancelKey struct{}

const (
	sseFirstFrame  = "data: {\"cursor\":\"c1\",\"line\":\"one\"}\n\n"
	sseSecondFrame = "data: {\"cursor\":\"c2\",\"line\":\"two\"}\n\n"
	sseErrorFrame  = "event: error\ndata: {\"status\":404,\"code\":\"not_found\",\"error\":\"Not Found\",\"message\":\"run gone\"}\n\n"
)

// newSSEWireProvider binds one server stream per handler outcome, with and
// without a declared continuation, on a real api plugin and HTTP server.
func newSSEWireProvider(t *testing.T) *phttp.ServerPlugin {
	t.Helper()
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	plugin := New(httpServer, WithClientService(ClientServiceOptions{
		Service: clientcontract.Service{ID: "logs", Audience: "https://logs.internal"},
	}))
	handlers := map[string]func(*ServerStreamContext[sseLogFrame]) error{
		"returns": func(c *ServerStreamContext[sseLogFrame]) error {
			if err := c.Send(sseLogFrame{Cursor: "c1", Line: "one"}); err != nil {
				return err
			}
			return c.Send(sseLogFrame{Cursor: "c2", Line: "two"})
		},
		"fails": func(c *ServerStreamContext[sseLogFrame]) error {
			_ = c.Send(sseLogFrame{Cursor: "c1", Line: "one"})
			return perrors.NotFound("run gone")
		},
		"drains": func(c *ServerStreamContext[sseLogFrame]) error {
			if err := c.Send(sseLogFrame{Cursor: "c1", Line: "one"}); err != nil {
				return err
			}
			<-c.Context.Context().Done()
			return nil
		},
		"canceled": func(c *ServerStreamContext[sseLogFrame]) error {
			_ = c.Send(sseLogFrame{Cursor: "c1", Line: "one"})
			c.Context.Context().Value(sseCancelKey{}).(context.CancelFunc)()
			<-c.Context.Context().Done()
			return c.Context.Context().Err()
		},
	}
	for outcome, handle := range handlers {
		for _, declared := range []bool{true, false} {
			options := ClientOperationOptions{}
			path := "/plain/" + outcome
			if declared {
				options.SSEContinuation = SSECursorContinuation("cursor", "cursor")
				path = "/continued/" + outcome
			}
			plugin.Register(Endpoint("GET", path).
				Query(reflect.TypeFor[sseLogQuery]()).
				Returns(StreamOf[sseLogFrame]()).
				Client(options).
				Handle(ServerStream(handle)))
		}
	}
	if err := plugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	return httpServer
}

// The provider rows of the shared wire corpus, replayed through a real api
// plugin: what a provider writes for each request, byte for byte.
func TestSSEWire_ProviderReplaysTheSharedWireCorpus(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "negotiated-sse-wire",
		"a-negotiated-sse-stream-is-acknowledged-and-ends-with-complete")
	spectest.Proves(t, "go/api-contracts", "negotiated-sse-wire",
		"an-sse-request-that-does-not-negotiate-keeps-the-legacy-framing")
	spectest.Proves(t, "go/api-contracts", "negotiated-sse-wire",
		"a-failed-or-canceled-negotiated-stream-never-ends-with-complete")

	fixture := loadSSEWireFixture(t)
	if fixture.Header != clientcontract.SSEWireHeader || fixture.Token != clientcontract.SSEWireV1 ||
		fixture.CompleteFrame != clientcontract.SSECompleteFrame {
		t.Fatalf("the corpus names %q/%q/%q; the contract names %q/%q/%q", fixture.Header, fixture.Token,
			fixture.CompleteFrame, clientcontract.SSEWireHeader, clientcontract.SSEWireV1, clientcontract.SSECompleteFrame)
	}
	server := newSSEWireProvider(t)
	for _, row := range fixture.Provider {
		t.Run(row.Name, func(t *testing.T) {
			path := "/plain/"
			if row.RouteDeclaresContinuation {
				path = "/continued/"
			}
			handler := row.Handler
			if handler == "never-runs" {
				handler = "returns"
			}
			target := path + handler + "?run=r1"
			if row.Admission == "refused" {
				target = path + handler // the required selector is missing
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, sseCancelKey{}, cancel)
			req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
			req.Header.Set("Accept", "text/event-stream")
			if len(row.Request) > 0 {
				req.Header[http.CanonicalHeaderKey(clientcontract.SSEWireHeader)] = row.Request
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			acknowledged := clientcontract.NegotiatesSSEWire(rec.Header().Values(clientcontract.SSEWireHeader))
			if acknowledged != row.Expect.Acknowledges {
				t.Fatalf("acknowledged = %v, want %v (header %q)", acknowledged, row.Expect.Acknowledges,
					rec.Header().Values(clientcontract.SSEWireHeader))
			}
			if row.Admission == "refused" {
				if rec.Code < 400 || rec.Code >= 500 {
					t.Fatalf("status = %d, want a 4xx refusal", rec.Code)
				}
				return
			}
			var want string
			switch handler {
			case "returns":
				want = sseFirstFrame + sseSecondFrame
			case "fails":
				want = sseFirstFrame + sseErrorFrame
			case "canceled":
				want = sseFirstFrame
			}
			if row.Expect.Terminal == "complete" {
				want += clientcontract.SSECompleteFrame
			}
			if got := rec.Body.String(); got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
}

// Every negotiation row of the corpus reaches the provider as field lines and
// is acknowledged exactly when the contract's reading negotiates it.
func TestSSEWire_ProviderReadsTheNegotiationLinesOfTheCorpus(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "negotiated-sse-wire",
		"an-sse-request-that-does-not-negotiate-keeps-the-legacy-framing")

	fixture := loadSSEWireFixture(t)
	server := newSSEWireProvider(t)
	for _, row := range fixture.Negotiation {
		req := httptest.NewRequest("GET", "/continued/returns?run=r1", nil)
		req.Header.Set("Accept", "text/event-stream")
		if row.Lines != nil {
			req.Header[http.CanonicalHeaderKey(clientcontract.SSEWireHeader)] = row.Lines
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		acknowledged := rec.Header().Get(clientcontract.SSEWireHeader) == clientcontract.SSEWireV1
		ended := strings.HasSuffix(rec.Body.String(), clientcontract.SSECompleteFrame)
		if acknowledged != row.Negotiates || ended != row.Negotiates {
			t.Fatalf("lines %q: acknowledged = %v, complete = %v, want %v", row.Lines, acknowledged, ended, row.Negotiates)
		}
	}
}

// The acknowledgment and the terminal cross a real connection: the head
// carries the marker before the first body byte, and an old consumer's
// request on the same route reads the legacy bytes.
func TestSSEWire_ANegotiatedStreamCrossesARealConnection(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "negotiated-sse-wire",
		"a-negotiated-sse-stream-is-acknowledged-and-ends-with-complete")

	server := httptest.NewServer(newSSEWireProvider(t).Handler())
	defer server.Close()
	for _, negotiate := range []bool{true, false} {
		req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/continued/returns?run=r1", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "text/event-stream")
		if negotiate {
			req.Header.Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		want, ack := sseFirstFrame+sseSecondFrame, ""
		if negotiate {
			want, ack = want+clientcontract.SSECompleteFrame, clientcontract.SSEWireV1
		}
		if got := resp.Header.Get(clientcontract.SSEWireHeader); got != ack {
			t.Fatalf("negotiate=%v: acknowledgment = %q, want %q", negotiate, got, ack)
		}
		if string(body) != want {
			t.Fatalf("negotiate=%v: body = %q, want %q", negotiate, body, want)
		}
	}
}

func TestSSEContinuation_DeclarationsAreCopiedAndStandAloneFromExternal(t *testing.T) {
	options := ClientOperationOptions{SSEContinuation: SSECursorContinuation("cursor", "after")}
	copied := cloneClientOperationOptions(&options)
	options.SSEContinuation.Cursor.QueryParameter = "mutated"
	if copied.SSEContinuation.Cursor.QueryParameter != "after" {
		t.Fatal("a declared continuation was shared with the caller's options")
	}
	if best := SSEBestEffortContinuation(); best.Mode != clientcontract.SSEContinuationBestEffort || best.Cursor != nil {
		t.Fatalf("best-effort continuation = %+v", best)
	}
	external := ClientOperationOptions{External: "npm registry API", SSEContinuation: SSEBestEffortContinuation()}
	if _, err := external.ExternalAuthority(); err == nil || !strings.Contains(err.Error(), "SSEContinuation") {
		t.Fatalf("External beside SSEContinuation = %v, want a refusal naming it", err)
	}
	defer func() {
		if recovered := recover(); recovered == nil || !strings.Contains(recovered.(string), "carries no SSE") {
			t.Fatalf("a provider-owned wire with an sse continuation = %v, want a refusal", recovered)
		}
	}()
	Endpoint("GET", "/wire").
		Subprotocol("vendor.v1").
		Body(StreamOf[sseLogFrame]()).
		Returns(StreamOf[sseLogFrame]()).
		Client(ClientOperationOptions{SSEContinuation: SSEBestEffortContinuation()}).
		Handle(BidiStream(func(*BidiStreamContext[sseLogFrame, sseLogFrame]) error { return nil }))
}

// A provider that drains (a graceful stop, a scale to zero, an instance
// replacement) ends a negotiated stream with no terminal over a real
// connection: the consumer reads an interruption and continues elsewhere.
func TestSSEWire_ADrainingProviderEndsANegotiatedStreamWithoutATerminal(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "negotiated-sse-wire",
		"a-draining-provider-ends-a-negotiated-stream-without-a-terminal")

	httpServer := newSSEWireProvider(t)
	server := httptest.NewServer(httpServer.Handler())
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/continued/drains?run=r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get(clientcontract.SSEWireHeader); got != clientcontract.SSEWireV1 {
		t.Fatalf("acknowledgment = %q", got)
	}
	first := make([]byte, len(sseFirstFrame))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != sseFirstFrame {
		t.Fatalf("first frame = %q, %v", first, err)
	}
	if err := httpServer.Stop(t.Context(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the drained stream did not end cleanly: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("a drained stream wrote %q after its last message, want no terminal", rest)
	}
}
