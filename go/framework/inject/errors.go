package inject

import (
	"strconv"
	"strings"

	"go.putnami.dev/errors"
)

// Error codes for the inject package.
const (
	CodeNotRegistered     errors.Code = "inject.not_registered"
	CodeCircularDep       errors.Code = "inject.circular_dependency"
	CodeScopeViolation    errors.Code = "inject.scope_violation"
	CodeContainerClosed   errors.Code = "inject.container_closed"
	CodeDuplicateProvider errors.Code = "inject.duplicate_provider"
	CodeRequirementNotMet errors.Code = "inject.requirement_not_met"
	CodeValidation        errors.Code = "inject.validation"
	CodeTypeMismatch      errors.Code = "inject.type_mismatch"
	CodeFactoryFailed     errors.Code = "inject.factory_failed"
	CodeIllegalState      errors.Code = "inject.illegal_state"
)

// newNotRegisteredError creates an error for when a token is not found in the container hierarchy.
func newNotRegisteredError(token Token, context string) *errors.Error {
	attrs := []errors.Attr{errors.String("token", TokenName(token))}
	if context != "" {
		attrs = append(attrs, errors.String("context", context))
	}
	return errors.New(CodeNotRegistered, "no provider registered for "+TokenName(token), attrs...)
}

// newCircularDependencyError creates an error for when a circular dependency is detected.
func newCircularDependencyError(chain []Token) *errors.Error {
	names := make([]string, len(chain))
	for i, t := range chain {
		names[i] = TokenName(t)
	}
	return errors.New(CodeCircularDep, "circular dependency detected: "+strings.Join(names, " -> "),
		errors.String("chain", strings.Join(names, " -> ")),
	)
}

// newScopeViolationError creates an error for when a singleton depends on a scoped provider.
func newScopeViolationError(singleton, scoped Token) *errors.Error {
	return errors.New(CodeScopeViolation,
		"singleton "+TokenName(singleton)+" cannot depend on scoped "+TokenName(scoped)+" (resolve it from the request context where it is used)",
		errors.String("singleton", TokenName(singleton)),
		errors.String("scoped", TokenName(scoped)),
	)
}

// newScopeResolutionViolation builds a scope-violation error for a scoped
// provider being resolved at its owner's (root/singleton) lifetime instead of
// within a scope. chain holds the active resolution path; its last element, when
// present, is the dependent whose factory triggered the resolution.
func newScopeResolutionViolation(scoped Token, chain []Token) *errors.Error {
	if n := len(chain); n > 0 {
		return newScopeViolationError(chain[n-1], scoped)
	}
	return errors.New(CodeScopeViolation,
		"scoped "+TokenName(scoped)+" cannot be resolved outside a scope (resolve it within a Scope())",
		errors.String("scoped", TokenName(scoped)),
	)
}

// newContainerClosedError creates an error for when a closed container is accessed.
func newContainerClosedError(container string) *errors.Error {
	return errors.New(CodeContainerClosed, "container \""+container+"\" is closed",
		errors.String("container", container),
	)
}

// newDuplicateProviderError creates an error for when the same token is registered twice.
func newDuplicateProviderError(token Token, container string) *errors.Error {
	return errors.New(CodeDuplicateProvider,
		"duplicate provider for "+TokenName(token)+" in container \""+container+"\"",
		errors.String("token", TokenName(token)),
		errors.String("container", container),
	)
}

// newRequirementNotMetError creates an error for when a module requirement is not satisfied.
func newRequirementNotMetError(token Token, module string) *errors.Error {
	return errors.New(CodeRequirementNotMet,
		"requirement "+TokenName(token)+" not met in module \""+module+"\"",
		errors.String("token", TokenName(token)),
		errors.String("module", module),
	)
}

// newValidationError creates an error that aggregates multiple validation issues.
//
// The issues are attached as wrapped causes (via an AggregateError) rather than
// only embedding their .Error() strings, so the individual codes of each issue
// (for example CodeCircularDep or CodeNotRegistered) remain recoverable with
// errors.Is on the error returned from Start(). The errors package's tree walk
// descends into AggregateError's children, so errors.Is matches every contained
// code regardless of how many issues there are.
func newValidationError(issues []error) *errors.Error {
	if len(issues) == 1 {
		return errors.Wrap(issues[0], CodeValidation,
			errors.Int("issue_count", 1),
		)
	}
	msg := "container validation failed with " + strconv.Itoa(len(issues)) + " issues"
	return errors.Wrap(
		errors.NewAggregate(msg, issues),
		CodeValidation,
		errors.Int("issue_count", len(issues)),
	)
}

// newTypeMismatchError creates an error for when a resolved value doesn't match the expected type.
func newTypeMismatchError(token Token, expected string) *errors.Error {
	return errors.New(CodeTypeMismatch,
		"type mismatch for "+TokenName(token)+": expected "+expected,
		errors.String("token", TokenName(token)),
		errors.String("expected", expected),
	)
}

// formatResolutionChain formats a chain of tokens for error messages.
func formatResolutionChain(chain []Token) string {
	names := make([]string, len(chain))
	for i, t := range chain {
		names[i] = TokenName(t)
	}
	return strings.Join(names, " -> ")
}
