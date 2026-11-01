package parse

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
)

// CoverageSummary holds aggregate coverage metrics.
//
// LineCoverage / TotalLines / CoveredLines are derived solely from LCOV's
// instrumented lines: the DA lines of each record, or its LF/LH when it lists
// none. Source files that tests never imported are
// surfaced via UncoveredFileCount and as not-analyzed per-file entries, but are
// intentionally kept out of the line-percentage denominator: counting their
// raw source lines (imports, comments, type-only declarations) as a proxy for
// executable lines would skew the percentage relative to the per-file LCOV
// numbers.
type CoverageSummary struct {
	LineCoverage     float64 `json:"lineCoverage"`
	FunctionCoverage float64 `json:"functionCoverage"`
	TotalLines       int     `json:"totalLines"`
	CoveredLines     int     `json:"coveredLines"`
	UncoveredLines   int     `json:"uncoveredLines"`
	TotalFunctions   int     `json:"totalFunctions"`
	CoveredFunctions int     `json:"coveredFunctions"`
	FileCount        int     `json:"fileCount"`
	// UncoveredFileCount is the number of project source files that never
	// appeared in the LCOV report (0% coverage, not analyzed). These are
	// reported for visibility but excluded from the line-coverage denominator.
	UncoveredFileCount int `json:"uncoveredFileCount"`
}

// FileCoverage holds per-file line coverage data.
type FileCoverage struct {
	Path         string  `json:"path"`
	TotalLines   int     `json:"totalLines"`
	CoveredLines int     `json:"coveredLines"`
	Coverage     float64 `json:"coverage"`
	// Analyzed reports whether the file appeared in the LCOV report. When
	// false, the file was never imported by a test: its coverage is treated as
	// 0% "not analyzed" and its lines do not contribute to the aggregate
	// line-coverage denominator.
	Analyzed bool `json:"analyzed"`
}

// ParseLCOVFileForProject parses an LCOV file and augments the per-file
// breakdown with project source files that tests never imported. Such files
// are reported as not-analyzed (0% coverage) entries for visibility, but their
// raw source lines are deliberately NOT folded into the aggregate line-coverage
// denominator: a non-empty-line count is a poor proxy for LCOV's instrumented
// lines and would make the reported project percentage inconsistent with
// the per-file LCOV numbers (see CoverageSummary doc comment).
//
// Returns the aggregate summary (line counts from LCOV only) and the combined
// per-file breakdown.
func ParseLCOVFileForProject(lcovPath, projectPath string) (CoverageSummary, []FileCoverage, error) {
	summary, coveredFiles, files, err := parseLCOV(lcovPath)
	if err != nil {
		return summary, nil, err
	}
	if projectPath == "" {
		return summary, files, nil
	}

	srcDir := filepath.Join(projectPath, "src")
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		return summary, files, nil
	}

	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".gen" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}

		if !isSourceFile(d.Name()) {
			return nil
		}

		rel, _ := filepath.Rel(projectPath, path)
		rel = filepath.ToSlash(rel)
		if coveredFiles[rel] {
			return nil
		}

		// File was never imported by a test. Record it as not-analyzed so it
		// shows up in the breakdown as 0% coverage, but leave the aggregate
		// line counts (TotalLines/CoveredLines/LineCoverage) untouched.
		summary.FileCount++
		summary.UncoveredFileCount++
		files = append(files, FileCoverage{
			Path:         rel,
			TotalLines:   0,
			CoveredLines: 0,
			Coverage:     0,
			Analyzed:     false,
		})
		return nil
	})
	if err != nil {
		return summary, files, err
	}

	// Percentages are unchanged: they remain anchored to LCOV's instrumented
	// lines/functions so the aggregate stays consistent with the per-file
	// LCOV numbers. Not-analyzed files are surfaced via UncoveredFileCount.

	return summary, files, nil
}

// parseLCOV parses an LCOV file and returns the summary, the set of
// covered file paths, and per-file coverage data. A missing file is an empty
// summary; files outside the project (paths starting with "..") are skipped.
func parseLCOV(path string) (CoverageSummary, map[string]bool, []FileCoverage, error) {
	report, diagnostics := readers.ReadFile(path, readers.ReadLCOV)
	if len(diagnostics) == 1 && diagnostics[0].Code == readers.CodeMissing {
		return CoverageSummary{}, nil, nil, nil
	}
	if diag.HasErrors(diagnostics) {
		return CoverageSummary{}, nil, nil, errors.New(diag.ErrorText(diagnostics))
	}

	coveredFiles := make(map[string]bool)
	var files []FileCoverage
	var totalLF, totalLH, totalFNF, totalFNH int
	for _, file := range report.Files {
		if strings.HasPrefix(file.Path, "..") {
			continue
		}
		totalLF += file.LineCounts.Total
		totalLH += file.LineCounts.Covered
		totalFNF += file.Functions.Total
		totalFNH += file.Functions.Covered
		coveredFiles[file.Path] = true
		files = append(files, FileCoverage{
			Path:         file.Path,
			TotalLines:   file.LineCounts.Total,
			CoveredLines: file.LineCounts.Covered,
			Coverage:     percent(file.LineCounts.Covered, file.LineCounts.Total),
			Analyzed:     true,
		})
	}

	return CoverageSummary{
		LineCoverage:     percent(totalLH, totalLF),
		FunctionCoverage: percent(totalFNH, totalFNF),
		TotalLines:       totalLF,
		CoveredLines:     totalLH,
		UncoveredLines:   totalLF - totalLH,
		TotalFunctions:   totalFNF,
		CoveredFunctions: totalFNH,
		FileCount:        len(files),
	}, coveredFiles, files, nil
}

// percent is covered over total in percent, rounded to two decimals; 100 when
// nothing is instrumented.
func percent(covered, total int) float64 {
	if total == 0 {
		return 100
	}
	return math.Round(float64(covered)/float64(total)*10000) / 100
}

var sourceFileRe = regexp.MustCompile(`\.(ts|tsx)$`)
var testFileRe = regexp.MustCompile(`\.(test|spec)\.(ts|tsx)$`)

// isSourceFile returns true for .ts/.tsx files that are not test files.
func isSourceFile(name string) bool {
	return sourceFileRe.MatchString(name) && !testFileRe.MatchString(name)
}
