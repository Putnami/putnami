package parse

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// ESLintCategory names a message ESLint reports without a rule, such as a
// parsing error.
const ESLintCategory = "eslint"

// rawESLintFile is one entry of ESLint's `--format json` output.
type rawESLintFile struct {
	FilePath string             `json:"filePath"`
	Messages []rawESLintMessage `json:"messages"`
}

type rawESLintMessage struct {
	RuleID    *string `json:"ruleId"`
	Severity  int     `json:"severity"`
	Message   string  `json:"message"`
	Line      int     `json:"line"`
	Column    int     `json:"column"`
	EndLine   int     `json:"endLine"`
	EndColumn int     `json:"endColumn"`
	Fatal     bool    `json:"fatal"`
}

// ParseESLintOutput reads ESLint's `--format json` output into the report the
// lint job emits. Each message becomes one diagnostic: severity 2 is an
// error, severity 1 a warning; its category is the rule, or ESLintCategory for
// a message no rule reports. A file path is made relative to base, in slash
// form, when it lies under base. Output that is not ESLint's JSON array is an
// error.
func ParseESLintOutput(output, base string) (BiomeReport, error) {
	report := EmptyBiomeReport()
	text := strings.TrimSpace(output)
	if text == "" {
		return report, fmt.Errorf("eslint printed no JSON report")
	}
	var files []rawESLintFile
	if err := json.Unmarshal([]byte(text), &files); err != nil {
		return report, fmt.Errorf("eslint printed no JSON report: %w", err)
	}
	for _, file := range files {
		path := relativeTo(base, file.FilePath)
		for _, m := range file.Messages {
			category := ESLintCategory
			if m.RuleID != nil && *m.RuleID != "" {
				category = *m.RuleID
			}
			severity := "warning"
			if m.Severity >= 2 || m.Fatal {
				severity = "error"
			}
			switch severity {
			case "error":
				report.Summary.Errors++
			default:
				report.Summary.Warnings++
			}
			report.Diagnostics = append(report.Diagnostics, BiomeDiagnostic{
				Category:    category,
				Severity:    severity,
				Description: m.Message,
				File:        path,
				Line:        m.Line,
				Column:      m.Column,
				EndLine:     m.EndLine,
				EndColumn:   m.EndColumn,
			})
		}
	}
	return report, nil
}

// relativeTo returns path relative to base in slash form when path lies under
// base, and path in slash form otherwise.
func relativeTo(base, path string) string {
	if base == "" || path == "" {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
