package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

type observedOctetReader struct {
	io.Reader
	reads int
}

func TestBinaryStreamUsesItsRouteLimitAndMapsOverflowTo413(t *testing.T) {
	server := phttp.NewServerPlugin(phttp.ServerConfig{MaxBodySize: 4})
	server.Use(phttp.CORS(phttp.CORSOptions{}))
	provider := New(server)
	provider.Register(Endpoint("POST", "/stream").Body(BinaryStream(8)).Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
		body, err := BinaryStreamBody(ctx)
		if err != nil {
			return phttp.InternalError("missing stream")
		}
		if _, err := io.ReadAll(body); err != nil {
			return phttp.InternalError("read failed")
		}
		return phttp.NoContent()
	}))
	if err := provider.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	server.POST("/json", func(ctx *phttp.Context) *phttp.Response {
		_, _ = io.ReadAll(ctx.Request.Body)
		return phttp.NoContent()
	})

	accepted := httptest.NewRequest("POST", "/stream", strings.NewReader("1234567"))
	accepted.Header.Set("Content-Type", "application/octet-stream")
	acceptedRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(acceptedRecorder, accepted)
	if acceptedRecorder.Code != http.StatusNoContent {
		t.Fatalf("route-specific limit response = %d, want 204", acceptedRecorder.Code)
	}

	oversized := httptest.NewRequest("POST", "/stream", strings.NewReader("123456789"))
	oversized.ContentLength = -1
	oversized.Header.Set("Content-Type", "application/octet-stream")
	oversizedRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(oversizedRecorder, oversized)
	if oversizedRecorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("overflow response = %d %q, want 413", oversizedRecorder.Code, oversizedRecorder.Body.String())
	}
	announced := httptest.NewRequest("POST", "/stream", strings.NewReader("123456789"))
	announced.Header.Set("Content-Type", "application/octet-stream")
	announced.Header.Set("Origin", "https://consumer.example")
	announcedRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(announcedRecorder, announced)
	if announcedRecorder.Code != http.StatusRequestEntityTooLarge || announcedRecorder.Header().Get("Access-Control-Allow-Origin") != "https://consumer.example" {
		t.Fatalf("announced overflow bypassed middleware: %d %#v", announcedRecorder.Code, announcedRecorder.Header())
	}

	json := httptest.NewRequest("POST", "/json", strings.NewReader("1234567"))
	jsonRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(jsonRecorder, json)
	if jsonRecorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("global JSON limit response = %d, want 413", jsonRecorder.Code)
	}
}

func (r *observedOctetReader) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func TestBinaryStreamLeavesTheBodyUnreadUntilTheHandlerConsumesIt(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "streamed-binary-payloads", "a-streamed-body-preserves-octets-and-the-callers-media-type-without-buffering")
	payload := []byte{0, 255, 128, 10}
	for _, mediaType := range []string{"application/gzip", "application/vnd.putnami.migration-bundle.v1.tar; version=1", "application/json"} {
		source := &observedOctetReader{Reader: bytes.NewReader(payload)}
		definition := Endpoint("POST", "/echo").Body(BinaryStream(4096)).Returns(BinaryStream(4096)).
			Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
				if source.reads != 0 {
					t.Fatal("the endpoint pipeline buffered a streamed request")
				}
				body, err := BinaryStreamBody(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return BinaryStreamResponse(200, ctx.Request.Header.Get("Content-Type"), body)
			})
		if meta := definition.BodyBinary(); meta.MediaType != "*/*" || !meta.Streamed || meta.MaxBytes != 4096 || definition.IsStream() {
			t.Fatalf("raw HTTP body metadata = %#v", meta)
		}
		request := httptest.NewRequest("POST", "/echo", source)
		request.Header.Set("Content-Type", mediaType)
		recorder := httptest.NewRecorder()
		response := definition.BuildHandler()(phttp.NewContext(recorder, request))
		if err := response.WriteTo(recorder); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != 200 || recorder.Header().Get("Content-Type") != mediaType || !bytes.Equal(recorder.Body.Bytes(), payload) {
			t.Fatalf("response = %d %q %x", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
		}
	}
}

