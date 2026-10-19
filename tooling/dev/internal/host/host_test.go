package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/sdk/extension/hostrpc"
)

const testToken = "launch-token"

// testRemedy stands for the launch a command names when it has no channel.
const testRemedy = "the test command needs an interactive launch by putnami; run `putnami version test`"

// testLines is the answer serveHost gives every version.lines request.
var testLines = wire.VersionLinesResult{
	SourceRevision: "0123456789abcdef0123456789abcdef01234567",
	Lines: []wire.VersionLine{{
		Line: "tooling", TagPattern: "tooling/v{version}", Version: "0.1.0-20260101000000-0123456",
		Release: &wire.LineRelease{Version: "0.1.0", Tag: "tooling/v0.1.0"},
		Commits: []wire.LineCommit{{SHA: "0123456789abcdef0123456789abcdef01234567", Subject: "feat: a"}},
	}},
}

// serveHost answers every request on a Unix socket with testLines, or with a
// refusal when the request carries another token. It returns the socket and
// the number of connections it accepted.
func serveHost(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	// A Unix socket path is limited to about 100 bytes, shorter than a
	// test's temporary directory on macOS.
	dir, err := os.MkdirTemp("/tmp", "pd")
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
	connections := &atomic.Int32{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go answer(conn)
		}
	}()
	return socket, connections
}

// answer serves one connection until it closes.
func answer(conn net.Conn) {
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
			response.Result, _ = json.Marshal(testLines)
		}
		reply, err := wire.EncodeFrame(response)
		if err != nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

func channelFor(socket, token string) *Channel {
	return &Channel{socket: socket, token: token, remedy: testRemedy, connect: hostrpc.Connect}
}

func TestVersionLinesReturnsWhatTheHostAnswersOverOneConnection(t *testing.T) {
	t.Parallel()
	socket, connections := serveHost(t)
	channel := channelFor(socket, testToken)
	defer channel.Close()
	for range 2 {
		got, err := channel.VersionLines(context.Background(), "tooling")
		if err != nil {
			t.Fatal(err)
		}
		if got.SourceRevision != testLines.SourceRevision || len(got.Lines) != 1 || got.Lines[0].Release.Tag != "tooling/v0.1.0" {
			t.Errorf("version lines %+v, want %+v", got, testLines)
		}
	}
	if n := connections.Load(); n != 1 {
		t.Errorf("%d connections, want the one the first call opened", n)
	}
	if err := channel.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := channel.Close(); err != nil {
		t.Errorf("a second close: %v", err)
	}
}

func TestVersionLinesReportsAHostRefusalAsTheWireError(t *testing.T) {
	t.Parallel()
	socket, _ := serveHost(t)
	channel := channelFor(socket, "another-token")
	defer channel.Close()
	_, err := channel.VersionLines(context.Background(), "")
	var refusal *wire.Error
	if !errors.As(err, &refusal) || refusal.Code != wire.CodeUnauthorized {
		t.Fatalf("error %v, want the host's refusal", err)
	}
}

func TestVersionLinesWithoutAChannelNeedsAnInteractiveLaunch(t *testing.T) {
	t.Setenv(wire.SocketEnv, "")
	t.Setenv(wire.TokenEnv, "")
	channel := Take(testRemedy)
	defer channel.Close()
	_, err := channel.VersionLines(context.Background(), "")
	want := testRemedy + " (a hosted run opens no channel; " + wire.SocketEnv + " and " + wire.TokenEnv + " not set)"
	if err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
}

// TestTakeRemovesTheChannelFromTheEnvironment pins that no process the
// command starts, git included, inherits the channel or its token.
func TestTakeRemovesTheChannelFromTheEnvironment(t *testing.T) {
	t.Setenv(wire.SocketEnv, "/tmp/absent.sock")
	t.Setenv(wire.TokenEnv, testToken)
	channel := Take(testRemedy)
	defer channel.Close()
	if channel.socket != "/tmp/absent.sock" || channel.token != testToken {
		t.Errorf("channel %q %q, want the environment's", channel.socket, channel.token)
	}
	for _, name := range []string{wire.SocketEnv, wire.TokenEnv} {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("%s=%q is still in the environment", name, value)
		}
	}
}

// TestLaunchErrorNamesTheRemedyOnlyWhenTheCommandHasOne pins both forms of
// the missing-channel error: with a remedy, the remedy leads and the missing
// variables follow in parentheses; without one, only the missing variables
// remain.
func TestLaunchErrorNamesTheRemedyOnlyWhenTheCommandHasOne(t *testing.T) {
	t.Parallel()
	missing := &hostrpc.NoHostError{Missing: []string{wire.TokenEnv}}
	for _, tc := range []struct {
		remedy, want string
	}{
		{testRemedy, testRemedy + " (a hosted run opens no channel; " + wire.TokenEnv + " not set)"},
		{"", "a hosted run opens no channel; " + wire.TokenEnv + " not set"},
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

func TestVersionLinesReportsAChannelItCannotReach(t *testing.T) {
	t.Parallel()
	channel := channelFor(filepath.Join(t.TempDir(), "absent.sock"), testToken)
	defer channel.Close()
	if _, err := channel.VersionLines(context.Background(), ""); err == nil {
		t.Fatal("a call to a missing socket succeeded")
	}
}
