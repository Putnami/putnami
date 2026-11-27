package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// A declared redirect is the operation's answer: the bound call hands back the
// 3xx and its Location, ignores the hypertext note a server writes beside it,
// and never follows it. A 3xx the operation does not declare stays a failure.
func TestCallOperationResponseKeepsADeclaredRedirect(t *testing.T) {
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		status := http.StatusFound
		if request.URL.Query().Get("shape") == "undeclared" {
			status = http.StatusMovedPermanently
		}
		http.Redirect(writer, request, target.URL+"/blob", status)
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectUnaryOperation([]clientcontract.Transport{connectRestTransport()}, nil, nil)
	operation.Successes = []OperationSuccess{{Status: http.StatusFound}}
	call := func(shape string) *OperationCall {
		input := newConnectCallInput()
		input.Request.Query = map[string]string{"shape": shape}
		return input
	}

	response, err := CallOperationResponse(t.Context(), bound, call("declared"), operation)
	if err != nil || response.StatusCode != http.StatusFound || response.Headers.Get("Location") != target.URL+"/blob" {
		t.Fatalf("declared 302: response=%v err=%v", response, err)
	}
	if _, err := CallOperationResponse(t.Context(), bound, call("undeclared"), operation); err == nil {
		t.Fatal("an undeclared 301 was accepted as the operation's answer")
	}
	if hits := followed.Load(); hits != 0 {
		t.Fatalf("the call followed the redirect %d time(s)", hits)
	}
}

// The plain HTTP transport follows redirects for a hand-written call, and keeps
// the 3xx when the request asks for it, as a generated redirect operation does.
func TestHTTPTransportKeepsARedirectOnlyWhenAsked(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer server.Close()
	transport := NewHTTPTransport(HTTPTransportConfig{BaseURL: server.URL})

	followed, err := transport.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/"})
	if err != nil || followed.StatusCode != http.StatusOK {
		t.Fatalf("followed redirect: response=%v err=%v", followed, err)
	}
	kept, err := transport.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/", KeepRedirect: true})
	if err != nil || kept.StatusCode != http.StatusFound || kept.Headers.Get("Location") != target.URL {
		t.Fatalf("kept redirect: response=%v err=%v", kept, err)
	}
}

func TestOperationAnsweredAcceptsOnlyDeclaredRedirects(t *testing.T) {
	operation := Operation{Successes: []OperationSuccess{{Status: http.StatusFound}, {Status: http.StatusNotModified}}}
	for status, want := range map[int]bool{
		http.StatusOK:               true,
		http.StatusFound:            true,
		http.StatusMovedPermanently: false,
		http.StatusNotModified:      false,
		http.StatusNotFound:         false,
	} {
		if got := operation.answered(&Response{StatusCode: status}); got != want {
			t.Errorf("answered(%d) = %v, want %v", status, got, want)
		}
	}
	if !operation.declaresRedirect() || (Operation{Successes: []OperationSuccess{{Status: http.StatusNotModified}}}).declaresRedirect() {
		t.Error("declaresRedirect must count a 3xx success and never a 304")
	}
}

// An unbound client — no service binding, a plain HTTP transport that follows
// redirects for a hand-written call — keeps a declared redirect too.
func TestUnboundCallKeepsADeclaredRedirect(t *testing.T) {
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/blob", http.StatusFound)
	}))
	defer server.Close()
	unbound, err := NewBuilder().BaseURL(server.URL).Build()
	if err != nil {
		t.Fatal(err)
	}
	operation := Operation{
		ID: "openBlob",
		Contract: clientcontract.OperationV1{
			Stream:     clientcontract.StreamUnary,
			Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/blob", Encoding: clientcontract.EncodingJSON}},
			Security:   clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		},
		Successes: []OperationSuccess{{Status: http.StatusFound}},
	}
	response, err := CallOperationResponse(t.Context(), unbound, &OperationCall{Request: &Request{Method: http.MethodGet, Path: "/blob"}}, operation)
	if err != nil || response.StatusCode != http.StatusFound || response.Headers.Get("Location") != target.URL+"/blob" {
		t.Fatalf("unbound declared 302: response=%v err=%v", response, err)
	}
	if hits := followed.Load(); hits != 0 {
		t.Fatalf("the unbound call followed the redirect %d time(s)", hits)
	}
}
