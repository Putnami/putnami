package agentsession

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const sessionURLRequirement = "session-url"

// sessionAddresses answers the sessionUrl of each sync the provider
// received, from index from: "-" for a sync that carried none.
func sessionAddresses(h *harness, from int) []string {
	var addresses []string
	for _, sync := range h.provider.recordedSyncs()[from:] {
		if sync.SessionURL == nil {
			addresses = append(addresses, "-")
			continue
		}
		addresses = append(addresses, *sync.SessionURL)
	}
	return addresses
}

func wantAddresses(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the syncs carried sessionUrl %q; want %q", got, want)
	}
}

func TestACloudSessionSendsItsWebAddress(t *testing.T) {
	spectest.Proves(t, sessionFeature, sessionURLRequirement, "a-cloud-session-sends-its-web-address")
	h := newHarness(t)
	h.environ[remoteSessionVariable] = "cse_01AbCdEf"
	h.environ[bridgeSessionVariable] = "session_bridge"
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	wantAddresses(t, sessionAddresses(h, 0), "https://claude.ai/code/session_01AbCdEf", "-")
	if got := h.cache(claude, claudeSession).SessionURL; got != "https://claude.ai/code/session_01AbCdEf" {
		t.Fatalf("the cache records %q as sent", got)
	}

	// An id without the cloud prefix goes as it is.
	h = newHarness(t)
	h.environ[remoteSessionVariable] = "session_01XyZ"
	h.hook(claude, "session-start", claudeSession, nil)
	wantAddresses(t, sessionAddresses(h, 0), "https://claude.ai/code/session_01XyZ")
}

