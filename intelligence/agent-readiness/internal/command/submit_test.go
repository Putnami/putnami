package command

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/internal/testrepo"
	protocolcli "go.putnami.dev/protocol/cli"
)

type recordingTransport struct{ request *http.Request }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.request = req
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
}

func TestExactBodyRefusesMismatchedGeneratedBytes(t *testing.T) {
	next := &recordingTransport{}
	transport := &exactBody{next: next, body: []byte(`{"a":1,"b":2}`)}
	request := httptest.NewRequest(http.MethodPost, "https://api.example"+reportsPath, strings.NewReader(`{"a":1,"b":3}`))
	if _, err := transport.RoundTrip(request); !errors.Is(err, errBodyMismatch) || !transport.refused || next.request != nil {
		t.Fatalf("mismatched request reached transport: %v", err)
	}
}

func TestExactBodySendsOnlyThePrintedBytes(t *testing.T) {
	next := &recordingTransport{}
	printed := []byte(`{"b":2,"a":1}`)
	transport := &exactBody{next: next, body: printed}
	request := httptest.NewRequest(http.MethodPost, "https://api.example/gateway"+reportsPath, strings.NewReader(`{"a":1,"b":2}`))
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	sent, _ := io.ReadAll(next.request.Body)
	if !bytes.Equal(sent, printed) || next.request.ContentLength != int64(len(printed)) {
		t.Fatalf("sent %q (%d bytes)", sent, next.request.ContentLength)
	}
	again, _ := next.request.GetBody()
	if replay, _ := io.ReadAll(again); !bytes.Equal(replay, printed) {
		t.Fatalf("GetBody = %q", replay)
	}
	for _, hostile := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://api.example"+reportsPath, nil),
		httptest.NewRequest(http.MethodPost, "https://api.example/other", strings.NewReader(`{}`)),
	} {
		next.request = nil
		if _, err := transport.RoundTrip(hostile); !errors.Is(err, errBodyMismatch) || next.request != nil {
			t.Fatalf("unexpected request reached transport: %v", err)
		}
	}
}

func TestGeneratedClientCannotReencodeThePrintedPayload(t *testing.T) {
	fixedCollection(t)
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The generated time serializer removes the redundant fraction. The changed
	// body still validates the JSON schema, but must be refused before a send.
	altered := bytes.Replace(built.Bytes, []byte(`"2026-09-26T12:00:00Z"`), []byte(`"2026-09-26T12:00:00.000Z"`), 1)
	if bytes.Equal(altered, built.Bytes) {
		t.Fatal("fixed time is absent")
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	_, err = Submit(context.Background(), server.URL, nil, altered)
	if err == nil || requests != 0 || !strings.Contains(err.Error(), "nothing was sent") {
		t.Fatalf("changed encoding: requests=%d error=%v", requests, err)
	}
}

func TestInvalidAPIURLFailsBeforeSend(t *testing.T) {
	built, err := BuildPayload(context.Background(), testrepo.New(t), "0.1.0", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Submit(context.Background(), "http://example.com", nil, built.Bytes)
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Fatalf("error=%v", err)
	}
}
