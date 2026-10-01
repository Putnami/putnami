package client

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// CodeClientRemote is the stable framework code for an undeclared remote error.
const CodeClientRemote errors.Code = "client.remote"

// RemoteError is the typed, sanitized result of a non-success service response.
// It deliberately carries no raw response, token, request headers, or URL.
type RemoteError struct {
	// ServiceID is the provider service the failing call was bound to.
	ServiceID string
	// OperationID is the generated operation that produced the failure.
	OperationID string
	StatusCode  int
	RemoteCode  string
	GRPCCode    *int
	// Payload is the provider-declared, schema-validated `details` member of the
	// first-party error envelope — never the envelope itself (ADR 0006). It is
	// nil when details are undeclared or must be omitted to redact a credential
	// while preserving the declared schema. A malformed declared body is
	// surfaced as client.response instead of a RemoteError.
	Payload json.RawMessage
	// Message is the first-party envelope's free-text `message` member (ADR
	// 0003), with every occurrence of this call's own credential material —
	// raw, standard base64 or raw URL base64 — replaced inline by
	// "[REDACTED]", so the surrounding prose survives; a message that is one
	// opaque base64 value whose bytes disclose a secret is replaced whole. It
	// is empty unless the binding sets ServiceBinding.CarryRemoteMessage, and
	// empty when the response carried no message. The consumer opts in because
	// the prose is written for the human who made the request: a consumer that
	// displays it owns what it logs, and one that only forwards the failure
	// keeps the pre-opt-in exposure. Unlike Payload it needs no
	// provider-declared schema: WriteHTTPError only ever places genuinely
	// client-safe text there (a generic placeholder for internal/infra/bug
	// categories, the handler's own text otherwise — see
	// go/framework/errors/http.go), so redacting known secrets is enough to
	// carry it safely once the consumer asked for it. The envelope's `error`
	// member is never carried: every first-party writer sets it to
	// http.StatusText(StatusCode), which a caller already has.
	Message   string
	retryable bool
	framework *errors.Error
}

// Error names the code, the service and the operation. A caller reading one
// log line can tell which dependency failed without correlating a request id,
// and the identity comes from the generated contract, never from the response.
func (e *RemoteError) Error() string {
	if e == nil {
		return "client.remote: remote service request failed"
	}
	identity := e.ServiceID
	if e.OperationID != "" {
		if identity == "" {
			identity = e.OperationID
		} else {
			identity += "." + e.OperationID
		}
	}
	if identity == "" {
		return fmt.Sprintf("%s: remote service request failed", e.RemoteCode)
	}
	return fmt.Sprintf("%s: remote service %s request failed", e.RemoteCode, identity)
}

// Service returns the provider service identity carried by the error.
func (e *RemoteError) Service() string {
	if e == nil {
		return ""
	}
	return e.ServiceID
}

// Operation returns the generated operation identity carried by the error.
func (e *RemoteError) Operation() string {
	if e == nil {
		return ""
	}
	return e.OperationID
}

// Unwrap exposes a sanitized framework error for errors.Is/GetCode.
func (e *RemoteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.framework
}

// Code returns the provider-declared stable error code.
func (e *RemoteError) Code() string {
	if e == nil {
		return ""
	}
	return e.RemoteCode
}

// Retryable reports the provider-declared retry classification.
func (e *RemoteError) Retryable() bool { return e != nil && e.retryable }

// callIdentity names the generated call a remote error came from. It is read
// from the embedded contract, so it is present even when the response body is
// empty or undeclared.
type callIdentity struct {
	serviceID   string
	operationID string
}

