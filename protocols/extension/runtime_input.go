package extension

import (
	"fmt"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Bounds of runtime.inputSelectors. An entry is an input selector name, which
// the workspace protocol caps at 128 bytes.
const (
	maxRuntimeInputSelectors     = 16
	maxRuntimeInputSelectorBytes = 128
)

// runtimeInputSelectorPattern is the workspace protocol's grammar of an input
// selector name (protocol/workspace IsInputSelectorName): `<scheme>:<name>`,
// where the scheme `git` is reserved for the core's `git:` file pattern.
// schemas/extension.json states the same pattern and bound for
// runtime.inputSelectors, and refuses the reserved scheme with `not`.
const runtimeInputSelectorPattern = `^[a-z][a-z0-9-]*:[a-z0-9][a-z0-9-]*$`

// runtimeInputSelectorReservedScheme is the scheme of the core's own `git:`
// file pattern. No selector uses it.
const runtimeInputSelectorReservedScheme = "git"

var runtimeInputSelectorRE = regexp.MustCompile(runtimeInputSelectorPattern)

// validInputSelectorName reports whether value is an input selector name.
func validInputSelectorName(value string) bool {
	if len(value) > maxRuntimeInputSelectorBytes || !runtimeInputSelectorRE.MatchString(value) {
		return false
	}
	scheme, _, _ := strings.Cut(value, ":")
	return scheme != runtimeInputSelectorReservedScheme
}

// validateRuntimeInputSelectors checks runtime.inputSelectors: each entry is
// an input selector name, named once, and the list is bounded.
func validateRuntimeInputSelectors(selectors []string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if len(selectors) > maxRuntimeInputSelectors {
		diags = append(diags, diag.Errorf("limit-exceeded", "runtime.inputSelectors",
			"runtime.inputSelectors lists %d selectors; at most %d are allowed", len(selectors), maxRuntimeInputSelectors))
	}
	seen := map[string]bool{}
	for i, selector := range selectors {
		item := fmt.Sprintf("runtime.inputSelectors[%d]", i)
		if seen[selector] {
			diags = append(diags, diag.Errorf("duplicate-value", item, "input selector %q is listed more than once", selector))
		}
		seen[selector] = true
		if !validInputSelectorName(selector) {
			diags = append(diags, diag.Errorf("invalid-value", item,
				"input selector %q must match %s, use a scheme other than %q and be at most %d bytes",
				selector, runtimeInputSelectorPattern, runtimeInputSelectorReservedScheme, maxRuntimeInputSelectorBytes))
		}
	}
	return diags
}
