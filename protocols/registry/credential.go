package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// credential-provider/v1 is the purpose-keyed credential seam
// (doc/adr/0002-one-credential-call-per-purpose.md). The engine asks one
// extension command, resolved by the reserved name CredentialProviderCommand,
// for one credential per purpose. The provider answers with the bearer, the
// instant it expires and the hosts it is valid for. The engine sends that
// bearer to those hosts and to no other.
//
// The provider is an out-of-process extension command that reads one JSON
// request per line on stdin and writes one JSON response per line on stdout:
//
//	→ {"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":["credential-v1"]}}
//	← {"protocolVersion":1,"id":1,"ok":true,"payload":{"protocolVersion":1,"capabilities":["credential-v1"]}}
//	→ {"protocolVersion":1,"id":2,"op":"credential","payload":{"purpose":"read"}}
//	← {"protocolVersion":1,"id":2,"ok":true,"payload":{"credential":{"bearer":"…","expiresAt":"2026-09-28T12:00:00Z","hosts":["put.putnami.dev"]}}}
//	→ {"protocolVersion":1,"id":3,"op":"shutdown"}
//	← {"protocolVersion":1,"id":3,"ok":true}
//
// An answer without a credential member is absence: a supported answer that
// leaves the request on its native credentials. An answer with ok false is a
// refusal: a bounded machine code the engine reports as the failure reason,
// never a fallback.
const (
	// CredentialProviderCommand is the reserved extension command that serves
	// credential-provider/v1. Two loaded extensions declaring it is an error.
	CredentialProviderCommand = "credential-provider" // #nosec G101 -- command name, not a credential
	// CredentialProtocolVersion is the credential-provider RPC version.
	CredentialProtocolVersion = 1
	// CapabilityCredentialV1 must be echoed at initialize by a provider that
	// answers the credential op described here.
	CapabilityCredentialV1 = "credential-v1" // #nosec G101 -- capability name, not a credential

	// PurposeRead is the credential an engine uses to read from a registry:
	// archive and package downloads.
	PurposeRead = "read"
	// PurposePublish is the credential an engine uses to write to a registry.
	PurposePublish = "publish"

	// MaxCredentialLineBytes bounds one request or response line.
	MaxCredentialLineBytes = 64 << 10
	// MaxCredentialHosts bounds the hosts one credential names.
	MaxCredentialHosts = 64
	// MaxCredentialHostBytes bounds one hosts entry: a 253-byte name and a port.
	MaxCredentialHostBytes = 253 + len(":65535")
	// MaxBearerBytes bounds one bearer.
	MaxBearerBytes = 16 << 10
	// MaxRefusalMessageBytes bounds the human message of a refusal.
	MaxRefusalMessageBytes = 512
	// MaxCredentialCapabilities bounds the capability names of an initialize.
	MaxCredentialCapabilities = 16
	// MaxProviderNameBytes bounds the provider name an initialize reports.
	MaxProviderNameBytes = 128
	// MaxRunCredentialBytes bounds the run credential an initialize carries.
	MaxRunCredentialBytes = 16 << 10
)

