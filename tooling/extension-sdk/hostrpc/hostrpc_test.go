package hostrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	feature     = "tooling/extension-authoring"
	requirement = "host-rpc-client"
	// ownersRequirement is the requirement of the workspace.owners client.
	ownersRequirement = "host-rpc-workspace-owners-client"
	testToken         = "test-token"
)

// fakeHost serves one end of a pipe: answer receives each request and returns
// the frame to write back, or nil to write nothing.
func fakeHost(t *testing.T, answer func(wire.Request) []byte) *Client {
	t.Helper()
	clientConn, hostConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = hostConn.Close()
	})
	go func() {
		reader := bufio.NewReader(hostConn)
		for {
			frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
			if err != nil {
				return
			}
			var request wire.Request
			if err := json.Unmarshal(frame, &request); err != nil {
				return
			}
			if reply := answer(request); reply != nil {
				if _, err := hostConn.Write(reply); err != nil {
					return
				}
			}
		}
	}()
	return newClient(clientConn, testToken)
}

func result(t *testing.T, request wire.Request, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Error(err)
		return nil
	}
	frame, err := wire.EncodeFrame(wire.Response{JSONRPC: wire.JSONRPCVersion, ID: request.ID, Result: encoded})
	if err != nil {
		t.Error(err)
		return nil
	}
	return frame
}

func TestDialWithoutAChannelNamesPutnamiAndTheMissingVariables(t *testing.T) {
	spectest.Proves(t, feature, requirement, "dial-without-a-channel-names-putnami-and-the-missing-variables")
	for _, tc := range []struct {
		socket, token string
		missing       []string
	}{
		{"", "", []string{wire.SocketEnv, wire.TokenEnv}},
		{"/nowhere/host.sock", "", []string{wire.TokenEnv}},
		{"", "abc", []string{wire.SocketEnv}},
	} {
		t.Setenv(wire.SocketEnv, tc.socket)
		t.Setenv(wire.TokenEnv, tc.token)
		_, err := Dial()
		var noHost *NoHostError
		if !errors.As(err, &noHost) {
			t.Fatalf("Dial = %v, want a *NoHostError", err)
		}
		if strings.Join(noHost.Missing, ",") != strings.Join(tc.missing, ",") {
			t.Errorf("Missing = %v, want %v", noHost.Missing, tc.missing)
		}
		message := err.Error()
		for _, want := range append([]string{"putnami", "interactive"}, tc.missing...) {
			if !strings.Contains(message, want) {
				t.Errorf("the error %q does not name %q", message, want)
			}
		}
	}
}

func TestDialSaysWhyTheSocketIsUnreachable(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	t.Setenv(wire.SocketEnv, socket)
	t.Setenv(wire.TokenEnv, "abc")
	_, err := Dial()
	if err == nil || !strings.Contains(err.Error(), socket) {
		t.Fatalf("Dial = %v, want an error naming %s", err, socket)
	}
}

func TestEveryRequestCarriesTheTokenAndAFreshID(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, requirement, "every-request-carries-the-token-and-a-fresh-id")
	requests := make(chan wire.Request, 2)
	client := fakeHost(t, func(request wire.Request) []byte {
		requests <- request
		return result(t, request, wire.HelloResult{Protocol: wire.ProtocolVersion, Methods: wire.Methods()})
	})
	for range 2 {
		hello, err := client.Hello(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if hello.Protocol != wire.ProtocolVersion || !slices.Equal(hello.Methods, wire.Methods()) {
			t.Fatalf("Hello = %+v", hello)
		}
	}
	first, second := <-requests, <-requests
	for _, request := range []wire.Request{first, second} {
		if request.Token != testToken || request.JSONRPC != wire.JSONRPCVersion || request.Method != wire.MethodHello {
			t.Errorf("request %+v does not carry the token, the version and the method", request)
		}
		if len(request.Params) != 0 {
			t.Errorf("host.hello sent params %s", request.Params)
		}
	}
	if string(first.ID) == string(second.ID) {
		t.Errorf("two requests share the id %s", first.ID)
	}
}

