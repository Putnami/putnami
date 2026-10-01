package client

import (
	"context"
	stderrors "errors"
	"io"
	"sync"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// StreamedBinaryPayload is an unframed HTTP success body. Body belongs to the
// caller, who must close it even when only part of the payload is consumed.
type StreamedBinaryPayload struct {
	Status      int
	ContentType string
	Body        io.ReadCloser
}

// CallOperationBinaryStream executes a generated raw HTTP download without
// buffering it. ContentType retains the provider's full header, including its
// parameters, and Body returns the provider's bytes unchanged.
func CallOperationBinaryStream(ctx context.Context, client *Client, call *OperationCall, operation Operation) (*StreamedBinaryPayload, error) {
	if client == nil || call == nil || call.Request == nil {
		return nil, errors.New(CodeClientConfig, "generated binary stream requires a client and request")
	}
	if err := validateBinaryStreamOperation(operation); err != nil {
		return nil, err
	}
	transport, err := dispatchTransport(operation, supportedUnaryTransport)
	if err != nil {
		return nil, err
	}
	if transport.Protocol != clientcontract.TransportRESTJSON {
		return nil, errors.New(CodeClientConfig, "only rest-json carries binary streams unchanged")
	}
	request := cloneRequest(call.Request)
	request.StreamResponse = true
	response, err := doOperationCall(ctx, client, call, client, request, operation)
	if err != nil {
		return nil, err
	}
	content, err := successContent(response, operation.Successes)
	if err == nil && (content == nil || !content.Streamed || !content.IsBinary() || response.BodyStream == nil || response.Body != nil) {
		err = errors.New(CodeClientResponse, "service response is not the declared binary stream")
	}
	if err != nil {
		closeResponseStream(response)
		return nil, err
	}
	return &StreamedBinaryPayload{Status: response.StatusCode, ContentType: response.Headers.Get("Content-Type"), Body: response.BodyStream}, nil
}

func operationHasBinaryStream(operation Operation) bool {
	if operation.Request != nil {
		for _, content := range operation.Request.Content {
			if content.Streamed {
				return true
			}
		}
	}
	for _, success := range operation.Successes {
		for _, content := range success.Content {
			if content.Streamed {
				return true
			}
		}
	}
	return false
}

func validateBinaryStreamOperation(operation Operation) error {
	if !operationHasBinaryStream(operation) {
		return nil
	}
	if operation.Contract.Resilience != nil && operation.Contract.Resilience.Cache != nil {
		return errors.New(CodeClientConfig, "binary streams cannot use the response cache")
	}
	transport, err := dispatchTransport(operation, supportedUnaryTransport)
	if err != nil {
		return err
	}
	if operation.Contract.Stream != clientcontract.StreamUnary || transport.Protocol != clientcontract.TransportRESTJSON {
		return errors.New(CodeClientConfig, "binary streams require a unary rest-json operation")
	}
	validate := func(contents []OperationContent) error {
		for _, content := range contents {
			if content.Streamed && (!content.IsBinary() || content.MediaType != "*/*" || content.MaxBytes <= 0 || len(contents) != 1) {
				return errors.New(CodeClientConfig, "generated binary stream contract is invalid")
			}
		}
		return nil
	}
	if operation.Request != nil {
		if err := validate(operation.Request.Content); err != nil {
			return err
		}
	}
	for _, success := range operation.Successes {
		if err := validate(success.Content); err != nil {
			return err
		}
	}
	return nil
}

// ownedResponseStream keeps unary deadlines alive after the response headers
// arrive. EOF, a read failure, cancellation and Close all release it once.
type ownedResponseStream struct {
	body     io.ReadCloser
	cancel   context.CancelFunc
	once     sync.Once
	err      error
	registry *ServiceBindings
	finish   func(error)
}

func ownResponseStream(ctx context.Context, body io.ReadCloser, cancel context.CancelFunc) io.ReadCloser {
	stream := newOwnedResponseStream(body, cancel)
	context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck // Cancellation owns cleanup; Close retains its error for the caller.
	})
	return stream
}

func newOwnedResponseStream(body io.ReadCloser, cancel context.CancelFunc) *ownedResponseStream {
	return &ownedResponseStream{body: body, cancel: cancel}
}

func (stream *ownedResponseStream) Read(value []byte) (int, error) {
	n, err := stream.body.Read(value)
	if err != nil {
		stream.close(err) //nolint:errcheck // Preserve the terminal read result; Close retains its own error.
	}
	return n, err
}

func (stream *ownedResponseStream) Close() error {
	return stream.close(nil)
}

func (stream *ownedResponseStream) close(terminalErr error) error {
	stream.once.Do(func() {
		stream.err = stream.body.Close()
		if stream.registry != nil {
			stream.registry.mu.Lock()
			delete(stream.registry.binaryStreams, stream)
			stream.registry.mu.Unlock()
		}
		if stream.finish != nil {
			if stderrors.Is(terminalErr, io.EOF) {
				terminalErr = nil
			}
			if terminalErr == nil {
				terminalErr = stream.err
			}
			stream.finish(terminalErr)
		}
		stream.cancel()
	})
	return stream.err
}

func closeResponseStream(response *Response) {
	if response != nil && response.BodyStream != nil {
		response.BodyStream.Close() //nolint:errcheck // Preserve the primary error rejecting this response.
	}
}
