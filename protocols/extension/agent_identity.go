package extension

import "net/http"

const (
	// AgentHarnessHeader carries the MCP harness name and version.
	AgentHarnessHeader = "Putnami-Agent-Harness"
	// AgentModelHeader carries the explicitly configured active model.
	AgentModelHeader = "Putnami-Agent-Model"

	// AgentHarnessEnv exposes AgentIdentity.Harness to extension tool subprocesses.
	AgentHarnessEnv = "PUTNAMI_AGENT_HARNESS"
	// AgentModelEnv exposes AgentIdentity.Model to extension tool subprocesses.
	AgentModelEnv = "PUTNAMI_AGENT_MODEL"
	// AgentUserAgentEnv exposes AgentIdentity.UserAgent to extension tool subprocesses.
	AgentUserAgentEnv = "PUTNAMI_AGENT_USER_AGENT"

	// MaxAgentIdentityValueLength bounds every client-supplied identity value.
	MaxAgentIdentityValueLength = 128
	maxAgentUserAgentLength     = 512
)

// AgentIdentity is optional, privacy-aware provenance for an MCP-originated
// extension tool call. It intentionally contains no prompts, credentials,
// token counts, user identifiers, or session identifiers.
type AgentIdentity struct {
	ClientName    string `json:"clientName,omitempty"`
	ClientVersion string `json:"clientVersion,omitempty"`
	Harness       string `json:"harness,omitempty"`
	Model         string `json:"model,omitempty"`
	UserAgent     string `json:"userAgent,omitempty"`
}

// ApplyHTTPHeaders adds the opted-in provenance headers to req without
// replacing values already chosen by the caller. Values are validated again
// at this boundary so a directly invoked extension tool cannot inject control
// characters or unbounded header data through a forged stdin request.
func (a *AgentIdentity) ApplyHTTPHeaders(req *http.Request) {
	if a == nil || req == nil {
		return
	}
	setSafeHeader(req, "User-Agent", a.UserAgent, maxAgentUserAgentLength)
	setSafeHeader(req, AgentHarnessHeader, a.Harness, MaxAgentIdentityValueLength)
	setSafeHeader(req, AgentModelHeader, a.Model, MaxAgentIdentityValueLength)
}

func setSafeHeader(req *http.Request, name, value string, max int) {
	if req.Header.Get(name) != "" || !safeHeaderValue(value, max) {
		return
	}
	req.Header.Set(name, value)
}

func safeHeaderValue(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f || value[i] > 0x7e {
			return false
		}
	}
	return true
}
