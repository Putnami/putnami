package clicore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
)

// decodeJSONObject decodes a JSON object answer and turns a status outside
// allowStatuses (default 200) into the CLI's exit-coded APIError, naming the
// server's error_description, else its error, else the status line.
func decodeJSONObject(url string, text []byte, statusCode int, status string, allowStatuses []int) (map[string]any, error) {
	data := map[string]any{}
	if len(text) > 0 {
		if err := json.Unmarshal(text, &data); err != nil {
			return nil, InvalidJSONResponseError(url, statusCode, text)
		}
	}
	if !statusAllowed(allowStatuses, statusCode) {
		return nil, unexpectedStatusError(url, statusCode, ValueString(data, "code"), ServerErrorMessage(data, status, "error_description", "error"))
	}
	return data, nil
}

// invalidJSONSnippetMax bounds how much of an undecodable response body the CLI
// echoes back. A few hundred bytes is enough to identify a gateway HTML page, a
// proxy timeout, or a truncated body, and small enough that an unexpected
// upstream response can never dump unbounded output into a terminal or a CI log.
const invalidJSONSnippetMax = 256

// InvalidJSONResponseError renders a response the CLI could not decode as JSON
// into a DIAGNOSABLE error. The bare "invalid JSON response from <url>" it
// replaces threw away the two facts that identify the cause — the HTTP status
// and what the body actually was — which is why a control-plane handler that
// outlived its own write timeout and returned a 133-byte Cloud Run 503 read, to
// every operator, as an unexplained client-side parse failure.
//
// The body snippet is bounded (invalidJSONSnippetMax) and sanitized: only
// printable runes survive, so an unexpected upstream body cannot smuggle
// terminal escape sequences, unbounded output, or raw binary into CLI logs. It
// is still an untrusted upstream value — never parse it, only show it.
func InvalidJSONResponseError(url string, statusCode int, body []byte) error {
	message := "invalid JSON response from " + url
	if statusCode != 0 {
		message += " (HTTP " + StatusLine(statusCode) + ")"
	}
	if snippet := sanitizeBodySnippet(body, invalidJSONSnippetMax); snippet != "" {
		message += ": " + snippet
	}
	return NewError(message, ExitAPI)
}

// sanitizeBodySnippet returns at most maxBytes of body rendered safe for a
// terminal: invalid UTF-8 is repaired, every non-printable rune (control
// characters, ANSI escapes, NULs) becomes a space, runs of whitespace collapse
// to one, and a truncated snippet is marked as such.
func sanitizeBodySnippet(body []byte, maxBytes int) string {
	text := strings.ToValidUTF8(string(body), " ")
	truncated := false
	if len(text) > maxBytes {
		text = text[:maxBytes]
		// Drop a partial trailing rune left by the byte-wise cut.
		text = strings.ToValidUTF8(text, "")
		truncated = true
	}
	var b strings.Builder
	b.Grow(len(text))
	space := true // leading whitespace is dropped
	for _, r := range text {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		b.WriteRune(r)
		space = false
	}
	out := strings.TrimRight(b.String(), " ")
	if out == "" {
		return ""
	}
	if truncated {
		out += " […truncated]"
	}
	return out
}

// JSONBody marshals a request body for a caller that builds its own
// *http.Request and hands it to SendJSON.
func JSONBody(body any) (io.Reader, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, NewError("marshal request body: "+err.Error(), ExitAPI)
	}
	return bytes.NewReader(data), nil
}

// RequestBuildError is the CLI error for a request that could not be built,
// worded like every other failure of the same request.
func RequestBuildError(url string, err error) error {
	return NewError(fmt.Sprintf("request failed for %s: %s", url, err.Error()), ExitAPI)
}

// SendJSON issues a request the caller built itself and decodes a JSON object
// answer: the unified User-Agent, Accept (and Content-Type when the request
// has a body) set to application/json, allowStatuses (default 200) as
// success, 401 mapped to ExitAuth. It serves the calls whose wire contract is
// an external standard rather than a first-party provider contract (the OAuth
// 2.0 and OpenID Connect legs listed in clientgen.external.json): each builds
// its request where the standard is cited, and only the response handling is
// shared. A Putnami provider is reached through its generated client instead.
func SendJSON(client *http.Client, req *http.Request, allowStatuses []int) (map[string]any, error) {
	url := req.URL.String()
	text, statusCode, status, err := doJSONRequest(client, url, req)
	if err != nil {
		return nil, err
	}
	return decodeJSONObject(url, text, statusCode, status, allowStatuses)
}

// SendJSONInto is the typed variant of SendJSON: it decodes the successful
// answer into *T instead of a map, with the same headers, the same
// allowStatuses (default 200) and the same 401 → ExitAuth mapping. A refusal
// names the server's message, read from error_description, error, then
// message. It serves an external read whose answer is not a JSON object, such
// as the go.dev download index (a JSON array).
func SendJSONInto[T any](client *http.Client, req *http.Request, allowStatuses []int) (*T, error) {
	url := req.URL.String()
	text, statusCode, status, err := doJSONRequest(client, url, req)
	if err != nil {
		return nil, err
	}
	if !statusAllowed(allowStatuses, statusCode) {
		return nil, APIErrorFromResponse(url, statusCode, status, text)
	}
	var out T
	if len(text) > 0 {
		if err := json.Unmarshal(text, &out); err != nil {
			return nil, InvalidJSONResponseError(url, statusCode, text)
		}
	}
	return &out, nil
}

