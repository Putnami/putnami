package openapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/api"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/security"
)

// The raw octet cell, end to end and in one process: a real first-party
// provider declares a bounded binary body and a bounded binary response, the
// published document is read back by the strict reader, and the real client
// runtime calls it over a real socket. Nothing here is scripted — the bytes
// that arrive are the bytes the provider wrote.

// blobBound is small on purpose: the over-bound cases must be provable without
// moving megabytes through a loopback socket.
const blobBound int64 = 32

// nonUTF8Blob is not valid UTF-8 and is not valid JSON. A pipeline that
// re-encoded it as a JSON string, or decoded it as text, would corrupt it.
var nonUTF8Blob = []byte{0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a}

type blobStored struct {
	Size int64 `json:"size" validate:"required"`
}

type blobParams struct {
	ID string `json:"id" validate:"required"`
}

// binaryProvider stands up the real provider and returns a bound client plus
// the generated operation metadata the strict reader produced for it.
func binaryProvider(t *testing.T) (*client.Client, map[string]client.Operation, *blobState) {
	t.Helper()
	state := &blobState{blobs: map[string][]byte{}}
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	httpServer.Use(security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		if ctx.Header("X-Blob-Key") != "sample-blob-key" {
			return nil
		}
		return &phttp.Claims{Subject: "blob-consumer", ClientID: ctx.Header("X-Client-Id")}
	}))
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service:     clientcontract.Service{ID: "blobs", Audience: "https://blobs.internal"},
		Credentials: map[string]clientcontract.CredentialProfile{"blob-key": {Kind: clientcontract.CredentialAPIKey, Header: "X-Blob-Key"}},
	}))

	apiPlugin.Register(api.Endpoint("PUT", "/blobs/{id}").
		Description("Store one blob verbatim").
		Params(api.Type[blobParams]()).
		Body(api.Binary("application/octet-stream", blobBound)).
		Returns(api.Type[blobStored]()).
		Handle(state.store))

	apiPlugin.Register(api.Endpoint("GET", "/blobs/{id}").
		Description("Read one blob verbatim").
		Params(api.Type[blobParams]()).
		Returns(api.Binary("application/octet-stream", blobBound)).
		Secure(security.Options{Client: []string{"blob.consumer"}}).
		Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
			AllOf: []clientcontract.SecurityRequirement{{Profile: "blob-key"}},
		}}}}).
		MayThrow(perrors.CodeNotFound).
		Handle(state.read))

	openapiPlugin := NewPlugin(PluginOptions{Title: "Blobs", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("publish provider document: %v", err)
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("strict reader rejected the provider document: %v", err)
	}
	operations := generatedOperations(ir)

	server := httptest.NewServer(httpServer.Handler())
	t.Cleanup(server.Close)
	state.baseURL = server.URL
	bound, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL: server.URL, ClientID: "blob.consumer", AllowInsecure: true,
		Credentials: map[string]client.CredentialBinding{
			"blob-key": {Source: client.CredentialSourceStatic, Value: "sample-blob-key"},
		},
	}, client.ServiceDescriptor{Contract: *ir.Contract, Schemas: ir.Schemas})
	if err != nil {
		t.Fatalf("bind the generated client: %v", err)
	}
	return bound, operations, state
}

// generatedOperations projects the reader's IR into exactly the metadata a
// generated method embeds — contract, successes and request — so the runtime
// under test is the one a generated client drives.
func generatedOperations(ir api.SpecIR) map[string]client.Operation {
	operations := map[string]client.Operation{}
	for _, service := range ir.Services {
		for _, method := range service.Methods {
			if method.Client == nil {
				continue
			}
			operation := client.Operation{ID: method.OperationID, Contract: *method.Client}
			for _, success := range method.Successes {
				entry := client.OperationSuccess{Status: success.Status}
				for _, content := range success.Content {
					entry.Content = append(entry.Content, client.OperationContent{
						MediaType: content.MediaType, Schema: content.Schema, MaxBytes: content.MaxBytes,
					})
				}
				operation.Successes = append(operation.Successes, entry)
			}
			if method.Request != nil {
				request := client.OperationRequest{Required: method.Request.Required}
				for _, content := range method.Request.Content {
					request.Content = append(request.Content, client.OperationContent{
						MediaType: content.MediaType, Schema: content.Schema, MaxBytes: content.MaxBytes,
					})
				}
				operation.Request = &request
			}
			operations[method.OperationID] = operation
		}
	}
	return operations
}

type blobState struct {
	blobs   map[string][]byte
	baseURL string
	// oversize makes the provider answer more octets than it declared, so the
	// client's own bound can be proven rather than assumed.
	oversize bool
	// stall blocks the read handler until the test releases it, so a caller
	// canceling mid-read is observable.
	stall chan struct{}
}

