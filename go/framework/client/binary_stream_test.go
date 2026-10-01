package client

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func binaryStreamOperation() Operation {
	operation := binaryOperation(clientcontract.TransportRESTJSON)
	operation.Contract.Errors = []clientcontract.DeclaredError{}
	operation.Successes[0].Content[0].MediaType = "*/*"
	operation.Successes[0].Content[0].MaxBytes = 4096
	operation.Successes[0].Content[0].Streamed = true
	return operation
}

func binaryStreamUploadOperation() Operation {
	operation := binaryStreamOperation()
	operation.Request = &OperationRequest{Required: true, Content: operation.Successes[0].Content}
	operation.Successes = []OperationSuccess{{Status: http.StatusNoContent}}
	return operation
}

func TestBinaryStreamUploadSendsBeforeTheSourceEnds(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binary-payloads", "unframed-binary-streams-preserve-bytes-media-type-and-lifetime")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	firstRead := make(chan []byte, 1)
	allRead := make(chan []byte, 1)
	payload := []byte{0, 0xff, 0x80, '\n', '\r', 0}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("content type = %q", got)
		}
		first := make([]byte, 2)
		_, err := io.ReadFull(request.Body, first)
		if err != nil {
			t.Errorf("initial request read: %v", err)
		}
		firstRead <- first
		rest, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("request read: %v", err)
		}
		allRead <- append(first, rest...)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	source, input := io.Pipe()
	defer source.Close()
	defer input.Close()
	request := &Request{Method: http.MethodPut, Path: "/blob", BodyStream: source}
	request.SetHeader("Content-Type", "application/json; charset=utf-8")
	result := make(chan error, 1)
	go func() {
		result <- CallOperationVoid(ctx, client, &OperationCall{Request: request}, binaryStreamUploadOperation())
	}()
	if _, err := input.Write(payload[:2]); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-firstRead:
		if !bytes.Equal(first, payload[:2]) {
			t.Fatalf("initial bytes = %x", first)
		}
	case <-ctx.Done():
		t.Fatal("request was buffered before dispatch")
	}
	if _, err := input.Write(payload[2:]); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := <-allRead; !bytes.Equal(got, payload) {
		t.Fatalf("request bytes = %x, want %x", got, payload)
	}
}

func TestBinaryStreamDownloadReturnsBeforeTheBodyEnds(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binary-payloads", "unframed-binary-streams-preserve-bytes-media-type-and-lifetime")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	payload := []byte{0xff, 0, 0x80, '\n', '{', '}'}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = writer.Write(payload[:2])
		writer.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = writer.Write(payload[2:])
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	operation := binaryStreamOperation()
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{MaxResponseBytes: int64Pointer(1)}
	response, err := CallOperationBinaryStream(ctx, client, &OperationCall{Request: &Request{Path: "/blob"}}, operation)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Status != 200 || response.ContentType != "application/json; charset=utf-8" {
		t.Fatalf("response metadata = %#v", response)
	}
	first := make([]byte, 2)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatalf("response context ended at header return: %v", err)
	}
	if !bytes.Equal(first, payload[:2]) {
		t.Fatalf("initial response = %x", first)
	}
	release <- struct{}{}
	rest, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(append(first, rest...), payload) {
		t.Fatalf("response bytes = %x, error = %v", rest, err)
	}
	client.service.registry.mu.Lock()
	tracked := len(client.service.registry.binaryStreams)
	client.service.registry.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("EOF kept %d streams registered", tracked)
	}
}

