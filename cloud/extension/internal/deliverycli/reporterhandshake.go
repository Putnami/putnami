package deliverycli

import (
	"encoding/json"

	protocolcli "go.putnami.dev/protocol/cli"
)

// Session reporting protocol 2 (protocolcli.SessionReportingCredentialVersion)
// is protocol 1 opened by a two-line handshake. A hosted run (--credential-fd)
// hands its reporters the run credential this way, never in their
// environment: the engine sends initialize, which carries no credential, and
// only after the reporter accepts it, authenticate, which carries the
// credential. An engine without a run credential sends no handshake, so the
// first line is a chunk, and the reporter keeps the token the engine placed
// in its environment (protocol 1).
//
// Each answer echoes the line's version and operation. A refusal carries one
// of these bounded machine codes, never a message, and the engine closes a
// reporter that refuses authenticate.
const (
	// handshakeCodeInvalidCredential refuses an authenticate line whose run
	// credential is absent or not a well-formed one: 1 to
	// protocolcli.SessionReportingMaxCredentialBytes bytes of UTF-8 with no
	// whitespace (protocolcli.ValidSessionReportingCredential). It is the
	// code the credential provider refuses a malformed run credential with.
	handshakeCodeInvalidCredential = "invalid_run_credential" //nolint:gosec // G101: a refusal code, not a credential
	// handshakeCodeInvalidLine refuses a handshake line that the protocol's
	// strict parser rejects for any other reason, such as an unknown member.
	handshakeCodeInvalidLine = "invalid_handshake"
	// handshakeCodeOutOfOrder refuses a second initialize, and an authenticate
	// that does not follow an accepted initialize or follows an accepted
	// authenticate.
	handshakeCodeOutOfOrder = "handshake_out_of_order"
)

// reportingHandshake is the reporter half of the protocol 2 handshake.
// serveReportingFrames hands it lines only before the first chunk: from the
// first chunk on, the stream is protocol 1.
type reportingHandshake struct {
	// cfg is the reporter's configuration. Accepting authenticate sets its
	// bearer, which every later chunk uses.
	cfg           *sessionReporterConfig
	initialized   bool
	authenticated bool
}

// handshakeOp returns the operation of a handshake line: one whose
// protocolVersion is protocolcli.SessionReportingCredentialVersion and whose op
// is initialize or authenticate. A protocol 1 chunk carries protocolVersion 1,
// so it is never one. It returns the protocol's constant, never the line's
// bytes, so diagnostics that name the operation print no input.
func handshakeOp(line []byte) (string, bool) {
	var probe struct {
		ProtocolVersion int    `json:"protocolVersion"`
		Op              string `json:"op"`
	}
	if json.Unmarshal(line, &probe) != nil || probe.ProtocolVersion != protocolcli.SessionReportingCredentialVersion {
		return "", false
	}
	switch probe.Op {
	case protocolcli.SessionReportingOpInitialize:
		return protocolcli.SessionReportingOpInitialize, true
	case protocolcli.SessionReportingOpAuthenticate:
		return protocolcli.SessionReportingOpAuthenticate, true
	}
	return "", false
}

// answer answers one handshake line whose operation handshakeOp returned.
//
// initialize is accepted once, and only by a process that denied inspection
// (newReporterConfig): a process that could not refuses it with
// ackCodeInspectionGuard, so the engine never sends it the credential, and
// starts it again without one.
//
// authenticate is accepted once, after an accepted initialize. Its credential
// then becomes the bearer of every chunk, in place of any token from the
// environment. The credential stays in memory: no answer, diagnostic or error
// carries it, and a refusal names only its code.
func (h *reportingHandshake) answer(op string, line []byte) protocolcli.SessionReportingHandshakeResult {
	echo := protocolcli.SessionReportingHandshake{ProtocolVersion: protocolcli.SessionReportingCredentialVersion, Op: op}
	if op == protocolcli.SessionReportingOpInitialize {
		if h.initialized {
			return echo.Refuse(handshakeCodeOutOfOrder)
		}
		if _, err := protocolcli.ParseSessionReportingHandshake(line); err != nil {
			return echo.Refuse(handshakeCodeInvalidLine)
		}
		if h.cfg.inspectionGuardFailed {
			return echo.Refuse(ackCodeInspectionGuard)
		}
		h.initialized = true
		return echo.Accept()
	}
	if !h.initialized || h.authenticated {
		return echo.Refuse(handshakeCodeOutOfOrder)
	}
	parsed, err := protocolcli.ParseSessionReportingHandshake(line)
	if err != nil {
		if !holdsValidCredential(line) {
			return echo.Refuse(handshakeCodeInvalidCredential)
		}
		return echo.Refuse(handshakeCodeInvalidLine)
	}
	h.cfg.token = parsed.RunCredential
	h.authenticated = true
	return echo.Accept()
}

// holdsValidCredential reports whether a handshake line has a runCredential
// member that is a well-formed run credential. It tells a malformed credential
// from another defect of an authenticate line the strict parser refused.
func holdsValidCredential(line []byte) bool {
	var members map[string]json.RawMessage
	if json.Unmarshal(line, &members) != nil {
		return false
	}
	raw, present := members["runCredential"]
	if !present {
		return false
	}
	var credential string
	return json.Unmarshal(raw, &credential) == nil && protocolcli.ValidSessionReportingCredential(credential)
}