func (s *blobState) store(ctx *phttp.EndpointContext) *phttp.Response {
	params, err := phttp.ParamsAs[blobParams](ctx)
	if err != nil {
		return phttp.ErrorResponse(perrors.BadRequest("invalid blob id"))
	}
	body, err := api.BinaryBody(ctx)
	if err != nil {
		return phttp.ErrorResponse(perrors.BadRequest("invalid blob body"))
	}
	s.blobs[params.ID] = append([]byte(nil), body...)
	return phttp.JSON(blobStored{Size: int64(len(body))})
}

func (s *blobState) read(ctx *phttp.EndpointContext) *phttp.Response {
	params, err := phttp.ParamsAs[blobParams](ctx)
	if err != nil {
		return phttp.ErrorResponse(perrors.BadRequest("invalid blob id"))
	}
	if s.stall != nil {
		select {
		case <-s.stall:
		case <-ctx.Context.Context().Done():
			return phttp.ErrorResponse(perrors.New(perrors.CodeCancelled, "caller left"))
		}
	}
	blob, ok := s.blobs[params.ID]
	if !ok {
		return phttp.ErrorResponse(perrors.NotFound("no such blob"))
	}
	if s.oversize {
		blob = bytes.Repeat([]byte{0xAB}, int(blobBound)+1)
	}
	return api.BinaryResponse(http.StatusOK, "application/octet-stream", blob)
}

// TestOpenAPI_BinaryPayloadsCrossTheWireUnchanged is the raw octet cell: empty
// octets, non-UTF-8 octets, and the declared bound, all through the published
// contract and the real runtime.
func TestOpenAPI_BinaryPayloadsCrossTheWireUnchanged(t *testing.T) {
	bound, operations, _ := binaryProvider(t)
	store, read := operations["putBlobs_Id"], operations["getBlobs_Id"]
	if store.ID == "" || read.ID == "" {
		t.Fatalf("the provider document declares no blob operations: %v", operations)
	}

	for _, payload := range [][]byte{{}, nonUTF8Blob, bytes.Repeat([]byte{0x01}, int(blobBound))} {
		stored, err := putBlob(t.Context(), bound, store, payload)
		if err != nil {
			t.Fatalf("store %d octets: %v", len(payload), err)
		}
		if stored.Size != int64(len(payload)) {
			t.Fatalf("provider received %d octets, sent %d", stored.Size, len(payload))
		}
		result, err := client.CallOperationBinary(t.Context(), bound, blobCall(http.MethodGet, "one", nil, ""), read)
		if err != nil {
			t.Fatalf("read %d octets back: %v", len(payload), err)
		}
		if !bytes.Equal(result.Body, payload) {
			t.Fatalf("round trip changed the payload: %x, want %x", result.Body, payload)
		}
		if result.ContentType != "application/octet-stream" || result.Status != http.StatusOK {
			t.Fatalf("declared representation lost: %d %q", result.Status, result.ContentType)
		}
	}
}

// TestOpenAPI_BinaryPayloadsAreNeverJSONWrapped pins the one degradation a
// binary declaration exists to refuse: the octets must not appear base64 or
// quoted anywhere on the wire.
func TestOpenAPI_BinaryPayloadsAreNeverJSONWrapped(t *testing.T) {
	bound, operations, state := binaryProvider(t)
	if _, err := putBlob(t.Context(), bound, operations["putBlobs_Id"], nonUTF8Blob); err != nil {
		t.Fatalf("store: %v", err)
	}
	response, err := http.Get(state.baseURL + "/blobs/one") //nolint:noctx // the provider is a loopback httptest server
	if err != nil {
		t.Fatalf("raw GET: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read raw body: %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		// Without the declared credential the operation is refused, which is the
		// other half of the same guarantee. Re-issue it with the key.
		t.Fatalf("an undeclared anonymous call reached the payload: %d", response.StatusCode)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, state.baseURL+"/blobs/one", nil)
	if err != nil {
		t.Fatalf("build authorized request: %v", err)
	}
	request.Header.Set("X-Blob-Key", "sample-blob-key")
	request.Header.Set("X-Client-Id", "blob.consumer")
	authorized, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("authorized GET: %v", err)
	}
	defer func() { _ = authorized.Body.Close() }()
	raw, err := io.ReadAll(authorized.Body)
	if err != nil {
		t.Fatalf("read authorized body: %v", err)
	}
	if !bytes.Equal(raw, nonUTF8Blob) {
		t.Fatalf("the wire carried %x, want the declared octets %x", raw, nonUTF8Blob)
	}
	if got := authorized.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content type on the wire = %q", got)
	}
}

