package parse

import (
	"regexp"
	"strconv"
	"strings"
)

// TscDiagnostic represents a single TypeScript compiler diagnostic.
type TscDiagnostic struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
	Category string `json:"category"` // error, warning, info
}

var tscLineRe = regexp.MustCompile(`^(.+)\((\d+),(\d+)\): (error|warning|info) (TS\d+): (.+)$`)
var tscNoLocRe = regexp.MustCompile(`^(error|warning|info) (TS\d+): (.+)$`)

// ParseTscOutput parses tsc command-line output into diagnostics.
// Supports two formats:
//   - file(line,col): category TScode: message
//   - category TScode: message (locationless errors)
func ParseTscOutput(output string) []TscDiagnostic {
	var diagnostics []TscDiagnostic

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if match := tscLineRe.FindStringSubmatch(line); match != nil {
			lineNum, _ := strconv.Atoi(match[2])
			colNum, _ := strconv.Atoi(match[3])

			diagnostics = append(diagnostics, TscDiagnostic{
				File:     match[1],
				Line:     lineNum,
				Column:   colNum,
				Category: match[4],
				Code:     match[5],
				Message:  strings.TrimSpace(match[6]),
			})
		} else if match := tscNoLocRe.FindStringSubmatch(line); match != nil {
			diagnostics = append(diagnostics, TscDiagnostic{
				Category: match[1],
				Code:     match[2],
				Message:  strings.TrimSpace(match[3]),
			})
		}
	}

	return diagnostics
}
