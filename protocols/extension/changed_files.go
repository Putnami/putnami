package extension

import (
	"fmt"

	diag "go.putnami.dev/protocol/diagnostic"
)

// DeclaresChangedFiles reports whether a step of the manifest gates on the
// run's changed files (activation.changedFiles) or binds a context input, the
// vocabulary of protocolcli.ChangedFilesContract. A reader at that contract
// plans such a step only for a project that owns a changed file and binds the
// files into the step's cache key.
func DeclaresChangedFiles(m *Manifest) bool {
	if m == nil {
		return false
	}
	for _, command := range m.Commands {
		for _, step := range command.Run {
			if step.Activation != nil && len(step.Activation.ChangedFiles) > 0 {
				return true
			}
			for _, binding := range step.With {
				if binding.From != "" {
					return true
				}
			}
		}
	}
	return false
}

// validateBindingSources refuses an input binding the planner cannot resolve:
// a `from` other than InputBindingFromContext, a context path other than
// ContextPathChangedFiles, or a path with no source. The CLI resolves exactly
// that one fact, so any other spelling would reach the task as no value and
// key the step without the input its author meant to bind.
func validateBindingSources(field string, step PipelineStep) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, name := range sortedKeys(step.With) {
		binding := step.With[name]
		bindingField := fmt.Sprintf("%s.with.%s", field, name)
		switch {
		case binding.From == "" && binding.FromStep == "" && binding.Path != "":
			diags = append(diags, diag.Errorf("unresolvable-binding", bindingField+".path",
				"binding %q names path %q without a source; a context binding declares from %q",
				name, binding.Path, InputBindingFromContext))
		case binding.From == "":
		case binding.From != InputBindingFromContext:
			diags = append(diags, diag.Errorf("unresolvable-binding", bindingField+".from",
				"binding %q reads from %q, which the planner does not resolve; the only binding source is %q",
				name, binding.From, InputBindingFromContext))
		case binding.Path != ContextPathChangedFiles:
			diags = append(diags, diag.Errorf("unresolvable-binding", bindingField+".path",
				"binding %q reads context path %q, which the planner does not resolve; the only context path is %q",
				name, binding.Path, ContextPathChangedFiles))
		}
	}
	return diags
}