func TestBinaryStreamRefusesInvalidLabelsBeforeReading(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "streamed-binary-payloads", "a-streamed-body-refuses-a-missing-or-nonconcrete-media-type-before-reading")
	for _, mediaType := range []string{"", "*/*", "text/*", "gzip", "bad media/type", "application/gzip; broken", "application/gzip\r\n"} {
		source := &observedOctetReader{Reader: strings.NewReader("unread")}
		request := httptest.NewRequest("POST", "/echo", source)
		request.Header.Set("Content-Type", mediaType)
		endpoint := Endpoint("POST", "/echo").Body(BinaryStream(4096)).Handle(func(*phttp.EndpointContext) *phttp.Response {
			t.Fatal("invalid label reached handler")
			return nil
		})
		response := endpoint.BuildHandler()(phttp.NewContext(httptest.NewRecorder(), request))
		if response.Status != http.StatusUnsupportedMediaType || source.reads != 0 {
			t.Fatalf("label %q: status=%d reads=%d", mediaType, response.Status, source.reads)
		}
	}
	for _, ctx := range []*phttp.EndpointContext{nil, {}, {DecodedBody: []byte("bounded")}} {
		if _, err := BinaryStreamBody(ctx); err == nil {
			t.Fatal("stream accessor accepted a non-stream body")
		}
	}
}

func TestBinaryStreamStrictReaderAndGeneratorKeepStreamingExplicit(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "streamed-binary-payloads", "the-contract-and-generated-client-preserve-explicit-streaming-and-caller-owned-media-types")
	document := strings.ReplaceAll(binaryDocument, "application/octet-stream", "*/*")
	document = strings.Replace(document, `"x-putnami-max-bytes":4096`, `"x-putnami-streamed":true,"x-putnami-max-bytes":4096`, 1)
	ir, err := ReadOpenAPISpec([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	content := ir.Services[0].Methods[0].Successes[0].Content[0]
	if !content.Streamed || content.MaxBytes != 4096 || content.MediaType != "*/*" {
		t.Fatalf("content = %#v", content)
	}
	for _, replacement := range []string{`"x-putnami-streamed":true,"x-putnami-max-bytes":0`, `"x-putnami-streamed":true,"x-putnami-max-bytes":-1`, `"x-putnami-streamed":"true"`, `"x-putnami-streamed":false`} {
		if _, err := ReadOpenAPISpec([]byte(strings.Replace(document, `"x-putnami-streamed":true,"x-putnami-max-bytes":4096`, replacement, 1))); err == nil {
			t.Fatalf("accepted invalid stream metadata %s", replacement)
		}
	}
	if _, err := ReadOpenAPISpec([]byte(strings.Replace(document, `,"x-putnami-max-bytes":4096`, "", 1))); err == nil {
		t.Fatal("streaming marker accepted without a byte bound")
	}
	if _, err := ReadOpenAPISpec([]byte(strings.ReplaceAll(document, "*/*", "image/png"))); err == nil {
		t.Fatal("streaming marker accepted on a fixed media type")
	}
	spec := binaryFixtureSpec()
	spec.Services[0].Methods[0].Request.Content[0] = content
	spec.Services[0].Methods[1].Successes[0].Content[0] = content
	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Body io.Reader", "Body io.ReadCloser", "ContentType string", "BodyStream: in.Body", `headers.Set("Content-Type", in.ContentType)`, "client.CallOperationBinaryStream", `\"streamed\":true`} {
		if !strings.Contains(source, want) {
			t.Errorf("generated stream is missing %q", want)
		}
	}
	if strings.Contains(source, "ReadBoundedBody") {
		t.Fatal("generated stream buffers its upload")
	}
	again, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"})
	if err != nil || source != again {
		t.Fatalf("streamed client generation is not deterministic: %v", err)
	}
	spec.Services[0].Methods[1].Client = cachedUnaryOperation("/blobs/{id}")
	if _, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "blobsclient", ClientName: "BlobsClient"}); err == nil || !strings.Contains(err.Error(), "cache a streamed octet") {
		t.Fatalf("streamed cache declaration = %v", err)
	}
}

type binaryStreamCloser struct {
	io.Reader
	closed bool
}

func (r *binaryStreamCloser) Close() error { r.closed = true; return nil }

func TestBinaryStreamResponseRefusesAnInvalidLabelAndClosesItsSource(t *testing.T) {
	source := &binaryStreamCloser{Reader: strings.NewReader("private")}
	response := BinaryStreamResponse(200, "*/*", source)
	if response.Status != 500 || !source.closed {
		t.Fatalf("response=%d closed=%v", response.Status, source.closed)
	}
}