func TestBinaryStreamBoundFailureFinishesAttemptTelemetryAtBodyFailure(t *testing.T) {
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write([]byte("12345"))
	}))
	defer server.Close()
	client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	operation := binaryStreamOperation()
	operation.Successes[0].Content[0].MaxBytes = 4
	response, err := CallOperationBinaryStream(t.Context(), client, &OperationCall{Request: &Request{Path: "/blob"}}, operation)
	if err != nil {
		t.Fatal(err)
	}
	observer.mu.Lock()
	if len(observer.attempts) != 0 {
		observer.mu.Unlock()
		t.Fatal("attempt telemetry finished at response headers")
	}
	observer.mu.Unlock()
	if _, err := io.ReadAll(response.Body); !perrors.Is(err, CodeClientResponse) {
		t.Fatalf("body failure = %v", err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.attempts) != 1 || observer.attempts[0].StatusCode != http.StatusOK || observer.attempts[0].Code != string(CodeClientResponse) {
		t.Fatalf("attempt results = %#v", observer.attempts)
	}
}

func TestBinaryStreamUploadEnforcesItsDeclaredBoundWithoutBuffering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	operation := binaryStreamUploadOperation()
	operation.Request.Content[0].MaxBytes = 4
	request := &Request{Method: http.MethodPut, Path: "/blob", BodyStream: strings.NewReader("12345")}
	request.SetHeader("Content-Type", "application/octet-stream")
	if err := CallOperationVoid(t.Context(), client, &OperationCall{Request: request}, operation); !perrors.Is(err, CodeClientRequest) {
		t.Fatalf("oversized upload = %v", err)
	}
}

func TestUnboundBinaryStreamCallsKeepTheirDeclaredBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPut {
			_, _ = io.Copy(io.Discard, request.Body)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write([]byte("12345"))
	}))
	defer server.Close()
	client, err := NewBuilder().BaseURL(server.URL).Build()
	if err != nil {
		t.Fatal(err)
	}
	upload := binaryStreamUploadOperation()
	upload.Request.Content[0].MaxBytes = 4
	request := &Request{Method: http.MethodPut, Path: "/blob", BodyStream: strings.NewReader("12345")}
	request.SetHeader("Content-Type", "application/octet-stream")
	if err := CallOperationVoid(t.Context(), client, &OperationCall{Request: request}, upload); !perrors.Is(err, CodeClientRequest) {
		t.Errorf("unbound upload exceeded its bound: %v", err)
	}
	download := binaryStreamOperation()
	download.Successes[0].Content[0].MaxBytes = 4
	response, err := CallOperationBinaryStream(t.Context(), client, &OperationCall{Request: &Request{Path: "/blob"}}, download)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); !perrors.Is(err, CodeClientResponse) {
		t.Errorf("unbound download exceeded its bound: %v", err)
	}
}

func TestBinaryStreamReaderAcceptsTheLargestPositiveBound(t *testing.T) {
	reader := &boundedWireReader{Reader: strings.NewReader("payload"), remaining: math.MaxInt64}
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "payload" {
		t.Fatalf("largest bound: %q, %v", got, err)
	}
}

func TestBufferedBinaryResponseStillValidatesItsStreamedRequestContract(t *testing.T) {
	client, err := NewBuilder().BaseURL("http://localhost").Transport(binaryTransportFunc(func(context.Context, *Request) (*Response, error) {
		t.Fatal("an invalid streamed request reached the transport")
		return nil, nil
	})).Build()
	if err != nil {
		t.Fatal(err)
	}
	operation := binaryStreamUploadOperation()
	operation.Request.Content[0].MaxBytes = 0
	operation.Successes = binaryOperation(clientcontract.TransportRESTJSON).Successes
	request := &Request{Method: http.MethodPut, Path: "/blob", BodyStream: strings.NewReader("payload")}
	request.SetHeader("Content-Type", "image/png")
	_, err = CallOperationBinary(t.Context(), client, &OperationCall{Request: request}, operation)
	if !perrors.Is(err, CodeClientConfig) {
		t.Fatalf("invalid streamed request contract = %v", err)
	}
}

