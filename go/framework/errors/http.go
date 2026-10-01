package errors

import (
	"encoding/json"
	"net/http"
	"sync"
)

// HTTP-specific error codes.
const (
	CodeBadRequest       Code = "http.bad_request"
	CodeMethodNotAllowed Code = "http.method_not_allowed"
	CodePayloadTooLarge  Code = "http.payload_too_large"
	// CodeUnsupportedMediaType is what a provider answers when the request
	// carries a media type the endpoint never declared. It is the refusal a
	// binary endpoint needs: a body it cannot interpret is not a body it should
	// read.
	CodeUnsupportedMediaType Code = "http.unsupported_media_type"
	CodeUnprocessableEntity  Code = "http.unprocessable_entity"
	CodeTooManyRequests      Code = "http.too_many_requests"
	CodeInternalServer       Code = "http.internal_server"
	CodeBadGateway           Code = "http.bad_gateway"
	CodeServiceUnavailable   Code = "http.service_unavailable"
	CodeGatewayTimeout       Code = "http.gateway_timeout"
)

var statusMu sync.RWMutex

// codeToStatus maps error codes to HTTP status codes.
var codeToStatus = map[Code]int{
	// HTTP-specific
	CodeBadRequest:           http.StatusBadRequest,
	CodeMethodNotAllowed:     http.StatusMethodNotAllowed,
	CodePayloadTooLarge:      http.StatusRequestEntityTooLarge,
	CodeUnsupportedMediaType: http.StatusUnsupportedMediaType,
	CodeUnprocessableEntity:  http.StatusUnprocessableEntity,
	CodeTooManyRequests:      http.StatusTooManyRequests,
	CodeInternalServer:       http.StatusInternalServerError,
	CodeBadGateway:           http.StatusBadGateway,
	CodeServiceUnavailable:   http.StatusServiceUnavailable,
	CodeGatewayTimeout:       http.StatusGatewayTimeout,

	// Generic codes mapped to HTTP
	CodeUnauthorized:   http.StatusUnauthorized,
	CodeForbidden:      http.StatusForbidden,
	CodeNotFound:       http.StatusNotFound,
	CodeConflict:       http.StatusConflict,
	CodeNotImplemented: http.StatusNotImplemented,
	CodeInternal:       http.StatusInternalServerError,
	CodeUnavailable:    http.StatusServiceUnavailable,
	CodeTimeout:        http.StatusGatewayTimeout,
	CodeValidation:     http.StatusBadRequest,
	CodeInvalidArg:     http.StatusBadRequest,
	CodeRateLimit:      http.StatusTooManyRequests,
	CodeAlreadyExists:  http.StatusConflict,
	CodePrecondition:   http.StatusPreconditionFailed,
}

// statusToCode is the canonical reverse mapping used by FromStatus. The
// HTTP-namespaced code is preferred where one exists; otherwise the generic
// code is used.
var statusToCode = map[int]Code{
	http.StatusBadRequest:            CodeBadRequest,
	http.StatusUnauthorized:          CodeUnauthorized,
	http.StatusForbidden:             CodeForbidden,
	http.StatusNotFound:              CodeNotFound,
	http.StatusMethodNotAllowed:      CodeMethodNotAllowed,
	http.StatusConflict:              CodeConflict,
	http.StatusRequestEntityTooLarge: CodePayloadTooLarge,
	http.StatusUnsupportedMediaType:  CodeUnsupportedMediaType,
	http.StatusPreconditionFailed:    CodePrecondition,
	http.StatusUnprocessableEntity:   CodeUnprocessableEntity,
	http.StatusTooManyRequests:       CodeTooManyRequests,
	http.StatusInternalServerError:   CodeInternalServer,
	http.StatusNotImplemented:        CodeNotImplemented,
	http.StatusBadGateway:            CodeBadGateway,
	http.StatusServiceUnavailable:    CodeServiceUnavailable,
	http.StatusGatewayTimeout:        CodeGatewayTimeout,
}

// HTTPStatus returns the HTTP status code for an error.
// Returns 500 for unknown codes or non-Error errors.
func HTTPStatus(err error) int {
	e := GetError(err)
	if e == nil {
		return http.StatusInternalServerError
	}
	if status, ok := HTTPStatusForCode(e.code); ok {
		return status
	}
	return http.StatusInternalServerError
}

// HTTPStatusForCode returns the HTTP status registered for a structured error code.
func HTTPStatusForCode(code Code) (int, bool) {
	statusMu.RLock()
	status, ok := codeToStatus[code]
	statusMu.RUnlock()
	return status, ok
}