// Wire grammars, shared with schemas/credential-provider-v1.json.
const (
	// RefusalCodePattern is the grammar of a refusal code.
	RefusalCodePattern = `^[a-z][a-z0-9_]{0,63}$`
	// CredentialHostPattern is the grammar of one hosts entry: a lowercase DNS
	// name or IPv4 address, optionally followed by a port. It carries no
	// scheme, path, userinfo or wildcard.
	CredentialHostPattern = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[1-9][0-9]{0,4})?$` // #nosec G101 -- host grammar, not a credential
	// CapabilityPattern is the grammar of one capability name.
	CapabilityPattern = `^[a-z0-9][a-z0-9.-]{0,63}$`
	// RunCredentialPattern is the grammar of a run credential: one or more
	// characters, none of which unicode.IsSpace reports, the rule of
	// ValidBearer. The class lists the whitespace characters themselves, so
	// the schema needs no regular-expression escape whose meaning differs
	// between dialects.
	RunCredentialPattern = "^[^\t-\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$" // #nosec G101 -- grammar, not a credential
)

var (
	refusalCode       = regexp.MustCompile(RefusalCodePattern)
	credentialHost    = regexp.MustCompile(CredentialHostPattern)
	capabilityGrammar = regexp.MustCompile(CapabilityPattern)
)

// CredentialOp names one credential-provider RPC method.
type CredentialOp string

const (
	// CredentialOpInitialize negotiates the protocol version and capabilities.
	// It is sent first, once.
	CredentialOpInitialize CredentialOp = "initialize"
	// CredentialOpCredential asks for the credential of one purpose.
	CredentialOpCredential CredentialOp = "credential"
	// CredentialOpShutdown ends the session; the provider exits after answering.
	CredentialOpShutdown CredentialOp = "shutdown"
)

// Valid reports whether op is a credential-provider/v1 method.
func (op CredentialOp) Valid() bool {
	switch op {
	case CredentialOpInitialize, CredentialOpCredential, CredentialOpShutdown:
		return true
	}
	return false
}

// ValidPurpose reports whether purpose is a credential-provider/v1 purpose.
func ValidPurpose(purpose string) bool {
	return purpose == PurposeRead || purpose == PurposePublish
}

// CredentialRequest is one engine → provider line.
type CredentialRequest struct {
	// ProtocolVersion is the RPC version of this envelope (1).
	ProtocolVersion int `json:"protocolVersion"`
	// ID is the positive correlation id, increasing within a session.
	ID int64 `json:"id"`
	// Op is the method to invoke.
	Op CredentialOp `json:"op"`
	// Payload is the op body: CredentialInitializeParams for initialize,
	// CredentialParams for credential, absent for shutdown.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// String prints the payload's size instead of its bytes, which carry the run
// credential of an initialize.
func (r CredentialRequest) String() string {
	return fmt.Sprintf("{protocolVersion:%d id:%d op:%s payload:<%d bytes>}", r.ProtocolVersion, r.ID, r.Op, len(r.Payload))
}

// GoString keeps %#v from printing the payload.
func (r CredentialRequest) GoString() string { return "registry.CredentialRequest" + r.String() }

// CredentialResponse is one provider → engine line.
type CredentialResponse struct {
	// ProtocolVersion is the RPC version of this envelope (1).
	ProtocolVersion int `json:"protocolVersion"`
	// ID echoes the CredentialRequest.ID this line answers.
	ID int64 `json:"id"`
	// OK reports whether the op succeeded. False requires Error and forbids Payload.
	OK bool `json:"ok"`
	// Error is the refusal when OK is false.
	Error *CredentialRefusal `json:"error,omitempty"`
	// Payload is the op result when OK is true: CredentialInitializeResult for
	// initialize, CredentialResult for credential, absent or {} for shutdown.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// String prints the payload's size instead of its bytes, which carry the
// bearer of a credential result.
func (r CredentialResponse) String() string {
	return fmt.Sprintf("{protocolVersion:%d id:%d ok:%t error:%v payload:<%d bytes>}", r.ProtocolVersion, r.ID, r.OK, r.Error, len(r.Payload))
}

// GoString keeps %#v from printing the payload.
func (r CredentialResponse) GoString() string { return "registry.CredentialResponse" + r.String() }

// CredentialRefusal is a provider's bounded refusal. The engine reports Code
// as the failure reason of the work that needed the credential.
type CredentialRefusal struct {
	// Code is the machine code, matching RefusalCodePattern.
	Code string `json:"code"`
	// Message is a human explanation of at most MaxRefusalMessageBytes bytes
	// with no control characters. It never carries credential material.
	Message string `json:"message,omitempty"`
}

// CredentialInitializeParams opens a session.
type CredentialInitializeParams struct {
	// ProtocolVersion is the RPC version the engine speaks (1).
	ProtocolVersion int `json:"protocolVersion"`
	// Capabilities are the capability names the engine supports.
	Capabilities []string `json:"capabilities"`
	// RunCredential is the opaque bearer of the hosted run the engine executes,
	// present only when the engine holds one: ValidRunCredential. The provider
	// keeps it in memory; it never reaches a file, a log or the environment of
	// a process the provider starts.
	RunCredential string `json:"runCredential,omitempty"`
}

// String keeps formatted initialize params from printing the run credential.
func (p CredentialInitializeParams) String() string {
	suffix := ""
	if p.RunCredential != "" {
		suffix = " runCredential:<redacted>"
	}
	return fmt.Sprintf("{protocolVersion:%d capabilities:%v%s}", p.ProtocolVersion, p.Capabilities, suffix)
}

// GoString keeps %#v from printing the run credential.
func (p CredentialInitializeParams) GoString() string {
	return "registry.CredentialInitializeParams" + p.String()
}

// CredentialInitializeResult answers initialize.
type CredentialInitializeResult struct {
	// ProtocolVersion is the RPC version the provider selected; the engine requires 1.
	ProtocolVersion int `json:"protocolVersion"`
	// ProviderName identifies the provider implementation, for diagnostics only.
	ProviderName string `json:"providerName,omitempty"`
	// Capabilities are the capability names the provider supports; it must
	// echo CapabilityCredentialV1.
	Capabilities []string `json:"capabilities"`
}

// CredentialParams asks for the credential of one purpose.
type CredentialParams struct {
	// Purpose is PurposeRead or PurposePublish.
	Purpose string `json:"purpose"`
}

// CredentialResult answers credential. A nil Credential is absence.
type CredentialResult struct {
	// Credential is the provider's credential for the purpose; absent when the
	// provider holds none, which is a supported answer and not an error.
	Credential *Credential `json:"credential,omitempty"`
}

// Credential is one purpose's bearer, its expiry and the hosts it serves.
type Credential struct {
	// Bearer is the token sent as `Authorization: Bearer <bearer>`: visible
	// ASCII, no whitespace, at most MaxBearerBytes bytes.
	Bearer string `json:"bearer"`
	// ExpiresAt is the RFC 3339 UTC instant ("Z") the bearer stops being
	// valid. The engine asks again before it.
	ExpiresAt string `json:"expiresAt"`
	// Hosts are the sorted, unique hosts the bearer may be sent to, each
	// matching CredentialHostPattern: `host` names the default port of the
	// request's scheme, `host:port` names that port.
	Hosts []string `json:"hosts"`
}

// String keeps a formatted credential from printing its bearer.
func (c Credential) String() string {
	return fmt.Sprintf("{bearer:<redacted> expiresAt:%s hosts:%v}", c.ExpiresAt, c.Hosts)
}

// GoString keeps %#v from printing the bearer.
func (c Credential) GoString() string { return "registry.Credential" + c.String() }

// Expiry returns the parsed ExpiresAt of a validated credential.
func (c Credential) Expiry() (time.Time, error) {
	return parseUTCInstant(c.ExpiresAt)
}

// Serves reports whether the bearer may be sent to target. It is true only
// when target uses https, or http to a loopback host; carries no userinfo;
// and one of Hosts names target's host and effective port. The host is
// compared in lowercase. The effective port is target's explicit port, else
// 443 for https and 80 for http. A hosts entry without a port names that
// default port; an entry with a port names exactly that port.
func (c Credential) Serves(target *url.URL) bool {
	if target == nil || target.User != nil {
		return false
	}
	scheme := strings.ToLower(target.Scheme)
	hostname := strings.ToLower(target.Hostname())
	var defaultPort int
	switch scheme {
	case "https":
		defaultPort = 443
	case "http":
		if !loopbackHost(hostname) {
			return false
		}
		defaultPort = 80
	default:
		return false
	}
	port := defaultPort
	if raw := target.Port(); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			return false
		}
		port = parsed
	}
	for _, entry := range c.Hosts {
		name, entryPort, ok := splitCredentialHost(entry)
		if !ok {
			continue
		}
		if entryPort == 0 {
			entryPort = defaultPort
		}
		if name == hostname && entryPort == port {
			return true
		}
	}
	return false
}

func loopbackHost(hostname string) bool {
	if hostname == "localhost" {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// splitCredentialHost splits a valid hosts entry into its name and its port,
// 0 when the entry names none.
func splitCredentialHost(entry string) (string, int, bool) {
	if !ValidCredentialHost(entry) {
		return "", 0, false
	}
	name, rawPort, hasPort := strings.Cut(entry, ":")
	if !hasPort {
		return name, 0, true
	}
	port, _ := strconv.Atoi(rawPort)
	return name, port, true
}

// ValidCredentialHost reports whether entry is a valid hosts entry.
func ValidCredentialHost(entry string) bool {
	if len(entry) > MaxCredentialHostBytes || !credentialHost.MatchString(entry) {
		return false
	}
	name, rawPort, hasPort := strings.Cut(entry, ":")
	if len(name) > 253 {
		return false
	}
	if hasPort {
		port, err := strconv.Atoi(rawPort)
		return err == nil && port >= 1 && port <= 65535
	}
	return true
}

// ValidCredentialBearer reports whether bearer is a well-formed credential
// bearer: ValidBearer, visible ASCII only, at most MaxBearerBytes bytes.
func ValidCredentialBearer(bearer string) bool {
	if !ValidBearer(bearer) || len(bearer) > MaxBearerBytes {
		return false
	}
	for index := 0; index < len(bearer); index++ {
		if bearer[index] < 0x21 || bearer[index] > 0x7e {
			return false
		}
	}
	return true
}

// ValidRunCredential reports whether credential is a well-formed run
// credential: ValidBearer, valid UTF-8, at most MaxRunCredentialBytes bytes.
func ValidRunCredential(credential string) bool {
	return ValidBearer(credential) && len(credential) <= MaxRunCredentialBytes && utf8.ValidString(credential)
}

// ValidateCredential checks the bearer, the expiry grammar and the hosts. It
// never compares ExpiresAt with a clock: whether a credential is still usable
// is the engine's decision at the moment it sends a request.
func ValidateCredential(credential Credential) error {
	if !ValidCredentialBearer(credential.Bearer) {
		return errors.New("registry: credential bearer is empty, too long, or not visible ASCII")
	}
	if _, err := parseUTCInstant(credential.ExpiresAt); err != nil {
		return errors.New("registry: credential expiresAt must be an RFC 3339 UTC instant")
	}
	if len(credential.Hosts) == 0 || len(credential.Hosts) > MaxCredentialHosts {
		return fmt.Errorf("registry: credential hosts must list 1 to %d hosts", MaxCredentialHosts)
	}
	for index, host := range credential.Hosts {
		if !ValidCredentialHost(host) {
			return fmt.Errorf("registry: credential hosts[%d] is not a lowercase host or host:port", index)
		}
		if index > 0 && credential.Hosts[index-1] >= host {
			return errors.New("registry: credential hosts must be sorted and unique")
		}
	}
	return nil
}

// ValidateRefusal checks a refusal's code and message bounds.
func ValidateRefusal(refusal CredentialRefusal) error {
	if !refusalCode.MatchString(refusal.Code) {
		return fmt.Errorf("registry: refusal code must match %s", RefusalCodePattern)
	}
	if len(refusal.Message) > MaxRefusalMessageBytes || !boundedText(refusal.Message) {
		return fmt.Errorf("registry: refusal message must be at most %d bytes of text without control characters", MaxRefusalMessageBytes)
	}
	return nil
}

func parseUTCInstant(value string) (time.Time, error) {
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil || instant.Location() != time.UTC || !strings.HasSuffix(value, "Z") {
		return time.Time{}, errors.New("not an RFC 3339 UTC instant")
	}
	return instant, nil
}

func boundedText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < ' ' || char == 0x7f || char == utf8.RuneError {
			return false
		}
	}
	return true
}

func validCapabilities(capabilities []string) error {
	if capabilities == nil || len(capabilities) > MaxCredentialCapabilities {
		return fmt.Errorf("registry: capabilities must be an array of at most %d names", MaxCredentialCapabilities)
	}
	for _, capability := range capabilities {
		if !capabilityGrammar.MatchString(capability) {
			return fmt.Errorf("registry: capability %q must match %s", capability, CapabilityPattern)
		}
	}
	return nil
}

// ParseCredentialRequest strictly decodes one request line and its payload.
func ParseCredentialRequest(line []byte) (*CredentialRequest, error) {
	if err := strictLine(line); err != nil {
		return nil, err
	}
	var request CredentialRequest
	if err := strictDecode(line, &request, "protocolVersion", "id", "op"); err != nil {
		return nil, err
	}
	if request.ProtocolVersion != CredentialProtocolVersion || request.ID <= 0 || !request.Op.Valid() {
		return nil, errors.New("registry: invalid credential request envelope")
	}
	switch request.Op {
	case CredentialOpInitialize:
		if _, err := ParseCredentialInitializeParams(request.Payload); err != nil {
			return nil, err
		}
	case CredentialOpCredential:
		if _, err := ParseCredentialParams(request.Payload); err != nil {
			return nil, err
		}
	case CredentialOpShutdown:
		if len(request.Payload) != 0 {
			return nil, errors.New("registry: a shutdown request carries no payload")
		}
	}
	return &request, nil
}

// ParseCredentialResponse strictly decodes one response line answering op,
// including its payload or refusal.
func ParseCredentialResponse(line []byte, op CredentialOp) (*CredentialResponse, error) {
	if err := strictLine(line); err != nil {
		return nil, err
	}
	var response CredentialResponse
	if err := strictDecode(line, &response, "protocolVersion", "id", "ok"); err != nil {
		return nil, err
	}
	if response.ProtocolVersion != CredentialProtocolVersion || response.ID <= 0 || !op.Valid() {
		return nil, errors.New("registry: invalid credential response envelope")
	}
	if !response.OK {
		if response.Error == nil || len(response.Payload) != 0 {
			return nil, errors.New("registry: a refusal carries an error and no payload")
		}
		if err := ValidateRefusal(*response.Error); err != nil {
			return nil, err
		}
		return &response, nil
	}
	if response.Error != nil {
		return nil, errors.New("registry: a successful answer carries no error")
	}
	switch op {
	case CredentialOpInitialize:
		if _, err := ParseCredentialInitializeResult(response.Payload); err != nil {
			return nil, err
		}
	case CredentialOpCredential:
		if _, err := ParseCredentialResult(response.Payload); err != nil {
			return nil, err
		}
	case CredentialOpShutdown:
		if len(response.Payload) != 0 {
			var empty struct{}
			if err := strictDecode(response.Payload, &empty); err != nil {
				return nil, errors.New("registry: a shutdown answer carries no payload or {}")
			}
		}
	}
	return &response, nil
}

// ParseCredentialInitializeParams strictly decodes an initialize payload. A
// runCredential member, when present, is a valid run credential; its errors
// never quote it.
func ParseCredentialInitializeParams(payload json.RawMessage) (*CredentialInitializeParams, error) {
	var params CredentialInitializeParams
	if err := strictPayload(payload, &params, "protocolVersion", "capabilities"); err != nil {
		return nil, err
	}
	if params.ProtocolVersion < 1 {
		return nil, errors.New("registry: initialize protocolVersion must be positive")
	}
	if err := validCapabilities(params.Capabilities); err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		return nil, err
	}
	if _, present := members["runCredential"]; present && !ValidRunCredential(params.RunCredential) {
		return nil, fmt.Errorf("registry: initialize runCredential must be 1 to %d bytes of UTF-8 with no whitespace", MaxRunCredentialBytes)
	}
	return &params, nil
}

// ParseCredentialInitializeResult strictly decodes an initialize answer.
func ParseCredentialInitializeResult(payload json.RawMessage) (*CredentialInitializeResult, error) {
	var result CredentialInitializeResult
	if err := strictPayload(payload, &result, "protocolVersion", "capabilities"); err != nil {
		return nil, err
	}
	if result.ProtocolVersion < 1 {
		return nil, errors.New("registry: initialize protocolVersion must be positive")
	}
	if len(result.ProviderName) > MaxProviderNameBytes || !boundedText(result.ProviderName) {
		return nil, fmt.Errorf("registry: providerName must be at most %d bytes of text", MaxProviderNameBytes)
	}
	if err := validCapabilities(result.Capabilities); err != nil {
		return nil, err
	}
	return &result, nil
}

// ParseCredentialParams strictly decodes a credential payload.
func ParseCredentialParams(payload json.RawMessage) (*CredentialParams, error) {
	var params CredentialParams
	if err := strictPayload(payload, &params, "purpose"); err != nil {
		return nil, err
	}
	if !ValidPurpose(params.Purpose) {
		return nil, fmt.Errorf("registry: purpose %q is not %s or %s", params.Purpose, PurposeRead, PurposePublish)
	}
	return &params, nil
}

// ParseCredentialResult strictly decodes a credential answer. A result with
// no credential member is absence. The credential it returns is the value it
// validated.
func ParseCredentialResult(payload json.RawMessage) (*CredentialResult, error) {
	var result CredentialResult
	if err := strictPayload(payload, &result); err != nil {
		return nil, err
	}
	if result.Credential == nil {
		return &CredentialResult{}, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		return nil, err
	}
	var credential Credential
	if err := strictDecode(members["credential"], &credential, "bearer", "expiresAt", "hosts"); err != nil {
		return nil, err
	}
	if err := ValidateCredential(credential); err != nil {
		return nil, err
	}
	return &CredentialResult{Credential: &credential}, nil
}

// strictPayload decodes a required op payload. A payload that reached here
// through ParseCredentialRequest or ParseCredentialResponse was already
// checked by strictLine; a payload given alone is checked here.
func strictPayload(payload json.RawMessage, target any, required ...string) error {
	if len(payload) == 0 {
		return errors.New("registry: missing payload")
	}
	if err := strictLine(payload); err != nil {
		return err
	}
	return strictDecode(payload, target, required...)
}