// decodeRemoteError projects a non-success response onto the typed RemoteError.
// carryMessage is the consuming binding's ServiceBinding.CarryRemoteMessage: it
// is stated by every caller rather than defaulted, so a call site with no
// binding to read — an anonymous descriptor-only call, for one — cannot carry
// provider prose by omission.
func decodeRemoteError(identity callIdentity, response *Response, declared []clientcontract.DeclaredError, schemas map[string]clientcontract.Schema, secrets []string, carryMessage bool) error {
	status := 0
	var body []byte
	if response != nil {
		status = response.StatusCode
		body = response.Body
	}
	envelope := readFirstPartyErrorEnvelope(body)
	code := envelope.Code
	selected := selectDeclaredError(status, code, declared)
	if selected == nil {
		code = string(CodeClientRemote)
	} else {
		code = selected.Code
	}
	remote := &RemoteError{
		ServiceID:   identity.serviceID,
		OperationID: identity.operationID,
		StatusCode:  status,
		RemoteCode:  code,
		Message:     sanitizedErrorMessage(envelope.Message, secrets, carryMessage),
	}
	if selected != nil {
		remote.GRPCCode = selected.GRPCCode
		remote.retryable = selected.Retryable != nil && *selected.Retryable
		if selected.Schema != nil {
			// ADR 0006: the declared schema describes the `details` member, so it
			// is checked against `details` alone and never against the envelope
			// carrying it. An absent or non-conforming details body where a
			// schema is declared is a contract violation, not a degradation.
			var valid bool
			remote.Payload, valid = sanitizedErrorPayload(envelope.Details, selected.Schema, schemas, secrets)
			if !valid {
				return errors.New(CodeClientResponse, "remote service error details do not match the generated contract")
			}
		}
	}
	remote.framework = errors.New(errors.Code(code), "remote service request failed").WithRetryable(remote.retryable)
	return remote
}

func selectDeclaredError(status int, code string, declared []clientcontract.DeclaredError) *clientcontract.DeclaredError {
	var selected *clientcontract.DeclaredError
	for i := range declared {
		candidate := &declared[i]
		if candidate.Status != status || (code != "" && candidate.Code != code) {
			continue
		}
		if selected != nil && code == "" {
			return nil
		}
		selected = candidate
	}
	return selected
}

// firstPartyErrorEnvelope is the error body every first-party endpoint writes in
// either language: `{code, error, message, details?}` (ADR 0003). The stable
// code and the declared detail body live in separate members, so a declared
// schema is validated against Details alone (ADR 0006). `message` is read here
// and, when the binding opted in, scrubbed of this call's own credential
// material before reaching RemoteError.Message; `error` is never carried onto
// RemoteError because every first-party writer sets it to
// http.StatusText(StatusCode).
type firstPartyErrorEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

// sanitizedErrorMessage returns the envelope's free-text message with this
// call's own credential material redacted, or "" when the consuming binding
// did not opt in. The message differs from `details` on purpose: free text has
// no declared schema to break, so each occurrence of a secret — raw, in its
// standard base64 form or in its raw URL base64 form, the encodings
// secretMaterialInText already compares — is substituted inline and the prose
// around it survives, which is what a human reading the error needs. A
// structured scalar inside `details` has no such freedom and is dropped or
// replaced whole to keep the declared shape. The one whole-string case here is
// a message that is itself a base64 value whose decoded bytes disclose a
// secret: an opaque blob has no position to substitute at. The TypeScript
// runtime applies the same two rules (errors.ts, sanitizedErrorMessage).
func sanitizedErrorMessage(message string, secrets []string, carryMessage bool) string {
	if !carryMessage || message == "" {
		return ""
	}
	if decoded, ok := decodeBase64Payload(message); ok {
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(decoded, []byte(secret)) {
				return "[REDACTED]"
			}
		}
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, form := range []string{
			secret,
			base64.StdEncoding.EncodeToString([]byte(secret)),
			base64.RawURLEncoding.EncodeToString([]byte(secret)),
		} {
			message = strings.ReplaceAll(message, form, "[REDACTED]")
		}
	}
	return message
}

// readFirstPartyErrorEnvelope reads what the envelope carries. A body that is
// not a JSON object carries neither a code nor details; the caller then has no
// declared identity to select and reports client.remote rather than guessing
// one.
func readFirstPartyErrorEnvelope(body []byte) firstPartyErrorEnvelope {
	var envelope firstPartyErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return firstPartyErrorEnvelope{}
	}
	return envelope
}

// remoteCode is the stable code the envelope carries, for the callers that
// select a declared error without decoding its details.
func remoteCode(body []byte) string {
	return readFirstPartyErrorEnvelope(body).Code
}

func sanitizedErrorPayload(body []byte, schema *clientcontract.Schema, schemas map[string]clientcontract.Schema, secrets []string) (json.RawMessage, bool) {
	if _, ok := projectResponseJSON(body, schema, schemas, nil, false); !ok {
		return nil, false
	}
	projected, ok := projectResponseJSON(body, schema, schemas, secrets, true)
	if !ok {
		return nil, true
	}
	if _, ok := projectResponseJSON(projected, schema, schemas, nil, false); !ok {
		return nil, true
	}
	return projected, true
}

