package errors

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Core Error ---

func TestNew(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "creation", "new-captures-origin-stack")
	err := New(CodeInternal, "something broke", String("key", "val"))
	if err.Code() != CodeInternal {
		t.Errorf("code = %q, want %q", err.Code(), CodeInternal)
	}
	if err.Message() != "something broke" {
		t.Errorf("message = %q, want %q", err.Message(), "something broke")
	}
	if len(err.Stack()) == 0 {
		t.Error("New should capture stack")
	}
	if err.Time().IsZero() {
		t.Error("time should be set")
	}
	if len(err.Attrs()) != 1 || err.Attrs()[0].Key != "key" {
		t.Error("attrs not set")
	}
}

func TestNewf(t *testing.T) {
	err := Newf(CodeTimeout, "timed out after %ds", 30)
	if err.Message() != "timed out after 30s" {
		t.Errorf("message = %q", err.Message())
	}
	if len(err.Stack()) == 0 {
		t.Error("Newf should capture stack")
	}
}

func TestWrap(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "creation", "wrap-adds-meaning-without-a-second-stack")
	cause := stderrors.New("disk full")
	err := Wrap(cause, CodeInternal, String("op", "write"))
	if err.Cause() != cause {
		t.Error("cause not set")
	}
	if len(err.Stack()) != 0 {
		t.Error("Wrap should NOT capture stack")
	}
	if err.Code() != CodeInternal {
		t.Errorf("code = %q", err.Code())
	}
	// Verify Unwrap compatibility
	if !stderrors.Is(err, cause) {
		t.Error("errors.Is should find cause")
	}
}

func TestWrapNil(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "creation", "wrap-nil-cause-returns-nil")
	if Wrap(nil, CodeInternal) != nil {
		t.Error("Wrap(nil) should return nil")
	}
}

func TestWrapf(t *testing.T) {
	cause := stderrors.New("conn refused")
	err := Wrapf(cause, CodeConnection, "database unreachable")
	if err.Message() != "database unreachable" {
		t.Errorf("message = %q", err.Message())
	}
	if err.Cause() != cause {
		t.Error("cause not set")
	}
}

func TestBug(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "creation", "bug-captures-origin-stack")
	cause := stderrors.New("nil pointer")
	err := Bug(cause)
	if err.Category() != CategoryBug {
		t.Errorf("category = %q, want %q", err.Category(), CategoryBug)
	}
	if len(err.Stack()) == 0 {
		t.Error("Bug should capture stack")
	}
	if err.Code() != CodeInternal {
		t.Errorf("code = %q", err.Code())
	}
}

func TestBugNil(t *testing.T) {
	if Bug(nil) != nil {
		t.Error("Bug(nil) should return nil")
	}
}

func TestBugf(t *testing.T) {
	err := Bugf("invariant violated: x=%d", 42)
	if err.Category() != CategoryBug {
		t.Errorf("category = %q", err.Category())
	}
	if !strings.Contains(err.Message(), "42") {
		t.Errorf("message = %q", err.Message())
	}
}

func TestUser(t *testing.T) {
	err := User(CodeBadRequest, "invalid email")
	if err.Category() != CategoryUser {
		t.Errorf("category = %q, want %q", err.Category(), CategoryUser)
	}
	if len(err.Stack()) != 0 {
		t.Error("User should NOT capture stack")
	}
}

func TestRetryable(t *testing.T) {
	cause := stderrors.New("timeout")
	err := Retryable(cause)
	if !err.IsRetryable() {
		t.Error("should be retryable")
	}
	// On existing *Error — should return a copy, not mutate the original
	existing := New(CodeTimeout, "timed out")
	ret := Retryable(existing)
	if ret == existing {
		t.Error("Retryable on *Error should return a new copy, not the same pointer")
	}
	if !ret.IsRetryable() {
		t.Error("returned error should be retryable")
	}
	if existing.IsRetryable() {
		t.Error("original error should NOT be mutated to retryable")
	}
	if ret.Code() != existing.Code() || ret.Message() != existing.Message() {
		t.Error("copy should preserve code and message")
	}
}

