package test

import (
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/parse"
	protocolcli "go.putnami.dev/protocol/cli"
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

// recordTestCases applies the test-case contract to the cases one project ran,
// within maxBytes of encoded cases, and records them in its result data: the
// kept cases under testCases when there are any, and the count of cases left
// out under testCasesDropped when it is not zero.
func recordTestCases(resultData map[string]any, cases []protocolcli.TestCase, maxBytes int) {
	if resultData == nil || len(cases) == 0 {
		return
	}
	kept, dropped := protocolcli.BoundTestCasesWithin(cases, maxBytes)
	if len(kept) > 0 {
		resultData[runtimeproto.TestCasesResultDataKey] = kept
	}
	if dropped > 0 {
		resultData[runtimeproto.TestCasesDroppedResultDataKey] = dropped
	}
}