// TestOpenAPI_BinaryBoundsAreRefusedOnBothSides proves the bound is a contract
// and not documentation, on all three surfaces it has to hold: the generated
// client refuses an oversized source without draining it, the provider refuses
// an oversized request before it reads the body, and the client refuses an
// oversized response before it buffers it.
func TestOpenAPI_BinaryBoundsAreRefusedOnBothSides(t *testing.T) {
	bound, operations, state := binaryProvider(t)
	store, read := operations["putBlobs_Id"], operations["getBlobs_Id"]

	// 1. The emitted guard fires before a socket exists, and the source is
	// refused one byte past the bound rather than drained.
	source := &countingReader{data: bytes.Repeat([]byte{0x03}, 4096)}
	if _, err := client.ReadBoundedBody(source, blobBound); err == nil {
		t.Fatal("the emitted bound accepted an oversized reader")
	}
	if source.read > blobBound+1 {
		t.Fatalf("the bound read %d octets, want at most %d", source.read, blobBound+1)
	}

	// 2. The generated request contract refuses the same payload even when a
	// caller builds the request itself, so the bound is not only in the emitter.
	oversized := bytes.Repeat([]byte{0x02}, int(blobBound)+1)
	_, err := client.CallOperation[blobStored](t.Context(), bound,
		blobCall(http.MethodPut, "big", oversized, "application/octet-stream"), store)
	if err == nil || !perrors.Is(err, client.CodeClientRequest) {
		t.Fatalf("oversized generated request = %v, want a client.request refusal", err)
	}

	// 3. The provider refuses a foreign caller's oversized request *before* it
	// reads the body: the request announces more octets than the declaration
	// allows and then sends none, and the answer arrives anyway.
	status, code := rawBlobPut(t, state.baseURL, "big", int(blobBound)+1, nil)
	if status != http.StatusRequestEntityTooLarge || code != string(perrors.CodePayloadTooLarge) {
		t.Fatalf("announced oversized body = %d/%q, want 413/%s", status, code, perrors.CodePayloadTooLarge)
	}
	if _, stored := state.blobs["big"]; stored {
		t.Fatal("the handler ran on a body the provider had refused")
	}

	// 4. A caller that lies about its length is refused too: nothing is
	// announced, the octets arrive, and the bounded read stops one past the
	// bound.
	status, code = rawBlobPut(t, state.baseURL, "liar", 0, oversized)
	if status != http.StatusRequestEntityTooLarge || code != string(perrors.CodePayloadTooLarge) {
		t.Fatalf("unannounced oversized body = %d/%q, want 413/%s", status, code, perrors.CodePayloadTooLarge)
	}

	// 5. The response bound stops the read: the provider answers one octet more
	// than it declared, and the client never hands the payload over.
	if _, err := putBlob(t.Context(), bound, store, []byte("ok")); err != nil {
		t.Fatalf("store: %v", err)
	}
	state.oversize = true
	_, err = client.CallOperationBinary(t.Context(), bound, blobCall(http.MethodGet, "one", nil, ""), read)
	if err == nil {
		t.Fatal("the client accepted a response larger than the declared bound")
	}
	if !perrors.Is(err, client.CodeClientResponse) {
		t.Fatalf("oversized response = %v, want a client.response refusal", err)
	}
}

// rawBlobPut speaks HTTP/1.1 on the socket so the test controls exactly what
// reaches the provider: how many octets are announced, and how many are sent.
// It returns the status and the stable code of the first-party envelope.
//
// When announce is positive and body is nil, the request claims a payload and
// sends none. A provider that read the body before checking the announcement
// would block there until the read deadline; this test would then fail on the
// deadline instead of on the status, which is the distinction it exists to make.
func rawBlobPut(t *testing.T, baseURL, id string, announce int, body []byte) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(baseURL, "http://"))
	if err != nil {
		t.Fatalf("dial the provider: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	length := announce
	if length == 0 {
		length = len(body)
	}
	head := "PUT /blobs/" + id + " HTTP/1.1\r\nHost: provider\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Length: " + strconv.Itoa(length) + "\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write request head: %v", err)
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			t.Fatalf("write request body: %v", err)
		}
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return response.StatusCode, envelope.Code
}

