package host

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/sdk/extension/hostrpc"

	"go.putnami.dev/tooling/contributor/internal/verify"
)

var restoredRef = features.VerificationReportRef{Project: "/application", Task: "/application:test",
	Path: ".context/verification-report.json", Digest: "sha256:" + strings.Repeat("a", 64), Restored: true}

// reportsFor returns a Channel on socket with token that counts its dials.
func reportsFor(socket, token string, dials *atomic.Int32) *Channel {
	return &Channel{socket: socket, token: token, remedy: testRemedy, connect: func(socket, token string) (*hostrpc.Client, error) {
		dials.Add(1)
		return hostrpc.Connect(socket, token)
	}}
}

// TestTakeRemovesTheChannelFromTheEnvironment pins that the channel leaves
// the process environment before the command starts any process, and that
// the Channel keeps it.
func TestTakeRemovesTheChannelFromTheEnvironment(t *testing.T) {
	t.Setenv(wire.SocketEnv, "/tmp/host.sock")
	t.Setenv(wire.TokenEnv, "launch-token")
	reports := Take(testRemedy)
	for _, name := range []string{wire.SocketEnv, wire.TokenEnv} {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("%s is still set to %q", name, value)
		}
	}
	if reports.socket != "/tmp/host.sock" || reports.token != "launch-token" || reports.remedy != testRemedy {
		t.Errorf("Channel holds %q, %q and %q, want the channel the environment named", reports.socket, reports.token, reports.remedy)
	}
}

// TestRecoverWithoutAChannelIsANoChannelError pins the error the verifier
// turns into the verdict that names the missing channel.
func TestRecoverWithoutAChannelIsANoChannelError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		socket, token string
		missing       []string
	}{
		{"", "", []string{wire.SocketEnv, wire.TokenEnv}},
		{"", testToken, []string{wire.SocketEnv}},
		{"/tmp/host.sock", "", []string{wire.TokenEnv}},
	} {
		var dials atomic.Int32
		_, err := reportsFor(tc.socket, tc.token, &dials).Recover("", restoredRef)
		var noChannel *verify.NoChannelError
		if !errors.As(err, &noChannel) || !slices.Equal(noChannel.Missing, tc.missing) {
			t.Errorf("socket %q, token %q: error %v, want a NoChannelError missing %v", tc.socket, tc.token, err, tc.missing)
		}
		if dials.Load() != 0 {
			t.Errorf("socket %q, token %q: %d dials, want none", tc.socket, tc.token, dials.Load())
		}
	}
}

// TestRecoverReturnsTheReportTheHostAnswers pins one dial for every read of
// one verification, and a Close that can run twice.
func TestRecoverReturnsTheReportTheHostAnswers(t *testing.T) {
	t.Parallel()
	var dials atomic.Int32
	reports := reportsFor(serveHost(t, wire.VerificationReportResult{Report: []byte("report bytes")}), testToken, &dials)
	for range 2 {
		data, err := reports.Recover("", restoredRef)
		if err != nil || string(data) != "report bytes" {
			t.Fatalf("Recover = %q, %v", data, err)
		}
	}
	if dials.Load() != 1 {
		t.Errorf("%d dials, want 1", dials.Load())
	}
	if err := reports.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := reports.Close(); err != nil {
		t.Errorf("a second close: %v", err)
	}
}

// TestRecoverReportsARefusalAsAnError pins that a refusal by the host is an
// error, and not the missing-channel error.
func TestRecoverReportsARefusalAsAnError(t *testing.T) {
	t.Parallel()
	var dials atomic.Int32
	reports := reportsFor(serveHost(t, nil), "another-token", &dials)
	defer reports.Close()
	_, err := reports.Recover("", restoredRef)
	var refusal *wire.Error
	if !errors.As(err, &refusal) || refusal.Code != wire.CodeUnauthorized ||
		!strings.Contains(err.Error(), "verification.report through the putnami host") {
		t.Fatalf("error %v, want the host's refusal naming the call", err)
	}
	var noChannel *verify.NoChannelError
	if errors.As(err, &noChannel) {
		t.Errorf("a refusal reads as a missing channel: %v", err)
	}
}
