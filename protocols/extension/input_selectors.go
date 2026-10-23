package extension

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// A task file pattern with a scheme, `<scheme>:<rest>`, names an input
// selector, except under the scheme `git`, which is the core's own candidate
// pattern. The extension that declares the task lists the selector in
// runtime.inputSelectors and expands it through the workspace protocol's
// input-resolve call (protocol/cli InputSelectorsContract).
//
// The rule reads the scheme alone, so a pattern that spells a scheme and an
// invalid name, such as `go-embed:Build`, is refused instead of read as a glob.
var filePatternSchemeRE = regexp.MustCompile(`^[a-z][a-z0-9-]*:`)

// filePatternScheme returns the scheme a task file pattern names, after an
// optional "!", and false for a pattern without one and for a `git:` pattern.
func filePatternScheme(pattern string) (string, bool) {
	body := strings.TrimPrefix(pattern, "!")
	scheme := filePatternSchemeRE.FindString(body)
	if scheme == "" {
		return "", false
	}
	scheme = strings.TrimSuffix(scheme, ":")
	return scheme, scheme != runtimeInputSelectorReservedScheme
}

// NamesInputSelector reports whether a task file pattern names an input
// selector: it has a scheme other than `git`.
func NamesInputSelector(pattern string) bool {
	_, ok := filePatternScheme(pattern)
	return ok
}

// SelectorInput is one input selector a task names, with the other file
// patterns of the task input that names it: the patterns the core sends with
// the selector in an input-resolve query.
type SelectorInput struct {
	// Selector is the selector name.
	Selector string
	// Patterns are the input's other file patterns, includes and exclusions,
	// in declaration order. They name no selector.
	Patterns []string
}

// TaskSelectorInputs lists the input selectors task names in its project
// inputs: each `from: "project"` input in name order, or cache.key.files for
// a task that declares no inputs, each selector with the input's other
// patterns. A selector two inputs name is listed once per input. The list is
// nil for a task that names none.
func TaskSelectorInputs(task TaskDefinition) []SelectorInput {
	var inputs []SelectorInput
	add := func(files []string) {
		var selectors, others []string
		for _, pattern := range files {
			if NamesInputSelector(pattern) {
				selectors = append(selectors, pattern)
			} else {
				others = append(others, pattern)
			}
		}
		for _, selector := range selectors {
			inputs = append(inputs, SelectorInput{Selector: selector, Patterns: slices.Clone(others)})
		}
	}
	if len(task.Inputs) > 0 {
		for _, name := range sortedKeys(task.Inputs) {
			if port := task.Inputs[name]; port.From == TaskInputFromProject {
				add(port.Files)
			}
		}
		return inputs
	}
	if task.Cache != nil && task.Cache.Key != nil {
		add(task.Cache.Key.Files)
	}
	return inputs
}

// declaresInputSelector reports whether a task names an input selector in any
// file pattern: an input's files or its cache key's files, closure files or
// workspace files.
func declaresInputSelector(task TaskDefinition) bool {
	for _, input := range task.Inputs {
		if slices.ContainsFunc(input.Files, NamesInputSelector) {
			return true
		}
	}
	if task.Cache == nil || task.Cache.Key == nil {
		return false
	}
	key := task.Cache.Key
	return slices.ContainsFunc(key.Files, NamesInputSelector) ||
		slices.ContainsFunc(key.ClosureFiles, NamesInputSelector) ||
		slices.ContainsFunc(key.WorkspaceFiles, NamesInputSelector)
}

// declaresInputSelectors reports whether a manifest lists input selectors in
// runtime.inputSelectors or names one in a task file pattern: the vocabulary
// of protocol/cli InputSelectorsContract.
func declaresInputSelectors(m *Manifest) bool {
	if m == nil {
		return false
	}
	if m.Runtime != nil && len(m.Runtime.InputSelectors) > 0 {
		return true
	}
	for _, task := range m.Tasks {
		if declaresInputSelector(task) {
			return true
		}
	}
	return false
}

// validateInputSelectors checks every task file pattern that names an input
// selector: it is an include of a `from: "project"` input or of
// cache.key.files, and it names a selector runtime.inputSelectors lists. The
// negotiated loader and the full authoring validator share it, so both refuse
// a selector no extension expands.
func validateInputSelectors(m *Manifest) []diag.Diagnostic {
	var listed []string
	if m.Runtime != nil {
		listed = m.Runtime.InputSelectors
	}
	var diags []diag.Diagnostic
	check := func(pattern, where string, allowed bool) {
		if !NamesInputSelector(pattern) {
			return
		}
		switch {
		case !allowed:
			diags = append(diags, diag.Errorf("invalid-value", where,
				"input selector %q is only valid in a project task input or cache.key.files", pattern))
		case strings.HasPrefix(pattern, "!"):
			diags = append(diags, diag.Errorf("invalid-value", where,
				"input selector %q cannot be an exclusion; a task input names a selector to read its files", pattern))
		case !slices.Contains(listed, pattern):
			diags = append(diags, diag.Errorf("invalid-value", where,
				"input selector %q is not listed in runtime.inputSelectors; the extension that declares the task expands each selector it names",
				pattern))
		}
	}
	for _, name := range sortedKeys(m.Tasks) {
		task := m.Tasks[name]
		for _, portName := range sortedKeys(task.Inputs) {
			port := task.Inputs[portName]
			for index, pattern := range port.Files {
				check(pattern, fmt.Sprintf("tasks.%s.inputs.%s.files[%d]", name, portName, index), port.From == TaskInputFromProject)
			}
		}
		if task.Cache != nil && task.Cache.Key != nil {
			for index, pattern := range task.Cache.Key.Files {
				check(pattern, fmt.Sprintf("tasks.%s.cache.key.files[%d]", name, index), true)
			}
			for index, pattern := range task.Cache.Key.ClosureFiles {
				check(pattern, fmt.Sprintf("tasks.%s.cache.key.closureFiles[%d]", name, index), false)
			}
			for index, pattern := range task.Cache.Key.WorkspaceFiles {
				check(pattern, fmt.Sprintf("tasks.%s.cache.key.workspaceFiles[%d]", name, index), false)
			}
		}
	}
	return diags
}
