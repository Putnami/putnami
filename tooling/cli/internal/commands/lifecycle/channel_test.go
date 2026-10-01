package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// fakeChannelClient records every provider call a channel command makes, so a
// test can assert that promotion uploads nothing and touches the provider once.
type fakeChannelClient struct {
	heads          map[string]*distribution.ChannelHead
	setRequests    []distribution.ChannelSetRequest
	statusRequests []distribution.ChannelStatusRequest
	resolveCalls   int
	// resolveNamespaces records the namespace of every resolve, so a test can
	// prove the compare-and-swap expectation is read where the move writes.
	resolveNamespaces []string
	statuses          []*distribution.ChannelStatusResponse
	outcome           distribution.ReleaseOutcome
	// setErr makes the provider fail, which is how the host's own message is
	// asserted without a provider process.
	setErr error
}

func (client *fakeChannelClient) Resolve(_ context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	client.resolveCalls++
	client.resolveNamespaces = append(client.resolveNamespaces, request.Namespace)
	heads := make(map[string]*distribution.ChannelHead, len(request.Channels))
	for _, name := range request.Channels {
		heads[name] = client.heads[name]
	}
	return &distribution.ResolveResponse{ProtocolVersion: distribution.ProtocolVersion, Heads: heads}, nil
}

func (client *fakeChannelClient) ChannelSet(_ context.Context, request *distribution.ChannelSetRequest) (*distribution.ReleaseResponse, error) {
	client.setRequests = append(client.setRequests, *request)
	if client.setErr != nil {
		return nil, client.setErr
	}
	outcome := client.outcome
	if outcome == "" {
		outcome = distribution.ReleaseOutcomeReleased
	}
	return &distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         outcome,
		Current: map[string]*distribution.ChannelHead{
			request.Channel: {Ref: channelRef('c'), Generation: 8},
		},
	}, nil
}

func (client *fakeChannelClient) ChannelStatus(_ context.Context, request *distribution.ChannelStatusRequest) (*distribution.ChannelStatusResponse, error) {
	client.statusRequests = append(client.statusRequests, *request)
	index := min(len(client.statusRequests)-1, len(client.statuses)-1)
	return client.statuses[index], nil
}

func channelRef(fill byte) distribution.ReleaseSetRef {
	hex := strings.Repeat(string(fill), 64)
	return distribution.ReleaseSetRef{ID: "rs_" + hex, Digest: "sha256:" + hex}
}

// useChannelClient replaces the PROVIDER seam and returns the fake. The
// namespace is deliberately NOT part of this double: it is derived from the
// repository by channelNamespace, and the tests below exercise that derivation
// for real against a temporary workspace root.
func useChannelClient(t *testing.T, client *fakeChannelClient) *fakeChannelClient {
	t.Helper()
	previous := newChannelClient
	newChannelClient = func(context.Context, string, *wsproto.Config) (channelClient, error) {
		return client, nil
	}
	t.Cleanup(func() { newChannelClient = previous })
	return client
}

// channelWorkspaceConfig is the loaded workspace config these commands receive
// from App.Run (wsproto.Load), reduced to the one member the namespace fallback
// reads.
func channelWorkspaceConfig() *wsproto.Config {
	return &wsproto.Config{Name: "putnami"}
}

// D2: promotion and rollback are metadata-only. `channel set` reaches the
// provider exactly once, with the source it was given and the target channel's
// current head as its compare-and-swap expectation. No publisher runs.
func TestChannelSetCallsTheProviderOnce(t *testing.T) {
	spectest.Proves(t, "cli/channels", "channel-set-is-metadata-only", "channel-set-calls-the-provider-once")
	client := useChannelClient(t, &fakeChannelClient{
		heads: map[string]*distribution.ChannelHead{"latest": {Ref: channelRef('a'), Generation: 7}},
	})
	output, err := captureChannelOutput(t, func() error {
		return ChannelSet(context.Background(), t.TempDir(), channelWorkspaceConfig(), "latest", ChannelSetFlags{From: "canary"})
	})
	if err != nil {
		t.Fatalf("ChannelSet = %v", err)
	}
	if len(client.setRequests) != 1 {
		t.Fatalf("channel-set calls = %d, want exactly one", len(client.setRequests))
	}
	request := client.setRequests[0]
	if request.Channel != "latest" || request.From.Channel != "canary" || request.From.ReleaseID != "" {
		t.Fatalf("channel-set request = %+v", request)
	}
	if request.Expected == nil || request.Expected.ID != channelRef('a').ID {
		t.Fatalf("expectation = %+v, want the channel's current head", request.Expected)
	}
	if client.resolveCalls != 1 {
		t.Fatalf("resolve calls = %d, want one for the expectation", client.resolveCalls)
	}
	if !strings.Contains(output, "generation 8") {
		t.Fatalf("output = %q, want the new head and generation", output)
	}
}

