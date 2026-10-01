package extension

import (
	"net/http"
	"strings"
	"testing"
)

func TestAgentIdentityApplyHTTPHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := &AgentIdentity{
		Harness:   "claude-cli/1.0",
		Model:     "haiku",
		UserAgent: "putnami-mcp/v1.2.3 (harness=claude-cli/1.0; model=haiku)",
	}

	identity.ApplyHTTPHeaders(req)

	if got := req.Header.Get("User-Agent"); got != identity.UserAgent {
		t.Errorf("User-Agent = %q, want %q", got, identity.UserAgent)
	}
	if got := req.Header.Get(AgentHarnessHeader); got != identity.Harness {
		t.Errorf("%s = %q, want %q", AgentHarnessHeader, got, identity.Harness)
	}
	if got := req.Header.Get(AgentModelHeader); got != identity.Model {
		t.Errorf("%s = %q, want %q", AgentModelHeader, got, identity.Model)
	}
}

func TestAgentIdentityApplyHTTPHeadersRejectsInvalidValues(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "extension/1.0")
	identity := &AgentIdentity{
		Harness:   "bad\r\nharness",
		Model:     strings.Repeat("m", MaxAgentIdentityValueLength+1),
		UserAgent: "putnami-mcp/forged",
	}

	identity.ApplyHTTPHeaders(req)

	if got := req.Header.Get("User-Agent"); got != "extension/1.0" {
		t.Errorf("existing User-Agent was overwritten: %q", got)
	}
	if got := req.Header.Get(AgentHarnessHeader); got != "" {
		t.Errorf("unsafe harness was applied: %q", got)
	}
	if got := req.Header.Get(AgentModelHeader); got != "" {
		t.Errorf("unbounded model was applied: %q", got)
	}
}
