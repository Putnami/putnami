package main

import (
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/typescript/extension/internal/parse"
)

// recordTestCases turns the test cases of one project's JUnit report into
// protocol test cases, applies the test-case contract, and records them in
// the project's result data: the kept cases under testCases when there are
// any, and the count of cases left out under testCasesDropped when it is not
// zero. The kept cases fit in maxBytes of encoded cases. projectPath is the
// project directory bun ran in, relative to workspaceRoot.
func recordTestCases(resultData map[string]any, workspaceRoot, projectPath string, summary *parse.TestSummary, maxBytes int) {
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
	kept, dropped := protocolcli.BoundTestCasesWithin(cases, maxBytes)
	if len(kept) > 0 {
		resultData[runtimeproto.TestCasesResultDataKey] = kept
	}
	if dropped > 0 {
		resultData[runtimeproto.TestCasesDroppedResultDataKey] = dropped
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
