package output

import (
	"fmt"
	"io"
	"os"
	"strings"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// RenderDiagnosticEvent renders a single diagnostic event with source context.
// It shows the file path, line number, severity, message, and optionally
// a code snippet from the source file with the relevant line highlighted.
func RenderDiagnosticEvent(w io.Writer, event jobs.RawJobEvent, prefix string, useColor bool) {
	severity, _ := event.Data["severity"].(string)
	msg, _ := event.Data["message"].(string)
	code, _ := event.Data["code"].(string)
	loc := extractLocation(event.Data)

	// Severity color
	var sevColor string
	switch severity {
	case "error":
		sevColor = Red
	case "warning", "warn":
		sevColor = Yellow
		severity = "warning"
	case "info":
		sevColor = Blue
	default:
		sevColor = ""
	}

	// Header: file:line:col severity[code]: message
	header := buildDiagnosticHeader(loc, severity, code, msg, sevColor, useColor)
	iox.Fprintf(w, "%s%s\n", prefix, header)

	// Source snippet (if file and line are available)
	if loc.file != "" && loc.line > 0 {
		snippet := readSourceSnippet(loc.file, loc.line, loc.column)
		if snippet != "" {
			for _, line := range strings.Split(snippet, "\n") {
				iox.Fprintf(w, "%s%s\n", prefix, line)
			}
		}
	}
}

// diagnosticLocation holds parsed location data from a diagnostic event.
type diagnosticLocation struct {
	file   string
	line   int
	column int
}

func extractLocation(data map[string]any) diagnosticLocation {
	loc := diagnosticLocation{}

	// Direct fields
	if f, ok := data["file"].(string); ok {
		loc.file = f
	}
	if l, ok := data["line"].(float64); ok {
		loc.line = int(l)
	}
	if c, ok := data["column"].(float64); ok {
		loc.column = int(c)
	}

	// Nested "location" object
	if locMap, ok := data["location"].(map[string]any); ok {
		if f, ok := locMap["file"].(string); ok {
			loc.file = f
		}
		if l, ok := locMap["line"].(float64); ok {
			loc.line = int(l)
		}
		if c, ok := locMap["column"].(float64); ok {
			loc.column = int(c)
		}
	}

	return loc
}

func buildDiagnosticHeader(loc diagnosticLocation, severity, code, msg, sevColor string, useColor bool) string {
	var parts []string

	// Location
	if loc.file != "" {
		locStr := loc.file
		if loc.line > 0 {
			locStr = fmt.Sprintf("%s:%d", loc.file, loc.line)
			if loc.column > 0 {
				locStr = fmt.Sprintf("%s:%d:%d", loc.file, loc.line, loc.column)
			}
		}
		if useColor {
			parts = append(parts, colorize(locStr, Bold))
		} else {
			parts = append(parts, locStr)
		}
	}

	// Severity + code
	sevStr := severity
	if code != "" {
		sevStr = fmt.Sprintf("%s[%s]", severity, code)
	}
	if useColor && sevColor != "" {
		parts = append(parts, colorize(sevStr, Bold+sevColor))
	} else {
		parts = append(parts, sevStr)
	}

	// Message
	parts = append(parts, msg)

	return strings.Join(parts, " ")
}

// readSourceSnippet reads source lines around the given line number.
// Returns an empty string if the file can't be read.
func readSourceSnippet(file string, line, column int) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	if line <= 0 || line > len(lines) {
		return ""
	}

	// Show context: 1 line before, the line itself, 1 line after
	startLine := line - 2 // 0-indexed, 1 line before
	endLine := line + 1   // 0-indexed, 1 line after (exclusive)
	if startLine < 0 {
		startLine = 0
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}

	var buf strings.Builder
	lineNumWidth := len(fmt.Sprintf("%d", endLine))
	for i := startLine; i < endLine; i++ {
		lineNum := i + 1 // 1-indexed
		marker := " "
		if lineNum == line {
			marker = ">"
		}

		sourceLine := lines[i]
		// Truncate long lines
		if len(sourceLine) > 120 {
			sourceLine = sourceLine[:117] + "..."
		}

		iox.Fprintf(&buf, "  %s %*d │ %s", marker, lineNumWidth, lineNum, sourceLine)
		if i < endLine-1 {
			buf.WriteByte('\n')
		}

		// Column indicator on the target line
		if lineNum == line && column > 0 {
			padding := strings.Repeat(" ", lineNumWidth+6+column-1)
			buf.WriteByte('\n')
			iox.Fprintf(&buf, "%s^", padding)
		}
	}

	return buf.String()
}
