package client

import (
	"encoding/base64"
	stderrors "errors"
	"net/http"
	"strings"
	"testing"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func additionalFreeForm() *clientcontract.AdditionalProperties {
	return &clientcontract.AdditionalProperties{Allowed: boolPointer(true)}
}

// errorEnvelope wraps a declared details body in the first-party error envelope
// `{code, error, message, details?}` (ADR 0003). ADR 0006 is why every declared
// schema in this file describes the details body and never the envelope: the
// envelope is fixed by the contract, so it is not re-declared per error.
func errorEnvelope(code, details string) []byte {
	body := `{"code":"` + code + `","error":"Bad Request","message":"the request was rejected"`
	if details != "" {
		body += `,"details":` + details
	}
	return []byte(body + `}`)
}

func TestRemoteErrorNamesTheServiceAndTheOperation(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "remote-error-identity", "a-remote-error-names-the-service-and-the-operation")
	err := decodeRemoteError(
		callIdentity{serviceID: "inventory", operationID: "putItem"},
		&Response{StatusCode: http.StatusConflict, Body: []byte(`{"code":"errors.conflict"}`)},
		[]clientcontract.DeclaredError{{Status: http.StatusConflict, Code: "errors.conflict"}},
		nil, nil, false,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("declared error = %T %v", err, err)
	}
	if remote.Service() != "inventory" || remote.Operation() != "putItem" {
		t.Fatalf("identity = %q/%q", remote.Service(), remote.Operation())
	}
	if message := remote.Error(); !strings.Contains(message, "inventory.putItem") || !strings.Contains(message, "errors.conflict") {
		t.Fatalf("error message = %q", message)
	}

	// An undeclared status still carries the identity, because it is read from
	// the generated contract rather than from the response.
	undeclared := decodeRemoteError(
		callIdentity{serviceID: "inventory", operationID: "putItem"},
		&Response{StatusCode: http.StatusBadGateway}, nil, nil, nil, false,
	)
	if !stderrors.As(undeclared, &remote) || remote.Service() != "inventory" || remote.Operation() != "putItem" {
		t.Fatalf("undeclared error = %T %v", undeclared, undeclared)
	}
	if message := remote.Error(); !strings.Contains(message, string(CodeClientRemote)) {
		t.Fatalf("undeclared message = %q", message)
	}
}

// TestDeclaredErrorSchemaDescribesTheDetailsMember is the Go half of the
// cross-language fixture of ADR 0006. Its schema and its body are the corpus
// `not_found` error of `GET /widgets/{id}`
// (protocols/clientcontract/fixtures/openapi/valid/full.openapi.json), and the
// TypeScript half decodes the same bytes in
// typescript/framework/client/test/runtime/errors.test.ts.
func TestDeclaredErrorSchemaDescribesTheDetailsMember(t *testing.T) {
	const body = `{"code":"not_found","error":"Not Found",` +
		`"message":"widget 4f0c8f4e-0e8a-4d1e-9a2b-6f5a1c0d3b77 does not exist",` +
		`"details":{"resource":"widget","metadata":{"tenant":"acme"}}}`
	minLength, maxLength := 1, 128
	details := clientcontract.Schema{
		Type: "object",
		Properties: map[string]clientcontract.Schema{
			"resource": {Type: "string", MinLength: &minLength, MaxLength: &maxLength},
			"metadata": {
				Type:                 "object",
				AdditionalProperties: &clientcontract.AdditionalProperties{Schema: &clientcontract.Schema{Type: "string"}},
			},
		},
		Required:             []string{"resource"},
		AdditionalProperties: additionalForbidden(),
	}
	declared := []clientcontract.DeclaredError{{Status: http.StatusNotFound, Code: "not_found", Schema: &details}}

	err := decodeRemoteError(
		callIdentity{serviceID: "widgets", operationID: "getWidget"},
		&Response{StatusCode: http.StatusNotFound, Body: []byte(body)},
		declared, nil, nil, false,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("well-formed declared error = %T %v", err, err)
	}
	if remote.Code() != "not_found" || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("identity = %q/%d, want not_found/404", remote.Code(), remote.StatusCode)
	}
	if got := string(remote.Payload); got != `{"metadata":{"tenant":"acme"},"resource":"widget"}` {
		t.Fatalf("payload = %s, want the details member alone", got)
	}

	// The envelope is not the details body. A response that carries only the
	// envelope where a details schema is declared is a contract violation, and
	// the client says so instead of publishing a payload it never validated.
	missing := decodeRemoteError(
		callIdentity{serviceID: "widgets", operationID: "getWidget"},
		&Response{StatusCode: http.StatusNotFound, Body: errorEnvelope("not_found", "")},
		declared, nil, nil, false,
	)
	if !errors.Is(missing, CodeClientResponse) {
		t.Fatalf("missing details = %T %v, want client.response", missing, missing)
	}
}