// An rs_ id names the immutable set directly, and --expected replaces the
// resolve entirely, so an unattended move states what it believed.
func TestChannelSetTakesAnIdAndAnExplicitExpectation(t *testing.T) {
	client := useChannelClient(t, &fakeChannelClient{})
	source := channelRef('b').ID
	expected := channelRef('a').ID
	if _, err := captureChannelOutput(t, func() error {
		return ChannelSet(context.Background(), t.TempDir(), channelWorkspaceConfig(), "latest",
			ChannelSetFlags{From: source, Expected: expected})
	}); err != nil {
		t.Fatalf("ChannelSet = %v", err)
	}
	request := client.setRequests[0]
	if request.From.ReleaseID != source || request.From.Channel != "" {
		t.Fatalf("source = %+v, want the immutable id", request.From)
	}
	if request.Expected == nil || request.Expected.ID != expected {
		t.Fatalf("expectation = %+v, want the stated id", request.Expected)
	}
	if client.resolveCalls != 0 {
		t.Fatalf("resolve calls = %d, want none when --expected is given", client.resolveCalls)
	}
}

// Both commands refuse a name outside the portable alphabet before any
// provider call, and `set` refuses a missing source.
func TestChannelCommandsRefuseUnusableArguments(t *testing.T) {
	client := useChannelClient(t, &fakeChannelClient{})
	root := t.TempDir()
	cfg := channelWorkspaceConfig()
	if err := ChannelSet(context.Background(), root, cfg, "Latest", ChannelSetFlags{From: "canary"}); err == nil {
		t.Fatal("a non-portable channel name was accepted")
	}
	if err := ChannelSet(context.Background(), root, cfg, "latest", ChannelSetFlags{}); err == nil ||
		!strings.Contains(err.Error(), "--from") {
		t.Fatalf("channel set without --from = %v, want a usage refusal", err)
	}
	if err := ChannelStatus(context.Background(), root, cfg, "Latest", ChannelStatusFlags{}); err == nil {
		t.Fatal("channel status accepted a non-portable name")
	}
	if len(client.setRequests) != 0 || len(client.statusRequests) != 0 {
		t.Fatal("a refused argument still reached the provider")
	}
}

// D32: a registry behind the accepted generation is reported and exits
// non-zero, so a pipeline waits for a channel to be served instead of
// assuming it.
func TestChannelStatusExitsOneWhenBehind(t *testing.T) {
	behind := &distribution.ChannelStatusResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Desired:         &distribution.ChannelHead{Ref: channelRef('a'), Generation: 7},
		Observed:        map[string]uint64{"npm": 7, "oci": 6},
	}
	useChannelClient(t, &fakeChannelClient{statuses: []*distribution.ChannelStatusResponse{behind}})
	output, err := captureChannelOutput(t, func() error {
		return ChannelStatus(context.Background(), t.TempDir(), channelWorkspaceConfig(), "latest", ChannelStatusFlags{})
	})
	if err == nil || !strings.Contains(err.Error(), "oci") {
		t.Fatalf("behind status = %v, want a failure naming the registry", err)
	}
	if !strings.Contains(output, "desired "+channelRef('a').ID) || !strings.Contains(output, "behind") {
		t.Fatalf("output = %q, want desired versus observed", output)
	}
}