func TestRetryableNil(t *testing.T) {
	if Retryable(nil) != nil {
		t.Error("Retryable(nil) should return nil")
	}
}

// --- Chaining ---

func TestChaining(t *testing.T) {
	err := New(CodeInternal, "fail").
		WithSource("storage").
		WithCategory(CategoryInfra).
		WithRetryable(true).
		WithAttr(String("bucket", "uploads"))

	if err.Source() != "storage" {
		t.Errorf("source = %q", err.Source())
	}
	if err.Category() != CategoryInfra {
		t.Errorf("category = %q", err.Category())
	}
	if !err.IsRetryable() {
		t.Error("should be retryable")
	}
	if len(err.Attrs()) != 1 || err.Attrs()[0].Key != "bucket" {
		t.Error("attrs not chained")
	}
}

// --- Error interface ---

func TestErrorString(t *testing.T) {
	err := New(CodeNotFound, "user not found")
	expected := "not_found: user not found"
	if err.Error() != expected {
		t.Errorf("Error() = %q, want %q", err.Error(), expected)
	}

	cause := stderrors.New("conn refused")
	wrapped := Wrap(cause, CodeConnection)
	if !strings.Contains(wrapped.Error(), "conn refused") {
		t.Errorf("Error() = %q, should contain cause", wrapped.Error())
	}
}

// --- Inspect ---

func TestIs(t *testing.T) {
	inner := New(CodeNotFound, "not found")
	outer := Wrap(inner, CodeInternal)

	if !Is(outer, CodeInternal) {
		t.Error("should match outer code")
	}
	if !Is(outer, CodeNotFound) {
		t.Error("should match inner code via chain")
	}
	if Is(outer, CodeTimeout) {
		t.Error("should not match unrelated code")
	}
}

func TestGetCode(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "inspection", "code-discoverable-across-wrapped-tree")
	err := New(CodeTimeout, "timed out")
	if GetCode(err) != CodeTimeout {
		t.Errorf("GetCode = %q", GetCode(err))
	}
	if GetCode(stderrors.New("plain")) != CodeUnknown {
		t.Error("plain error should return CodeUnknown")
	}
}

func TestGetError(t *testing.T) {
	err := New(CodeInternal, "fail")
	if GetError(err) != err {
		t.Error("should return same error")
	}
	if GetError(stderrors.New("plain")) != nil {
		t.Error("plain error should return nil")
	}
}

func TestGetCategory(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "inspection", "category-discoverable")
	err := Bug(stderrors.New("oops"))
	if GetCategory(err) != CategoryBug {
		t.Errorf("GetCategory = %q", GetCategory(err))
	}
}

func TestGetAttrs(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "inspection", "attributes-collected-across-wrapped-tree")
	inner := New(CodeNotFound, "not found", String("id", "123"))
	outer := Wrap(inner, CodeInternal, String("handler", "GetUser"))

	attrs := GetAttrs(outer)
	if len(attrs) != 2 {
		t.Fatalf("expected 2 attrs, got %d", len(attrs))
	}
}

func TestIsRetryable(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "inspection", "retryability-discoverable-across-wrapped-tree")
	inner := New(CodeTimeout, "timeout").WithRetryable(true)
	outer := Wrap(inner, CodeInternal)
	if !IsRetryable(outer) {
		t.Error("should find retryable in chain")
	}
	if IsRetryable(New(CodeInternal, "fail")) {
		t.Error("should not be retryable")
	}
}

// --- Hooks ---

func TestHooks(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "observation", "hooks-fire-only-for-origin-and-bug-errors")
	defer resetHooks()

	var captured *Error
	OnError(func(err *Error) {
		captured = err
	})

	created := New(CodeInternal, "test hook")
	if captured != created {
		t.Error("hook should fire on New")
	}

	captured = nil
	Bug(stderrors.New("bug"))
	if captured == nil {
		t.Error("hook should fire on Bug")
	}

	captured = nil
	Wrap(stderrors.New("x"), CodeInternal)
	if captured != nil {
		t.Error("hook should NOT fire on Wrap")
	}
}

// --- HTTP ---