func TestCallCollaborationReturnsTheEnvelopeBytes(t *testing.T) {
	t.Parallel()
	envelope := `{"contract":"tasks","operation":"find","outcome":"ok"}`
	client := fakeHost(t, func(request wire.Request) []byte {
		var params wire.CollaborationCallParams
		if err := wire.DecodeStrict(request.Params, &params); err != nil ||
			params.Contract != "tasks" || params.Operation != "find" || string(params.Input) != `{"q":1}` {
			t.Errorf("params %s, %v", request.Params, err)
		}
		frame, _ := wire.EncodeFrame(wire.Response{JSONRPC: wire.JSONRPCVersion, ID: request.ID, Result: json.RawMessage(envelope)})
		return frame
	})
	got, err := client.CallCollaboration(t.Context(), "tasks", "find", json.RawMessage(`{"q":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != envelope {
		t.Fatalf("envelope %s, want %s", got, envelope)
	}
}

func TestVerificationReportDecodesTheReportBytes(t *testing.T) {
	t.Parallel()
	ref := wire.VerificationReportParams{Project: "p", Task: "p:test", Path: "out/report.xml", Digest: "sha256:00"}
	client := fakeHost(t, func(request wire.Request) []byte {
		var params wire.VerificationReportParams
		if err := wire.DecodeStrict(request.Params, &params); err != nil || params != ref {
			t.Errorf("params %s, %v", request.Params, err)
		}
		return result(t, request, wire.VerificationReportResult{Report: []byte("<report/>")})
	})
	report, err := client.VerificationReport(t.Context(), ref)
	if err != nil || string(report) != "<report/>" {
		t.Fatalf("VerificationReport = %q, %v", report, err)
	}
}

func TestWorkspaceOwnersReturnsTheHostsEntriesInRequestOrder(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, ownersRequirement, "workspace-owners-returns-the-hosts-entries-in-request-order")
	paths := []string{"b/x.go", "a.go", "b/x.go"}
	app := wire.OwningProject{ID: "/app", Path: "app"}
	client := fakeHost(t, func(request wire.Request) []byte {
		var params wire.WorkspaceOwnersParams
		if err := wire.DecodeStrict(request.Params, &params); err != nil || request.Method != wire.MethodWorkspaceOwners ||
			!slices.Equal(params.Paths, paths) {
			t.Errorf("%s params %s, %v", request.Method, request.Params, err)
		}
		return result(t, request, wire.WorkspaceOwnersResult{Owners: []wire.PathOwners{
			{Path: "b/x.go", Projects: []wire.OwningProject{app}},
			{Path: "a.go", Projects: []wire.OwningProject{}},
			{Path: "b/x.go", Projects: []wire.OwningProject{app}},
		}})
	})
	owners, err := client.WorkspaceOwners(t.Context(), paths)
	if err != nil || len(owners) != 3 {
		t.Fatalf("WorkspaceOwners = %+v, %v, want three entries", owners, err)
	}
	for i, path := range paths {
		if owners[i].Path != path {
			t.Errorf("entry %d is for %q, want %q", i, owners[i].Path, path)
		}
	}
	if len(owners[0].Projects) != 1 || owners[0].Projects[0] != app || len(owners[1].Projects) != 0 {
		t.Errorf("owners = %+v", owners)
	}
}

func TestWorkspaceOwnersRefusesWhatTheHostWouldRefuseWithoutSendingAnything(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, ownersRequirement, "workspace-owners-refuses-what-the-host-would-refuse-without-sending")
	received := make(chan wire.Request, 2)
	client := fakeHost(t, func(request wire.Request) []byte {
		received <- request
		return result(t, request, wire.HelloResult{Protocol: wire.ProtocolVersion, Methods: wire.Methods()})
	})
	for _, paths := range [][]string{nil, {}, {"a.go", "../a"}, {"."}, {"/a"}, {"a/"}} {
		if owners, err := client.WorkspaceOwners(t.Context(), paths); err == nil {
			t.Errorf("WorkspaceOwners(%q) = %+v, want a refusal", paths, owners)
		}
	}
	if _, err := client.Hello(t.Context()); err != nil {
		t.Fatalf("a refusal before sending broke the client: %v", err)
	}
	if request := <-received; request.Method != wire.MethodHello || string(request.ID) != "1" || len(received) != 0 {
		t.Fatalf("the host received %s as request %s and %d more, want only host.hello as request 1", request.Method, request.ID, len(received))
	}
}

func TestWorkspaceOwnersRejectsAnAnswerForOtherPathsAndStaysUsable(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, ownersRequirement, "workspace-owners-rejects-an-answer-for-other-paths-and-stays-usable")
	for name, owners := range map[string][]wire.PathOwners{
		"too few entries":  {{Path: "a.go", Projects: []wire.OwningProject{}}},
		"too many entries": {{Path: "a.go"}, {Path: "b.go"}, {Path: "c.go"}},
		"another path":     {{Path: "a.go"}, {Path: "c.go"}},
		"swapped paths":    {{Path: "b.go"}, {Path: "a.go"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := fakeHost(t, func(request wire.Request) []byte {
				if request.Method == wire.MethodHello {
					return result(t, request, wire.HelloResult{Protocol: wire.ProtocolVersion, Methods: wire.Methods()})
				}
				return result(t, request, wire.WorkspaceOwnersResult{Owners: owners})
			})
			if got, err := client.WorkspaceOwners(t.Context(), []string{"a.go", "b.go"}); err == nil {
				t.Fatalf("WorkspaceOwners accepted %+v", got)
			}
			if _, err := client.Hello(t.Context()); err != nil {
				t.Fatalf("the call after the rejected answer = %v, want the client usable", err)
			}
		})
	}
}

func TestAHostRefusalIsATypedError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, requirement, "a-host-refusal-is-a-typed-error")
	client := fakeHost(t, func(request wire.Request) []byte {
		// JSON-RPC lets an error carry data, which this client ignores.
		return []byte(`{"jsonrpc":"2.0","id":` + string(request.ID) +
			`,"error":{"code":-32003,"message":"no producer","data":"ignored"}}` + "\n")
	})
	_, err := client.VerificationReport(t.Context(), wire.VerificationReportParams{Project: "p", Task: "t", Path: "x", Digest: "d"})
	var refusal *wire.Error
	if !errors.As(err, &refusal) || refusal.Code != wire.CodeNotVerified || refusal.Message != "no producer" {
		t.Fatalf("error = %v, want a *wire.Error with code %d", err, wire.CodeNotVerified)
	}
	// A refusal leaves the connection usable.
	if _, err := client.Hello(t.Context()); !errors.As(err, &refusal) {
		t.Fatalf("the call after a refusal = %v, want the host's next answer", err)
	}
}

func TestAnAnswerTheClientCannotTrustBreaksIt(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, requirement, "an-answer-the-client-cannot-trust-breaks-it")
	cases := []struct {
		name   string
		answer func(wire.Request) []byte
	}{
		{name: "another request's id", answer: func(wire.Request) []byte {
			return []byte(`{"jsonrpc":"2.0","id":"other","result":{}}` + "\n")
		}},
		{name: "not JSON", answer: func(wire.Request) []byte {
			return []byte("not json\n")
		}},
		{name: "over the frame limit", answer: func(wire.Request) []byte {
			return []byte(strings.Repeat("x", wire.MaxFrameBytes+1))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := fakeHost(t, tc.answer)
			_, err := client.Hello(t.Context())
			if err == nil {
				t.Fatal("the client accepted the answer")
			}
			if tc.name == "over the frame limit" && !errors.Is(err, wire.ErrFrameTooLarge) {
				t.Errorf("error = %v, want ErrFrameTooLarge", err)
			}
			if _, again := client.Hello(t.Context()); again == nil || again.Error() != err.Error() {
				t.Errorf("the next call = %v, want the broken client's error %v", again, err)
			}
		})
	}
}

func TestAnEndedContextBreaksTheClient(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, requirement, "an-answer-the-client-cannot-trust-breaks-it")
	received := make(chan struct{})
	client := fakeHost(t, func(wire.Request) []byte {
		close(received)
		return nil // never answers
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.Hello(ctx)
		done <- err
	}()
	<-received
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Hello = %v, want context.Canceled", err)
	}
	if _, again := client.Hello(t.Context()); !errors.Is(again, context.Canceled) {
		t.Fatalf("the next call = %v, want the broken client's error", again)
	}
}

// endsOnAnswer is a client connection that ends the call's context once the
// host's whole answer is read, and hands the answer over only after the
// client's context watcher has moved the deadline into the past.
type endsOnAnswer struct {
	net.Conn
	end     context.CancelFunc
	pastSet chan struct{}
	once    sync.Once
}

func (c *endsOnAnswer) SetDeadline(deadline time.Time) error {
	err := c.Conn.SetDeadline(deadline)
	if !deadline.IsZero() && deadline.Before(time.Now()) {
		c.once.Do(func() { close(c.pastSet) })
	}
	return err
}

func (c *endsOnAnswer) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.end != nil && bytes.IndexByte(p[:n], '\n') >= 0 {
		end := c.end
		c.end = nil
		end()
		<-c.pastSet
	}
	return n, err
}

func TestAnAnswerThatArrivedWholeStandsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	clientConn, hostConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = hostConn.Close()
	})
	go func() {
		reader := bufio.NewReader(hostConn)
		for {
			frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
			if err != nil {
				return
			}
			var request wire.Request
			if err := json.Unmarshal(frame, &request); err != nil {
				return
			}
			if _, err := hostConn.Write(result(t, request, wire.HelloResult{Protocol: wire.ProtocolVersion, Methods: wire.Methods()})); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newClient(&endsOnAnswer{Conn: clientConn, end: cancel, pastSet: make(chan struct{})}, testToken)
	if hello, err := client.Hello(ctx); err != nil || hello.Protocol != wire.ProtocolVersion {
		t.Fatalf("Hello = %+v, %v, want the answer that arrived before the context ended", hello, err)
	}
	if ctx.Err() == nil {
		t.Fatal("the context did not end during the call")
	}
	if _, err := client.Hello(t.Context()); err != nil {
		t.Fatalf("the next call = %v, want the host's answer", err)
	}
}

func TestACallWithAnEndedContextSendsNothing(t *testing.T) {
	t.Parallel()
	received := make(chan wire.Request, 2)
	client := fakeHost(t, func(request wire.Request) []byte {
		received <- request
		return result(t, request, wire.HelloResult{Protocol: wire.ProtocolVersion, Methods: wire.Methods()})
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Hello(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Hello = %v, want context.Canceled", err)
	}
	if _, err := client.Hello(t.Context()); err != nil {
		t.Fatalf("a call refused before it was sent broke the client: %v", err)
	}
	if request := <-received; string(request.ID) != "1" || len(received) != 0 {
		t.Fatalf("the host received request %s and %d more, want only request 1", request.ID, len(received))
	}
}