// --wait polls until every registry has applied the accepted head.
func TestChannelStatusWaitsUntilEveryRegistryConverges(t *testing.T) {
	behind := &distribution.ChannelStatusResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Desired:         &distribution.ChannelHead{Ref: channelRef('a'), Generation: 7},
		Observed:        map[string]uint64{"npm": 7, "oci": 6},
	}
	converged := &distribution.ChannelStatusResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Desired:         &distribution.ChannelHead{Ref: channelRef('a'), Generation: 7},
		Observed:        map[string]uint64{"npm": 7, "oci": 7},
	}
	client := useChannelClient(t, &fakeChannelClient{statuses: []*distribution.ChannelStatusResponse{behind, converged}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := captureChannelOutput(t, func() error {
		return ChannelStatus(ctx, t.TempDir(), channelWorkspaceConfig(), "latest", ChannelStatusFlags{Wait: 20 * time.Second})
	}); err != nil {
		t.Fatalf("converging status = %v, want success", err)
	}
	if len(client.statusRequests) != 2 {
		t.Fatalf("status calls = %d, want one more after the first report", len(client.statusRequests))
	}
}

// A channel no registry reports on is named rather than counted as converged.
func TestChannelStatusReportsAnAbsentSubscriber(t *testing.T) {
	response := &distribution.ChannelStatusResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Desired:         &distribution.ChannelHead{Ref: channelRef('a'), Generation: 7},
	}
	useChannelClient(t, &fakeChannelClient{statuses: []*distribution.ChannelStatusResponse{response}})
	output, err := captureChannelOutput(t, func() error {
		return ChannelStatus(context.Background(), t.TempDir(), channelWorkspaceConfig(), "latest", ChannelStatusFlags{})
	})
	if err == nil || !strings.Contains(err.Error(), "any registry") {
		t.Fatalf("status without a subscriber = %v, want an unconverged failure", err)
	}
	if !strings.Contains(output, "no registry reports") {
		t.Fatalf("output = %q, want the absent subscriber named", output)
	}
}

func captureChannelOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var runErr error
	output, err := iox.CaptureStdout(func() error {
		runErr = fn()
		return nil
	})
	if err != nil {
		t.Fatalf("capture stdout: %v", err)
	}
	return output, runErr
}

// The namespace a channel command addresses is the PUBLISHER's, not the
// workspace's local name. A large consumer workspace is the case that made this a defect: its
// workspace is named `consumer` while every publish writes to the declared
// `cloud`, so `channel set stable --from rs_…` asked the provider about a
// namespace nothing had ever written and could only fail.
func TestChannelCommandsAddressTheDeclaredDistributionNamespace(t *testing.T) {
	client := useChannelClient(t, &fakeChannelClient{
		heads: map[string]*distribution.ChannelHead{"stable": {Ref: channelRef('a'), Generation: 3}},
		statuses: []*distribution.ChannelStatusResponse{{
			ProtocolVersion: distribution.ProtocolVersion,
			Desired:         &distribution.ChannelHead{Ref: channelRef('a'), Generation: 3},
			Observed:        map[string]uint64{"npm": 3},
		}},
	})
	cfg := channelWorkspaceConfig()
	root := writeChannelDistributionPolicy(t, `"namespace": "cloud"`)

	if _, err := captureChannelOutput(t, func() error {
		return ChannelSet(context.Background(), root, cfg, "stable", ChannelSetFlags{From: channelRef('b').ID})
	}); err != nil {
		t.Fatalf("ChannelSet = %v", err)
	}
	if _, err := captureChannelOutput(t, func() error {
		return ChannelStatus(context.Background(), root, cfg, "stable", ChannelStatusFlags{})
	}); err != nil {
		t.Fatalf("ChannelStatus = %v", err)
	}

	if got := client.setRequests[0].Namespace; got != "cloud" {
		t.Errorf("channel-set namespace = %q, want the declared distribution.namespace", got)
	}
	if got := client.statusRequests[0].Namespace; got != "cloud" {
		t.Errorf("channel-status namespace = %q, want the declared distribution.namespace", got)
	}
	// The exact regression: the workspace name is a DIFFERENT string and must
	// not be what either command asked about.
	if client.setRequests[0].Namespace == cfg.Name || client.statusRequests[0].Namespace == cfg.Name {
		t.Errorf("a channel command fell back to the workspace name %q", cfg.Name)
	}
}