func TestBinaryStreamCancellationFinishesAttemptAsCanceled(t *testing.T) {
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()
	client := boundTestClient(t, "http://localhost", testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	client.transport = binaryTransportFunc(func(context.Context, *Request) (*Response, error) {
		return &Response{StatusCode: 200, Headers: http.Header{"Content-Type": {"image/png"}}, BodyStream: io.NopCloser(strings.NewReader("unread"))}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response, err := CallOperationBinaryStream(ctx, client, &OperationCall{Request: &Request{Path: "/blob"}}, binaryStreamOperation())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Body.Close()
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.attempts) != 1 || observer.attempts[0].Code != string(CodeClientCanceled) {
		t.Fatalf("canceled stream attempts = %#v", observer.attempts)
	}
}

func TestBinaryStreamUploadIsNeverReplayed(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls, refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Location", "/redirected")
				writer.WriteHeader(status)
				_, _ = writer.Write([]byte(`{"code":"refused"}`))
			}))
			defer server.Close()
			descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
				"user": {Kind: clientcontract.CredentialForwardedUserToken},
			})
			client := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
				"user": {Source: CredentialSourceForwardedUser, Refresh: func(context.Context) (string, error) {
					refreshes.Add(1)
					return "fresh", nil
				}},
			}})
			operation := binaryStreamUploadOperation()
			operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "user"}}}}}
			request := &Request{Method: http.MethodPut, Path: "/blob", BodyStream: strings.NewReader("consumed")}
			request.SetHeader("Content-Type", "image/png")
			err := CallOperationVoid(WithForwardedUserToken(t.Context(), "original"), client, &OperationCall{Request: request}, operation)
			var remote *RemoteError
			if !stderrors.As(err, &remote) || remote.StatusCode != status || calls.Load() != 1 || refreshes.Load() != 0 {
				t.Fatalf("error=%v calls=%d refreshes=%d", err, calls.Load(), refreshes.Load())
			}
		})
	}
}

type observedBinaryBody struct {
	io.Reader
	closed atomic.Int32
}

func (body *observedBinaryBody) Close() error {
	body.closed.Add(1)
	return nil
}

type binaryTransportFunc func(context.Context, *Request) (*Response, error)

func (transport binaryTransportFunc) Do(ctx context.Context, request *Request) (*Response, error) {
	return transport(ctx, request)
}

func TestBinaryStreamRejectsHeadersStatusAndCacheBeforeExposure(t *testing.T) {
	for _, test := range []struct {
		name, mediaType string
		status          int
	}{
		{"missing", "", 200},
		{"wildcard", "*/*", 200},
		{"wildcard subtype", "image/*", 200},
		{"no subtype", "text", 200},
		{"broken parameter", "text/plain; charset", 200},
		{"line break", "text/plain\r\n", 200},
		{"undeclared status", "image/png", 201},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &observedBinaryBody{Reader: bytes.NewReader([]byte{0xff})}
			client, err := NewBuilder().BaseURL("http://localhost").Transport(binaryTransportFunc(func(context.Context, *Request) (*Response, error) {
				return &Response{StatusCode: test.status, Headers: http.Header{"Content-Type": {test.mediaType}}, BodyStream: body}, nil
			})).Build()
			if err != nil {
				t.Fatal(err)
			}
			_, err = CallOperationBinaryStream(t.Context(), client, &OperationCall{Request: &Request{Path: "/blob"}}, binaryStreamOperation())
			if !perrors.Is(err, CodeClientResponse) || body.closed.Load() != 1 {
				t.Fatalf("error=%v closes=%d", err, body.closed.Load())
			}
		})
	}
	operation := binaryStreamOperation()
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{Cache: &clientcontract.CachePolicy{}}
	client, err := NewBuilder().BaseURL("http://localhost").Transport(binaryTransportFunc(func(context.Context, *Request) (*Response, error) {
		t.Fatal("cached stream reached the transport")
		return nil, nil
	})).Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = CallOperationBinaryStream(WithoutResponseCache(t.Context()), client, &OperationCall{Request: &Request{Path: "/blob"}}, operation)
	if !perrors.Is(err, CodeClientConfig) {
		t.Fatalf("cached stream = %v", err)
	}
}

