package extension

import (
	"fmt"
	"strings"

	proto "go.putnami.dev/protocol/extension"
)

// StepSeparator is used in step job names: build~transpile.
const StepSeparator = "~"

// StepJobName builds a composite name: {commandName}~{stepId}.
func StepJobName(commandName, stepId string) string {
	return commandName + StepSeparator + stepId
}

// NamespaceForExtension returns the stable namespace used in internal job
// names when multiple extensions contribute steps to the same command.
func NamespaceForExtension(identity string) string {
	namespace := strings.TrimSpace(identity)
	if namespace == "" {
		return "extension"
	}
	replacer := strings.NewReplacer(
		StepSeparator, "_",
		":", "_",
		" ", "_",
	)
	return replacer.Replace(namespace)
}

// NamespacedCommandJobName builds an internal name for a non-pipeline command
// contributed alongside other extensions.
func NamespacedCommandJobName(commandName, namespace string) string {
	return commandName + StepSeparator + NamespaceForExtension(namespace)
}

// NamespacedStepJobName builds an internal name for a pipeline step contributed
// alongside other extensions.
func NamespacedStepJobName(commandName, namespace, stepID string) string {
	return commandName + StepSeparator + NamespaceForExtension(namespace) + StepSeparator + stepID
}

// IsExternalRef returns true if the dependency reference is cross-project.
// External refs start with ^ (upstream), / (workspace), or * (all).
func IsExternalRef(ref string) bool {
	if len(ref) == 0 {
		return false
	}
	c := ref[0]
	return c == '^' || c == '/' || c == '*'
}

// ExpandedStep is the result of expanding a pipeline step into a job definition.
type ExpandedStep struct {
	JobDef *JobDefinition
	Step   PipelineStep
	Task   TaskDefinition
}

// PipelineExpansionOptions controls planner-only details of pipeline expansion.
type PipelineExpansionOptions struct {
	Namespace string
	// PlanContext carries planner-owned, invocation-stable expression roots.
	// It deliberately excludes Params (supplied separately) and step results
	// (which do not exist at plan time). CommandParams holds the fully resolved
	// params of every effective provider/project command, allowing a condition
	// to prove replacement work uses the same compile configuration. A nil
	// context keeps expressions that reference planner facts instead of guessing
	// and pruning work.
	PlanContext *WhenContext

	// StepActive, when set, decides whether a step's plan-time activation gate
	// is satisfied for the target project. It is consulted only for steps that
	// declare an Activation. An inactive step is spliced out of the pipeline
	// before `if`/dead-code handling, with its dependents inheriting its
	// dependencies so ordering and data-flow are preserved. A nil predicate
	// leaves every step active (no project-aware pruning).
	StepActive func(PipelineStep) bool
}

// ExpandPipeline expands a command's pipeline steps into individual job definitions.
//
// Each step becomes a JobDefinition with:
//   - Name: {commandName}~{stepId}
//   - Dependencies: intra-pipeline deps are prefixed, external refs pass through
//   - Task's cache policy, timeout, file patterns
//
// Steps excluded by plan-time `if` conditions and dead-code elimination are removed.
func ExpandPipeline(
	commandName string,
	commandDef *JobDefinition,
	steps []PipelineStep,
	ext *ExtensionDescription,
	commandParams map[string]any,
) ([]ExpandedStep, error) {
	return ExpandPipelineWithOptions(commandName, commandDef, steps, ext, commandParams, PipelineExpansionOptions{})
}