func TestARemoteControlSessionSendsItsWebAddress(t *testing.T) {
	spectest.Proves(t, sessionFeature, sessionURLRequirement, "a-remote-control-session-sends-its-web-address")
	h := newHarness(t)
	h.environ[bridgeSessionVariable] = "session_01Bridge"
	h.denySyncs(true)
	h.hook(claude, "session-start", claudeSession, nil)
	if got := h.cache(claude, claudeSession).SessionURL; got != "" {
		t.Fatalf("a failed sync recorded %q as sent", got)
	}
	h.denySyncs(false)
	h.hook(claude, "stop", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	// The failed sync and the next one carry the address; once one passed,
	// the next carries none.
	wantAddresses(t, sessionAddresses(h, 0), "https://claude.ai/code/session_01Bridge", "https://claude.ai/code/session_01Bridge", "-")
}

func TestASessionWithoutAnAddressSendsNone(t *testing.T) {
	spectest.Proves(t, sessionFeature, sessionURLRequirement, "a-session-without-an-address-sends-none")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	wantAddresses(t, sessionAddresses(h, 0), "-", "-")

	// An id the address cannot carry as it is gives no address.
	for _, id := range []string{"session/../x", "session_01#top", `session_"01"`, "session 01", strings.Repeat("a", 257)} {
		h = newHarness(t)
		h.environ[bridgeSessionVariable] = id
		h.hook(claude, "session-start", claudeSession, nil)
		wantAddresses(t, sessionAddresses(h, 0), "-")
	}
	h = newHarness(t)
	h.environ[remoteSessionVariable] = "cse_"
	h.environ[bridgeSessionVariable] = "session_01Bridge"
	h.hook(claude, "session-start", claudeSession, nil)
	wantAddresses(t, sessionAddresses(h, 0), "-")

	// Codex names no web session, whatever the environment holds.
	h = newHarness(t)
	h.environ[remoteSessionVariable] = "cse_01AbCdEf"
	h.environ[bridgeSessionVariable] = "session_01Bridge"
	h.hook(codex, "session-start", codexSession, nil)
	h.hook(codex, "stop", codexSession, nil)
	if syncs := h.provider.recordedSyncs(); len(syncs) == 0 {
		t.Fatal("the Codex session sent no sync")
	}
	for _, address := range sessionAddresses(h, 0) {
		if address != "-" {
			t.Fatalf("a Codex sync carried sessionUrl %q", address)
		}
	}
}

func TestAGoneRemoteControlAddressIsClearedOnce(t *testing.T) {
	spectest.Proves(t, sessionFeature, sessionURLRequirement, "a-gone-remote-control-address-is-cleared-once")
	h := newHarness(t)
	h.environ[bridgeSessionVariable] = "session_01Bridge"
	h.hook(claude, "session-start", claudeSession, nil)
	delete(h.environ, bridgeSessionVariable)
	h.hook(claude, "stop", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	h.environ[bridgeSessionVariable] = "session_01Again"
	h.hook(claude, "stop", claudeSession, nil)
	wantAddresses(t, sessionAddresses(h, 0),
		"https://claude.ai/code/session_01Bridge", "", "-", "https://claude.ai/code/session_01Again")
}

// olderCLI refuses a sync that carries sessionUrl as invalid, as a CLI that
// does not know the member does, and passes every other call on. A message
// replaces the refusal's text.
type olderCLI struct {
	Host
	refused int
	message string
}

func (o *olderCLI) Call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	if operation == collab.OperationSync && strings.Contains(string(input), `"sessionUrl"`) {
		o.refused++
		message := o.message
		if message == "" {
			message = `unknown member "sessionUrl"`
		}
		return collab.Envelope{Contract: contract, Version: 1, Operation: operation, Outcome: collab.OutcomeInvalid,
			Error: &collab.Error{Message: message, Reason: collab.ReasonRequestInvalid}}, nil
	}
	return o.Host.Call(ctx, contract, operation, input)
}

func TestASyncRefusedForTheAddressGoesOnceWithoutIt(t *testing.T) {
	spectest.Proves(t, sessionFeature, sessionURLRequirement, "a-sync-refused-for-the-address-goes-once-without-it")
	h := newHarness(t)
	cli := &olderCLI{Host: h.provider}
	h.env.Host = cli
	h.environ[bridgeSessionVariable] = "session_01Bridge"
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	if cli.refused != 1 {
		t.Fatalf("the older CLI refused %d syncs; want 1", cli.refused)
	}
	wantAddresses(t, sessionAddresses(h, 0), "-", "-")
	if got := h.cache(claude, claudeSession).SessionURL; got != "https://claude.ai/code/session_01Bridge" {
		t.Fatalf("the cache records %q as sent; want the refused address", got)
	}
	if !logSays(h.hookLog(), "the session address, which goes again once it changes") {
		t.Errorf("hook.log %v", h.hookLog())
	}

	// A provider's own refusal of session_url keeps it recorded too.
	h = newHarness(t)
	cli = &olderCLI{Host: h.provider, message: "session_url: the address is refused"}
	h.env.Host = cli
	h.environ[bridgeSessionVariable] = "session_01Bridge"
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	if cli.refused != 1 {
		t.Fatalf("the provider refused %d syncs; want 1", cli.refused)
	}

	// A refusal of the plan forgets the address, so the next sync, without
	// a new plan, carries it again.
	h = newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "post-tool-use-todo-write", claudeSession, nil)
	h.provider.refusePlan = true
	h.environ[bridgeSessionVariable] = "session_01Bridge"
	h.hook(claude, "stop", claudeSession, nil)
	wantAddresses(t, sessionAddresses(h, 1), "https://claude.ai/code/session_01Bridge", "-")
	if got := h.cache(claude, claudeSession).SessionURL; got != "" {
		t.Fatalf("the cache records %q as sent after a refusal of the plan", got)
	}
	if !logSays(h.hookLog(), "the session address, which goes again with the next sync") {
		t.Errorf("hook.log %v", h.hookLog())
	}
	from := len(h.provider.recordedSyncs())
	h.hook(claude, "stop", claudeSession, nil)
	wantAddresses(t, sessionAddresses(h, from), "https://claude.ai/code/session_01Bridge")
	if got := h.cache(claude, claudeSession).SessionURL; got != "https://claude.ai/code/session_01Bridge" {
		t.Fatalf("the cache records %q as sent; want the resent address", got)
	}
}
