package useragent

import (
	"net/http"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

func TestStringUsesSanitizedVersion(t *testing.T) {
	SetVersion("v1.2.3 beta")
	t.Cleanup(func() { SetVersion("dev") })

	if got, want := String(), "putnami-cli/v1.2.3-beta"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestSetDoesNotOverwriteExistingUserAgent(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "custom/1.0")

	Set(req)

	if got := req.Header.Get("User-Agent"); got != "custom/1.0" {
		t.Fatalf("User-Agent = %q, want custom/1.0", got)
	}
}

func TestMCPIdentityAppliesStructuredProvenance(t *testing.T) {
	identity := MCPIdentity("v1.2.3", "Claude CLI", "1.0 beta", " claude-sonnet-4-5 ")
	if identity.ClientName != "Claude CLI" || identity.ClientVersion != "1.0 beta" {
		t.Fatalf("captured client = %q %q", identity.ClientName, identity.ClientVersion)
	}
	if identity.Harness != "Claude-CLI/1.0-beta" {
		t.Fatalf("Harness = %q", identity.Harness)
	}
	if identity.Model != "claude-sonnet-4-5" {
		t.Fatalf("Model = %q", identity.Model)
	}
	if identity.UserAgent != "putnami-mcp/v1.2.3 (harness=Claude-CLI/1.0-beta; model=claude-sonnet-4-5)" {
		t.Fatalf("UserAgent = %q", identity.UserAgent)
	}

	SetAgentIdentity(identity)
	t.Cleanup(ClearAgentIdentity)
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	Set(req)

	if got := req.Header.Get("User-Agent"); got != identity.UserAgent {
		t.Errorf("User-Agent = %q, want %q", got, identity.UserAgent)
	}
	if got := req.Header.Get(proto.AgentHarnessHeader); got != identity.Harness {
		t.Errorf("%s = %q", proto.AgentHarnessHeader, got)
	}
	if got := req.Header.Get(proto.AgentModelHeader); got != identity.Model {
		t.Errorf("%s = %q", proto.AgentModelHeader, got)
	}
}

func TestMCPIdentityRejectsControlsAndBoundsValues(t *testing.T) {
	invalid := MCPIdentity("v1", "bad\nname", "1.0", "bad\rmodel")
	if invalid.ClientName != "" || invalid.Harness != "" || invalid.Model != "" || invalid.UserAgent != "" {
		t.Fatalf("invalid identity was retained: %#v", invalid)
	}

	long := MCPIdentity("v1", "client", "1.0", strings.Repeat("m", proto.MaxAgentIdentityValueLength+20))
	if len(long.Model) != proto.MaxAgentIdentityValueLength {
		t.Fatalf("bounded model length = %d, want %d", len(long.Model), proto.MaxAgentIdentityValueLength)
	}
}

func TestMissingMCPIdentityPreservesCLIUserAgent(t *testing.T) {
	SetVersion("v2.0.0")
	t.Cleanup(func() { SetVersion("dev") })
	SetAgentIdentity(MCPIdentity("v2.0.0", "", "", ""))
	t.Cleanup(ClearAgentIdentity)
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	Set(req)

	if got := req.Header.Get("User-Agent"); got != "putnami-cli/v2.0.0" {
		t.Fatalf("User-Agent = %q, want putnami-cli/v2.0.0", got)
	}
	if got := req.Header.Get(proto.AgentHarnessHeader); got != "" {
		t.Errorf("unexpected %s = %q", proto.AgentHarnessHeader, got)
	}
	if got := req.Header.Get(proto.AgentModelHeader); got != "" {
		t.Errorf("unexpected %s = %q", proto.AgentModelHeader, got)
	}
}

func TestSetKeepsCustomUserAgentAndAddsStructuredHeaders(t *testing.T) {
	identity := MCPIdentity("v1", "cursor", "0.50", "gpt-5")
	SetAgentIdentity(identity)
	t.Cleanup(ClearAgentIdentity)
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "custom/1.0")

	Set(req)

	if got := req.Header.Get("User-Agent"); got != "custom/1.0" {
		t.Fatalf("User-Agent = %q, want custom/1.0", got)
	}
	if got := req.Header.Get(proto.AgentHarnessHeader); got != identity.Harness {
		t.Errorf("%s = %q", proto.AgentHarnessHeader, got)
	}
	if got := req.Header.Get(proto.AgentModelHeader); got != identity.Model {
		t.Errorf("%s = %q", proto.AgentModelHeader, got)
	}
}
