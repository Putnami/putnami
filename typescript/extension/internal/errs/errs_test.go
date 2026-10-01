package errs

import (
	stderrors "errors"
	"fmt"
	"testing"
)

func TestNew_CodeAndMessage(t *testing.T) {
	e := New(CodeGenerateFailed, "boom")
	if e.Code() != CodeGenerateFailed {
		t.Fatalf("Code() = %q, want %q", e.Code(), CodeGenerateFailed)
	}
	if got := e.Error(); got != "boom" {
		t.Fatalf("Error() = %q, want %q", got, "boom")
	}
	if e.Unwrap() != nil {
		t.Fatalf("Unwrap() = %v, want nil", e.Unwrap())
	}
}

func TestNewf_FormatsMessage(t *testing.T) {
	e := Newf(CodeTranspileError, "bad %s at line %d", "syntax", 7)
	if want := "bad syntax at line 7"; e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
	if e.Code() != CodeTranspileError {
		t.Fatalf("Code() = %q, want %q", e.Code(), CodeTranspileError)
	}
}

func TestWrap_PreservesCauseAndCode(t *testing.T) {
	cause := stderrors.New("disk full")
	e := Wrap(cause, CodeCompileError)

	if e.Code() != CodeCompileError {
		t.Fatalf("Code() = %q, want %q", e.Code(), CodeCompileError)
	}
	if !stderrors.Is(e.Unwrap(), cause) {
		t.Fatalf("Unwrap() = %v, want %v", e.Unwrap(), cause)
	}
	// With no message, Error() renders as the cause's text.
	if got := e.Error(); got != "disk full" {
		t.Fatalf("Error() = %q, want %q", got, "disk full")
	}
	// errors.Is must find the wrapped sentinel through the structured error.
	if !stderrors.Is(e, cause) {
		t.Fatalf("errors.Is(e, cause) = false, want true")
	}
}

func TestWrapf_PrependsMessageToCause(t *testing.T) {
	cause := stderrors.New("permission denied")
	e := Wrapf(cause, CodeGenerateFailed, "writing manifest")
	if want := "writing manifest: permission denied"; e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
	if !stderrors.Is(e, cause) {
		t.Fatalf("errors.Is(e, cause) = false, want true")
	}
}

func TestWrap_NilReturnsNil(t *testing.T) {
	if Wrap(nil, CodeCompileError) != nil {
		t.Fatalf("Wrap(nil, ...) = non-nil, want nil")
	}
	if Wrapf(nil, CodeCompileError, "msg") != nil {
		t.Fatalf("Wrapf(nil, ...) = non-nil, want nil")
	}
}

func TestCodeOf_DirectError(t *testing.T) {
	e := New(CodeNoServeExport, "no ./serve export")
	if got := CodeOf(e); got != CodeNoServeExport {
		t.Fatalf("CodeOf() = %q, want %q", got, CodeNoServeExport)
	}
}

func TestCodeOf_WrappedChain(t *testing.T) {
	// A structured error wrapped by fmt.Errorf with %w must still surface its
	// code: CodeOf walks the chain via errors.As.
	inner := New(CodeNoRunEntrypoint, "no run entrypoint found")
	outer := fmt.Errorf("resolving entrypoint: %w", inner)
	if got := CodeOf(outer); got != CodeNoRunEntrypoint {
		t.Fatalf("CodeOf(wrapped) = %q, want %q", got, CodeNoRunEntrypoint)
	}
}

func TestCodeOf_PlainErrorReturnsUnknown(t *testing.T) {
	if got := CodeOf(stderrors.New("plain")); got != CodeUnknown {
		t.Fatalf("CodeOf(plain) = %q, want CodeUnknown (%q)", got, CodeUnknown)
	}
	if CodeUnknown != "" {
		t.Fatalf("CodeUnknown = %q, want empty string", CodeUnknown)
	}
}

func TestCodeOf_NilReturnsUnknown(t *testing.T) {
	if got := CodeOf(nil); got != CodeUnknown {
		t.Fatalf("CodeOf(nil) = %q, want CodeUnknown", got)
	}
}

func TestCategory_DefaultAndWithCategory(t *testing.T) {
	e := New(CodeNoServeExport, "no export")
	if e.Category() != CategoryNone {
		t.Fatalf("default Category() = %q, want CategoryNone", e.Category())
	}

	e2 := New(CodeGenerateFailed, "boom").WithCategory(CategoryInfra)
	if e2.Category() != CategoryInfra {
		t.Fatalf("Category() = %q, want %q", e2.Category(), CategoryInfra)
	}
}

func TestErrorsAs_FindsStructuredError(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", Newf(CodeTestsFailed, "tests failed"))
	var target *Error
	if !stderrors.As(wrapped, &target) {
		t.Fatalf("errors.As did not find *Error in chain")
	}
	if target.Code() != CodeTestsFailed {
		t.Fatalf("target.Code() = %q, want %q", target.Code(), CodeTestsFailed)
	}
}

func TestCodeString(t *testing.T) {
	if CodeTranspileError.String() != "TRANSPILE_ERROR" {
		t.Fatalf("Code.String() = %q, want %q", CodeTranspileError.String(), "TRANSPILE_ERROR")
	}
}
