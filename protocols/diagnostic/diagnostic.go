// Package diagnostic provides shared types for structured parse and
// validation diagnostics across protocol packages.
package diagnostic

import (
	"fmt"
	"strings"
)

// Severity classifies the importance of a diagnostic.
type Severity string

// Severity values classify diagnostics by importance.
const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Info    Severity = "info"
)

// SeverityRank returns the canonical display and comparison order: blocking
// errors first, then warnings, then informational and unknown severities.
func SeverityRank(severity Severity) int {
	switch severity {
	case Error:
		return 0
	case Warning:
		return 1
	case Info:
		return 2
	default:
		return 3
	}
}

// Diagnostic is a single structured finding from parsing or validation.
type Diagnostic struct {
	// Severity decides whether the finding blocks. Only Error does; Warning and
	// Info are reported and the caller proceeds.
	Severity Severity `json:"severity"`
	// Code is the stable, machine-readable identifier of the finding. Each
	// protocol package owns its own dotted namespace (for example
	// "doctor.unknown_field", "job.invalid_key") and pins the closed set it may
	// emit; a consumer branches on this, never on Message.
	Code string `json:"code"`
	// Message is the human-readable explanation. It is written for a person
	// reading a terminal and is not part of any package's frozen vocabulary, so
	// automation must not match on its text.
	Message string `json:"message"`
	// Field is the dotted path of the offending member inside the document being
	// parsed or validated ("summary.findings", "selectedProjects[1].path"). It
	// is empty when the finding is about the document as a whole — a parse
	// failure, say — which is exactly how String() decides whether to render it.
	Field string `json:"field,omitempty"`
}

// String returns a human-readable representation.
func (d Diagnostic) String() string {
	if d.Field != "" {
		return fmt.Sprintf("[%s] %s: %s (%s)", d.Severity, d.Field, d.Message, d.Code)
	}
	return fmt.Sprintf("[%s] %s (%s)", d.Severity, d.Message, d.Code)
}

// ErrorText renders the error-severity diagnostics as an indented bullet list,
// one String() per line, so each failure keeps its code and field path. It
// returns "" when no diagnostic has error severity.
func ErrorText(diags []Diagnostic) string {
	var b strings.Builder
	for i, d := range Errors(diags) {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("  - ")
		b.WriteString(d.String())
	}
	return b.String()
}

// Errorf creates an error-severity diagnostic.
func Errorf(code, field, format string, args ...any) Diagnostic {
	return Diagnostic{
		Severity: Error,
		Code:     code,
		Field:    field,
		Message:  fmt.Sprintf(format, args...),
	}
}

// Warningf creates a warning-severity diagnostic.
func Warningf(code, field, format string, args ...any) Diagnostic {
	return Diagnostic{
		Severity: Warning,
		Code:     code,
		Field:    field,
		Message:  fmt.Sprintf(format, args...),
	}
}

// HasErrors returns true if any diagnostic has error severity.
func HasErrors(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

// Errors filters diagnostics to only those with error severity.
func Errors(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Severity == Error {
			out = append(out, d)
		}
	}
	return out
}