// doJSONRequest stamps the JSON headers the caller left unset and the
// User-Agent, issues req and reads the whole answer. A nil client is
// http.DefaultClient.
func doJSONRequest(client *http.Client, url string, req *http.Request) (text []byte, statusCode int, status string, err error) {
	if client == nil {
		client = http.DefaultClient
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if req.Body != nil && req.Body != http.NoBody && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	SetUserAgent(req)
	resp, err := client.Do(req) //nolint:gosec // G704: CLI intentionally requests the user-configured Putnami Cloud endpoint
	if err != nil {
		return nil, 0, "", NewError(fmt.Sprintf("request failed for %s: %s", url, err.Error()), ExitAPI)
	}
	defer resp.Body.Close()
	text, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, "", NewError(fmt.Sprintf("request failed for %s: %s", url, err.Error()), ExitAPI)
	}
	return text, resp.StatusCode, resp.Status, nil
}

// statusAllowed reports whether statusCode is one the caller opted into,
// defaulting to 200 when AllowStatuses is unset.
func statusAllowed(allowed []int, statusCode int) bool {
	if len(allowed) == 0 {
		allowed = []int{http.StatusOK}
	}
	return containsStatus(allowed, statusCode)
}

// unexpectedStatusError renders a non-allowed status into a CLI error carrying
// the server's message, mapping 401 to ExitAuth so auth failures stay distinct.
func unexpectedStatusError(url string, statusCode int, apiCode, message string) error {
	code := ExitAPI
	if statusCode == http.StatusUnauthorized {
		code = ExitAuth
	}
	return &APIError{
		URL:        url,
		StatusCode: statusCode,
		Code:       apiCode,
		Message:    message,
		exit:       newExitError(fmt.Sprintf("request failed for %s: %s", url, message), code),
	}
}

// APIError is a non-success HTTP response with the control plane's stable
// machine code preserved separately from its human message. Callers use Code
// for typed guidance and never need to parse Error() strings.
type APIError struct {
	URL        string
	StatusCode int
	Code       string
	Message    string
	exit       *ExitError
}

func (e *APIError) Error() string {
	if e.exit != nil {
		return e.exit.Error()
	}
	return fmt.Sprintf("request failed for %s: %s", e.URL, e.Message)
}

func (e *APIError) Unwrap() error { return e.exit }

// APIErrorCode returns the stable server code carried by err, or "" when err
// is not a typed API response error.
func APIErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// APIErrorFromResponse decodes a standard control-plane error envelope and
// returns an APIError with the correct CLI exit classification.
func APIErrorFromResponse(url string, statusCode int, status string, body []byte) error {
	data := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &data)
	}
	return unexpectedStatusError(url, statusCode, ValueString(data, "code"), ServerErrorMessage(data, status, "error_description", "error", "message"))
}

// ServerErrorMessage extracts a user-facing message from a decoded server error
// object using the caller-supplied field precedence, falling back to status.
// Most control-plane handlers return {error,message}; deploy deliberately
// prefers message first because it carries failing-step context.
func ServerErrorMessage(data map[string]any, status string, fields ...string) string {
	if len(fields) == 0 {
		fields = []string{"error", "message"}
	}
	for _, field := range fields {
		if message := ValueString(data, field); message != "" {
			return message
		}
	}
	return status
}

// ServerErrorMessageFromBody is ServerErrorMessage for callers that only need
// an error-body surface and do not otherwise decode the response body.
func ServerErrorMessageFromBody(body []byte, status string, fields ...string) string {
	data := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &data)
	}
	return ServerErrorMessage(data, status, fields...)
}

// StatusLine renders an HTTP status code the same way net/http responses expose
// it, e.g. "404 Not Found". It keeps helpers that only return status codes from
// downgrading error fallbacks to bare reason phrases.
func StatusLine(status int) string {
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("%d %s", status, text)
	}
	return fmt.Sprintf("%d", status)
}

// BearerHeaders returns the Authorization header map for a raw access token.
func BearerHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// Bearer wraps an access token so it doesn't leak via %v / %+v / log
// formatters. String() returns "***"; Authorization() returns the real
// "Bearer <token>" header value (and is the only path that should ever
// be called on the token). Pass Bearer values across function
// boundaries rather than raw strings for any code that might end up in
// a structured-log payload.
type Bearer struct{ raw string }

// NewBearer wraps a raw access token in a redaction-safe Bearer.
func NewBearer(token string) Bearer { return Bearer{raw: token} }

// String returns a redacted placeholder so the token never leaks via %v / %s.
func (b Bearer) String() string { return "***" }

// GoString returns a redacted placeholder for %#v formatting.
func (b Bearer) GoString() string { return "bearer(***)" }

// MarshalJSON renders the token as a redacted JSON string.
func (b Bearer) MarshalJSON() ([]byte, error) { return []byte(`"***"`), nil }

// MarshalText renders the token as redacted text.
func (b Bearer) MarshalText() ([]byte, error) { return []byte("***"), nil }

// Authorization returns the real "Bearer <token>" header value — the only path
// that exposes the wrapped token.
func (b Bearer) Authorization() string { return "Bearer " + b.raw }

// Empty reports whether the wrapped token is the empty string.
func (b Bearer) Empty() bool { return b.raw == "" }

func containsStatus(statuses []int, status int) bool {
	for _, item := range statuses {
		if item == status {
			return true
		}
	}
	return false
}
