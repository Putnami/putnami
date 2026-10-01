package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// binaryEchoEndpoint declares a bounded octet body and hands the bytes straight
// back, so a test can compare what the pipeline delivered with what it sent.
func binaryEchoEndpoint(bound int64) EndpointDefinition {
	return Endpoint("POST", "/echo").
		Body(Binary("application/octet-stream", bound)).
		Returns(Binary("application/octet-stream", bound)).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			body, err := BinaryBody(ctx)
			if err != nil {
				return phttp.InternalError("no binary body")
			}
			return BinaryResponse(http.StatusOK, "application/octet-stream", body)
		})
}

func binaryRequestFor(payload []byte, contentType string, announce int) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(payload))
	request.Header.Set("Content-Type", contentType)
	if announce > 0 {
		request.Header.Set("Content-Length", strconv.Itoa(announce))
		request.ContentLength = int64(announce)
	}
	return request
}

// TestBinary_PipelineCarriesEveryOctetUnchanged covers the two payloads a JSON
// pipeline would silently damage: none, and octets that are not text.
func TestBinary_PipelineCarriesEveryOctetUnchanged(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "binary-payloads", "a-declared-raw-octet-body-reaches-the-handler-unchanged")
	handler := binaryEchoEndpoint(64).BuildHandler()
	for name, payload := range map[string][]byte{
		"empty":     {},
		"non-utf8":  {0x00, 0xff, 0xfe, 0x80},
		"json-like": []byte(`{"not":"a document"}`),
		"at-bound":  bytes.Repeat([]byte{0x7f}, 64),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			response := handler(phttp.NewContext(recorder, binaryRequestFor(payload, "application/octet-stream", 0)))
			if response.Status != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Status)
			}
			body, err := response.BodyBytes()
			if err != nil {
				t.Fatalf("response body: %v", err)
			}
			if !bytes.Equal(body, payload) {
				t.Fatalf("pipeline delivered %x, want %x", body, payload)
			}
			if got := response.Headers.Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("content type = %q", got)
			}
		})
	}
}

// TestBinary_PipelineRefusesTheUndeclared proves both declared facts are
// enforced, and that the announcement is refused before the body is read.
func TestBinary_PipelineRefusesTheUndeclared(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "binary-payloads", "an-oversized-raw-octet-request-is-refused-before-the-body-is-read")
	spectest.Proves(t, "go/api-contracts", "binary-payloads", "an-undeclared-request-media-type-is-refused")
	handler := binaryEchoEndpoint(16).BuildHandler()

	t.Run("undeclared media type", func(t *testing.T) {
		response := handler(phttp.NewContext(httptest.NewRecorder(), binaryRequestFor([]byte("hi"), "application/json", 0)))
		if response.Status != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", response.Status)
		}
	})

	t.Run("announced beyond the bound", func(t *testing.T) {
		// The reader is empty: only the announcement can produce the refusal, so
		// a pipeline that read first would answer 200 with zero octets.
		request := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(nil))
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("Content-Length", "1024")
		request.ContentLength = 1024
		response := handler(phttp.NewContext(httptest.NewRecorder(), request))
		if response.Status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", response.Status)
		}
	})

	t.Run("sent beyond the bound", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(bytes.Repeat([]byte{0x01}, 17)))
		request.Header.Set("Content-Type", "application/octet-stream")
		request.ContentLength = -1
		response := handler(phttp.NewContext(httptest.NewRecorder(), request))
		if response.Status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", response.Status)
		}
	})

	t.Run("charset parameter is not a different media type", func(t *testing.T) {
		response := handler(phttp.NewContext(httptest.NewRecorder(),
			binaryRequestFor([]byte("hi"), "application/octet-stream; charset=binary", 0)))
		if response.Status != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.Status)
		}
	})
}

// TestBinary_RefusesAnUndeclarableDeclaration keeps the authoring mistakes out
// of the published contract, where they would become a consumer's problem.
func TestBinary_RefusesAnUndeclarableDeclaration(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "binary-payloads", "a-raw-octet-payload-is-declared-with-its-media-type-and-bound")
	for name, declare := range map[string]func(){
		"json media type": func() { Binary("application/json", 16) },
		"structured json": func() { Binary("application/vnd.acme+json", 16) },
		"no bound":        func() { Binary("application/octet-stream", 0) },
		"negative bound":  func() { Binary("application/octet-stream", -1) },
		"not a media type": func() {
			Binary("not a media type", 16)
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("the declaration was accepted")
				}
			}()
			declare()
		})
	}
}

// TestBinary_DeclarationReachesTheDiscoveredRoute proves the two facts travel
// to every downstream consumer rather than staying inside the builder.
func TestBinary_DeclarationReachesTheDiscoveredRoute(t *testing.T) {
	definition := binaryEchoEndpoint(128)
	body, returns := definition.BodyBinary(), definition.ReturnsBinary()
	if body == nil || body.MediaType != "application/octet-stream" || body.MaxBytes != 128 {
		t.Fatalf("body declaration = %#v", body)
	}
	if returns == nil || returns.MaxBytes != 128 {
		t.Fatalf("returns declaration = %#v", returns)
	}
	// The accessor copies: a consumer cannot rewrite the endpoint's own bound.
	body.MaxBytes = 1
	if again := definition.BodyBinary(); again.MaxBytes != 128 {
		t.Fatalf("the discovered declaration is mutable: %d", again.MaxBytes)
	}
}

// TestBinaryBody_RefusesAJSONEndpoint keeps the accessor honest: reading JSON
// bytes through it would skip the validation the declaration asked for.
func TestBinaryBody_RefusesAJSONEndpoint(t *testing.T) {
	if _, err := BinaryBody(&phttp.EndpointContext{}); err == nil {
		t.Fatal("BinaryBody accepted an endpoint with no binary declaration")
	}
	if _, err := BinaryBody(nil); err == nil {
		t.Fatal("BinaryBody accepted a nil context")
	}
}