// TestOpenAPI_BinaryRequestRefusesAnUndeclaredMediaType proves the provider
// refuses a body it cannot interpret, with a declared code, instead of reading
// it and guessing.
func TestOpenAPI_BinaryRequestRefusesAnUndeclaredMediaType(t *testing.T) {
	bound, operations, _ := binaryProvider(t)
	call := blobCall(http.MethodPut, "one", []byte("hello"), "text/plain")
	_, err := client.CallOperation[blobStored](t.Context(), bound, call, operations["putBlobs_Id"])
	if err == nil {
		t.Fatal("an undeclared request media type was accepted")
	}
	// The generated request contract refuses it before the socket; the provider
	// refuses the same shape with 415 when a foreign caller sends it.
	if !perrors.Is(err, client.CodeClientRequest) {
		t.Fatalf("undeclared media type = %v, want a client.request refusal", err)
	}
	declared := operations["putBlobs_Id"].Contract.Errors
	if !declaresError(declared, http.StatusUnsupportedMediaType, string(perrors.CodeUnsupportedMediaType)) {
		t.Fatalf("the contract hides the provider's 415 refusal: %#v", declared)
	}
	if !declaresError(declared, http.StatusRequestEntityTooLarge, string(perrors.CodePayloadTooLarge)) {
		t.Fatalf("the contract hides the provider's 413 refusal: %#v", declared)
	}
}

// TestOpenAPI_BinaryDeclaredErrorArrivesTyped proves that declaring a binary
// success does not cost the endpoint its typed errors: the D0.1 envelope still
// decodes into the declared code.
func TestOpenAPI_BinaryDeclaredErrorArrivesTyped(t *testing.T) {
	bound, operations, _ := binaryProvider(t)
	_, err := client.CallOperationBinary(t.Context(), bound, blobCall(http.MethodGet, "absent", nil, ""), operations["getBlobs_Id"])
	var remote *client.RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("declared error on a binary endpoint = %T %v, want *client.RemoteError", err, err)
	}
	if remote.Code() != string(perrors.CodeNotFound) || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("typed error = %q/%d, want not_found/404", remote.Code(), remote.StatusCode)
	}
}

// TestOpenAPI_BinaryReadIsCancellable proves a caller that leaves mid-read ends
// the call with the cancellation code, not with bytes or a deadline.
func TestOpenAPI_BinaryReadIsCancellable(t *testing.T) {
	bound, operations, state := binaryProvider(t)
	if _, err := putBlob(t.Context(), bound, operations["putBlobs_Id"], nonUTF8Blob); err != nil {
		t.Fatalf("store: %v", err)
	}
	state.stall = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.CallOperationBinary(ctx, bound, blobCall(http.MethodGet, "one", nil, ""), operations["getBlobs_Id"])
		done <- err
	}()
	cancel()
	err := <-done
	close(state.stall)
	if err == nil {
		t.Fatal("a canceled binary read returned a payload")
	}
	if !perrors.Is(err, client.CodeClientCanceled) {
		t.Fatalf("canceled read = %v, want the cancellation code", err)
	}
}

// TestOpenAPI_BinaryOperationDeclaresOnlyRESTTransport pins the projection:
// Connect carries one encoded message, so it is never advertised for octets.
func TestOpenAPI_BinaryOperationDeclaresOnlyRESTTransport(t *testing.T) {
	_, operations, _ := binaryProvider(t)
	for _, id := range []string{"putBlobs_Id", "getBlobs_Id"} {
		for _, transport := range operations[id].Contract.Transports {
			if transport.Protocol != clientcontract.TransportRESTJSON {
				t.Fatalf("%s advertises %s for a raw octet payload", id, transport.Protocol)
			}
		}
	}
}

func declaresError(declared []clientcontract.DeclaredError, status int, code string) bool {
	for _, entry := range declared {
		if entry.Status == status && entry.Code == code {
			return true
		}
	}
	return false
}

// blobCall builds the call a generated method would build: the substituted
// REST path plus the same path parameters under their declared names.
func blobCall(method, id string, body []byte, contentType string) *client.OperationCall {
	headers := make(http.Header)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	return &client.OperationCall{
		Request: &client.Request{
			Method:  method,
			Path:    strings.ReplaceAll("/blobs/{id}", "{id}", id),
			Headers: headers,
			Body:    body,
		},
		PathParams: map[string]string{"id": id},
	}
}

// putBlob stores one payload the way a generated method does: the declared
// bound is applied to the source before the request exists.
func putBlob(ctx context.Context, bound *client.Client, operation client.Operation, payload []byte) (blobStored, error) {
	body, err := client.ReadBoundedBody(bytes.NewReader(payload), blobBound)
	if err != nil {
		return blobStored{}, err
	}
	call := blobCall(http.MethodPut, "one", body, "application/octet-stream")
	return client.CallOperation[blobStored](ctx, bound, call, operation)
}

// countingReader records how much of a source the bound actually consumed. An
// oversized source must be refused, not drained.
type countingReader struct {
	data []byte
	read int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if int(r.read) >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.read:])
	r.read += int64(n)
	return n, nil
}
