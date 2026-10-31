package test

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/parse"
	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"golang.org/x/mod/modfile"
)

// moduleTestCases returns the test cases of one module's `go test -json`
// output, locating each case's test file with locate. It returns none when the
// output is not JSON: a runtime without structured output reports no case.
func moduleTestCases(output string, useJSON bool, locate parse.TestFileLocator) []protocolcli.TestCase {
	if !useJSON {
		return nil
	}
	return parse.TestCaseList(output, locate)
}

// readModulePath returns the module path the go.mod in moduleRoot declares,
// or "" when it cannot read one.
func readModulePath(moduleRoot string) string {
	goMod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(modfile.ModulePath(goMod))
}

// observingTestCases returns the predicate that tells a Go test case that
// observes a declared check: the top-level test an observing test names, in
// the package of its declaration file. observing are the project's
// specreport.ObservingTests, and modulePath is the module at the project
// root. It returns nil when no test observes a check.
func observingTestCases(observing []featureproto.ObservationProvenance, modulePath string) func(protocolcli.TestCase) bool {
	if len(observing) == 0 || modulePath == "" {
		return nil
	}
	type caseKey struct{ suite, name string }
	keys := make(map[caseKey]bool, len(observing))
	for _, test := range observing {
		suite := modulePath
		if dir := path.Dir(test.Path); dir != "." {
			suite += "/" + dir
		}
		keys[caseKey{suite, test.Symbol}] = true
	}
	return func(testCase protocolcli.TestCase) bool {
		return keys[caseKey{testCase.Suite, testCase.Name}]
	}
}

// recordTestCases applies the test-case contract to the cases one project ran,
// within maxBytes of encoded cases, and records them in its result data: the
// kept cases under testCases when there are any, the count of cases left out
// under testCasesDropped when it is not zero, and their wall time under
// testCasesDroppedDurationMs when it is not zero. observes tells the cases
// that observe a declared check, which the bound keeps first among the
// passed ones.
func recordTestCases(resultData map[string]any, cases []protocolcli.TestCase, maxBytes int, observes func(protocolcli.TestCase) bool) {
	if resultData == nil || len(cases) == 0 {
		return
	}
	selection := protocolcli.SelectTestCases(cases, maxBytes, observes)
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
