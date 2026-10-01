package errors

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Wrap/Bug/Retryable must not duplicate the cause message ---

func TestErrorString_NoCauseDuplication(t *testing.T) {
	cause := stderrors.New("disk full")
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{"Wrap", Wrap(cause, CodeInternal), "internal: disk full"},
		{"Bug", Bug(cause), "internal: disk full"},
		{"Retryable", Retryable(cause), "unknown: disk full"},
		{"Wrapf", Wrapf(cause, CodeInternal, "save failed"), "internal: save failed: disk full"},
		{"New", New(CodeNotFound, "user not found"), "not_found: user not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Attr serializes with lowercase JSON keys ---

func TestAttrJSONKeys(t *testing.T) {
	// CategoryUser makes attrs client-safe and emitted; this test
	// asserts the JSON key casing of the attrs that are serialized.
	err := New(CodeNotFound, "nope", String("id", "abc")).WithSource("svc").WithCategory(CategoryUser)
	data, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"key":"id"`) {
		t.Errorf("attrs should use lowercase \"key\", got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"value":"abc"`) {
		t.Errorf("attrs should use lowercase \"value\", got: %s", jsonStr)
	}
	if strings.Contains(jsonStr, `"Key"`) || strings.Contains(jsonStr, `"Value"`) {
		t.Errorf("attrs must not use capitalized keys, got: %s", jsonStr)
	}

	// Round-trip: lowercase keys must unmarshal back into the Attr fields.
	var restored Error
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(restored.Attrs()) != 1 || restored.Attrs()[0].Key != "id" || restored.Attrs()[0].Value != "abc" {
		t.Errorf("round-tripped attr = %+v, want {id abc}", restored.Attrs())
	}
}

// --- FromStatus returns a deterministic code ---

func TestFromStatus_Deterministic(t *testing.T) {
	seen := map[Code]struct{}{}
	for range 50 {
		seen[FromStatus(http.StatusInternalServerError, "boom").Code()] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("FromStatus(500) returned %d distinct codes across calls, want 1: %v", len(seen), seen)
	}
	if got := FromStatus(http.StatusInternalServerError, "boom").Code(); got != CodeInternalServer {
		t.Errorf("FromStatus(500).Code() = %q, want %q", got, CodeInternalServer)
	}
	if got := FromStatus(http.StatusBadRequest, "x").Code(); got != CodeBadRequest {
		t.Errorf("FromStatus(400).Code() = %q, want %q", got, CodeBadRequest)
	}
	if got := FromStatus(599, "x").Code(); got != CodeUnknown {
		t.Errorf("FromStatus(unmapped).Code() = %q, want %q", got, CodeUnknown)
	}
}

func TestFromStatus_CustomDeterministicFallback(t *testing.T) {
	RegisterHTTPStatus(Code("custom.bbb"), 751)
	RegisterHTTPStatus(Code("custom.aaa"), 751)
	seen := map[Code]struct{}{}
	for range 50 {
		seen[FromStatus(751, "x").Code()] = struct{}{}
	}
	if len(seen) != 1 {
		t.Fatalf("FromStatus(751) not deterministic: %v", seen)
	}
	if got := FromStatus(751, "x").Code(); got != Code("custom.aaa") {
		t.Errorf("FromStatus(751).Code() = %q, want lexicographically smallest %q", got, "custom.aaa")
	}
}

// --- WriteHTTPError must not leak details for internal errors ---

func TestWriteHTTPError_DetailsGatedByCategory(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "disclosure", "http-details-gated-by-category")
	// Internal error carrying a "details" attr: details must NOT reach the client.
	w := httptest.NewRecorder()
	internal := InternalServerError("boom").WithAttr(Any("details", "secret stack info"))
	WriteHTTPError(w, internal)
	var body HTTPErrorBody
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Details != nil {
		t.Errorf("internal error leaked details = %v, want nil", body.Details)
	}
	if body.Message != "An internal error occurred" {
		t.Errorf("internal message = %q, should be hidden", body.Message)
	}

	// User error with a "details" attr: details SHOULD be exposed.
	w2 := httptest.NewRecorder()
	userErr := BadRequest("invalid input").WithAttr(Any("details", "field email required"))
	WriteHTTPError(w2, userErr)
	var body2 HTTPErrorBody
	if err := json.NewDecoder(w2.Body).Decode(&body2); err != nil {
		t.Fatal(err)
	}
	if body2.Details != "field email required" {
		t.Errorf("user error details = %v, want %q", body2.Details, "field email required")
	}
}

