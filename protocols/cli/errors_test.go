package cli

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCodeForError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitSuccess},
		{"unclassified", errors.New("boom"), ExitFailure},
		{"usage", ErrUsage, ExitUsage},
		{"no workspace", ErrNoWorkspace, ExitUsage},
		{"not found", ErrNotFound, ExitUsage},
		{"no match", ErrNoMatch, ExitUsage},
		{"invalid config", ErrInvalidConfig, ExitUsage},
		{"auth", ErrAuth, ExitAuth},
		{"api", ErrAPI, ExitAPI},
		{"wrapped usage", fmt.Errorf("bad flag: %w", ErrUsage), ExitUsage},
		{"classified not found", Classify(errors.New("no project foo"), ErrNotFound), ExitUsage},
		{"classified auth", Authf("token expired"), ExitAuth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCodeForError(c.err); got != c.want {
				t.Errorf("ExitCodeForError(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestErrorCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errors.New("boom"), "failure"},
		{ErrUsage, "usage"},
		{ErrAuth, "auth"},
		{ErrAPI, "api"},
		{ErrInvalidConfig, "usage"},
	}
	for _, c := range cases {
		if got := ErrorCode(c.err); got != c.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestClassifyPreservesMessage checks that classification is invisible to the
// message while still matching errors.Is against every class.
func TestClassifyPreservesMessage(t *testing.T) {
	err := Classify(errors.New("no template named quickstart"), ErrNotFound, ErrUsage)
	if err.Error() != "no template named quickstart" {
		t.Errorf("Error() = %q, want the original message", err.Error())
	}
	if !errors.Is(err, ErrNotFound) {
		t.Error("classified error should match ErrNotFound")
	}
	if !errors.Is(err, ErrUsage) {
		t.Error("classified error should match ErrUsage")
	}
	if errors.Is(err, ErrAuth) {
		t.Error("classified error should not match an unrelated class")
	}
}

func TestClassifyNil(t *testing.T) {
	if got := Classify(nil, ErrUsage); got != nil {
		t.Errorf("Classify(nil) = %v, want nil", got)
	}
}

// TestWithNextNilSafe verifies WithNext returns nil for a nil error and
// SuggestedNext returns "" for a nil error.
func TestWithNextNilSafe(t *testing.T) {
	if got := WithNext(nil, "putnami workspace init"); got != nil {
		t.Errorf("WithNext(nil) = %v, want nil", got)
	}
	if got := SuggestedNext(nil); got != "" {
		t.Errorf("SuggestedNext(nil) = %q, want \"\"", got)
	}
}

// TestSuggestedNextNoneWhenAbsent verifies SuggestedNext returns "" for an
// error that carries no suggestion, including a classified one.
func TestSuggestedNextNoneWhenAbsent(t *testing.T) {
	if got := SuggestedNext(errors.New("boom")); got != "" {
		t.Errorf("SuggestedNext(plain) = %q, want \"\"", got)
	}
	if got := SuggestedNext(Usagef("bad flag")); got != "" {
		t.Errorf("SuggestedNext(classified) = %q, want \"\"", got)
	}
}

// TestWithNextPreservesMessage verifies the suggestion is invisible to the
// presented message.
func TestWithNextPreservesMessage(t *testing.T) {
	err := WithNext(errors.New("no workspace found"), "putnami workspace init")
	if err.Error() != "no workspace found" {
		t.Errorf("Error() = %q, want the original message", err.Error())
	}
	if got := SuggestedNext(err); got != "putnami workspace init" {
		t.Errorf("SuggestedNext = %q, want %q", got, "putnami workspace init")
	}
}

// TestWithNextComposesWithClassify verifies WithNext and Classify compose in
// either nesting order: the suggestion is readable and errors.Is against the
// underlying class still matches (the carrier is transparent to Unwrap).
func TestWithNextComposesWithClassify(t *testing.T) {
	// WithNext(Classify(...)) — suggestion outermost, class beneath.
	outer := WithNext(Usagef("no workspace"), "putnami workspace init")
	if !errors.Is(outer, ErrUsage) {
		t.Error("WithNext(Usagef) should still match ErrUsage")
	}
	if got := SuggestedNext(outer); got != "putnami workspace init" {
		t.Errorf("SuggestedNext(outer) = %q, want %q", got, "putnami workspace init")
	}
	if outer.Error() != "no workspace" {
		t.Errorf("outer.Error() = %q, want %q", outer.Error(), "no workspace")
	}

	// Classify(WithNext(...), ...) — class outermost, suggestion beneath a
	// multi-error Unwrap; SuggestedNext must walk the whole tree to find it.
	inner := Classify(WithNext(errors.New("token expired"), "putnami login"), ErrAuth)
	if !errors.Is(inner, ErrAuth) {
		t.Error("Classify(WithNext(...)) should match ErrAuth")
	}
	if got := SuggestedNext(inner); got != "putnami login" {
		t.Errorf("SuggestedNext(inner) = %q, want %q", got, "putnami login")
	}
	if inner.Error() != "token expired" {
		t.Errorf("inner.Error() = %q, want %q", inner.Error(), "token expired")
	}
}

// TestWithNextInnermostWins verifies SuggestedNext returns the first suggestion
// encountered when the chain carries more than one.
func TestWithNextInnermostWins(t *testing.T) {
	err := WithNext(WithNext(errors.New("boom"), "putnami inner"), "putnami outer")
	// errors.As finds the outermost matching value first.
	if got := SuggestedNext(err); got != "putnami outer" {
		t.Errorf("SuggestedNext = %q, want the first (outermost) suggestion", got)
	}
}

// TestWithNextEmptyDoesNotShadow verifies an empty suggestion is a no-op: it
// never wraps, so it neither shadows an inner suggestion nor registers a
// carrier of its own.
func TestWithNextEmptyDoesNotShadow(t *testing.T) {
	// An empty outer suggestion must not hide the inner one.
	err := WithNext(WithNext(errors.New("boom"), "putnami login"), "")
	if got := SuggestedNext(err); got != "putnami login" {
		t.Errorf("SuggestedNext = %q, want inner %q (empty outer must not shadow)", got, "putnami login")
	}

	// WithNext(err, "") returns the original error unwrapped (identity).
	plain := errors.New("boom")
	if got := WithNext(plain, ""); !errors.Is(got, plain) {
		t.Errorf("WithNext(err, \"\") = %v, want the original error unwrapped", got)
	}
	if got := SuggestedNext(WithNext(errors.New("x"), "")); got != "" {
		t.Errorf("SuggestedNext(WithNext(err, \"\")) = %q, want \"\"", got)
	}
}

// A signal-classified error must carry 130, not the 1 of a genuine failure: an
// interrupted run's envelope has to agree with the code the process returns.
func TestExitCodeForError_Signal(t *testing.T) {
	err := Classify(errors.New("session interrupted by user"), ErrSignal)
	if got := ExitCodeForError(err); got != ExitSignal {
		t.Errorf("ExitCodeForError = %d, want %d (ExitSignal)", got, ExitSignal)
	}
	if got := ErrorCode(err); got != "signal" {
		t.Errorf("ErrorCode = %q, want signal", got)
	}
}

// The envelope built from that error must expose the same code, since consumers
// read exitCode from the JSON rather than waiting on the process.
//
// It asserts on the v2 envelope because that is the only one this package can
// build: the v1 constructor is deleted. The verdict differs by design —
// v1 folded a signal into "failure", v2 reports "aborted" (doc/02-result-v2.md,
// decision 2) — while the exit code, the thing a consumer acts on, is the same
// 130 in both.
func TestNewResultV2_SignalCarriesExitCode130(t *testing.T) {
	r := NewResultV2("build", nil, Classify(errors.New("session interrupted by user"), ErrSignal))
	if r.ExitCode != ExitSignal {
		t.Errorf("ResultV2.ExitCode = %d, want %d", r.ExitCode, ExitSignal)
	}
	if r.Status != StatusAborted {
		t.Errorf("ResultV2.Status = %q, want %q", r.Status, StatusAborted)
	}
	if r.Error == nil || r.Error.Code != "signal" {
		t.Errorf("ResultV2.Error = %+v, want code signal", r.Error)
	}
}