func TestHTTPStatus(t *testing.T) {
	tests := []struct {
		err    *Error
		status int
	}{
		{BadRequest("msg"), 400},
		{Unauthorized("msg"), 401},
		{Forbidden("msg"), 403},
		{NotFound("msg"), 404},
		{MethodNotAllowed("msg"), 405},
		{Conflict("msg"), 409},
		{UnprocessableEntity("msg"), 422},
		{TooManyRequests("msg"), 429},
		{InternalServerError("msg"), 500},
		{BadGateway("msg"), 502},
		{ServiceUnavailable("msg"), 503},
		{GatewayTimeout("msg"), 504},
	}

	for _, tt := range tests {
		t.Run(string(tt.err.Code()), func(t *testing.T) {
			if got := HTTPStatus(tt.err); got != tt.status {
				t.Errorf("HTTPStatus = %d, want %d", got, tt.status)
			}
		})
	}

	// Non-Error falls back to 500
	if HTTPStatus(stderrors.New("plain")) != 500 {
		t.Error("plain error should map to 500")
	}
}

func TestHTTPStatusForCode(t *testing.T) {
	status, ok := HTTPStatusForCode(CodeNotFound)
	if !ok {
		t.Fatal("expected CodeNotFound to have an HTTP status")
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
	if _, ok := HTTPStatusForCode(Code("custom.unregistered")); ok {
		t.Error("unregistered code should not have an HTTP status")
	}
}

func TestWriteHTTPError(t *testing.T) {
	w := httptest.NewRecorder()
	err := Forbidden("access denied")
	WriteHTTPError(w, err)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	var body HTTPErrorBody
	json.NewDecoder(w.Body).Decode(&body)
	if body.Message != "access denied" {
		t.Errorf("message = %q, security errors should expose message", body.Message)
	}
}

func TestWriteHTTPErrorHidesInternal(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "disclosure", "http-internal-message-replaced")
	w := httptest.NewRecorder()
	err := InternalServerError("secret details about DB")
	WriteHTTPError(w, err)

	var body HTTPErrorBody
	json.NewDecoder(w.Body).Decode(&body)
	if body.Message != "An internal error occurred" {
		t.Errorf("message = %q, internal errors should be hidden", body.Message)
	}
}

func TestHTTPConvenienceCategories(t *testing.T) {
	if BadRequest("x").Category() != CategoryUser {
		t.Error("BadRequest should be user category")
	}
	if Unauthorized("x").Category() != CategorySecurity {
		t.Error("Unauthorized should be security category")
	}
	if Forbidden("x").Category() != CategorySecurity {
		t.Error("Forbidden should be security category")
	}
	if !TooManyRequests("x").IsRetryable() {
		t.Error("TooManyRequests should be retryable")
	}
	if !ServiceUnavailable("x").IsRetryable() {
		t.Error("ServiceUnavailable should be retryable")
	}
	if !GatewayTimeout("x").IsRetryable() {
		t.Error("GatewayTimeout should be retryable")
	}
}

// --- Validation ---

func TestValidationErrors(t *testing.T) {
	ve := &ValidationErrors{}
	ve.Add("name", "is required", "required", nil)
	ve.Add("age", "must be >= 0", "min", -1)

	if !ve.HasErrors() {
		t.Error("should have errors")
	}
	if len(ve.Errors) != 2 {
		t.Errorf("expected 2 errors, got %d", len(ve.Errors))
	}

	perr := ve.ToError()
	if HTTPStatus(perr) != 400 {
		t.Errorf("expected 400, got %d", HTTPStatus(perr))
	}
}

func TestValidationErrorsSensitive(t *testing.T) {
	ve := &ValidationErrors{}
	ve.AddSensitive("password", "too short", "minlen")

	errMsg := ve.Errors[0].Error()
	if errMsg != "password: too short" {
		t.Errorf("expected 'password: too short', got %q", errMsg)
	}
}

// --- Aggregate ---

func TestAggregateError(t *testing.T) {
	errs := []error{
		stderrors.New("error 1"),
		stderrors.New("error 2"),
	}
	agg := NewAggregate("batch failed", errs)
	if agg == nil {
		t.Fatal("expected non-nil aggregate")
	}
	if !stderrors.Is(agg, errs[0]) {
		t.Error("should unwrap to first error")
	}

	if NewAggregate("empty", nil) != nil {
		t.Error("empty errors should return nil")
	}
}

