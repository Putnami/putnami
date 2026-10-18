package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

// testRemedy stands for the launch a command names when it has no channel.
const testRemedy = "the test command needs an interactive launch by putnami; run `putnami change test`"

func callerFor(socket, token string) *Channel {
	return &Channel{socket: socket, token: token, remedy: testRemedy, connect: hostrpc.Connect}
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
	caller := Take(testRemedy)
	defer caller.Close()
	_, err := caller.Call(context.Background(), collab.ContractTasks, collab.OperationFind, nil)
	if err == nil {
		t.Fatal("a call without a channel succeeded")
	}
	want := testRemedy + " (a hosted run opens no channel; " + wire.SocketEnv + " and " + wire.TokenEnv + " not set)"
	if err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
}

// TestLaunchErrorNamesTheRemedyOnlyWhenTheCommandHasOne pins both forms of
// the missing-channel error: with a remedy, the remedy leads and the missing
// variables follow in parentheses; without one, as for change verify, only
// the missing variables remain.
func TestLaunchErrorNamesTheRemedyOnlyWhenTheCommandHasOne(t *testing.T) {
	t.Parallel()
	missing := &hostrpc.NoHostError{Missing: []string{wire.SocketEnv, wire.TokenEnv}}
	for _, tc := range []struct {
		remedy, want string
	}{
		{testRemedy, testRemedy + " (a hosted run opens no channel; " + wire.SocketEnv + " and " + wire.TokenEnv + " not set)"},
		{"", "a hosted run opens no channel; " + wire.SocketEnv + " and " + wire.TokenEnv + " not set"},
	} {
		if got := (&Channel{remedy: tc.remedy}).launchError(missing); got == nil || got.Error() != tc.want {
			t.Errorf("remedy %q: error %v, want %q", tc.remedy, got, tc.want)
		}
	}
	other := errors.New("dial refused")
	if got := (&Channel{}).launchError(other); !errors.Is(got, other) || got.Error() != other.Error() {
		t.Errorf("launchError changed an error that is not a missing channel: %v", got)
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