// ExpandPipelineWithOptions expands a command pipeline with planner-specific
// options, such as internal namespacing for composable commands.
func ExpandPipelineWithOptions(
	commandName string,
	commandDef *JobDefinition,
	steps []PipelineStep,
	ext *ExtensionDescription,
	commandParams map[string]any,
	opts PipelineExpansionOptions,
) ([]ExpandedStep, error) {
	if ext.Tasks == nil {
		return nil, fmt.Errorf("extension '%s' has pipeline command '%s' but no tasks", ext.Name, commandName)
	}

	// Plan-time activation pruning. Steps whose project-aware activation gate is
	// unmet are spliced out before `if`/dead-code handling so their dependents
	// reconnect to the pruned step's dependencies (see spliceInactiveSteps).
	if opts.StepActive != nil {
		var inactive map[string]bool
		for _, step := range steps {
			if step.Activation != nil && !opts.StepActive(step) {
				if inactive == nil {
					inactive = make(map[string]bool)
				}
				inactive[step.ID] = true
			}
		}
		if len(inactive) > 0 {
			steps = spliceInactiveSteps(steps, inactive)
		}
	}

	excluded := computeExcludedSteps(steps, commandParams, opts.PlanContext)

	var expanded []ExpandedStep
	for _, step := range steps {
		if excluded[step.ID] {
			continue
		}

		task, ok := ext.Tasks[step.Task]
		if !ok {
			return nil, fmt.Errorf("step '%s' references unknown task '%s' in extension '%s'",
				step.ID, step.Task, ext.Name)
		}

		name := StepJobName(commandName, step.ID)
		internalName := ""
		if opts.Namespace != "" {
			internalName = NamespacedStepJobName(commandName, opts.Namespace, step.ID)
		}

		// Build dependency list
		var dependsOn []string
		for _, dep := range step.DependsOn {
			if IsExternalRef(dep) {
				dependsOn = append(dependsOn, dep)
			} else if !excluded[dep] {
				if opts.Namespace != "" {
					dependsOn = append(dependsOn, NamespacedStepJobName(commandName, opts.Namespace, dep))
				} else {
					dependsOn = append(dependsOn, StepJobName(commandName, dep))
				}
			}
		}

		// Determine cache behavior: step override > task policy > enabled by default
		taskCacheEnabled := task.Cache.IsEnabled()
		stepCacheEnabled := taskCacheEnabled
		if step.Cache != nil && step.Cache.Enabled != nil {
			stepCacheEnabled = *step.Cache.Enabled
		}

		timeoutMs := task.TimeoutMs
		if step.TimeoutMs != nil {
			timeoutMs = *step.TimeoutMs
		}

		// v2: derive file patterns from task.Inputs; v1: use task.Cache.Key.Files
		var filePatterns []string
		var effectiveCachePolicy *TaskCachePolicy
		if len(task.Inputs) > 0 {
			derivedKey := DeriveTaskCacheKey(task.Inputs)
			filePatterns = derivedKey.Files
			effectiveCachePolicy = &TaskCachePolicy{
				Key: &derivedKey,
			}
			if task.Cache != nil {
				effectiveCachePolicy.Enabled = task.Cache.Enabled
				effectiveCachePolicy.Deterministic = task.Cache.Deterministic
				effectiveCachePolicy.VersionAware = task.Cache.VersionAware
				effectiveCachePolicy.NoOutput = task.Cache.NoOutput
			}
		} else if task.Cache != nil && task.Cache.Key != nil {
			filePatterns = task.Cache.Key.Files
			effectiveCachePolicy = task.Cache
		} else {
			effectiveCachePolicy = task.Cache
		}

		jobDef := &JobDefinition{
			ExtensionName:        ext.Name,
			ExtensionPath:        ext.RelPath,
			Name:                 name,
			InternalName:         internalName,
			CommandName:          commandName,
			StepID:               step.ID,
			Visibility:           commandDef.Visibility,
			Kind:                 "command",
			Command:              task.Command,
			Args:                 task.Args,
			Cwd:                  task.Cwd,
			Env:                  task.Env,
			Toolchains:           runtimeToolchainRefs(ext.Runtime, task.Toolchains),
			TimeoutMs:            timeoutMs,
			DependsOn:            dependsOn,
			CommandDependsOn:     commandDef.CommandDependsOn,
			SessionPrerequisites: commandDef.SessionPrerequisites,
			AlsoRuns:             commandDef.AlsoRuns,
			BoundParams:          literalBindingParams(step.With),
			FilePatterns:         filePatterns,
			Writes:               task.Writes,
			Reads:                task.Reads,
			Resources:            task.Resources,
			Cache:                stepCacheEnabled,
			Priority:             commandDef.Priority,
			Quiet:                true, // pipeline steps are internal
			Flags:                nil,  // pipeline steps receive params via --putnamiContext, not CLI flags
			TaskCachePolicy:      effectiveCachePolicy,
			Batchable:            task.Batchable,
			Traits:               commandDef.Traits,
			Heavy:                step.Heavy,
			CPUWeight:            step.CPUWeight,
			ContractDigest:       proto.TaskContractDigest(task),
		}

		expanded = append(expanded, ExpandedStep{
			JobDef: jobDef,
			Step:   step,
			Task:   task,
		})
	}

	return expanded, nil
}

// literalBindingParams materializes the literal members of a step's `with`
// map into a fresh, step-local parameter bag. HasValue, rather than Value !=
// nil, is the discriminator because JSON null is a valid explicit binding.
// Context and producer bindings remain wiring declarations; they are resolved
// by their own planner/runtime paths and must not be mistaken for literals.
func literalBindingParams(bindings map[string]InputBinding) map[string]any {
	var params map[string]any
	for name, binding := range bindings {
		if !binding.HasValue {
			continue
		}
		if params == nil {
			params = make(map[string]any)
		}
		params[name] = binding.Value
	}
	return params
}

