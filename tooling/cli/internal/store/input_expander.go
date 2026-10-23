package store

import (
	"errors"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ErrInputSelector identifies an invalid input selection: a selector no
// expander answers, a failed or rejected expansion, an expansion that is an
// error, or a file it lists that cannot be read. A scheduler must fail such a
// task before cache restoration or execution; an unrelated cache-store failure
// may still fall back to execution.
var ErrInputSelector = errors.New("invalid input selector")

// InputExpansion is what an input selector reads in one directory: the
// extension's answer to the workspace protocol's input-resolve call.
type InputExpansion struct {
	// Files are every file the selector reads now: sorted, unique, clean
	// slash paths relative to the directory. The key hashes their exact
	// bytes.
	Files []string
	// Basis is what Files were computed from, as pattern sets relative to the
	// directory. Files stay exact while wsproto.InputBasisDigest of the
	// directory under Basis is unchanged.
	Basis wsproto.InputBasis
}

// InputExpander expands the input selectors that one job's file patterns
// name. An expander knows the job's task inputs, so it knows the other
// patterns of the input that names each selector.
type InputExpander interface {
	// ExpandInput lists what selector reads in dir. An error wraps
	// ErrInputSelector.
	ExpandInput(dir, selector string) (InputExpansion, error)
	// Identity names what the expander answers: two expanders with one
	// identity expand every selector in every directory alike. A
	// CacheManager memoizes a digest by directory, patterns, config scope and
	// this identity.
	Identity() string
}

// InputQuery is one input selector to expand: a selector a task input of one
// project names, with the input's other patterns.
type InputQuery struct {
	// Extension is the name of the extension that declares the task, which
	// lists the selector in its runtime.inputSelectors.
	Extension string
	// Dir is the absolute project directory.
	Dir string
	// Selector is the selector name.
	Selector string
	// Patterns are the other file patterns of the task input that names the
	// selector, as declared.
	Patterns []string
}

// InputResolver answers input queries through the extensions that list the
// selectors. A run's CacheManager carries one (SetInputResolver).
type InputResolver interface {
	// ResolveInput answers one query. An error wraps ErrInputSelector.
	ResolveInput(query InputQuery) (InputExpansion, error)
	// PrimeInputs asks each extension once for every query it keeps no
	// answer for. It reports nothing: ResolveInput reports a failure for the
	// input that reads it.
	PrimeInputs(queries []InputQuery)
}

// SetInputResolver attaches the resolver the run's keys expand input
// selectors with. A manager without one fails every selector.
func (cm *CacheManager) SetInputResolver(resolver InputResolver) {
	cm.inputResolverMu.Lock()
	defer cm.inputResolverMu.Unlock()
	cm.inputResolver = resolver
}

// InputResolver returns the resolver SetInputResolver attached, or nil.
func (cm *CacheManager) InputResolver() InputResolver {
	if cm == nil {
		return nil
	}
	cm.inputResolverMu.Lock()
	defer cm.inputResolverMu.Unlock()
	return cm.inputResolver
}