// scrubRemoteValue redacts an undeclared free-form value before it is exposed
// on a RemoteError. Undeclared data has no provider schema to preserve, so a
// leaking scalar is dropped outright; keep reports whether the caller may keep
// the value at all.
func scrubRemoteValue(value any, secrets []string) (result any, keep bool) {
	switch typed := value.(type) {
	case map[string]any:
		scrubbed := make(map[string]any, len(typed))
		for key, child := range typed {
			// Undeclared keys carry no contract, so a credential-shaped name is
			// still enough to drop them here. Declared properties are business
			// data and are handled by projectSchemaValue instead.
			if credentialShapedName(key) || secretMaterialInText(key, secrets) {
				continue
			}
			projected, keepChild := scrubRemoteValue(child, secrets)
			if !keepChild {
				continue
			}
			scrubbed[key] = projected
		}
		return scrubbed, true
	case []any:
		scrubbed := make([]any, 0, len(typed))
		for i := range typed {
			projected, keepItem := scrubRemoteValue(typed[i], secrets)
			if !keepItem {
				// A dropped element would silently reindex the array, so the whole
				// array goes instead of shifting positions under the reader.
				return nil, false
			}
			scrubbed = append(scrubbed, projected)
		}
		return scrubbed, true
	case string:
		if secretMaterialInText(typed, secrets) {
			return "[REDACTED]", true
		}
		return typed, true
	default:
		// json.Number and bool cannot hold a redaction marker without changing
		// their JSON type, so a scalar that equals a credential is dropped.
		return typed, !secretScalar(typed, secrets)
	}
}

// secretMaterialInText reports whether text discloses known credential material.
// It compares three encodings of the same bytes: the raw UTF-8 text, the base64
// form a provider may have used to carry binary details, and the bytes text
// itself decodes to when it is base64.
func secretMaterialInText(text string, secrets []string) bool {
	if text == "" {
		return false
	}
	decoded, decodedOK := decodeBase64Payload(text)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(text, secret) {
			return true
		}
		if strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(secret))) ||
			strings.Contains(text, base64.RawURLEncoding.EncodeToString([]byte(secret))) {
			return true
		}
		if decodedOK && bytes.Contains(decoded, []byte(secret)) {
			return true
		}
	}
	return false
}

// decodeBase64Payload decodes text as base64 in the standard and URL alphabets,
// padded or not. It reports false for text that is not base64 at all so plain
// prose is never compared as if it were bytes.
func decodeBase64Payload(text string) ([]byte, bool) {
	if len(text) < 4 {
		return nil, false
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(text); err == nil && len(decoded) > 0 {
			return decoded, true
		}
	}
	return nil, false
}

// secretScalar reports whether a non-string JSON scalar is exactly a known
// credential value. A provider that answers with the caller's token as a number
// or a boolean leaks it just as completely as a string would.
func secretScalar(value any, secrets []string) bool {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case bool:
		text = strconv.FormatBool(typed)
	case float64:
		text = strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return false
	}
	for _, secret := range secrets {
		if secret != "" && secret == text {
			return true
		}
	}
	return false
}

// credentialShapedName reports whether an undeclared key name looks like a
// credential anywhere in its text. Undeclared data has no contract to protect,
// so the substring match is the right trade there. It is deliberately not
// applied to provider-declared properties.
func credentialShapedName(key string) bool {
	lower := strings.ToLower(key)
	return strings.Contains(lower, "token") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey")
}

// credentialNames lists the field names that are a credential rather than data
// about one. The list is exact on purpose: a declared property is business data
// and must survive redaction unless its whole name is one of these.
var credentialNames = map[string]bool{
	"accesstoken":   true,
	"apikey":        true,
	"apitoken":      true,
	"authorization": true,
	"bearertoken":   true,
	"clientsecret":  true,
	"credential":    true,
	"credentials":   true,
	"idtoken":       true,
	"password":      true,
	"refreshtoken":  true,
	"secret":        true,
	"token":         true,
}

// credentialNamedField reports whether a provider-declared property name is
// exactly a credential name. tokenCount, authorizationLevel and passwordPolicy
// are business fields and are not matched; token and clientSecret are.
func credentialNamedField(key string) bool {
	normalized := strings.Map(func(char rune) rune {
		if char == '_' || char == '-' || char == '.' || char == ' ' {
			return -1
		}
		return char
	}, strings.ToLower(key))
	return credentialNames[normalized]
}
