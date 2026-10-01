package client

import (
	"context"
	"io"
	"strings"

	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// Raw octet operations.
//
// A binary operation is not a JSON operation with a different codec: there is
// no document to validate, no schema to project, and no encoding step that
// could lose a byte. What it does have is a declared bound on each side, and
// this file is where those bounds are honored — before a request is sent, and
// before an oversized response is buffered.

// BinaryPayload is one raw octet success payload, with the status and media
// type the provider answered.
type BinaryPayload struct {
	// Status is the HTTP status the provider answered.
	Status int
	// ContentType is the media type the provider labeled the payload with. It
	// matched the declared representation, or the call failed.
	ContentType string
	// Body is the payload, verbatim: no base64, no JSON, no re-encoding.
	Body []byte
}

// IsBinary reports a raw octet representation of a generated operation.
func (content OperationContent) IsBinary() bool {
	return content.Schema != nil && content.Schema.Type == "string" && content.Schema.Format == "binary"
}

// ReadBoundedBody reads at most max bytes from a generated request payload and
// refuses anything longer.
//
// It reads one byte past the bound and stops there: an oversized source is
// refused without being read to the end, so a caller streaming a large file
// pays one extra byte rather than the whole file. A nil reader carries no
// payload, which is not the same as an empty one being invalid — an empty
// declared binary body is a legitimate zero-length payload.
func ReadBoundedBody(reader io.Reader, max int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	if max <= 0 {
		return nil, errors.New(CodeClientConfig, "generated binary request has no declared bound")
	}
	body, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, errors.New(CodeClientRequest, "service request body could not be read")
	}
	if int64(len(body)) > max {
		return nil, errors.New(CodeClientRequest, "service request body exceeds the declared bound")
	}
	return body, nil
}

// CallOperationBinary executes one generated unary operation whose success
// payload is raw octets.
func CallOperationBinary(ctx context.Context, client *Client, call *OperationCall, operation Operation) (*BinaryPayload, error) {
	if err := validateBinaryStreamOperation(operation); err != nil {
		return nil, err
	}
	if call == nil || call.Request == nil {
		return nil, errors.New(CodeClientConfig, "generated call has no request")
	}
	transport, err := dispatchTransport(operation, supportedUnaryTransport)
	if err != nil {
		return nil, err
	}
	if transport.Protocol != clientcontract.TransportRESTJSON {
		// Connect carries one encoded message per call. Octets would have to
		// travel base64 inside it, which is the silent re-wrapping a binary
		// declaration refuses. The refusal names the transport rather than
		// degrading the payload.
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares a raw octet payload and dispatches on %s; only rest-json carries octets unchanged",
			operation.ID, transport.Protocol)
	}
	response, err := doOperationCall(ctx, client, call, client, call.Request, operation)
	if err != nil {
		return nil, err
	}
	content, err := successContent(response, operation.Successes)
	if err != nil {
		return nil, err
	}
	if content == nil || !content.IsBinary() || content.Streamed || response.BodyStream != nil {
		closeResponseStream(response)
		return nil, errors.New(CodeClientResponse, "service response is not the declared raw octet payload")
	}
	if content.MaxBytes > 0 && int64(len(response.Body)) > content.MaxBytes {
		return nil, errors.New(CodeClientResponse, "service response exceeds the declared bound")
	}
	return &BinaryPayload{
		Status:      response.StatusCode,
		ContentType: strings.TrimSpace(strings.Split(response.Headers.Get("Content-Type"), ";")[0]),
		Body:        response.Body,
	}, nil
}

// declaredBinaryResponseBytes reports the tightest declared raw octet bound
// across an operation's success variants, or zero when none is binary.
func declaredBinaryResponseBytes(operation Operation) int64 {
	var bound int64
	for i := range operation.Successes {
		for j := range operation.Successes[i].Content {
			content := operation.Successes[i].Content[j]
			if !content.IsBinary() || content.MaxBytes <= 0 {
				continue
			}
			if bound == 0 || content.MaxBytes < bound {
				bound = content.MaxBytes
			}
		}
	}
	return bound
}

func declaredBinaryRequestBytes(operation Operation) int64 {
	content := binaryRequestContent(operation.Request)
	if content == nil {
		return 0
	}
	return content.MaxBytes
}

// binaryRequestContent returns the declared raw octet request representation
// of an operation, or nil.
func binaryRequestContent(declared *OperationRequest) *OperationContent {
	if declared == nil {
		return nil
	}
	for i := range declared.Content {
		if declared.Content[i].IsBinary() {
			return &declared.Content[i]
		}
	}
	return nil
}

// validateBinaryRequest applies the two declared facts of a raw octet request:
// the media type it travels under, and the bound it must fit in. An empty
// payload is valid — zero octets are octets.
func validateBinaryRequest(request *Request, declared *OperationRequest, content *OperationContent) error {
	if len(declared.Content) != 1 {
		return errors.New(CodeClientConfig, "generated request body contract is invalid")
	}
	if content.Streamed {
		if request == nil || request.BodyStream == nil || request.Body != nil {
			return errors.New(CodeClientRequest, "service request requires a binary stream")
		}
		if !phttp.ConcreteMediaType(request.Headers.Get("Content-Type")) {
			return errors.New(CodeClientRequest, "service request requires a concrete content type")
		}
		return nil
	}
	if request != nil && request.BodyStream != nil {
		return errors.New(CodeClientRequest, "service request unexpectedly contains a binary stream")
	}
	mediaType := ""
	if request != nil && request.Headers != nil {
		mediaType = strings.TrimSpace(strings.Split(request.Headers.Get("Content-Type"), ";")[0])
	}
	if !strings.EqualFold(mediaType, content.MediaType) {
		return errors.New(CodeClientRequest, "service request content type does not match the generated contract")
	}
	if content.MaxBytes > 0 && request != nil && int64(len(request.Body)) > content.MaxBytes {
		return errors.New(CodeClientRequest, "service request body exceeds the declared bound")
	}
	return nil
}
