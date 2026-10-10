package clicore

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestContextBoundClientLeavesAClientWithoutDeadlineAlone(t *testing.T) {
	client := &http.Client{}
	if got := contextBoundClient(context.Background(), client); got != client {
		t.Fatal("a context that never ends wrapped the client")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound := contextBoundClient(ctx, client)
	if bound == client || client.Transport != nil {
		t.Fatal("binding changed the caller's client")
	}
	if _, ok := bound.Transport.(contextTransport); !ok {
		t.Fatalf("bound transport = %T", bound.Transport)
	}
}

func TestContextTransportKeepsTheRequestContext(t *testing.T) {
	type key struct{}
	var seen context.Context
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request.Context()
		return responseWith(http.StatusOK, `{}`), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	transport := contextTransport{ctx: ctx, base: base}
	request, err := http.NewRequestWithContext(context.WithValue(context.Background(), key{}, "kept"), http.MethodGet, "https://auth.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if seen.Value(key{}) != "kept" {
		t.Fatal("the request's own context was replaced")
	}
	if seen.Err() != nil {
		t.Fatalf("request context ended early: %v", seen.Err())
	}
	cancel()
	<-seen.Done()
	if !errors.Is(context.Cause(seen), context.Canceled) {
		t.Fatalf("cause = %v, want the bound context's", context.Cause(seen))
	}
	_ = response.Body.Close()
	_ = response.Body.Close() // a second close releases nothing twice

	if _, err := transport.RoundTrip(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip after the context ended = %v, want context.Canceled", err)
	}
}

func TestContextTransportReleasesAFailedRequest(t *testing.T) {
	failure := errors.New("connection refused")
	var seen context.Context
	transport := contextTransport{ctx: t.Context(), base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request.Context()
		return nil, failure
	})}
	request, err := http.NewRequest(http.MethodGet, "https://auth.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); !errors.Is(err, failure) {
		t.Fatalf("RoundTrip = %v, want the transport's error", err)
	}
	if seen.Err() == nil {
		t.Fatal("the failed request's context was not released")
	}
}