func TestBinaryStreamLifetimeEndsOnCloseCancelDeadlineAndRegistryStop(t *testing.T) {
	for _, end := range []string{"close", "cancel", "deadline", "registry"} {
		t.Run(end, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := &observedBinaryBody{Reader: strings.NewReader("payload")}
			client := boundTestClient(t, "http://localhost", testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
			var attemptCtx context.Context
			client.transport = binaryTransportFunc(func(ctx context.Context, request *Request) (*Response, error) {
				attemptCtx = ctx
				return &Response{StatusCode: 200, Headers: http.Header{"Content-Type": {"image/png"}}, BodyStream: body}, nil
			})
			operation := binaryStreamOperation()
			if end == "deadline" {
				operation.Contract.Resilience = &clientcontract.ResiliencePolicy{TimeoutMs: intPointer(100), AttemptTimeoutMs: intPointer(50)}
			}
			response, err := CallOperationBinaryStream(ctx, client, &OperationCall{Request: &Request{Path: "/blob"}}, operation)
			if err != nil {
				t.Fatal(err)
			}
			if attemptCtx.Err() != nil || body.closed.Load() != 0 {
				t.Fatal("stream closed before ownership transfer")
			}
			switch end {
			case "close":
				_ = response.Body.Close()
			case "cancel":
				cancel()
			case "registry":
				if err := client.service.registry.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-attemptCtx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("stream deadline resources were retained")
			}
			// Close joins any concurrent cancellation callback and is idempotent.
			_ = response.Body.Close()
			_ = response.Body.Close()
			if body.closed.Load() != 1 {
				t.Fatalf("body closed %d times", body.closed.Load())
			}
		})
	}
}

func TestBinaryStreamRequestFailsClosedWithoutReading(t *testing.T) {
	for _, contentType := range []string{"", "*/*", "text", "text/plain; charset", "image/*", "text/plain\r\n"} {
		source := &slowReader{remaining: 100}
		request := &Request{BodyStream: source, Headers: http.Header{"Content-Type": {contentType}}}
		if err := validateGeneratedRequest(request, binaryStreamUploadOperation().Request, nil); !perrors.Is(err, CodeClientRequest) || source.given != 0 {
			t.Fatalf("media=%q error=%v bytes=%d", contentType, err, source.given)
		}
	}
	source := &slowReader{remaining: 100}
	client, err := NewBuilder().BaseURL("http://localhost").Transport(binaryTransportFunc(func(context.Context, *Request) (*Response, error) {
		t.Fatal("unauthenticated stream reached the transport")
		return nil, nil
	})).Build()
	if err != nil {
		t.Fatal(err)
	}
	operation := binaryStreamUploadOperation()
	operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "required"}}}}}
	request := &Request{BodyStream: source, Headers: http.Header{"Content-Type": {"application/json"}}}
	err = CallOperationVoid(t.Context(), client, &OperationCall{Request: request}, operation)
	if !perrors.Is(err, CodeClientCredential) || source.given != 0 {
		t.Fatalf("unauthenticated upload error=%v bytes=%d", err, source.given)
	}
}

func TestBinaryStreamErrorsRemainBoundedAndTyped(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "typed refusal", true: "bounded refusal"}[oversized], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusForbidden)
				if oversized {
					_, _ = writer.Write(bytes.Repeat([]byte("x"), 100))
					return
				}
				_, _ = writer.Write([]byte(`{"code":"denied"}`))
			}))
			defer server.Close()
			client := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
			operation := binaryStreamOperation()
			operation.Contract.Resilience = &clientcontract.ResiliencePolicy{MaxResponseBytes: int64Pointer(32)}
			operation.Contract.Errors = []clientcontract.DeclaredError{{Status: http.StatusForbidden, Code: "denied"}}
			response, err := CallOperationBinaryStream(t.Context(), client, &OperationCall{Request: &Request{Path: "/blob"}}, operation)
			if response != nil {
				t.Fatal("error body was exposed as a stream")
			}
			if oversized {
				if !perrors.Is(err, CodeClientResponse) {
					t.Fatalf("unbounded error = %v", err)
				}
				return
			}
			var remote *RemoteError
			if !stderrors.As(err, &remote) || remote.RemoteCode != "denied" || remote.StatusCode != http.StatusForbidden {
				t.Fatalf("typed refusal = %v", err)
			}
		})
	}
}

