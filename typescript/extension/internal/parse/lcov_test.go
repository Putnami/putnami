package parse

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestParseLCOVFile_NotFound(t *testing.T) {
	summary, _, err := ParseLCOVFileForProject("/nonexistent/path/lcov.info", "")
	if err != nil {
		t.Errorf("expected nil error for missing file, got %v", err)
	}
	if summary.FileCount != 0 {
		t.Errorf("expected 0 files, got %d", summary.FileCount)
	}
}

func TestParseLCOVFile_BasicCoverage(t *testing.T) {
	content := `TN:
SF:src/foo.ts
FNF:5
FNH:4
LF:20
LH:15
end_of_record
TN:
SF:src/bar.ts
FNF:3
FNH:3
LF:10
LH:10
end_of_record
`
	path := writeTempFile(t, content)

	summary, _, err := ParseLCOVFileForProject(path, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", summary.FileCount)
	}
	if summary.TotalLines != 30 {
		t.Errorf("expected 30 total lines, got %d", summary.TotalLines)
	}
	if summary.CoveredLines != 25 {
		t.Errorf("expected 25 covered lines, got %d", summary.CoveredLines)
	}
	if summary.UncoveredLines != 5 {
		t.Errorf("expected 5 uncovered lines, got %d", summary.UncoveredLines)
	}
	if summary.TotalFunctions != 8 {
		t.Errorf("expected 8 total functions, got %d", summary.TotalFunctions)
	}
	if summary.CoveredFunctions != 7 {
		t.Errorf("expected 7 covered functions, got %d", summary.CoveredFunctions)
	}

	// Line coverage: 25/30 * 100 = 83.33%
	expectedLineCov := 83.33
	if summary.LineCoverage != expectedLineCov {
		t.Errorf("expected line coverage %.2f, got %.2f", expectedLineCov, summary.LineCoverage)
	}
	// Function coverage: 7/8 * 100 = 87.5%
	expectedFuncCov := 87.5
	if summary.FunctionCoverage != expectedFuncCov {
		t.Errorf("expected function coverage %.2f, got %.2f", expectedFuncCov, summary.FunctionCoverage)
	}
}

func TestParseLCOVFile_SkipsExternalDeps(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "structured-reports", "external-dependencies-are-excluded")
	// External deps have paths starting with ".."
	content := `SF:../node_modules/some-lib/index.js
FNF:10
FNH:5
LF:100
LH:50
end_of_record
SF:src/local.ts
FNF:2
FNH:2
LF:10
LH:10
end_of_record
`
	path := writeTempFile(t, content)

	summary, _, err := ParseLCOVFileForProject(path, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only local file counted
	if summary.FileCount != 1 {
		t.Errorf("expected 1 file (external skipped), got %d", summary.FileCount)
	}
	if summary.TotalLines != 10 {
		t.Errorf("expected 10 total lines, got %d", summary.TotalLines)
	}
}