// --- JSON ---

func TestMarshalJSON(t *testing.T) {
	err := New(CodeNotFound, "user not found", String("id", "abc")).
		WithSource("handler").
		WithCategory(CategoryUser)

	data, jsonErr := json.Marshal(err)
	if jsonErr != nil {
		t.Fatal(jsonErr)
	}

	var decoded map[string]any
	json.Unmarshal(data, &decoded)

	if decoded["code"].(string) != "not_found" {
		t.Errorf("code = %v", decoded["code"])
	}
	if decoded["message"].(string) != "user not found" {
		t.Errorf("message = %v", decoded["message"])
	}
	if decoded["source"].(string) != "handler" {
		t.Errorf("source = %v", decoded["source"])
	}
	if decoded["category"].(string) != "user" {
		t.Errorf("category = %v", decoded["category"])
	}
}

func TestMarshalJSON_CauseChainExcluded(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "disclosure", "json-excludes-cause-chain")
	// Create a wrapped error chain: inner cause → middle wrap → outer wrap.
	inner := stderrors.New("pq: connection refused to 10.0.0.1:5432")
	middle := Wrap(inner, CodeInternal, String("query", "SELECT * FROM users"))
	// CategoryUser so the outer error's own message is client-safe and emitted;
	// the cause chain must still be excluded regardless.
	outer := Wrapf(middle, CodeNotFound, "user lookup failed", String("user_id", "abc")).
		WithCategory(CategoryUser)

	data, err := json.Marshal(outer)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	jsonStr := string(data)

	// Cause chain details must NOT appear in JSON output.
	if strings.Contains(jsonStr, "pq:") {
		t.Error("JSON should not contain inner cause (DB driver error)")
	}
	if strings.Contains(jsonStr, "connection refused") {
		t.Error("JSON should not contain connection details")
	}
	if strings.Contains(jsonStr, "10.0.0.1") {
		t.Error("JSON should not contain internal IP addresses")
	}
	if strings.Contains(jsonStr, "SELECT") {
		t.Error("JSON should not contain query text from inner error attrs")
	}

	// The outer error's own fields SHOULD be present.
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["code"].(string) != "not_found" {
		t.Errorf("code = %v, want not_found", decoded["code"])
	}
	if decoded["message"].(string) != "user lookup failed" {
		t.Errorf("message = %v, want 'user lookup failed'", decoded["message"])
	}

	// Verify no "cause" or "stack" keys exist in the JSON.
	if _, ok := decoded["cause"]; ok {
		t.Error("JSON should not contain 'cause' key")
	}
	if _, ok := decoded["stack"]; ok {
		t.Error("JSON should not contain 'stack' key")
	}
}

func TestMarshalJSON_StackExcluded(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "disclosure", "json-excludes-stack")
	// New() captures a stack trace — verify it's excluded from JSON.
	err := New(CodeInternal, "something broke")
	if len(err.Stack()) == 0 {
		t.Fatal("expected stack to be captured")
	}

	data, jsonErr := json.Marshal(err)
	if jsonErr != nil {
		t.Fatalf("MarshalJSON: %v", jsonErr)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, "stack") {
		t.Error("JSON should not contain stack traces")
	}
	if strings.Contains(jsonStr, "TestMarshalJSON_StackExcluded") {
		t.Error("JSON should not contain function names from stack")
	}
}

func TestMarshalJSON_RoundTrip(t *testing.T) {
	// A client-safe category so all fields round-trip; non-client-safe categories
	// intentionally marshal a generic message (see TestMarshalJSON_GatesInternalErrors).
	original := New(CodeTimeout, "request timed out").
		WithCategory(CategoryUser).
		WithRetryable(true).
		WithSource("gateway").
		WithAttr(String("endpoint", "/api/users"))

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored Error
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if restored.Code() != original.Code() {
		t.Errorf("code: got %q, want %q", restored.Code(), original.Code())
	}
	if restored.Message() != original.Message() {
		t.Errorf("message: got %q, want %q", restored.Message(), original.Message())
	}
	if restored.Category() != original.Category() {
		t.Errorf("category: got %q, want %q", restored.Category(), original.Category())
	}
	if restored.IsRetryable() != original.IsRetryable() {
		t.Errorf("retryable: got %v, want %v", restored.IsRetryable(), original.IsRetryable())
	}
	if restored.Source() != original.Source() {
		t.Errorf("source: got %q, want %q", restored.Source(), original.Source())
	}
}

