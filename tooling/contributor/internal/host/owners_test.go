package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// TestOneChannelServesEveryCallOnOneConnection pins one connection for a
// run: a collaboration call, an owners query and a report read share the
// connection the first of them dialed.
func TestOneChannelServesEveryCallOnOneConnection(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "tooling/contributor-executable", "change-finalize-parity", "one-host-channel-serves-the-run")
	// One answer decodes as each of the three results.
	answer := map[string]any{"contract": "tasks", "operation": "find", "outcome": "ok", "result": map[string]any{},
		"report": []byte("report bytes"), "owners": []wire.PathOwners{{Path: "a.go", Projects: []wire.OwningProject{{ID: "/a", Path: "."}}}}}
	var dials atomic.Int32
	channel := reportsFor(serveHost(t, answer), testToken, &dials)
	defer channel.Close()
	if envelope, err := channel.Call(context.Background(), collab.ContractTasks, collab.OperationFind, nil); err != nil || envelope.Outcome != collab.OutcomeOK {
		t.Fatalf("Call = %+v, %v", envelope, err)
	}
	owners, err := channel.Owners(context.Background(), []string{"a.go"})
	if err != nil || len(owners) != 1 || owners[0].Path != "a.go" || !slices.Equal(owners[0].Projects, []wire.OwningProject{{ID: "/a", Path: "."}}) {
		t.Fatalf("Owners = %+v, %v", owners, err)
	}
	if report, err := channel.Recover("", restoredRef); err != nil || string(report) != "report bytes" {
		t.Fatalf("Recover = %q, %v", report, err)
	}
	if dials.Load() != 1 {
		t.Errorf("%d dials, want 1", dials.Load())
	}
}

// TestOwnersWithoutAChannelNeedsAnInteractiveLaunch pins that an owners
// query with no channel names the launch that opens one, and never dials.
func TestOwnersWithoutAChannelNeedsAnInteractiveLaunch(t *testing.T) {
	t.Parallel()
	var dials atomic.Int32
	_, err := reportsFor("", testToken, &dials).Owners(context.Background(), []string{"a.go"})
	if want := testRemedy + " (a hosted run opens no channel; " + wire.SocketEnv + " not set)"; err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
	if dials.Load() != 0 {
		t.Errorf("%d dials, want none", dials.Load())
	}
}

// TestOwnersReportsAHostRefusal pins that the host's refusal reaches the
// caller as the wire error, with the call named.
func TestOwnersReportsAHostRefusal(t *testing.T) {
	t.Parallel()
	var dials atomic.Int32
	channel := reportsFor(serveHost(t, nil), "another-token", &dials)
	defer channel.Close()
	_, err := channel.Owners(context.Background(), []string{"a.go"})
	var refusal *wire.Error
	if !errors.As(err, &refusal) || refusal.Code != wire.CodeUnauthorized ||
		!strings.HasPrefix(err.Error(), "workspace.owners through the putnami host: ") {
		t.Fatalf("error %v, want the host's refusal naming the call", err)
	}
}