func TestParseLCOVFile_FullCoverage(t *testing.T) {
	content := `SF:src/perfect.ts
FNF:3
FNH:3
LF:15
LH:15
end_of_record
`
	path := writeTempFile(t, content)

	summary, _, err := ParseLCOVFileForProject(path, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.LineCoverage != 100.0 {
		t.Errorf("expected 100%% line coverage, got %.2f", summary.LineCoverage)
	}
	if summary.FunctionCoverage != 100.0 {
		t.Errorf("expected 100%% function coverage, got %.2f", summary.FunctionCoverage)
	}
}

func TestParseLCOVFile_EmptyFile(t *testing.T) {
	path := writeTempFile(t, "")

	summary, _, err := ParseLCOVFileForProject(path, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No lines found → defaults to 100% (totalLF == 0 branch)
	if summary.LineCoverage != 100.0 {
		t.Errorf("expected 100%% for empty file, got %.2f", summary.LineCoverage)
	}
	if summary.FileCount != 0 {
		t.Errorf("expected 0 files, got %d", summary.FileCount)
	}
}

func TestParseLCOVFile_ZeroCoverage(t *testing.T) {
	content := `SF:src/untested.ts
FNF:5
FNH:0
LF:20
LH:0
end_of_record
`
	path := writeTempFile(t, content)

	summary, _, err := ParseLCOVFileForProject(path, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.LineCoverage != 0.0 {
		t.Errorf("expected 0%% line coverage, got %.2f", summary.LineCoverage)
	}
	if summary.FunctionCoverage != 0.0 {
		t.Errorf("expected 0%% function coverage, got %.2f", summary.FunctionCoverage)
	}
	if summary.UncoveredLines != 20 {
		t.Errorf("expected 20 uncovered lines, got %d", summary.UncoveredLines)
	}
}

func TestParseLCOVFileForProject_IncludesUncoveredFiles(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "structured-reports", "uncovered-project-sources-are-counted")
	// Set up a fake project with src/ containing 3 files,
	// but only 1 file appears in the LCOV.
	projectDir := t.TempDir()
	srcDir := filepath.Join(projectDir, "src")
	os.MkdirAll(srcDir, 0755)

	// covered.ts — will appear in LCOV
	os.WriteFile(filepath.Join(srcDir, "covered.ts"), []byte("line1\nline2\nline3\nline4\nline5\n"), 0644)
	// uncovered.ts — NOT in LCOV (never imported by a test)
	os.WriteFile(filepath.Join(srcDir, "uncovered.ts"), []byte("export const a = 1;\nexport const b = 2;\nexport const c = 3;\n"), 0644)
	// also-uncovered.tsx — NOT in LCOV
	os.WriteFile(filepath.Join(srcDir, "also-uncovered.tsx"), []byte("export function App() {\n  return null;\n}\n"), 0644)
	// test file should be excluded
	os.WriteFile(filepath.Join(srcDir, "covered.test.ts"), []byte("import './covered';\ntest('ok', () => {});\n"), 0644)

	// LCOV only covers src/covered.ts (LF:5 LH:3 → 60% of that file)
	lcovContent := "SF:src/covered.ts\nFNF:2\nFNH:1\nLF:5\nLH:3\nend_of_record\n"
	lcovPath := filepath.Join(projectDir, "lcov.info")
	os.WriteFile(lcovPath, []byte(lcovContent), 0644)

	summary, files, err := ParseLCOVFileForProject(lcovPath, projectDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 files: covered.ts + uncovered.ts + also-uncovered.tsx (test file excluded)
	if summary.FileCount != 3 {
		t.Errorf("expected 3 files, got %d", summary.FileCount)
	}

	// Two of the three files were never analyzed (not in LCOV).
	if summary.UncoveredFileCount != 2 {
		t.Errorf("expected 2 uncovered (not-analyzed) files, got %d", summary.UncoveredFileCount)
	}

	// Aggregate line counts stay anchored to LCOV's instrumented lines (LF/LH)
	// only — the not-analyzed files do NOT inflate the denominator with a
	// non-empty-line proxy.
	if summary.TotalLines != 5 {
		t.Errorf("expected 5 total (instrumented) lines, got %d", summary.TotalLines)
	}
	if summary.CoveredLines != 3 {
		t.Errorf("expected 3 covered lines, got %d", summary.CoveredLines)
	}

	// LineCoverage: 3/5 * 100 = 60% (consistent with the per-file LCOV number).
	expectedCov := 60.0
	if summary.LineCoverage != expectedCov {
		t.Errorf("expected line coverage %.2f, got %.2f", expectedCov, summary.LineCoverage)
	}

	// Per-file coverage: 3 files total (1 analyzed + 2 not-analyzed)
	if len(files) != 3 {
		t.Fatalf("expected 3 file coverage entries, got %d", len(files))
	}

	// Build a map for easier assertions
	fileMap := make(map[string]FileCoverage)
	for _, f := range files {
		fileMap[f.Path] = f
	}

	// covered.ts: 3/5 = 60%, analyzed
	if fc, ok := fileMap["src/covered.ts"]; !ok {
		t.Error("missing file coverage for src/covered.ts")
	} else {
		if !fc.Analyzed {
			t.Error("covered.ts: expected Analyzed=true")
		}
		if fc.CoveredLines != 3 {
			t.Errorf("covered.ts: expected 3 covered lines, got %d", fc.CoveredLines)
		}
		if fc.TotalLines != 5 {
			t.Errorf("covered.ts: expected 5 total lines, got %d", fc.TotalLines)
		}
		if fc.Coverage != 60.0 {
			t.Errorf("covered.ts: expected 60%% coverage, got %.2f", fc.Coverage)
		}
	}

	// uncovered.ts: not analyzed, 0% with no proxy line count
	if fc, ok := fileMap["src/uncovered.ts"]; !ok {
		t.Error("missing file coverage for src/uncovered.ts")
	} else {
		if fc.Analyzed {
			t.Error("uncovered.ts: expected Analyzed=false")
		}
		if fc.TotalLines != 0 {
			t.Errorf("uncovered.ts: expected 0 total lines (not analyzed), got %d", fc.TotalLines)
		}
		if fc.Coverage != 0 {
			t.Errorf("uncovered.ts: expected 0%% coverage, got %.2f", fc.Coverage)
		}
	}

	// also-uncovered.tsx: not analyzed, 0% with no proxy line count
	if fc, ok := fileMap["src/also-uncovered.tsx"]; !ok {
		t.Error("missing file coverage for src/also-uncovered.tsx")
	} else {
		if fc.Analyzed {
			t.Error("also-uncovered.tsx: expected Analyzed=false")
		}
		if fc.TotalLines != 0 {
			t.Errorf("also-uncovered.tsx: expected 0 total lines (not analyzed), got %d", fc.TotalLines)
		}
		if fc.Coverage != 0 {
			t.Errorf("also-uncovered.tsx: expected 0%% coverage, got %.2f", fc.Coverage)
		}
	}
}

func TestParseLCOVFileForProject_NoSrcDir(t *testing.T) {
	// Project without src/ directory — should return LCOV-only results
	projectDir := t.TempDir()

	lcovContent := "SF:src/foo.ts\nLF:10\nLH:8\nend_of_record\n"
	lcovPath := filepath.Join(projectDir, "lcov.info")
	os.WriteFile(lcovPath, []byte(lcovContent), 0644)

	summary, files, err := ParseLCOVFileForProject(lcovPath, projectDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.FileCount != 1 {
		t.Errorf("expected 1 file, got %d", summary.FileCount)
	}
	if summary.TotalLines != 10 {
		t.Errorf("expected 10 total lines, got %d", summary.TotalLines)
	}

	// Per-file: one entry from LCOV
	if len(files) != 1 {
		t.Fatalf("expected 1 file coverage entry, got %d", len(files))
	}
	if files[0].Coverage != 80.0 {
		t.Errorf("expected 80%% coverage, got %.2f", files[0].Coverage)
	}
}

func TestParseLCOVFileForProject_SkipsNonSourceFiles(t *testing.T) {
	projectDir := t.TempDir()
	srcDir := filepath.Join(projectDir, "src")
	os.MkdirAll(srcDir, 0755)

	// Non-source files should be ignored
	os.WriteFile(filepath.Join(srcDir, "readme.md"), []byte("# Hello\n"), 0644)
	os.WriteFile(filepath.Join(srcDir, "data.json"), []byte("{}\n"), 0644)
	os.WriteFile(filepath.Join(srcDir, "real.ts"), []byte("export const x = 1;\n"), 0644)

	// LCOV is empty — no files covered
	lcovPath := filepath.Join(projectDir, "lcov.info")
	os.WriteFile(lcovPath, []byte(""), 0644)

	summary, files, err := ParseLCOVFileForProject(lcovPath, projectDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only real.ts counted as a (not-analyzed) source file; .md/.json ignored.
	if summary.FileCount != 1 {
		t.Errorf("expected 1 file, got %d", summary.FileCount)
	}
	if summary.UncoveredFileCount != 1 {
		t.Errorf("expected 1 uncovered (not-analyzed) file, got %d", summary.UncoveredFileCount)
	}
	// No LCOV records → no instrumented lines counted toward the aggregate.
	if summary.TotalLines != 0 {
		t.Errorf("expected 0 instrumented lines, got %d", summary.TotalLines)
	}

	// Per-file: one not-analyzed entry at 0% with no proxy line count.
	if len(files) != 1 {
		t.Fatalf("expected 1 file coverage entry, got %d", len(files))
	}
	if files[0].Analyzed {
		t.Error("expected the lone source file to be marked not-analyzed")
	}
	if files[0].TotalLines != 0 {
		t.Errorf("expected 0 total lines (not analyzed), got %d", files[0].TotalLines)
	}
	if files[0].Coverage != 0 {
		t.Errorf("expected 0%% coverage, got %.2f", files[0].Coverage)
	}
}

func TestParseLCOVFileForProject_EmptyProjectPath(t *testing.T) {
	lcovContent := "SF:src/foo.ts\nLF:10\nLH:10\nend_of_record\n"
	path := writeTempFile(t, lcovContent)

	// Empty projectPath reports only the files the LCOV report lists
	summary, files, err := ParseLCOVFileForProject(path, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.LineCoverage != 100.0 {
		t.Errorf("expected 100%% line coverage, got %.2f", summary.LineCoverage)
	}

	// Per-file: one entry from LCOV
	if len(files) != 1 {
		t.Fatalf("expected 1 file coverage entry, got %d", len(files))
	}
	if files[0].Path != "src/foo.ts" {
		t.Errorf("expected path src/foo.ts, got %s", files[0].Path)
	}
	if files[0].Coverage != 100.0 {
		t.Errorf("expected 100%% coverage, got %.2f", files[0].Coverage)
	}
}

func TestParseLCOV_PerFileCoverage(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "structured-reports", "coverage-parses-into-a-per-file-report")
	content := `SF:src/full.ts
LF:10
LH:10
end_of_record
SF:src/partial.ts
LF:20
LH:5
end_of_record
SF:src/none.ts
LF:8
LH:0
end_of_record
`
	path := writeTempFile(t, content)

	_, _, files, err := parseLCOV(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	fileMap := make(map[string]FileCoverage)
	for _, f := range files {
		fileMap[f.Path] = f
	}

	if fc := fileMap["src/full.ts"]; fc.Coverage != 100.0 {
		t.Errorf("full.ts: expected 100%%, got %.2f", fc.Coverage)
	}
	if fc := fileMap["src/partial.ts"]; fc.Coverage != 25.0 {
		t.Errorf("partial.ts: expected 25%%, got %.2f", fc.Coverage)
	}
	if fc := fileMap["src/none.ts"]; fc.Coverage != 0.0 {
		t.Errorf("none.ts: expected 0%%, got %.2f", fc.Coverage)
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "lcov.info")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return path
}

// A report names every file in slash form, whatever separator the host's
// coverage writer used: bun on Windows writes "SF:src\covered.ts". The file
// the report names is not counted again as a never-imported source.
func TestParseLCOVFileForProject_ReportsHostPathsInSlashForm(t *testing.T) {
	projectDir := t.TempDir()
	srcDir := filepath.Join(projectDir, "src", "nested")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"covered.ts", "uncovered.ts"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("export const a = 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lcovContent := "SF:" + filepath.FromSlash("src/nested/covered.ts") + "\nLF:2\nLH:1\nend_of_record\n"
	lcovPath := filepath.Join(projectDir, "lcov.info")
	if err := os.WriteFile(lcovPath, []byte(lcovContent), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, files, err := ParseLCOVFileForProject(lcovPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FileCount != 2 || summary.UncoveredFileCount != 1 {
		t.Errorf("files = %d, not analyzed = %d, want 2 and 1", summary.FileCount, summary.UncoveredFileCount)
	}
	got := map[string]bool{}
	for _, file := range files {
		got[file.Path] = file.Analyzed
	}
	want := map[string]bool{"src/nested/covered.ts": true, "src/nested/uncovered.ts": false}
	if len(got) != len(want) || got["src/nested/covered.ts"] != true || got["src/nested/uncovered.ts"] != false {
		t.Errorf("file paths = %v, want %v", got, want)
	}
}
