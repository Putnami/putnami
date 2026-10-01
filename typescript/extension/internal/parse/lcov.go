package parse

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// CoverageSummary holds aggregate coverage metrics.
//
// LineCoverage / TotalLines / CoveredLines are derived solely from LCOV's
// instrumented-line counts (LF/LH). Source files that tests never imported are
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
// lines (LF) and would make the reported project percentage inconsistent with
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
// covered file paths, and per-file coverage data.
func parseLCOV(path string) (CoverageSummary, map[string]bool, []FileCoverage, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CoverageSummary{}, nil, nil, nil
		}
		return CoverageSummary{}, nil, nil, err
	}
	defer f.Close()

	coveredFiles := make(map[string]bool)
	var files []FileCoverage

	var (
		totalLF, totalLH       int
		totalFNF, totalFNH     int
		fileCount              int
		currentLF, currentLH   int
		currentFNF, currentFNH int
		currentFile            string
		inFile                 bool
		skip                   bool
	)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "TN:") {
			continue
		}

		if strings.HasPrefix(line, "SF:") {
			// A report path is in slash form on every host: bun writes the
			// host's separator, a backslash on Windows.
			filePath := filepath.ToSlash(line[3:])
			// Skip external dependencies (paths starting with ..)
			skip = strings.HasPrefix(filePath, "..")
			if !skip {
				inFile = true
				currentFile = filePath
				currentLF = 0
				currentLH = 0
				currentFNF = 0
				currentFNH = 0
			}
			continue
		}

		if skip {
			if line == "end_of_record" {
				skip = false
			}
			continue
		}

		if !inFile {
			continue
		}

		if strings.HasPrefix(line, "FNF:") {
			currentFNF, _ = strconv.Atoi(line[4:])
		} else if strings.HasPrefix(line, "FNH:") {
			currentFNH, _ = strconv.Atoi(line[4:])
		} else if strings.HasPrefix(line, "LF:") {
			currentLF, _ = strconv.Atoi(line[3:])
		} else if strings.HasPrefix(line, "LH:") {
			currentLH, _ = strconv.Atoi(line[3:])
		} else if line == "end_of_record" {
			totalLF += currentLF
			totalLH += currentLH
			totalFNF += currentFNF
			totalFNH += currentFNH
			fileCount++
			coveredFiles[currentFile] = true

			pct := 100.0
			if currentLF > 0 {
				pct = math.Round(float64(currentLH)/float64(currentLF)*10000) / 100
			}
			files = append(files, FileCoverage{
				Path:         currentFile,
				TotalLines:   currentLF,
				CoveredLines: currentLH,
				Coverage:     pct,
				Analyzed:     true,
			})

			inFile = false
		}
	}

	lineCov := 100.0
	if totalLF > 0 {
		lineCov = math.Round(float64(totalLH)/float64(totalLF)*10000) / 100
	}

	funcCov := 100.0
	if totalFNF > 0 {
		funcCov = math.Round(float64(totalFNH)/float64(totalFNF)*10000) / 100
	}

	return CoverageSummary{
		LineCoverage:     lineCov,
		FunctionCoverage: funcCov,
		TotalLines:       totalLF,
		CoveredLines:     totalLH,
		UncoveredLines:   totalLF - totalLH,
		TotalFunctions:   totalFNF,
		CoveredFunctions: totalFNH,
		FileCount:        fileCount,
	}, coveredFiles, files, nil
}

var sourceFileRe = regexp.MustCompile(`\.(ts|tsx)$`)
var testFileRe = regexp.MustCompile(`\.(test|spec)\.(ts|tsx)$`)

// isSourceFile returns true for .ts/.tsx files that are not test files.
func isSourceFile(name string) bool {
	return sourceFileRe.MatchString(name) && !testFileRe.MatchString(name)
}
