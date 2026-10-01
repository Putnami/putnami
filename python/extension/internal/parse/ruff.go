// Package parse provides output parsers for Python tools (ruff, pytest).
package parse

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var (
	ruffIssueLine     = regexp.MustCompile(`^[^:\n]+:\d+:\d+:\s`)
	ruffFinding       = regexp.MustCompile(`^([^:\n]+):(\d+):(\d+):\s+([A-Z][A-Z0-9]*)\s+(.*)$`)
	ruffFoundError    = regexp.MustCompile(`Found\s+(\d+)\s+error`)
	ruffFileCount     = regexp.MustCompile(`(\d+)\s+file`)
	ruffWouldReformat = regexp.MustCompile(`^Would reformat:\s+(.+)$`)
)

// RuffFinding is one lint issue parsed from ruff check output.
type RuffFinding struct {
	File     string
	Line     int
	Column   int
	Code     string
	Message  string
	Severity string
}

// RuffCheckFindings parses ruff check output into per-issue findings so
// they can be emitted as individual diagnostic events (the runtime
// protocol's lint contract) instead of one aggregate text blob.
func RuffCheckFindings(output string) []RuffFinding {
	lines := strings.Split(output, "\n")
	findings := make([]RuffFinding, 0, len(lines))
	for _, line := range lines {
		m := ruffFinding.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		lineNo, _ := strconv.Atoi(m[2])
		colNo, _ := strconv.Atoi(m[3])
		findings = append(findings, RuffFinding{
			File:    m[1],
			Line:    lineNo,
			Column:  colNo,
			Code:    m[4],
			Message: m[5],
		})
	}
	return findings
}

// RuffFormatIssues parses ruff format output for the number of files needing formatting.
func RuffFormatIssues(output string) int {
	for _, line := range strings.Split(output, "\n") {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "Would reformat:") || ruffFileCount.MatchString(stripped) {
			if m := ruffFileCount.FindStringSubmatch(stripped); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// ruffJSONFinding mirrors one element of `ruff check --output-format=json`.
type ruffJSONFinding struct {
	Code     *string `json:"code"`
	Filename string  `json:"filename"`
	Message  string  `json:"message"`
	Severity string  `json:"severity"`
	Location struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	} `json:"location"`
}

// RuffCheckJSONFindings parses `ruff check --output-format=json` stdout into
// per-issue findings. Each finding carries the (usually absolute) file path
// ruff resolved, so a batch invocation can attribute every diagnostic back to
// its owning project. The second return is false when stdout is not a JSON
// array (e.g. a ruff crash), letting callers fall back to a fail-closed result.
func RuffCheckJSONFindings(stdout string) ([]RuffFinding, bool) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || trimmed[0] != '[' {
		return nil, false
	}
	var raw []ruffJSONFinding
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, false
	}
	findings := make([]RuffFinding, 0, len(raw))
	for _, r := range raw {
		code := ""
		if r.Code != nil {
			code = *r.Code
		}
		severity := r.Severity
		if severity == "" {
			severity = "error"
		}
		findings = append(findings, RuffFinding{
			File:     r.Filename,
			Line:     r.Location.Row,
			Column:   r.Location.Column,
			Code:     code,
			Message:  r.Message,
			Severity: severity,
		})
	}
	return findings, true
}

// RuffFormatFiles extracts the file paths reported by `ruff format --check` as
// needing reformatting (the "Would reformat: <path>" lines). Paths are emitted
// relative to ruff's working directory, so a batch invocation run from the
// workspace root yields workspace-relative paths ready for per-project split.
func RuffFormatFiles(output string) []string {
	var files []string
	for _, line := range strings.Split(output, "\n") {
		m := ruffWouldReformat.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if path := strings.TrimSpace(m[1]); path != "" {
			files = append(files, path)
		}
	}
	return files
}

// RuffCheckIssues parses ruff check output for the number of lint errors.
func RuffCheckIssues(output string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if ruffIssueLine.MatchString(strings.TrimSpace(line)) {
			count++
		}
	}
	if count > 0 {
		return count
	}

	if m := ruffFoundError.FindStringSubmatch(output); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}
