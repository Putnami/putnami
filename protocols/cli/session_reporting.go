package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Session reporter discovery names and bounded v1 wire constants.
const (
	SessionReporterCommand     = "session-reporter"
	SessionReporterEnv         = "PUTNAMI_SESSION_REPORTER"
	SessionReporterTokenEnv    = "PUTNAMI_SESSION_REPORTER_TOKEN"
	SessionReportingVersion    = 1
	SessionReportingChunkBytes = 64 * 1024
	SessionReportingLineBytes  = 96 * 1024
	SessionReportingSchemaID   = "https://putnami.dev/schemas/putnami-session-reporting.json"
)

// Session reporting v2 is v1 opened by a handshake that hands the reporter the
// run credential of a hosted run (--credential-fd). The engine sends
// initialize, which carries no credential; only after a reporter answers it
// with SessionReportingCredentialVersion does the engine send authenticate,
// which carries the credential. A v1 reporter rejects initialize, whose version
// and members it does not know, and so never receives the credential. Chunks
// and acknowledgements carry SessionReportingVersion in every stream, and an
// engine without a run credential sends no handshake.
const (
	SessionReportingCredentialVersion = 2
	SessionReportingOpInitialize      = "initialize"
	SessionReportingOpAuthenticate    = "authenticate"
	// SessionReportingMaxCredentialBytes bounds, in UTF-8 bytes, the run
	// credential that authenticate carries (ValidSessionReportingCredential).
	// The schema's maxLength holds the same number in characters, a looser
	// bound for a multi-byte credential.
	SessionReportingMaxCredentialBytes = 16 << 10
)

// Log reporter discovery names. The log reporter speaks the same v1 wire as
// the session reporter, restricted to the events.jsonl artifact: it never
// receives a session.json frame, and its events final marker follows graph
// termination.
const (
	LogReporterCommand  = "log-reporter"
	LogReporterEnv      = "PUTNAMI_LOG_REPORTER"
	LogReporterTokenEnv = "PUTNAMI_LOG_REPORTER_TOKEN" // #nosec G101 -- environment variable name, not a credential
)

// SessionReportingArtifacts returns the artifacts the reporter capability named
// by its reserved command receives, in the order they close: session.json then
// events.jsonl for the session reporter, events.jsonl alone for the log
// reporter. Any other command receives nothing. The result is a fresh slice.
func SessionReportingArtifacts(command string) []string {
	switch command {
	case SessionReporterCommand:
		return []string{"session.json", "events.jsonl"}
	case LogReporterCommand:
		return []string{"events.jsonl"}
	}
	return nil
}

// SessionReportingChunk transports the original persisted artifact bytes. A
// final marker is a separate empty chunk at EOF. Its identity includes Final,
// so it cannot collide with the last data chunk. Providers acknowledge only
// after durably accepting the bytes, and reject conflicting bytes at an offset.
type SessionReportingChunk struct {
	// ProtocolVersion identifies the reporter wire contract; currently 1.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionID is the stable identity of the original retained execution.
	SessionID string `json:"sessionId"`
	// Artifact names the canonical events.jsonl or session.json document.
	Artifact string `json:"artifact"`
	// Offset is the zero-based position in decoded artifact bytes.
	Offset int64 `json:"offset"`
	// Sequence is the zero-based per-artifact ordinal, including the final marker.
	Sequence int64 `json:"sequence"`
	// Data holds at most 64 KiB of original bytes, encoded as canonical base64
	// in JSON. Only a final marker carries an empty byte slice.
	Data []byte `json:"data"`
	// SHA256 is the lowercase hexadecimal SHA-256 digest of decoded Data.
	SHA256 string `json:"sha256"`
	// Final marks a separate empty EOF frame, never the last nonempty data frame.
	Final bool `json:"final"`
}