// --- Hooks are panic-isolated and reentrancy-safe ---

func TestFireHooks_PanicIsolation(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "observation", "hooks-are-panic-isolated")
	resetHooks()
	defer resetHooks()

	OnError(func(*Error) { panic("hook boom") })
	ran := false
	OnError(func(*Error) { ran = true })

	// A panicking hook must not abort error creation or the remaining hooks.
	err := New(CodeInternal, "x")
	if err == nil {
		t.Fatal("New returned nil after panicking hook")
	}
	if !ran {
		t.Error("hooks after a panicking hook should still run")
	}
}

func TestFireHooks_ReentrantNoDeadlock(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "observation", "hooks-are-reentrancy-safe")
	resetHooks()
	defer resetHooks()

	// Hooks that re-enter the hook registry must not deadlock (RWMutex is not
	// reentrant, so fireHooks must invoke hooks without holding the lock).
	OnError(func(*Error) { OnError(func(*Error) {}) })
	OnError(func(*Error) { resetHooks() })

	done := make(chan struct{})
	go func() {
		_ = New(CodeInternal, "x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant hook deadlocked fireHooks")
	}
}

// --- OnError returns a working, idempotent deregistration handle ---

func TestOnError_Deregister(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "observation", "hooks-are-removable")
	resetHooks()
	defer resetHooks()

	var count int
	remove := OnError(func(*Error) { count++ })

	_ = New(CodeInternal, "one")
	if count != 1 {
		t.Fatalf("hook fired %d times, want 1", count)
	}

	// After deregistration the hook must stop firing.
	remove()
	_ = New(CodeInternal, "two")
	if count != 1 {
		t.Errorf("hook fired after deregister: count = %d, want 1", count)
	}

	// Deregister is idempotent and must not remove an unrelated hook.
	var other int
	OnError(func(*Error) { other++ })
	remove() // second call — no-op
	_ = New(CodeInternal, "three")
	if other != 1 {
		t.Errorf("second deregister removed the wrong hook: other = %d, want 1", other)
	}
}

// --- Exported-symbol coverage ---

func TestCodeString(t *testing.T) {
	if CodeNotFound.String() != "not_found" {
		t.Errorf("Code.String() = %q, want %q", CodeNotFound.String(), "not_found")
	}
	if Code("db.connection").String() != "db.connection" {
		t.Errorf("Code.String() lost value")
	}
}

func TestDurationAttr(t *testing.T) {
	d := 5 * time.Second
	attr := Duration("elapsed", d)
	if attr.Key != "elapsed" {
		t.Errorf("Duration key = %q", attr.Key)
	}
	if attr.Value != d {
		t.Errorf("Duration value = %v, want %v", attr.Value, d)
	}
}

func TestAggregateErrorString(t *testing.T) {
	// Zero errors: falls back to the bare message.
	if got := (&AggregateError{Message: "batch failed"}).Error(); got != "batch failed" {
		t.Errorf("empty aggregate Error() = %q, want %q", got, "batch failed")
	}
	// Single error.
	single := &AggregateError{Message: "batch failed", Errors: []error{stderrors.New("e1")}}
	if got := single.Error(); got != "batch failed: e1" {
		t.Errorf("single aggregate Error() = %q, want %q", got, "batch failed: e1")
	}
	// Many errors.
	many := &AggregateError{Message: "batch failed", Errors: []error{
		stderrors.New("e1"), stderrors.New("e2"),
	}}
	if got := many.Error(); got != "batch failed: e1; e2" {
		t.Errorf("multi aggregate Error() = %q, want %q", got, "batch failed: e1; e2")
	}
}