// computeExcludedSteps determines which steps should be excluded:
// 1. Steps whose plan-time `if` evaluates to false.
// 2. Dead-code elimination: prerequisite steps only feeding excluded steps.
func computeExcludedSteps(steps []PipelineStep, commandParams map[string]any, planContext *WhenContext) map[string]bool {
	excluded := make(map[string]bool)

	ctx := &WhenContext{
		Params: commandParams,
		Steps:  make(map[string]*StepResult),
	}
	if planContext != nil {
		ctx.Commands = planContext.Commands
		ctx.CommandParams = planContext.CommandParams
		ctx.ProjectType = planContext.ProjectType
	}

	// Evaluate plan-time `if` conditions
	for _, step := range steps {
		if step.If != "" {
			// ExpandPipeline predates planner-owned condition roots and remains a
			// public compatibility helper. If a caller has no plan context, a
			// condition that needs it keeps the step: an optimization must fail
			// closed rather than turn an unknown invocation into permission to
			// delete work. Params-only conditions retain their exact behavior.
			if planContext == nil && usesPlanConditionContext(step.If) {
				continue
			}
			if !EvaluateExpression(step.If, ctx) {
				excluded[step.ID] = true
			}
		}
	}

	// Dead-code elimination: find leaves in original topology,
	// keep only those that survived `if` filtering, walk backwards.
	allDependedOn := make(map[string]bool)
	for _, step := range steps {
		for _, dep := range step.DependsOn {
			if !IsExternalRef(dep) {
				allDependedOn[dep] = true
			}
		}
	}

	// Original leaves: steps that no other step depends on
	var survivingLeaves []string
	for _, step := range steps {
		if !allDependedOn[step.ID] && !excluded[step.ID] {
			survivingLeaves = append(survivingLeaves, step.ID)
		}
	}

	// Walk backwards from surviving leaves to collect needed steps
	stepMap := make(map[string]PipelineStep, len(steps))
	for _, s := range steps {
		stepMap[s.ID] = s
	}

	needed := make(map[string]bool)
	pending := append([]string(nil), survivingLeaves...)
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if needed[id] || excluded[id] {
			continue
		}
		needed[id] = true
		if step, ok := stepMap[id]; ok {
			for _, dep := range step.DependsOn {
				if !IsExternalRef(dep) && !needed[dep] {
					pending = append(pending, dep)
				}
			}
		}
	}

	// Mark unreachable steps as excluded
	for _, step := range steps {
		if !excluded[step.ID] && !needed[step.ID] {
			excluded[step.ID] = true
		}
	}

	return excluded
}

// usesPlanConditionContext reports whether an expression references one of the
// planner-owned roots. The scanner ignores quoted literals, so a string value
// containing "commands." does not accidentally change compatibility behavior.
func usesPlanConditionContext(expr string) bool {
	var inQuote byte
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		for _, root := range []string{"commands", "commandParams", "project"} {
			if !strings.HasPrefix(expr[i:], root) {
				continue
			}
			end := i + len(root)
			if end < len(expr) && expr[end] != '.' {
				continue
			}
			// A planner root begins an operand. Requiring an expression
			// boundary avoids mistaking historical nested params such as
			// params.commands.enabled for the planner-owned commands root.
			previous := i - 1
			for previous >= 0 && (expr[previous] == ' ' || expr[previous] == '\t' || expr[previous] == '\n' || expr[previous] == '\r') {
				previous--
			}
			if previous < 0 || strings.ContainsRune("(!&|=", rune(expr[previous])) {
				return true
			}
		}
	}
	return false
}

// spliceInactiveSteps removes steps whose plan-time activation gate is unmet
// and reconnects the graph: every surviving step's dependency on an inactive
// step is replaced by that step's own (recursively spliced) dependencies.
// External refs are carried through unchanged, so the transitive ordering and
// data-flow across the remaining steps are preserved — splicing out a node
// that would only have skipped at runtime is behavior-neutral.
//
// This differs from `if`-based exclusion, which drops a step's edges instead
// of bridging them: an `if`-excluded step is one the pipeline author chose to
// remove, whereas an activation-pruned step is a no-op pass-through that other
// steps still depend on transitively.
func spliceInactiveSteps(steps []PipelineStep, inactive map[string]bool) []PipelineStep {
	byID := make(map[string]PipelineStep, len(steps))
	for _, s := range steps {
		byID[s.ID] = s
	}

	// bridge resolves a dependency list, replacing inactive intra-pipeline deps
	// with their own bridged dependencies. The visiting set guards against a
	// cycle through inactive steps (the manifest DAG is acyclic, but failing
	// safe keeps a malformed manifest from looping the planner).
	var bridge func(deps []string, visiting map[string]bool) []string
	bridge = func(deps []string, visiting map[string]bool) []string {
		out := make([]string, 0, len(deps))
		for _, dep := range deps {
			if IsExternalRef(dep) || !inactive[dep] {
				out = append(out, dep)
				continue
			}
			if visiting[dep] {
				continue
			}
			visiting[dep] = true
			if step, ok := byID[dep]; ok {
				out = append(out, bridge(step.DependsOn, visiting)...)
			}
			delete(visiting, dep)
		}
		return out
	}

	result := make([]PipelineStep, 0, len(steps))
	for _, s := range steps {
		if inactive[s.ID] {
			continue
		}
		if len(s.DependsOn) > 0 {
			s.DependsOn = dedupePreserveOrder(bridge(s.DependsOn, map[string]bool{}))
		}
		result = append(result, s)
	}
	return result
}

// dedupePreserveOrder returns deps with duplicates removed, keeping first
// occurrence order so bridged dependency lists stay stable for tests and
// diagnostics.
func dedupePreserveOrder(deps []string) []string {
	if len(deps) <= 1 {
		return deps
	}
	seen := make(map[string]bool, len(deps))
	out := deps[:0]
	for _, d := range deps {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