// SessionReportingAck echoes the whole chunk identity. Code is a bounded
// machine code, never arbitrary provider output or a credential-bearing URL.
type SessionReportingAck struct {
	// ProtocolVersion echoes the acknowledged chunk's reporter wire version.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionID echoes the acknowledged chunk's retained execution identity.
	SessionID string `json:"sessionId"`
	// Artifact echoes the acknowledged canonical document name.
	Artifact string `json:"artifact"`
	// Offset echoes the acknowledged chunk's decoded byte position, not its end.
	Offset int64 `json:"offset"`
	// Sequence echoes the acknowledged chunk's per-artifact ordinal.
	Sequence int64 `json:"sequence"`
	// SHA256 echoes the digest of the acknowledged decoded bytes.
	SHA256 string `json:"sha256"`
	// Final echoes whether the acknowledged chunk is the empty EOF marker.
	Final bool `json:"final"`
	// OK reports durable acceptance of this exact identity, including identical
	// replay. A successful acknowledgement cannot carry Code or Retryable=true.
	OK bool `json:"ok"`
	// Retryable permits another bounded attempt after a negative acknowledgement.
	Retryable bool `json:"retryable,omitempty"`
	// Code is required when OK is false: a lowercase machine code matching
	// [a-z][a-z0-9_]{0,63}, never arbitrary provider output or credential material.
	Code string `json:"code,omitempty"`
}

var reportingSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var reportingCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var reportingDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// SessionReportingDigest returns the lowercase SHA-256 identity of artifact bytes.
func SessionReportingDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NewSessionReportingChunk copies bytes into a frame with their computed digest.
// Call Validate before transporting an externally constructed identity.
func NewSessionReportingChunk(sessionID, artifact string, offset, sequence int64, data []byte, final bool) SessionReportingChunk {
	return SessionReportingChunk{SessionReportingVersion, sessionID, artifact, offset, sequence, append([]byte{}, data...), SessionReportingDigest(data), final}
}

// Validate checks the identity, byte bounds, final marker and content digest.
func (c SessionReportingChunk) Validate() error {
	if err := validateReportingIdentity(c.ProtocolVersion, c.SessionID, c.Artifact, c.Offset, c.SHA256); err != nil {
		return err
	}
	if c.Sequence < 0 || c.Sequence > 9007199254740991 {
		return fmt.Errorf("invalid reporting sequence")
	}
	if len(c.Data) > SessionReportingChunkBytes || c.Final != (len(c.Data) == 0) {
		return fmt.Errorf("invalid reporting chunk size or final marker")
	}
	if c.SHA256 != SessionReportingDigest(c.Data) {
		return fmt.Errorf("reporting chunk digest mismatch")
	}
	return nil
}

// Ack constructs the successful acknowledgement of this exact frame.
func (c SessionReportingChunk) Ack() SessionReportingAck {
	return SessionReportingAck{ProtocolVersion: c.ProtocolVersion, SessionID: c.SessionID, Artifact: c.Artifact, Offset: c.Offset, Sequence: c.Sequence, SHA256: c.SHA256, Final: c.Final, OK: true}
}

// Matches compares every identity field without interpreting acceptance status.
func (a SessionReportingAck) Matches(c SessionReportingChunk) bool {
	return a.ProtocolVersion == c.ProtocolVersion && a.SessionID == c.SessionID && a.Artifact == c.Artifact && a.Offset == c.Offset && a.Sequence == c.Sequence && a.SHA256 == c.SHA256 && a.Final == c.Final
}

func validateReportingIdentity(version int, sessionID, artifact string, offset int64, digest string) error {
	if version != SessionReportingVersion || !reportingSessionID.MatchString(sessionID) || (artifact != "events.jsonl" && artifact != "session.json") || offset < 0 || offset > 9007199254740991 || !reportingDigest.MatchString(digest) {
		return fmt.Errorf("invalid reporting identity")
	}
	return nil
}