// RegisterHTTPStatus registers a custom code → HTTP status mapping.
func RegisterHTTPStatus(code Code, status int) {
	statusMu.Lock()
	codeToStatus[code] = status
	statusMu.Unlock()
}

// HTTPErrorBody is the JSON body written by WriteHTTPError.
type HTTPErrorBody struct {
	Code    string `json:"code"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// WriteHTTPError writes a structured JSON error response.
// Only user/security category errors expose their message;
// internal errors get a generic message.
func WriteHTTPError(w http.ResponseWriter, err error) {
	status, body := HTTPErrorResponse(err)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body) //nolint:errcheck // HTTP error handler — write failure is unrecoverable
}

// HTTPErrorResponse returns the sanitized status and wire body used by
// WriteHTTPError. Transport adapters use this to preserve the same stable code
// and disclosure policy without recreating the envelope.
func HTTPErrorResponse(err error) (int, HTTPErrorBody) {
	e := GetError(err)
	status := HTTPStatus(err)

	body := HTTPErrorBody{
		Code:    string(CodeInternal),
		Error:   http.StatusText(status),
		Message: genericErrorMessage,
	}

	if e != nil {
		body.Code = string(e.code)
		// Only client-safe categories expose the message and any "details" attr.
		// Gating details inside the category check prevents internal/infra/bug
		// errors from leaking a "details" payload to the client.
		if clientSafeCategory(e.category) {
			body.Message = e.message
			for _, attr := range e.attrs {
				if attr.Key == "details" {
					body.Details = attr.Value
				}
			}
		}
	}

	return status, body
}

// --- HTTP convenience constructors ---

// BadRequest creates a 400 user error.
func BadRequest(msg string, attrs ...Attr) *Error {
	return User(CodeBadRequest, msg, attrs...)
}

// Unauthorized creates a 401 security error.
func Unauthorized(msg string, attrs ...Attr) *Error {
	e := User(CodeUnauthorized, msg, attrs...)
	e.category = CategorySecurity
	return e
}

// Forbidden creates a 403 security error.
func Forbidden(msg string, attrs ...Attr) *Error {
	e := User(CodeForbidden, msg, attrs...)
	e.category = CategorySecurity
	return e
}

// NotFound creates a 404 user error.
func NotFound(msg string, attrs ...Attr) *Error {
	return User(CodeNotFound, msg, attrs...)
}

// MethodNotAllowed creates a 405 user error.
func MethodNotAllowed(msg string, attrs ...Attr) *Error {
	return User(CodeMethodNotAllowed, msg, attrs...)
}

// Conflict creates a 409 user error.
func Conflict(msg string, attrs ...Attr) *Error {
	return User(CodeConflict, msg, attrs...)
}

// UnprocessableEntity creates a 422 user error.
func UnprocessableEntity(msg string, attrs ...Attr) *Error {
	return User(CodeUnprocessableEntity, msg, attrs...)
}

// TooManyRequests creates a 429 retryable error.
func TooManyRequests(msg string, attrs ...Attr) *Error {
	e := User(CodeTooManyRequests, msg, attrs...)
	e.retryable = true
	return e
}

// InternalServerError creates a 500 infra error.
func InternalServerError(msg string, attrs ...Attr) *Error {
	return New(CodeInternalServer, msg, attrs...).WithCategory(CategoryInfra)
}

// BadGateway creates a 502 infra error.
func BadGateway(msg string, attrs ...Attr) *Error {
	return New(CodeBadGateway, msg, attrs...).WithCategory(CategoryInfra)
}

// ServiceUnavailable creates a 503 retryable transient error.
func ServiceUnavailable(msg string, attrs ...Attr) *Error {
	e := New(CodeServiceUnavailable, msg, attrs...).WithCategory(CategoryTransient)
	e.retryable = true
	return e
}

// GatewayTimeout creates a 504 retryable transient error.
func GatewayTimeout(msg string, attrs ...Attr) *Error {
	e := New(CodeGatewayTimeout, msg, attrs...).WithCategory(CategoryTransient)
	e.retryable = true
	return e
}

// FromStatus creates an error from an HTTP status code. The returned Code is
// deterministic: well-known statuses use the canonical statusToCode table, and
// custom statuses registered via RegisterHTTPStatus fall back to the
// lexicographically smallest matching code so repeated calls are stable.
func FromStatus(status int, msg string) *Error {
	if code, ok := statusToCode[status]; ok {
		return New(code, msg)
	}
	statusMu.RLock()
	defer statusMu.RUnlock()
	var match Code
	found := false
	for code, s := range codeToStatus {
		if s == status && (!found || code < match) {
			match = code
			found = true
		}
	}
	if found {
		return New(match, msg)
	}
	return New(CodeUnknown, msg)
}
