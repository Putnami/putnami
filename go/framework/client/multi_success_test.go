package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// multiSuccessOperation declares three success statuses, each with its own
// representation: an item document, a string, and nothing. The bodyless one is
// 202 rather than 204 because an HTTP server never writes a 204 body, and the
// test needs a provider that does.
func multiSuccessOperation(transports []clientcontract.Transport) Operation {
	operation := connectUnaryOperation(transports, nil, nil)
	item := connectItemSchema()
	other := clientcontract.Schema{Type: "string"}
	operation.Successes = []OperationSuccess{
		{Status: http.StatusOK, Content: []OperationContent{{MediaType: "application/json", Schema: &item}}},
		{Status: http.StatusCreated, Content: []OperationContent{{MediaType: "application/json", Schema: &other}}},
		{Status: http.StatusAccepted},
	}
	return operation
}

// CallOperationResponse hands back the status the provider answered, and
// DecodeResponse decodes the body under that status's own schema. What the
// operation does not declare fails in the runtime, not in the caller.
func TestCallOperationResponseKeepsTheAnsweredStatusAndItsDeclaredBody(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
	}{
		"ok":         {http.StatusOK, `{"value":"done"}`},
		"created":    {http.StatusCreated, `"queued"`},
		"empty":      {http.StatusAccepted, ``},
		"undeclared": {http.StatusNonAuthoritativeInfo, `{"value":"done"}`},
		"extra-body": {http.StatusAccepted, `{"value":"done"}`},
		"wrong-body": {http.StatusCreated, `{"value":"done"}`},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		answer := answers[request.URL.Query().Get("shape")]
		if answer.body != "" {
			writer.Header().Set("Content-Type", "application/json")
		}
		writer.WriteHeader(answer.status)
		_, _ = writer.Write([]byte(answer.body))
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := multiSuccessOperation([]clientcontract.Transport{connectRestTransport()})
	call := func(shape string) *OperationCall {
		input := newConnectCallInput()
		input.Request.Query = map[string]string{"shape": shape}
		return input
	}

	response, err := CallOperationResponse(t.Context(), bound, call("ok"), operation)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("200: response=%v err=%v", response, err)
	}
	item, err := DecodeResponse[connectItem](bound, response, operation)
	if err != nil || item.Value != "done" {
		t.Fatalf("200 body = %+v, %v", item, err)
	}

	response, err = CallOperationResponse(t.Context(), bound, call("created"), operation)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("201: response=%v err=%v", response, err)
	}
	queued, err := DecodeResponse[string](bound, response, operation)
	if err != nil || queued != "queued" {
		t.Fatalf("201 body = %q, %v", queued, err)
	}

	response, err = CallOperationResponse(t.Context(), bound, call("empty"), operation)
	if err != nil || response.StatusCode != http.StatusAccepted || len(response.Body) != 0 {
		t.Fatalf("202: response=%v err=%v", response, err)
	}

	for _, shape := range []string{"undeclared", "extra-body", "wrong-body"} {
		if _, err := CallOperationResponse(t.Context(), bound, call(shape), operation); perrors.GetCode(err) != CodeClientResponse {
			t.Fatalf("%s: err = %v, want %s", shape, err, CodeClientResponse)
		}
	}
}

// A Connect response carries one status, so an operation declaring several is
// refused before a request leaves, rather than collapsed onto the first.
func TestCallOperationResponseRefusesConnectForSeveralSuccessStatuses(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := multiSuccessOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)})
	if _, err := CallOperationResponse(t.Context(), bound, newConnectCallInput(), operation); perrors.GetCode(err) != CodeClientConfig {
		t.Fatalf("connect multi-success err = %v, want %s", err, CodeClientConfig)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("a refused dispatch reached the provider %d times", got)
	}
}