func TestValidationErrorsString(t *testing.T) {
	single := &ValidationErrors{}
	single.Add("name", "is required", "required", nil)
	if got := single.Error(); got != "validation failed: name: is required" {
		t.Errorf("single ValidationErrors.Error() = %q", got)
	}

	multi := &ValidationErrors{}
	multi.Add("name", "is required", "required", nil)
	multi.Add("age", "must be >= 0", "min", -1)
	got := multi.Error()
	if !strings.HasPrefix(got, "validation failed (2 errors): ") {
		t.Errorf("multi ValidationErrors.Error() = %q, want '(2 errors)' prefix", got)
	}
	if !strings.Contains(got, "name: is required") || !strings.Contains(got, "age: must be >= 0 (got -1)") {
		t.Errorf("multi ValidationErrors.Error() missing field detail: %q", got)
	}
}

func TestFieldErrorString(t *testing.T) {
	// With a non-sensitive value.
	withVal := FieldError{Field: "age", Message: "too small", Value: -1}
	if got := withVal.Error(); got != "age: too small (got -1)" {
		t.Errorf("FieldError.Error() = %q, want %q", got, "age: too small (got -1)")
	}
	// Without a value.
	noVal := FieldError{Field: "name", Message: "is required"}
	if got := noVal.Error(); got != "name: is required" {
		t.Errorf("FieldError.Error() = %q, want %q", got, "name: is required")
	}
	// Sensitive value is never rendered.
	ve := &ValidationErrors{}
	ve.AddSensitive("password", "too short", "minlen")
	if got := ve.Errors[0].Error(); got != "password: too short" {
		t.Errorf("sensitive FieldError.Error() = %q, want %q", got, "password: too short")
	}
}

// --- Inspect helpers descend into AggregateError children ---

func TestInspectDescendsIntoAggregate(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "inspection", "inspection-descends-into-aggregate")
	// child carries a code, a retryable flag, and an attr; it is NOT the first
	// error in the aggregate, so a single-level Unwrap walk never reaches it.
	child := New(CodeNotFound, "missing", String("id", "abc")).WithRetryable(true)
	agg := NewAggregate("startup failed", []error{
		New(CodeUnknown, "other"),
		child,
	})

	if !Is(agg, CodeNotFound) {
		t.Error("Is(agg, CodeNotFound) = false, want true (code on an aggregate child must be visible)")
	}
	if !IsRetryable(agg) {
		t.Error("IsRetryable(agg) = false, want true (retryable aggregate child must be visible)")
	}
	if attrs := GetAttrs(agg); len(attrs) != 1 || attrs[0].Key != "id" || attrs[0].Value != "abc" {
		t.Errorf("GetAttrs(agg) = %+v, want one {id abc} attr from the aggregate child", attrs)
	}

	// A code present on no child must still report false (no over-matching).
	if Is(agg, CodeForbidden) {
		t.Error("Is(agg, CodeForbidden) = true, want false")
	}
}

func TestInspectDescendsIntoNestedAggregate(t *testing.T) {
	spectest.Proves(t, "go/structured-errors", "inspection", "inspection-descends-into-nested-aggregate")
	// *Error -> (cause) AggregateError -> child *Error: a mixed, nested tree.
	deep := New(CodeConflict, "deep").WithAttr(String("k", "v"))
	inner := NewAggregate("inner", []error{stderrors.New("plain"), deep})
	outer := Wrap(inner, CodeInternal)

	if !Is(outer, CodeConflict) {
		t.Error("Is(outer, CodeConflict) = false, want true through *Error -> aggregate -> child")
	}
	if !IsRetryable(New(CodeInternal, "x").WithRetryable(true)) {
		t.Error("IsRetryable regression on a plain *Error")
	}
	if got := GetAttrs(outer); len(got) != 1 || got[0].Key != "k" {
		t.Errorf("GetAttrs(outer) = %+v, want the nested child's single attr", got)
	}
}

func TestStackFrames(t *testing.T) {
	err := New(CodeInternal, "boom")
	frames := err.Stack().Frames()
	count := 0
	found := false
	for {
		frame, more := frames.Next()
		count++
		if strings.Contains(frame.Function, "TestStackFrames") {
			found = true
		}
		if !more {
			break
		}
	}
	if count == 0 {
		t.Fatal("Stack.Frames() yielded no frames")
	}
	if !found {
		t.Error("Stack.Frames() should include the calling test function")
	}
}