// ParseSessionReportingChunk strictly decodes one bounded frame and verifies bytes.
func ParseSessionReportingChunk(line []byte) (*SessionReportingChunk, error) {
	var c SessionReportingChunk
	if err := parseReporting(line, &c, "protocolVersion", "sessionId", "artifact", "offset", "sequence", "data", "sha256", "final"); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(line, &fields)
	var encoded string
	_ = json.Unmarshal(fields["data"], &encoded)
	if encoded != base64.StdEncoding.EncodeToString(c.Data) {
		return nil, fmt.Errorf("noncanonical reporting base64")
	}
	return &c, nil
}

// ParseSessionReportingAck strictly decodes one bounded acknowledgement.
func ParseSessionReportingAck(line []byte) (*SessionReportingAck, error) {
	var a SessionReportingAck
	if err := parseReporting(line, &a, "protocolVersion", "sessionId", "artifact", "offset", "sequence", "sha256", "final", "ok"); err != nil {
		return nil, err
	}
	if a.Sequence < 0 || a.Sequence > 9007199254740991 {
		return nil, fmt.Errorf("invalid reporting sequence")
	}
	if err := validateReportingIdentity(a.ProtocolVersion, a.SessionID, a.Artifact, a.Offset, a.SHA256); err != nil {
		return nil, err
	}
	if a.OK && (a.Code != "" || a.Retryable) || !a.OK && !reportingCode.MatchString(a.Code) {
		return nil, fmt.Errorf("invalid reporting acknowledgement status")
	}
	return &a, nil
}

// SessionReportingHandshake is one engine line of the v2 handshake: initialize,
// then authenticate. Formatting redacts the credential; the wire encoding
// carries it.
type SessionReportingHandshake struct {
	// ProtocolVersion is SessionReportingCredentialVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Op is SessionReportingOpInitialize or SessionReportingOpAuthenticate.
	Op string `json:"op"`
	// RunCredential is the hosted run's opaque bearer
	// (ValidSessionReportingCredential). Only authenticate carries it.
	RunCredential string `json:"runCredential,omitempty"`
}

// SessionReportingHandshakeResult is the reporter's answer to one handshake
// line. Code is a bounded machine code, never arbitrary reporter output.
type SessionReportingHandshakeResult struct {
	// ProtocolVersion echoes SessionReportingCredentialVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Op echoes the answered operation.
	Op string `json:"op"`
	// OK accepts the operation. A refusal carries Code.
	OK bool `json:"ok"`
	// Code is required when OK is false and absent otherwise: a lowercase
	// machine code matching [a-z][a-z0-9_]{0,63}.
	Code string `json:"code,omitempty"`
}

// NewSessionReportingInitialize returns the first handshake line. It carries
// no credential.
func NewSessionReportingInitialize() SessionReportingHandshake {
	return SessionReportingHandshake{ProtocolVersion: SessionReportingCredentialVersion, Op: SessionReportingOpInitialize}
}

// NewSessionReportingAuthenticate returns the handshake line that hands the
// reporter credential. Call Validate before transporting it.
func NewSessionReportingAuthenticate(credential string) SessionReportingHandshake {
	return SessionReportingHandshake{ProtocolVersion: SessionReportingCredentialVersion, Op: SessionReportingOpAuthenticate, RunCredential: credential}
}

// String formats the line without the credential.
func (h SessionReportingHandshake) String() string {
	credential := ""
	if h.RunCredential != "" {
		credential = "<redacted>"
	}
	return fmt.Sprintf("{protocolVersion:%d op:%s runCredential:%s}", h.ProtocolVersion, h.Op, credential)
}

// Format renders String for every verb, so no verb prints the credential.
func (h SessionReportingHandshake) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, h.String())
}

// Validate checks the version, the operation, and that only authenticate
// carries a credential, a valid one.
func (h SessionReportingHandshake) Validate() error {
	if h.ProtocolVersion != SessionReportingCredentialVersion {
		return fmt.Errorf("invalid reporting handshake version")
	}
	switch h.Op {
	case SessionReportingOpInitialize:
		if h.RunCredential != "" {
			return fmt.Errorf("reporting initialize carries a credential")
		}
	case SessionReportingOpAuthenticate:
		if !ValidSessionReportingCredential(h.RunCredential) {
			return fmt.Errorf("invalid reporting credential")
		}
	default:
		return fmt.Errorf("invalid reporting handshake operation")
	}
	return nil
}