func TestRemoteErrorRedactsCredentialsInEveryEncodingItCanTake(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "error-payload-redaction", "a-credential-carried-as-a-number-or-a-boolean-never-reaches-the-payload")
	spectest.Proves(t, "go/typed-service-clients", "error-payload-redaction", "a-credential-carried-as-base64-bytes-never-reaches-the-payload")
	stringField := clientcontract.Schema{Type: "string"}
	for name, test := range map[string]struct {
		schema  clientcontract.Schema
		secrets []string
		details string
		want    string
	}{
		"declared integer equal to the token": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{
					"reason": stringField, "issued": {Type: "integer"},
				}, Required: []string{"reason"}, AdditionalProperties: additionalForbidden(),
			},
			secrets: []string{"4815162342"},
			details: `{"reason":"quota exhausted","issued":4815162342}`,
			want:    "",
		},
		"declared boolean equal to the token": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{
					"reason": stringField, "enabled": {Type: "boolean"},
				}, Required: []string{"reason"}, AdditionalProperties: additionalForbidden(),
			},
			secrets: []string{"true"},
			details: `{"reason":"quota exhausted","enabled":true}`,
			want:    "",
		},
		"declared string carrying the token base64 encoded": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{
					"reason": stringField, "detail": stringField,
				}, Required: []string{"reason"}, AdditionalProperties: additionalForbidden(),
			},
			secrets: []string{"active-service-token"},
			details: `{"reason":"quota exhausted","detail":"` +
				base64.StdEncoding.EncodeToString([]byte("rejected credential active-service-token")) + `"}`,
			want: `{"detail":"[REDACTED]","reason":"quota exhausted"}`,
		},
		"declared byte payload whose decoded bytes contain the token": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{
					"reason": stringField, "blob": {Type: "string", Format: "byte"},
				}, Required: []string{"reason", "blob"}, AdditionalProperties: additionalForbidden(),
			},
			secrets: []string{"active-service-token"},
			details: `{"reason":"quota exhausted","blob":"` +
				base64.StdEncoding.EncodeToString([]byte("\x00\x01active-service-token\xff")) + `"}`,
			// The marker is not base64, so the declared format cannot be honored
			// and the whole payload is dropped rather than published.
			want: "",
		},
		"undeclared number equal to the token": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{"reason": stringField},
				Required: []string{"reason"}, AdditionalProperties: additionalFreeForm(),
			},
			secrets: []string{"4815162342"},
			details: `{"reason":"quota exhausted","attempt":4815162342}`,
			want:    `{"reason":"quota exhausted"}`,
		},
		"undeclared boolean equal to the token": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{"reason": stringField},
				Required: []string{"reason"}, AdditionalProperties: additionalFreeForm(),
			},
			secrets: []string{"false"},
			details: `{"reason":"quota exhausted","cached":false}`,
			want:    `{"reason":"quota exhausted"}`,
		},
		"undeclared string decoding to the token": {
			schema: clientcontract.Schema{
				Type: "object", Properties: map[string]clientcontract.Schema{"reason": stringField},
				Required: []string{"reason"}, AdditionalProperties: additionalFreeForm(),
			},
			secrets: []string{"active-service-token"},
			details: `{"reason":"quota exhausted","dump":"` +
				base64.StdEncoding.EncodeToString([]byte("active-service-token")) + `"}`,
			want: `{"dump":"[REDACTED]","reason":"quota exhausted"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			declared := []clientcontract.DeclaredError{{Status: http.StatusBadRequest, Code: "errors.invalid", Schema: &test.schema}}
			err := decodeRemoteError(
				callIdentity{serviceID: "inventory", operationID: "putItem"},
				&Response{StatusCode: http.StatusBadRequest, Body: errorEnvelope("errors.invalid", test.details)},
				declared, nil, test.secrets, false,
			)
			var remote *RemoteError
			if !stderrors.As(err, &remote) || remote.Code() != "errors.invalid" {
				t.Fatalf("declared error = %T %v", err, err)
			}
			if got := string(remote.Payload); got != test.want {
				t.Fatalf("safe payload = %q, want %q", got, test.want)
			}
			for _, secret := range test.secrets {
				if strings.Contains(string(remote.Payload), secret) {
					t.Fatalf("payload leaked %q: %s", secret, remote.Payload)
				}
			}
		})
	}
}

func TestRedactionKeepsProviderDeclaredBusinessFields(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "error-payload-redaction", "a-declared-business-field-survives-redaction")
	schema := clientcontract.Schema{
		Type: "object",
		Properties: map[string]clientcontract.Schema{
			"reason":             {Type: "string"},
			"tokenCount":         {Type: "integer"},
			"authorizationLevel": {Type: "string"},
			"passwordPolicy":     {Type: "string"},
			"token":              {Type: "string"},
		},
		Required:             []string{"reason"},
		AdditionalProperties: additionalForbidden(),
	}
	declared := []clientcontract.DeclaredError{{Status: http.StatusBadRequest, Code: "errors.invalid", Schema: &schema}}
	err := decodeRemoteError(
		callIdentity{serviceID: "inventory", operationID: "putItem"},
		&Response{StatusCode: http.StatusBadRequest, Body: errorEnvelope("errors.invalid",
			`{"reason":"quota exhausted","tokenCount":3,"authorizationLevel":"reader","passwordPolicy":"rotate-90d","token":"unrelated-value"}`)},
		declared, nil, []string{"active-service-token"}, false,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("declared error = %T %v", err, err)
	}
	payload := string(remote.Payload)
	for _, field := range []string{`"tokenCount":3`, `"authorizationLevel":"reader"`, `"passwordPolicy":"rotate-90d"`} {
		if !strings.Contains(payload, field) {
			t.Fatalf("declared business field dropped: %s missing from %s", field, payload)
		}
	}
	// A property whose whole name is a credential is still removed: it holds a
	// credential rather than data about one.
	if strings.Contains(payload, "unrelated-value") || strings.Contains(payload, `"token"`) {
		t.Fatalf("credential-named property survived: %s", payload)
	}
}

func TestUndeclaredCredentialShapedKeysAreStillDropped(t *testing.T) {
	schema := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"reason": {Type: "string"}},
		Required: []string{"reason"}, AdditionalProperties: additionalFreeForm(),
	}
	declared := []clientcontract.DeclaredError{{Status: http.StatusBadRequest, Code: "errors.invalid", Schema: &schema}}
	err := decodeRemoteError(
		callIdentity{serviceID: "inventory", operationID: "putItem"},
		&Response{StatusCode: http.StatusBadRequest, Body: errorEnvelope("errors.invalid",
			`{"reason":"quota exhausted","refreshTokenHint":"opaque","attempts":2}`)},
		declared, nil, nil, false,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("declared error = %T %v", err, err)
	}
	if got := string(remote.Payload); got != `{"attempts":2,"reason":"quota exhausted"}` {
		t.Fatalf("free-form payload = %s", got)
	}
}

// carryRemoteMessageBody is the provider answer both runtimes decode in the
// CarryRemoteMessage opt-in tests: one complete first-party envelope with a
// stable code, the status text, free-text prose and a declared details member.
// The TypeScript half reads the same bytes in
// typescript/framework/client/test/runtime/errors.test.ts, so the two gates
// are readable side by side.
const carryRemoteMessageBody = `{"code":"not_found","error":"Not Found",` +
	`"message":"widget 4f0c8f4e does not exist",` +
	`"details":{"resource":"widget"}}`

func carryRemoteMessageDeclared() []clientcontract.DeclaredError {
	details := clientcontract.Schema{
		Type:                 "object",
		Properties:           map[string]clientcontract.Schema{"resource": {Type: "string"}},
		Required:             []string{"resource"},
		AdditionalProperties: additionalForbidden(),
	}
	return []clientcontract.DeclaredError{{Status: http.StatusNotFound, Code: "not_found", Schema: &details}}
}

// TestRemoteErrorDropsTheEnvelopeMessageWithoutTheBindingOptIn pins the default:
// an unset ServiceBinding.CarryRemoteMessage drops the envelope message, so a
// consumer that only logs or forwards a failure never widens its own exposure
// to the provider's prose. The declared details member is unaffected: it has a
// schema, the message does not.
func TestRemoteErrorDropsTheEnvelopeMessageWithoutTheBindingOptIn(t *testing.T) {
	err := decodeRemoteError(
		callIdentity{serviceID: "widgets", operationID: "getWidget"},
		&Response{StatusCode: http.StatusNotFound, Body: []byte(carryRemoteMessageBody)},
		carryRemoteMessageDeclared(), nil, nil, false,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("declared error = %T %v", err, err)
	}
	if remote.Message != "" {
		t.Fatalf("message = %q, want no provider prose without the opt-in", remote.Message)
	}
	if got := string(remote.Payload); got != `{"resource":"widget"}` {
		t.Fatalf("payload = %s, want the declared details member", got)
	}

	// An undeclared code takes the same default: the option gates the envelope,
	// not the declaration.
	undeclared := decodeRemoteError(
		callIdentity{serviceID: "widgets", operationID: "getWidget"},
		&Response{StatusCode: http.StatusBadGateway, Body: []byte(
			`{"code":"upstream.unreachable","error":"Bad Gateway","message":"upstream inventory service is unreachable"}`,
		)},
		nil, nil, nil, false,
	)
	var undeclaredRemote *RemoteError
	if !stderrors.As(undeclared, &undeclaredRemote) {
		t.Fatalf("undeclared error = %T %v", undeclared, undeclared)
	}
	if undeclaredRemote.Message != "" {
		t.Fatalf("undeclared message = %q, want no provider prose without the opt-in", undeclaredRemote.Message)
	}
}

// TestRemoteErrorCarriesTheEnvelopeMessageWhenTheBindingOptsIn is the Go half
// of the CarryRemoteMessage opt-in: a CLI that shows a provider failure to
// the human who typed the command sets CarryRemoteMessage on its binding and
// gets the provider's own text (`errors.NotFound("widget 4f0c... does not
// exist")`) instead of a generic status line. WriteHTTPError already gates that text to client-safe
// categories server-side (go/framework/errors/http.go) before it reaches the
// wire, so the opt-in adds the consumer's consent, not a new trust.
func TestRemoteErrorCarriesTheEnvelopeMessageWhenTheBindingOptsIn(t *testing.T) {
	err := decodeRemoteError(
		callIdentity{serviceID: "widgets", operationID: "getWidget"},
		&Response{StatusCode: http.StatusNotFound, Body: []byte(carryRemoteMessageBody)},
		carryRemoteMessageDeclared(), nil, nil, true,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("declared error = %T %v", err, err)
	}
	if remote.Message != "widget 4f0c8f4e does not exist" {
		t.Fatalf("message = %q", remote.Message)
	}
	if got := string(remote.Payload); got != `{"resource":"widget"}` {
		t.Fatalf("payload = %s, want the declared details member", got)
	}

	// Undeclared: the code has no matching DeclaredError, so decodeRemoteError
	// falls back to CodeClientRemote, and the same opt-in carries the message.
	undeclared := decodeRemoteError(
		callIdentity{serviceID: "widgets", operationID: "getWidget"},
		&Response{StatusCode: http.StatusBadGateway, Body: []byte(
			`{"code":"upstream.unreachable","error":"Bad Gateway","message":"upstream inventory service is unreachable"}`,
		)},
		nil, nil, nil, true,
	)
	var undeclaredRemote *RemoteError
	if !stderrors.As(undeclared, &undeclaredRemote) {
		t.Fatalf("undeclared error = %T %v", undeclared, undeclared)
	}
	if undeclaredRemote.Message != "upstream inventory service is unreachable" {
		t.Fatalf("undeclared message = %q", undeclaredRemote.Message)
	}
}

// TestRemoteErrorRedactsSecretsFromTheEnvelopeMessage proves the opt-in never
// bypasses this call's own credential redaction: a handler that carelessly
// echoes the caller's own token in its message text (e.g.
// errors.BadRequest("invalid token "+token)) never reaches RemoteError.Message
// verbatim, whatever the binding asked for. The secret is substituted inline
// so the prose around it survives; the TypeScript runtime produces the same
// string for the same input (errors.test.ts).
func TestRemoteErrorRedactsSecretsFromTheEnvelopeMessage(t *testing.T) {
	secret := "active-service-token"
	err := decodeRemoteError(
		callIdentity{serviceID: "inventory", operationID: "putItem"},
		&Response{StatusCode: http.StatusBadRequest, Body: []byte(
			`{"code":"errors.invalid","error":"Bad Request","message":"invalid token active-service-token"}`,
		)},
		nil, nil, []string{secret}, true,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("error = %T %v", err, err)
	}
	if remote.Message != "invalid token [REDACTED]" {
		t.Fatalf("message = %q, want the secret substituted inline", remote.Message)
	}
	if strings.Contains(remote.Message, secret) {
		t.Fatalf("message leaked the secret: %q", remote.Message)
	}
}

// TestSanitizedErrorMessageRedactsInlineAndReplacesAnOpaqueBlobWhole pins the
// two rules both runtimes share for the message, as distinct from `details`:
// a secret in prose — raw or in either base64 form secretMaterialInText
// compares — is substituted inline and the words around it survive; a message
// that is itself one base64 value whose bytes disclose a secret is replaced
// whole, because an opaque blob has no position to substitute at.
func TestSanitizedErrorMessageRedactsInlineAndReplacesAnOpaqueBlobWhole(t *testing.T) {
	bearer := "active-service-token"
	secrets := []string{bearer}
	for name, test := range map[string]struct{ message, want string }{
		"inline raw":        {"credential " + bearer + " expired", "credential [REDACTED] expired"},
		"inline every hit":  {bearer + " and again " + bearer, "[REDACTED] and again [REDACTED]"},
		"inline std base64": {"credential " + base64.StdEncoding.EncodeToString([]byte(bearer)) + " expired", "credential [REDACTED] expired"},
		"inline url base64": {"credential " + base64.RawURLEncoding.EncodeToString([]byte(bearer)) + " expired", "credential [REDACTED] expired"},
		"opaque blob":       {base64.StdEncoding.EncodeToString([]byte("credential " + bearer + " expired")), "[REDACTED]"},
		"clean prose":       {"widget 4f0c does not exist", "widget 4f0c does not exist"},
		"empty secret":      {"credential  expired", "credential  expired"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sanitizedErrorMessage(test.message, secrets, true); got != test.want {
				t.Fatalf("sanitizedErrorMessage(%q) = %q, want %q", test.message, got, test.want)
			}
			if got := sanitizedErrorMessage(test.message, secrets, false); got != "" {
				t.Fatalf("without the opt-in = %q, want nothing", got)
			}
		})
	}
	if got := sanitizedErrorMessage("credential  expired", []string{""}, true); got != "credential  expired" {
		t.Fatalf("an empty secret must redact nothing, got %q", got)
	}
}

// TestWebSocketErrorFrameFollowsTheSameMessageOptIn keeps the two transports of
// one runtime — and the two runtimes — saying the same thing. A provider error
// frame carries the same free-text `message` an HTTP envelope does
// (clientcontract.WebSocketRemoteErrorV1), the TypeScript service WebSocket
// transport already decodes it through the same path as an HTTP body, and both
// now read one consumer opt-in rather than one per transport.
func TestWebSocketErrorFrameFollowsTheSameMessageOptIn(t *testing.T) {
	frame := &clientcontract.WebSocketErrorFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameError,
		Error: clientcontract.WebSocketRemoteErrorV1{
			Status: http.StatusNotFound, Code: "not_found", Message: "widget 4f0c8f4e does not exist",
		},
	}
	identity := callIdentity{serviceID: "widgets", operationID: "watchWidget"}
	declared := []clientcontract.DeclaredError{{Status: http.StatusNotFound, Code: "not_found"}}

	var remote *RemoteError
	if err := decodeWebSocketError(identity, frame, declared, nil, nil, false); !stderrors.As(err, &remote) {
		t.Fatalf("default error = %T %v", err, err)
	}
	if remote.Message != "" {
		t.Fatalf("message = %q, want no provider prose without the opt-in", remote.Message)
	}

	if err := decodeWebSocketError(identity, frame, declared, nil, nil, true); !stderrors.As(err, &remote) {
		t.Fatalf("opted-in error = %T %v", err, err)
	}
	if remote.Message != "widget 4f0c8f4e does not exist" {
		t.Fatalf("opted-in message = %q", remote.Message)
	}

	// The opt-in never bypasses this call's own credential redaction.
	leaking := &clientcontract.WebSocketErrorFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameError,
		Error: clientcontract.WebSocketRemoteErrorV1{
			Status: http.StatusBadRequest, Code: "errors.invalid", Message: "invalid token active-service-token",
		},
	}
	if err := decodeWebSocketError(identity, leaking, nil, nil, []string{"active-service-token"}, true); !stderrors.As(err, &remote) {
		t.Fatalf("leaking error = %T %v", err, err)
	}
	if remote.Message != "invalid token [REDACTED]" {
		t.Fatalf("leaking message = %q, want the secret substituted inline", remote.Message)
	}
}