func TestBinaryStreamLowLevelClientKeepsTotalDeadlineAndNeverReplaysUploads(t *testing.T) {
	body := &observedBinaryBody{Reader: strings.NewReader("body")}
	var attempt context.Context
	var calls atomic.Int32
	client, err := NewBuilder().BaseURL("http://localhost").TotalTimeout(time.Second).
		Transport(binaryTransportFunc(func(ctx context.Context, request *Request) (*Response, error) {
			attempt = ctx
			calls.Add(1)
			if request.BodyStream != nil {
				return &Response{StatusCode: http.StatusServiceUnavailable}, nil
			}
			return &Response{StatusCode: 200, Headers: http.Header{"Content-Type": {"image/png"}}, BodyStream: body}, nil
		})).Build()
	if err != nil {
		t.Fatal(err)
	}
	response, err := CallOperationBinaryStream(t.Context(), client, &OperationCall{Request: &Request{Path: "/blob"}}, binaryStreamOperation())
	if err != nil || attempt.Err() != nil {
		t.Fatalf("stream returned with canceled context: %v / %v", err, attempt.Err())
	}
	_ = response.Body.Close()
	if attempt.Err() == nil || body.closed.Load() != 1 {
		t.Fatal("closing the body did not release its context")
	}
	request := &Request{BodyStream: strings.NewReader("source"), Headers: http.Header{"Content-Type": {"image/png"}}}
	err = CallOperationVoid(t.Context(), client, &OperationCall{Request: request}, binaryStreamUploadOperation())
	if err == nil || calls.Load() != 2 {
		t.Fatalf("upload error=%v total attempts=%d", err, calls.Load())
	}
}

func TestBinaryStreamRejectsMalformedDeclarations(t *testing.T) {
	for _, malformed := range []string{"unbounded", "media", "schema", "mixed", "framed", "connect"} {
		t.Run(malformed, func(t *testing.T) {
			operation := binaryStreamOperation()
			content := &operation.Successes[0].Content[0]
			switch malformed {
			case "unbounded":
				content.MaxBytes = 0
			case "media":
				content.MediaType = "image/png"
			case "schema":
				content.Schema = nil
			case "mixed":
				operation.Successes[0].Content = append(operation.Successes[0].Content, OperationContent{MediaType: "application/json"})
			case "framed":
				operation.Contract.Stream = clientcontract.StreamServer
			case "connect":
				operation.Contract.Transports[0].Protocol = clientcontract.TransportConnect
			}
			if err := validateBinaryStreamOperation(operation); !perrors.Is(err, CodeClientConfig) {
				t.Fatalf("malformed declaration accepted: %v", err)
			}
		})
	}
}

func TestBinaryStreamUploadDoesNotFollowAnHTTPRedirect(t *testing.T) {
	for _, payload := range []string{"", "source"} {
		t.Run(payload, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Location", "/redirected")
				writer.WriteHeader(http.StatusSeeOther)
			}))
			defer server.Close()
			client, err := NewBuilder().BaseURL(server.URL).Build()
			if err != nil {
				t.Fatal(err)
			}
			request := &Request{Method: http.MethodPut, Path: "/blob", BodyStream: strings.NewReader(payload)}
			request.SetHeader("Content-Type", "image/png")
			err = CallOperationVoid(t.Context(), client, &OperationCall{Request: request}, binaryStreamUploadOperation())
			var remote *RemoteError
			if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusSeeOther || calls.Load() != 1 {
				t.Fatalf("redirected upload error=%v calls=%d", err, calls.Load())
			}
		})
	}
}
