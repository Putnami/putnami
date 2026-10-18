package host

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/sdk/extension/hostrpc"
)

const testToken = "launch-token"

// serveHost answers every request on a Unix socket with result, or with a
// refusal when the request carries another token. It returns the socket.
func serveHost(t *testing.T, result any) string {
	t.Helper()
	// A Unix socket path is limited to about 100 bytes, shorter than a
	// test's temporary directory on macOS.
	dir, err := os.MkdirTemp("/tmp", "ph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "host.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
					if err != nil {
						return
					}
					var request wire.Request
					if json.Unmarshal(frame, &request) != nil {
						return
					}
					response := wire.Response{JSONRPC: wire.JSONRPCVersion, ID: request.ID}
					if request.Token != testToken {
						response.Error = &wire.Error{Code: wire.CodeUnauthorized, Message: "refused"}
					} else {
						response.Result, _ = json.Marshal(result)
					}
					reply, err := wire.EncodeFrame(response)
					if err != nil {
						return
					}
					if _, err := conn.Write(reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	return socket
}

func callerFor(socket, token string) *Caller {
	return &Caller{dial: func() (*hostrpc.Client, error) { return hostrpc.Connect(socket, token) }}
}

func TestCallReturnsTheEnvelopeTheHostAnswers(t *testing.T) {
	t.Parallel()
	want := collab.Envelope{Contract: collab.ContractTasks, Operation: collab.OperationFind, Outcome: collab.OutcomeOK, Result: json.RawMessage(`{"items":[]}`)}
	caller := callerFor(serveHost(t, want), testToken)
	defer caller.Close()
	for range 2 {
		got, err := caller.Call(context.Background(), collab.ContractTasks, collab.OperationFind, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != collab.OutcomeOK || got.Contract != want.Contract || string(got.Result) != string(want.Result) {
			t.Errorf("envelope %+v, want %+v", got, want)
		}
	}
	if err := caller.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := caller.Close(); err != nil {
		t.Errorf("a second close: %v", err)
	}
}

func TestCallReportsAHostRefusalAsAnError(t *testing.T) {
	t.Parallel()
	caller := callerFor(serveHost(t, nil), "another-token")
	defer caller.Close()
	_, err := caller.Call(context.Background(), collab.ContractTasks, collab.OperationFind, nil)
	if err == nil || !strings.Contains(err.Error(), "tasks find through the putnami host") {
		t.Fatalf("error %v, want the refusal naming the call", err)
	}
}

func TestCallReportsAnAnswerThatIsNotAnEnvelope(t *testing.T) {
	t.Parallel()
	caller := callerFor(serveHost(t, "not an envelope"), testToken)
	defer caller.Close()
	_, err := caller.Call(context.Background(), collab.ContractTasks, collab.OperationFind, nil)
	if err == nil || !strings.Contains(err.Error(), "not an envelope") {
		t.Fatalf("error %v, want the decode failure", err)
	}
}

func TestCallWithoutAChannelNeedsAnInteractiveLaunch(t *testing.T) {
	t.Setenv(wire.SocketEnv, "")
	t.Setenv(wire.TokenEnv, "")
	caller := NewCaller()
	defer caller.Close()
	_, err := caller.Call(context.Background(), collab.ContractTasks, collab.OperationFind, nil)
	if err == nil {
		t.Fatal("a call without a channel succeeded")
	}
	for _, want := range []string{"needs an interactive launch by putnami", "putnami change language tasks", wire.SocketEnv, wire.TokenEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

func TestCallReportsAChannelItCannotReach(t *testing.T) {
	t.Parallel()
	caller := callerFor(filepath.Join(t.TempDir(), "absent.sock"), testToken)
	defer caller.Close()
	if _, err := caller.Call(context.Background(), collab.ContractTasks, collab.OperationFind, nil); err == nil {
		t.Fatal("a call to a missing socket succeeded")
	}
}
