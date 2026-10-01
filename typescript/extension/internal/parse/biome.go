// Package parse provides output parsers for external tools (Biome, JUnit, LCOV, tsc).
package parse

import (
	"encoding/json"
)

// BiomeDiagnostic represents a single lint/format diagnostic.
type BiomeDiagnostic struct {
	Category    string   `json:"category"`
	Severity    string   `json:"severity"` // error, warning, info
	Description string   `json:"description"`
	File        string   `json:"file,omitempty"`
	Line        int      `json:"line,omitempty"`
	Column      int      `json:"column,omitempty"`
	EndLine     int      `json:"endLine,omitempty"`
	EndColumn   int      `json:"endColumn,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// BiomeSummary holds aggregate counts from a Biome report.
type BiomeSummary struct {
	Errors    int `json:"errors"`
	Warnings  int `json:"warnings"`
	Infos     int `json:"infos"`
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
	Skipped   int `json:"skipped"`
}

// BiomeReport is the parsed output of biome --reporter=json.
type BiomeReport struct {
	Summary     BiomeSummary      `json:"summary"`
	Diagnostics []BiomeDiagnostic `json:"diagnostics"`
	Command     string            `json:"command,omitempty"`
}

// EmptyBiomeReport returns a report with zero counts.
func EmptyBiomeReport() BiomeReport {
	return BiomeReport{Diagnostics: []BiomeDiagnostic{}}
}

// rawBiomeOutput matches Biome's --reporter=json shape.
type rawBiomeOutput struct {
	Summary struct {
		Errors    int `json:"errors"`
		Warnings  int `json:"warnings"`
		Infos     int `json:"infos"`
		Changed   int `json:"changed"`
		Unchanged int `json:"unchanged"`
		Skipped   int `json:"skipped"`
	} `json:"summary"`
	Diagnostics []rawDiagnostic `json:"diagnostics"`
	Command     string          `json:"command"`
}

type rawDiagnostic struct {
	Category string `json:"category"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Location *struct {
		Path  string `json:"path"`
		Start *struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		} `json:"start"`
		End *struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		} `json:"end"`
	} `json:"location"`
	Tags []string `json:"tags"`
}

// ParseBiomeOutput parses Biome's JSON reporter output into a structured report.
func ParseBiomeOutput(jsonStr string) BiomeReport {
	if len(jsonStr) == 0 {
		return EmptyBiomeReport()
	}

	var raw rawBiomeOutput
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return EmptyBiomeReport()
	}

	if raw.Diagnostics == nil {
		return EmptyBiomeReport()
	}

	diagnostics := make([]BiomeDiagnostic, 0, len(raw.Diagnostics))
	for _, d := range raw.Diagnostics {
		diag := BiomeDiagnostic{
			Category:    d.Category,
			Severity:    mapSeverity(d.Severity),
			Description: d.Message,
			Tags:        d.Tags,
		}
		if d.Location != nil {
			diag.File = d.Location.Path
			if d.Location.Start != nil {
				diag.Line = d.Location.Start.Line
				diag.Column = d.Location.Start.Column
			}
			if d.Location.End != nil {
				diag.EndLine = d.Location.End.Line
				diag.EndColumn = d.Location.End.Column
			}
		}
		diagnostics = append(diagnostics, diag)
	}

	return BiomeReport{
		Summary: BiomeSummary{
			Errors:    raw.Summary.Errors,
			Warnings:  raw.Summary.Warnings,
			Infos:     raw.Summary.Infos,
			Changed:   raw.Summary.Changed,
			Unchanged: raw.Summary.Unchanged,
			Skipped:   raw.Summary.Skipped,
		},
		Diagnostics: diagnostics,
		Command:     raw.Command,
	}
}

func mapSeverity(s string) string {
	switch s {
	case "error":
		return "error"
	case "warning":
		return "warning"
	default:
		return "info"
	}
}
