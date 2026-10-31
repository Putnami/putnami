package main

import (
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	features "go.putnami.dev/protocol/features"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/typescript/extension/internal/parse"
)

// recordTestCases turns the test cases of one project's JUnit report into
// protocol test cases, applies the test-case contract, and records them in
// the project's result data: the kept cases under testCases when there are
// any, the count of cases left out under testCasesDropped when it is not zero,
// and their wall time under testCasesDroppedDurationMs when it is not zero.
// The kept cases fit in maxBytes of encoded cases. projectPath is the project
// directory bun ran in, relative to workspaceRoot. observing are the project's
// specreport.ObservingTests: the bound keeps their cases first among the
// passed ones.
func recordTestCases(
	resultData map[string]any,
	workspaceRoot, projectPath string,
	summary *parse.TestSummary,
	maxBytes int,
	observing []features.ObservationProvenance,
) {
	if resultData == nil || summary == nil || len(summary.Cases) == 0 {
		return
	}
	cases := make([]protocolcli.TestCase, 0, len(summary.Cases))
	for _, reported := range summary.Cases {
		testCase := protocolcli.TestCase{
			Name:       reported.Name,
			Suite:      workspaceTestPath(workspaceRoot, projectPath, reported.SuiteFile),
			Status:     reported.Status,
			DurationMs: reported.DurationMs,
			Output:     reported.Output,
			File:       workspaceTestPath(workspaceRoot, projectPath, reported.File),
		}
		if testCase.File != "" {
			testCase.Line = reported.Line
		}
		cases = append(cases, testCase)
	}
	selection := protocolcli.SelectTestCases(cases, maxBytes, observingTestCases(workspaceRoot, projectPath, observing))
	if len(selection.Kept) > 0 {
		resultData[runtimeproto.TestCasesResultDataKey] = selection.Kept
	}
	if selection.Dropped > 0 {
		resultData[runtimeproto.TestCasesDroppedResultDataKey] = selection.Dropped
		if selection.DroppedDurationMs > 0 {
			resultData[runtimeproto.TestCasesDroppedDurationMsResultDataKey] = selection.DroppedDurationMs
		}
	}
}

// observingTestCases returns the predicate that tells a test case that
// observes a declared check: a case declared in, or reported under, the file
// of an observing test, whose name is that test's name or ends with it after
// the describe names. It returns nil when no test observes a check.
func observingTestCases(workspaceRoot, projectPath string, observing []features.ObservationProvenance) func(protocolcli.TestCase) bool {
	symbols := make(map[string][]string, len(observing))
	for _, test := range observing {
		if file := workspaceTestPath(workspaceRoot, projectPath, test.Path); file != "" {
			symbols[file] = append(symbols[file], test.Symbol)
		}
	}
	if len(symbols) == 0 {
		return nil
	}
	return func(testCase protocolcli.TestCase) bool {
		for _, file := range []string{testCase.File, testCase.Suite} {
			for _, symbol := range symbols[file] {
				if testCase.Name == symbol || strings.HasSuffix(testCase.Name, " > "+symbol) {
					return true
				}
			}
		}
		return false
	}
}

// workspaceTestPath renders a path from a bun report, absolute or relative to
// the project directory bun ran in, as a workspace-relative slash path. It
// returns "" for an empty path and for a path outside the workspace.
func workspaceTestPath(workspaceRoot, projectPath, file string) string {
	if file == "" {
		return ""
	}
	projectDir := projectPath
	if !filepath.IsAbs(projectDir) {
		projectDir = filepath.Join(workspaceRoot, projectDir)
	}
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(projectDir, abs)
	}
	rel, err := filepath.Rel(workspaceRoot, abs)
	if err != nil || filepath.IsAbs(rel) || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}