// The compare-and-swap expectation is read from the same namespace the move
// writes to. Resolving it anywhere else would compare a head from one namespace
// against a channel in another, which is the one mistake a CAS cannot catch.
func TestChannelSetResolvesTheExpectationInTheDeclaredNamespace(t *testing.T) {
	client := useChannelClient(t, &fakeChannelClient{
		heads: map[string]*distribution.ChannelHead{"stable": {Ref: channelRef('a'), Generation: 3}},
	})
	root := writeChannelDistributionPolicy(t, `"namespace": "cloud"`)
	if _, err := captureChannelOutput(t, func() error {
		return ChannelSet(context.Background(), root, channelWorkspaceConfig(), "stable", ChannelSetFlags{From: "canary"})
	}); err != nil {
		t.Fatalf("ChannelSet = %v", err)
	}
	if client.resolveNamespaces[0] != "cloud" {
		t.Fatalf("expectation resolved in namespace %q, want cloud", client.resolveNamespaces[0])
	}
}

// A repository that declares no distribution policy keeps the workspace name.
// This is the fallback the publisher applies (releaseSetNamespace), so the two
// stay one rule rather than two that agree today.
func TestChannelCommandsFallBackToTheWorkspaceName(t *testing.T) {
	client := useChannelClient(t, &fakeChannelClient{})
	cfg := channelWorkspaceConfig()
	if _, err := captureChannelOutput(t, func() error {
		return ChannelSet(context.Background(), t.TempDir(), cfg, "latest", ChannelSetFlags{From: channelRef('b').ID})
	}); err != nil {
		t.Fatalf("ChannelSet = %v", err)
	}
	if got := client.setRequests[0].Namespace; got != cfg.Name {
		t.Fatalf("namespace without a policy = %q, want the workspace name %q", got, cfg.Name)
	}
}

// An unreadable policy stops the command instead of guessing: a move published
// into the wrong namespace is not recoverable by a compare-and-swap.
func TestChannelCommandsRefuseAnInvalidDistributionPolicy(t *testing.T) {
	client := useChannelClient(t, &fakeChannelClient{})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("write %s: %v", ciproto.Filename, err)
	}
	err := ChannelSet(context.Background(), root, channelWorkspaceConfig(), "latest", ChannelSetFlags{From: "canary"})
	if err == nil {
		t.Fatal("an invalid distribution policy was accepted")
	}
	if len(client.setRequests) != 0 || client.resolveCalls != 0 {
		t.Fatal("a refused policy still reached the provider")
	}
}

// A workspace with neither a declared namespace nor a name has nothing to
// address. Naming that here beats the provider's opaque non-zero exit, which is
// all an operator saw when the namespace was wrong.
func TestChannelCommandsRefuseAnEmptyNamespace(t *testing.T) {
	useChannelClient(t, &fakeChannelClient{})
	err := ChannelStatus(context.Background(), t.TempDir(), &wsproto.Config{}, "latest", ChannelStatusFlags{})
	if err == nil || !strings.Contains(err.Error(), ciproto.Filename) {
		t.Fatalf("empty namespace = %v, want a refusal naming %s", err, ciproto.Filename)
	}
}

// A provider failure names the namespace and channel that were asked for. The
// provider's stderr stays out (it is human-owned and may carry credentials), and
// the distribution protocol defines no exit-status vocabulary to translate — so
// the actionable part is the request, and the namespace is the part of it nobody
// typed.
func TestChannelProviderFailureNamesTheNamespace(t *testing.T) {
	useChannelClient(t, &fakeChannelClient{setErr: errors.New("release-set provider channel-set failed: exit status 4")})
	root := writeChannelDistributionPolicy(t, `"namespace": "cloud"`)
	_, err := captureChannelOutput(t, func() error {
		return ChannelSet(context.Background(), root, channelWorkspaceConfig(), "stable",
			ChannelSetFlags{From: channelRef('b').ID, Expected: channelRef('a').ID})
	})
	if err == nil {
		t.Fatal("a provider failure was swallowed")
	}
	for _, want := range []string{"namespace cloud", "stable", "exit status 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("provider failure %q does not name %q", err, want)
		}
	}
}

// writeChannelDistributionPolicy writes a minimal CI document carrying the given
// distribution members and returns the workspace root holding it.
func writeChannelDistributionPolicy(t *testing.T, members string) string {
	t.Helper()
	root := t.TempDir()
	document := `{"version": 3, "commands": ["build"], "distribution": {` + members + `}}`
	if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte(document), 0o644); err != nil {
		t.Fatalf("write %s: %v", ciproto.Filename, err)
	}
	return root
}