// MarshalJSON must gate message/attrs by category like WriteHTTPError,
// so a direct json.Marshal(err) cannot leak internal text or sensitive attrs.
func TestMarshalJSON_GatesInternalErrors(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "disclosure", "json-gates-internal-message-and-attributes")
	secret := New(CodeInternal, "password authentication failed for user admin",
		String("dsn", "postgres://admin:s3cr3t@db:5432")).WithCategory(CategoryInfra)

	data, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)
	if strings.Contains(js, "password authentication") {
		t.Errorf("internal message leaked: %s", js)
	}
	if strings.Contains(js, "s3cr3t") || strings.Contains(js, `"dsn"`) {
		t.Errorf("sensitive attrs leaked: %s", js)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["message"] != "An internal error occurred" {
		t.Errorf("message = %v, want generic", decoded["message"])
	}
	if _, ok := decoded["attrs"]; ok {
		t.Errorf("attrs must be omitted for non-client-safe categories: %v", decoded["attrs"])
	}
	if decoded["code"] != "internal" {
		t.Errorf("code = %v, want internal (code/category stay for client branching)", decoded["code"])
	}

	// An uncategorized error is treated as non-client-safe too.
	plain, _ := json.Marshal(New(CodeInternal, "boom", String("k", "v")))
	if strings.Contains(string(plain), "boom") || strings.Contains(string(plain), `"k"`) {
		t.Errorf("uncategorized error leaked message/attrs: %s", plain)
	}

	// A client-safe error still exposes its message and attrs.
	safe, _ := json.Marshal(BadRequest("title is required", String("field", "title")))
	if !strings.Contains(string(safe), "title is required") || !strings.Contains(string(safe), "field") {
		t.Errorf("client-safe error should expose message/attrs: %s", safe)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	data := `{"code":"timeout","message":"request timed out","retryable":true,"category":"transient"}`
	var err Error
	if e := json.Unmarshal([]byte(data), &err); e != nil {
		t.Fatal(e)
	}
	if err.Code() != CodeTimeout {
		t.Errorf("code = %q", err.Code())
	}
	if !err.IsRetryable() {
		t.Error("should be retryable")
	}
	if err.Category() != CategoryTransient {
		t.Errorf("category = %q", err.Category())
	}
}

// --- Stack ---

func TestStackCapture(t *testing.T) {
	err := New(CodeInternal, "test")
	stack := err.Stack()
	if len(stack) == 0 {
		t.Fatal("stack should be captured")
	}
	formatted := stack.Format()
	if !strings.Contains(formatted, "TestStackCapture") {
		t.Errorf("stack should contain test function name, got:\n%s", formatted)
	}
}

// --- Attrs ---

func TestAttrTypes(t *testing.T) {
	attrs := []Attr{
		String("s", "val"),
		Int("i", 42),
		Int64("i64", 123),
		Float64("f", 3.14),
		Bool("b", true),
		Any("a", []int{1, 2}),
	}
	if len(attrs) != 6 {
		t.Errorf("expected 6 attrs, got %d", len(attrs))
	}
	if attrs[0].Value != "val" {
		t.Error("String attr value wrong")
	}
	if attrs[1].Value != 42 {
		t.Error("Int attr value wrong")
	}
}

// --- Re-exports ---

func TestStdlibReExports(t *testing.T) {
	inner := stderrors.New("root")
	outer := Wrap(inner, CodeInternal)

	var target *Error
	if !As(outer, &target) {
		t.Error("As should find *Error")
	}

	if Unwrap(outer) != inner {
		t.Error("Unwrap should return cause")
	}

	joined := Join(stderrors.New("a"), stderrors.New("b"))
	if joined == nil {
		t.Error("Join should return non-nil")
	}
}
