package cmderr

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyPreservesMessage(t *testing.T) {
	base := fmt.Errorf("project not found: %s", "api")
	got := Classify(base, ErrNotFound)
	if got.Error() != base.Error() {
		t.Errorf("Classify changed message: got %q, want %q", got.Error(), base.Error())
	}
}

func TestClassifyMatchesSentinel(t *testing.T) {
	err := Classify(errors.New("boom"), ErrNotFound)
	if !errors.Is(err, ErrNotFound) {
		t.Error("classified error should match its sentinel via errors.Is")
	}
	if errors.Is(err, ErrInvalidConfig) {
		t.Error("classified error should not match an unrelated sentinel")
	}
}

func TestClassifyMatchesMultipleSentinels(t *testing.T) {
	err := Classify(errors.New("boom"), ErrNotFound, ErrNoMatch)
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrNoMatch) {
		t.Error("classified error should match every sentinel it was tagged with")
	}
}

func TestClassifyPreservesUnderlyingCause(t *testing.T) {
	cause := errors.New("disk gone")
	err := Classify(fmt.Errorf("read config: %w", cause), ErrInvalidConfig)
	if !errors.Is(err, cause) {
		t.Error("classification must not break the original cause chain")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Error("classified error should also match the sentinel")
	}
}

func TestClassifyNilIsNil(t *testing.T) {
	if Classify(nil, ErrNotFound) != nil {
		t.Error("Classify(nil, ...) should be nil so it is safe to wrap results inline")
	}
}

func TestConstructorsClassifyAndFormat(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    error
		message string
	}{
		{"Usagef", Usagef("project name required: %s", "describe"), ErrUsage, "project name required: describe"},
		{"NotFoundf", NotFoundf("project not found: %s", "api"), ErrNotFound, "project not found: api"},
		{"InvalidConfigf", InvalidConfigf("invalid semver version: %s", "1.x"), ErrInvalidConfig, "invalid semver version: 1.x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.want) {
				t.Errorf("%s should match its sentinel via errors.Is", tc.name)
			}
			if tc.err.Error() != tc.message {
				t.Errorf("%s message: got %q, want %q", tc.name, tc.err.Error(), tc.message)
			}
		})
	}
}

func TestConstructorsPreserveWrappedCause(t *testing.T) {
	cause := errors.New("bad digest")
	err := InvalidConfigf("invalid integrity %q: %w", "sha256-x", cause)
	if !errors.Is(err, cause) {
		t.Error("a %w-wrapped cause must remain reachable through the constructor")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Error("constructor error should also match its sentinel")
	}
}
