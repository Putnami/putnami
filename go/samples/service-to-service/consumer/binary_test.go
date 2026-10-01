package consumer

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
)

// The Go→Go raw octet cell: the Go provider declares a bounded binary echo and
// a bounded binary read, and the generated Go client calls both over a real
// loopback socket. Every assertion is on octets, never on text.

// nonUTF8 is neither valid UTF-8 nor valid JSON. A pipeline that re-encoded it
// as a JSON string, or decoded it as text, would corrupt it.
var nonUTF8 = []byte{0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a}

// TestBinaryEchoCarriesEveryOctet covers the payloads a JSON pipeline damages:
// none, octets that are not text, and exactly the declared bound.
func TestBinaryEchoCarriesEveryOctet(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)
	for name, payload := range map[string][]byte{
		"empty":     {},
		"non-utf8":  nonUTF8,
		"json-like": []byte(`{"not":"a document"}`),
		"at-bound":  bytes.Repeat([]byte{0x7f}, service.BlobMaxBytes),
	} {
		t.Run(name, func(t *testing.T) {
			echoed, err := generated.CreateBlobsEcho(t.Context(), itemsclient.CreateBlobsEchoInput{
				Body: bytes.NewReader(payload),
			})
			if err != nil {
				t.Fatalf("echo %d octets: %v", len(payload), err)
			}
			if !bytes.Equal(echoed.Body, payload) {
				t.Fatalf("round trip changed the payload: %x, want %x", echoed.Body, payload)
			}
			if echoed.Status != http.StatusOK || echoed.ContentType != service.BlobMediaType {
				t.Fatalf("declared representation lost: %d %q", echoed.Status, echoed.ContentType)
			}
		})
	}
}

// TestBinaryBoundIsRefusedBeforeTheSourceIsDrained proves the emitted bound is
// a contract: an oversized reader never reaches a socket, and it is not read
// to the end either.
func TestBinaryBoundIsRefusedBeforeTheSourceIsDrained(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)
	source := &countingReader{remaining: service.BlobMaxBytes * 4}
	_, err := generated.CreateBlobsEcho(t.Context(), itemsclient.CreateBlobsEchoInput{Body: source})
	if err == nil {
		t.Fatal("an oversized payload was sent")
	}
	if !perrors.Is(err, client.CodeClientRequest) {
		t.Fatalf("oversized payload = %v, want a client.request refusal", err)
	}
	if source.given > service.BlobMaxBytes+1 {
		t.Fatalf("the bound consumed %d octets, want at most %d", source.given, service.BlobMaxBytes+1)
	}
}

// TestBinaryReadComposesParametersAndCredentials proves a raw octet response
// costs the operation nothing else: the path parameter is typed, the declared
// credential is injected by the binding, and the payload arrives verbatim.
func TestBinaryReadComposesParametersAndCredentials(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)

	blob, err := generated.GetBlobs(t.Context(), itemsclient.GetBlobsInput{Path: itemsclient.GetBlobsPath{Id: "1"}})
	if err != nil {
		t.Fatalf("GetBlobs: %v", err)
	}
	if !bytes.Equal(blob.Body, nonUTF8) {
		t.Fatalf("stored blob = %x, want %x", blob.Body, nonUTF8)
	}

	// Zero octets are octets: an empty stored payload round-trips as an empty
	// payload, not as a missing body.
	empty, err := generated.GetBlobs(t.Context(), itemsclient.GetBlobsInput{Path: itemsclient.GetBlobsPath{Id: "2"}})
	if err != nil {
		t.Fatalf("GetBlobs empty: %v", err)
	}
	if len(empty.Body) != 0 {
		t.Fatalf("empty stored blob = %x, want zero octets", empty.Body)
	}
}

// TestBinaryEndpointStillDeclaresItsTypedError proves the D0.1 envelope
// survives a binary success declaration: the 404 arrives as the generated
// typed error, not as an unattributed remote failure.
func TestBinaryEndpointStillDeclaresItsTypedError(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)
	_, err := generated.GetBlobs(t.Context(), itemsclient.GetBlobsInput{Path: itemsclient.GetBlobsPath{Id: "absent"}})
	var typed *itemsclient.GetBlobsNotFoundError
	if !errors.As(err, &typed) {
		t.Fatalf("declared error on a binary endpoint = %T %v, want the generated typed error", err, err)
	}
	if typed.Remote.Code() != string(perrors.CodeNotFound) || typed.Remote.StatusCode != http.StatusNotFound {
		t.Fatalf("typed error = %q/%d, want not_found/404", typed.Remote.Code(), typed.Remote.StatusCode)
	}
}

// TestBinaryReadNeverCallsAnonymously proves the credential rule holds on a
// binary endpoint too: an unbound declared profile stops the call before a
// socket exists.
func TestBinaryReadNeverCallsAnonymously(t *testing.T) {
	options := sampleBinding(realProvider(t))
	binding := options.Services["items"]
	delete(binding.Credentials, "catalog-key")
	options.Services["items"] = binding

	_, err := boundClient(t, options).GetBlobs(t.Context(), itemsclient.GetBlobsInput{
		Path: itemsclient.GetBlobsPath{Id: "1"},
	})
	if err == nil {
		t.Fatal("a binary read with no declared credential reached the provider")
	}
	if !perrors.Is(err, client.CodeClientCredential) {
		t.Fatalf("unbound credential = %v, want a client.credential refusal", err)
	}
}

// countingReader hands out one octet per Read and counts what it gave, so a
// bound that drains its source is visible rather than assumed.
type countingReader struct {
	remaining int
	given     int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 0x5a
	r.remaining--
	r.given++
	return 1, nil
}