// Accept constructs the reporter's acceptance of this line.
func (h SessionReportingHandshake) Accept() SessionReportingHandshakeResult {
	return SessionReportingHandshakeResult{ProtocolVersion: h.ProtocolVersion, Op: h.Op, OK: true}
}

// Refuse constructs the reporter's refusal of this line with code.
func (h SessionReportingHandshake) Refuse(code string) SessionReportingHandshakeResult {
	return SessionReportingHandshakeResult{ProtocolVersion: h.ProtocolVersion, Op: h.Op, Code: code}
}

// Answers reports whether the result answers h: the same version and
// operation, whatever its status.
func (r SessionReportingHandshakeResult) Answers(h SessionReportingHandshake) bool {
	return r.ProtocolVersion == h.ProtocolVersion && r.Op == h.Op
}

// ValidSessionReportingCredential reports whether credential is a well-formed
// run credential: non-empty, valid UTF-8, at most
// SessionReportingMaxCredentialBytes bytes, and free of every character
// unicode.IsSpace reports. It is the rule of registry.ValidRunCredential,
// restated so this module depends on no other protocol.
func ValidSessionReportingCredential(credential string) bool {
	return credential != "" && len(credential) <= SessionReportingMaxCredentialBytes && utf8.ValidString(credential) &&
		strings.IndexFunc(credential, unicode.IsSpace) < 0
}

// ParseSessionReportingHandshake strictly decodes one engine handshake line.
// It refuses an initialize line that holds the runCredential member, even an
// empty one, as the schema does.
func ParseSessionReportingHandshake(line []byte) (*SessionReportingHandshake, error) {
	var h SessionReportingHandshake
	if err := parseReporting(line, &h, "protocolVersion", "op"); err != nil {
		return nil, err
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	// RunCredential decodes an empty member and an absent one alike, so the
	// member's presence is read from the line, which parseReporting decoded.
	var members map[string]json.RawMessage
	_ = json.Unmarshal(line, &members)
	if _, present := members["runCredential"]; present && h.Op == SessionReportingOpInitialize {
		return nil, fmt.Errorf("reporting initialize carries a credential")
	}
	return &h, nil
}

// ParseSessionReportingHandshakeResult strictly decodes one reporter answer to
// a handshake line.
func ParseSessionReportingHandshakeResult(line []byte) (*SessionReportingHandshakeResult, error) {
	var r SessionReportingHandshakeResult
	if err := parseReporting(line, &r, "protocolVersion", "op", "ok"); err != nil {
		return nil, err
	}
	if r.ProtocolVersion != SessionReportingCredentialVersion {
		return nil, fmt.Errorf("invalid reporting handshake version")
	}
	if r.Op != SessionReportingOpInitialize && r.Op != SessionReportingOpAuthenticate {
		return nil, fmt.Errorf("invalid reporting handshake operation")
	}
	if r.OK && r.Code != "" || !r.OK && !reportingCode.MatchString(r.Code) {
		return nil, fmt.Errorf("invalid reporting handshake status")
	}
	return &r, nil
}

func parseReporting(line []byte, target any, required ...string) error {
	if len(line) > SessionReportingLineBytes {
		return fmt.Errorf("reporting line exceeds limit")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return fmt.Errorf("invalid reporting JSON")
	}
	for key, value := range fields {
		if bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("null reporting field")
		}
		if key == "code" {
			var code string
			if json.Unmarshal(value, &code) != nil || !reportingCode.MatchString(code) {
				return fmt.Errorf("invalid reporting code")
			}
		}
	}
	for _, key := range required {
		value, ok := fields[key]
		if !ok || bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("missing reporting field %s", key)
		}
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("invalid reporting fields")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing reporting JSON")
	}
	return nil
}
