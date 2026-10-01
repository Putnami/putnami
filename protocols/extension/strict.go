// Strict parsing and validation for extension manifests.
//
// Contract 3 removed the middle tier. There used to be
// a recoverable class — reserved global-flag shadows in a manifest whose
// declared cliContract predated the current one — that LoadManifest adapted
// away with a warning. It is gone: a manifest either declares the contract this
// build implements, in which case it is enforced strictly, or it does not load
// at all. Adapting a manifest meant interpreting a document written against a
// contract nobody could still check, and the four contracts that move together
// at 3 (task contract v3, job context v2, runtime events v2, lock v2) are not
// expressible as a flag-surface edit.
//
// Strictness still lives at the authoring and packaging surfaces too:
// FullValidateManifest / `putnami extensions validate` and the package-time
// gate stay maximally strict, so a non-conforming manifest never earns a
// cliContract stamp or reaches a registry.

package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// ParseManifest decodes JSON data into a Manifest using strict mode.
// Unknown fields are rejected and structured diagnostics are returned.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse extension manifest: %v", err),
		}
	}
	return &m, nil
}

// ValidateManifest checks structural invariants on a parsed manifest.
func ValidateManifest(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-manifest", "", "manifest is nil"),
		}
	}

	var diags []diag.Diagnostic

	// A content-only extension is a complete manifest: it contributes agent
	// instructions and runs nothing. An empty contribution still fails, because
	// ValidateAgentContent rejects a section that names neither a source nor a
	// digest-bound tree, so an empty section can never stand in for a
	// contribution.
	if len(m.Commands) == 0 && len(m.Tools) == 0 && !m.DeclaresAgentContent() {
		diags = append(diags, diag.Errorf("required-field", "commands", "at least one command, MCP tool or agent-content contribution is required"))
	}
	diags = append(diags, ValidateNoReservedFlagShadows(m)...)

	// Validate commands in sorted order for deterministic diagnostics.
	cmdNames := sortedKeys(m.Commands)
	for _, name := range cmdNames {
		cmd := m.Commands[name]
		for i, dep := range cmd.DependsOn {
			target := strings.TrimPrefix(dep, "!")
			if isExternalRef(target) {
				diags = append(diags, diag.Errorf(
					"invalid-command-dependency",
					fmt.Sprintf("commands.%s.dependsOn[%d]", name, i),
					"dependency %q uses pipeline-step syntax; command-level dependsOn accepts bare command names or !command session barriers",
					dep,
				))
			}
		}
		seenCompanions := make(map[string]int, len(cmd.AlsoRuns))
		for i, companion := range cmd.AlsoRuns {
			field := fmt.Sprintf("commands.%s.alsoRuns[%d]", name, i)
			switch {
			case companion == "" || strings.HasPrefix(companion, "!") || isExternalRef(companion):
				diags = append(diags, diag.Errorf("invalid-also-runs", field, "companion %q must be a bare command name", companion))
			case companion == name:
				diags = append(diags, diag.Errorf("cyclic-also-runs", field, "command %q cannot also-run itself", name))
			default:
				if _, exists := m.Commands[companion]; !exists {
					diags = append(diags, diag.Errorf("unknown-also-runs-target", field, "companion %q is not a command in this manifest", companion))
				}
				if previous, duplicate := seenCompanions[companion]; duplicate {
					diags = append(diags, diag.Errorf("duplicate-also-runs", field, "companion %q is repeated (also alsoRuns[%d])", companion, previous))
				} else {
					seenCompanions[companion] = i
				}
			}
		}
		for i, prerequisite := range cmd.SessionPrerequisites {
			field := fmt.Sprintf("commands.%s.sessionPrerequisites[%d]", name, i)
			if !ValidateExpressionSyntax(prerequisite.If) {
				diags = append(diags, diag.Errorf("invalid-session-prerequisite-expression", field+".if", "invalid session prerequisite expression %q", prerequisite.If))
			}
			if !ValidateExpressionSyntax(prerequisite.ProjectIf) {
				diags = append(diags, diag.Errorf("invalid-session-prerequisite-expression", field+".projectIf", "invalid session prerequisite expression %q", prerequisite.ProjectIf))
			}
			if prerequisite.Command == "" {
				diags = append(diags, diag.Errorf("required-field", field+".command", "prerequisite command is required"))
			} else if strings.HasPrefix(prerequisite.Command, "!") || isExternalRef(prerequisite.Command) {
				diags = append(diags, diag.Errorf("invalid-session-prerequisite", field+".command", "command %q must be a bare command name", prerequisite.Command))
			} else if prerequisite.Command == name {
				diags = append(diags, diag.Errorf("cyclic-session-prerequisite", field+".command", "command %q cannot require itself", name))
			}
			if prerequisite.ProjectsFromParam != "" {
				flag, ok := cmd.Flags[prerequisite.ProjectsFromParam]
				if !ok {
					diags = append(diags, diag.Errorf("unknown-session-prerequisite-selector", field+".projectsFromParam", "owning command has no %q flag", prerequisite.ProjectsFromParam))
				} else if flag.Type != "string" {
					diags = append(diags, diag.Errorf("invalid-session-prerequisite-selector", field+".projectsFromParam", "flag %q must have type string", prerequisite.ProjectsFromParam))
				}
			}
			for j, gate := range prerequisite.DependsOn {
				gateField := fmt.Sprintf("%s.dependsOn[%d]", field, j)
				if gate == "" || strings.HasPrefix(gate, "!") || isExternalRef(gate) {
					diags = append(diags, diag.Errorf("invalid-session-prerequisite", gateField, "gate %q must be a bare command name", gate))
				} else if gate == prerequisite.Command || gate == name {
					diags = append(diags, diag.Errorf("cyclic-session-prerequisite", gateField, "gate %q would create a prerequisite cycle", gate))
				}
			}
			paramNames := make([]string, 0, len(prerequisite.Params))
			for paramName := range prerequisite.Params {
				paramNames = append(paramNames, paramName)
			}
			sort.Strings(paramNames)
			for _, paramName := range paramNames {
				binding := prerequisite.Params[paramName]
				bindingField := field + ".params." + paramName
				if paramName == "" {
					diags = append(diags, diag.Errorf("required-field", field+".params", "prerequisite parameter name is required"))
				}
				hasValue := binding.Value != nil
				hasProjectParam := binding.FromProjectParam != ""
				if hasValue == hasProjectParam {
					diags = append(diags, diag.Errorf("invalid-session-prerequisite-param", bindingField, "exactly one of value or fromProjectParam is required"))
				}
			}
		}
		if len(cmd.Run) == 0 {
			diags = append(diags, diag.Errorf("empty-pipeline", fmt.Sprintf("commands.%s.run", name), "command must have at least one pipeline step"))
		}
		diags = append(diags, validateReservedCacheCommand(name, cmd, m.Tasks)...)
		finalizers := make(map[string]bool, len(cmd.Run))
		stepsByID := make(map[string]PipelineStep, len(cmd.Run))
		for _, step := range cmd.Run {
			if step.ID != "" {
				stepsByID[step.ID] = step
			}
			if step.IsFinalizer() && step.ID != "" {
				finalizers[step.ID] = true
			}
		}
		claimed := finalizerClaims{
			producers: make(map[string]string),
			consumers: make(map[string]string),
		}
		for i, step := range cmd.Run {
			field := fmt.Sprintf("commands.%s.run[%d]", name, i)
			if step.ID == "" {
				diags = append(diags, diag.Errorf("required-field", field+".id", "pipeline step id is required"))
			}
			if step.Task == "" {
				diags = append(diags, diag.Errorf("required-field", field+".task", "pipeline step task is required"))
			}
			// Verify referenced task exists.
			if step.Task != "" && m.Tasks != nil {
				if _, ok := m.Tasks[step.Task]; !ok {
					diags = append(diags, diag.Warningf("unresolved-task", field+".task", "task %q is not defined in this manifest", step.Task))
				}
			}
			if step.RunOn != "" && !IsValidStepRunOn(step.RunOn) {
				diags = append(diags, diag.Errorf("invalid-enum", field+".runOn",
					"invalid step run condition %q; must be one of: %s",
					step.RunOn, strings.Join(ValidStepRunOn, ", ")))
			}
			if step.IsFinalizer() {
				if step.Finalizes == nil {
					diags = append(diags, diag.Errorf("required-field", field+".finalizes",
						"a runOn finally step must declare the producer it cleans up and its complete consumer frontier"))
				} else {
					diags = append(diags, validateFinalizerRelation(
						field, step, stepsByID, finalizers, claimed, m.Tasks)...)
				}
				if len(step.DependsOn) != 0 {
					diags = append(diags, diag.Errorf("finalizer-dependency-conflict", field+".dependsOn",
						"a finalizer's trigger and completion frontier come from finalizes; ordinary dependsOn edges are not allowed"))
				}
				for _, bindingName := range sortedKeys(step.With) {
					if step.With[bindingName].FromStep == "" {
						continue
					}
					diags = append(diags, diag.Errorf("finalizer-result-input",
						fmt.Sprintf("%s.with.%s.fromStep", field, bindingName),
						"a finalizer must locate invocation artifacts through {invocationArtifactRoot}, not a step result"))
				}
				if task, ok := m.Tasks[step.Task]; ok && len(task.Outputs) != 0 {
					diags = append(diags, diag.Errorf("finalizer-export", "tasks."+step.Task+".outputs",
						"task %q is used as a finalizer and must not export result ports", step.Task))
				}
			} else if step.Finalizes != nil {
				diags = append(diags, diag.Errorf("finalizer-run-condition", field+".finalizes",
					"finalizes is valid only on a step with runOn %q", StepRunOnFinally))
			}
			// A finalizer runs whatever happened to the work it tears down, so
			// it has no result another step could depend on. Binding one is a
			// data-flow edge the scheduler can never honor.
			for _, bindingName := range sortedKeys(step.With) {
				binding := step.With[bindingName]
				if binding.FromStep == "" || !finalizers[binding.FromStep] {
					continue
				}
				diags = append(diags, diag.Errorf("finalizer-binding",
					fmt.Sprintf("%s.with.%s.fromStep", field, bindingName),
					"step binds input %q from %q, which runs with runOn %q; a finalizer runs regardless of "+
						"outcome and produces no result a dependent can consume",
					bindingName, binding.FromStep, StepRunOnFinally))
			}
		}
		for _, outputName := range sortedKeys(cmd.Outputs) {
			output := cmd.Outputs[outputName]
			if output.FromStep == "" || !finalizers[output.FromStep] {
				continue
			}
			diags = append(diags, diag.Errorf("finalizer-export",
				fmt.Sprintf("commands.%s.outputs.%s.fromStep", name, outputName),
				"command output %q exports a result from finalizer %q; finalizers do not publish results",
				outputName, output.FromStep))
		}
	}

	// Validate command groups in sorted order for deterministic diagnostics.
	groupNames := sortedKeys(m.CommandGroups)
	for _, groupName := range groupNames {
		group := m.CommandGroups[groupName]
		groupField := fmt.Sprintf("commandGroups.%s", groupName)
		if len(group.Subcommands) == 0 {
			diags = append(diags, diag.Errorf("required-field", groupField+".subcommands", "command group must have at least one subcommand"))
			continue
		}
		diags = append(diags, validateSubcommands(m, group.Subcommands, groupField+".subcommands", "")...)
		if group.Default != "" {
			if _, ok := group.Subcommands[group.Default]; !ok {
				diags = append(diags, diag.Errorf("unresolved-default-subcommand", groupField+".default",
					"command group default %q names no subcommand of the group; declare one of: %s",
					group.Default, strings.Join(sortedKeys(group.Subcommands), ", ")))
			}
		}
	}

	// Validate tasks in sorted order for deterministic diagnostics.
	taskNames := sortedKeys(m.Tasks)
	for _, name := range taskNames {
		task := m.Tasks[name]
		field := fmt.Sprintf("tasks.%s", name)
		if task.Kind == "" {
			diags = append(diags, diag.Errorf("required-field", field+".kind", "task kind is required"))
		}
		if task.Command == "" {
			diags = append(diags, diag.Errorf("required-field", field+".command", "task command is required"))
		}
		if task.Batchable != nil {
			if strings.TrimSpace(task.Batchable.Tool) == "" {
				diags = append(diags, diag.Errorf("required-field", field+".batchable.tool", "batchable tool is required"))
			}
			if task.Batchable.MaxProjects != 0 && task.Batchable.MaxProjects < 2 {
				diags = append(diags, diag.Errorf(
					"invalid-value",
					field+".batchable.maxProjects",
					"batchable maxProjects must be at least 2 when set",
				))
			}
			diags = append(diags, validateBatchProjectsParam(m, name, task, field)...)
			if task.Batchable.MaxWorkers < 0 {
				diags = append(diags, diag.Errorf(
					"invalid-value",
					field+".batchable.maxWorkers",
					"batchable maxWorkers must be at least 1 when set",
				))
			}
			for i, candidate := range task.Batchable.ConfigFiles {
				if strings.TrimSpace(candidate) == "" {
					diags = append(diags, diag.Errorf(
						"invalid-value",
						fmt.Sprintf("%s.batchable.configFiles[%d]", field, i),
						"batchable config candidate cannot be empty",
					))
				}
			}
		}
		inputNames := sortedKeys(task.Inputs)
		for _, inputName := range inputNames {
			port := task.Inputs[inputName]
			if port.From == "" {
				diags = append(diags, diag.Errorf("required-field", fmt.Sprintf("%s.inputs.%s.from", field, inputName), "input port from is required"))
			} else {
				validFrom := map[string]bool{
					TaskInputFromProject:   true,
					TaskInputFromWorkspace: true,
					TaskInputFromClosure:   true,
					TaskInputFromTask:      true,
					TaskInputFromParams:    true,
					TaskInputFromEnv:       true,
					TaskInputFromRuntime:   true,
				}
				if !validFrom[port.From] {
					diags = append(diags, diag.Errorf("invalid-enum", fmt.Sprintf("%s.inputs.%s.from", field, inputName),
						"invalid input source %q; must be one of: project, workspace, closure, task, params, env, runtime", port.From))
				}
			}
		}
		diags = append(diags, validateResourceRefs(field+".writes", task.Writes)...)
		diags = append(diags, validateResourceRefs(field+".reads", task.Reads)...)
		diags = append(diags, validateResourceClaims(field+".resources", task.Resources)...)
	}

	// v3 task contract, through the same harness a conformance fixture and the
	// extension SDK's authoring gate run: exact paths, one owner per output,
	// honest effects. Returns nothing for a manifest whose tasks carry no
	// `declares` block, so v2 manifests keep their exact verdict.
	diags = append(diags, ValidateTaskContracts(m)...)

	// The two manifest-level lifecycle sections, each inert for a manifest that
	// does not declare it, in a fixed order so the diagnostics are stable.
	diags = append(diags, ValidateRuntime(m)...)
	diags = append(diags, ValidateWorkspaceAdapter(m)...)

	// Ecosystem profiles (ecosystem.go). Inert for a manifest that declares
	// neither `ecosystems` nor `uses`, so every manifest written before profiles
	// existed keeps its exact verdict. Cross-manifest resolution — one owner per
	// id, no use of an undeclared id — belongs to ResolveProfiles, which sees
	// every installed extension; this half is what one file can decide alone.
	diags = append(diags, validateEcosystems(m)...)

	// The agent-content section (agent_content.go). Inert for a manifest that
	// declares none.
	diags = append(diags, ValidateAgentContent(m)...)

	// Validate MCP tool declarations in sorted order for deterministic
	// diagnostics. A malformed tool must fail package-time validation instead
	// of becoming a surprising agent-facing capability at runtime.
	toolNames := sortedKeys(m.Tools)
	for _, name := range toolNames {
		diags = append(diags, validateToolDefinition(name, m.Tools[name])...)
	}

	return diags
}

// validateReservedCacheCommand checks the shape of the reserved cache-lifecycle
// commands (cache.go).
//
// Core's `cache clean`/`cache gc` fan-out only recognizes an extension when the
// reserved command resolved to a runnable SINGLE-step job whose task exists. A
// manifest that declares `cache-clean` with two steps, or a step referencing an
// undefined task, would pass a shape-blind validation and the package gate, and
// then be silently never asked — the extension believes it collects while core
// never calls. That silent-no-collection state is exactly what removing the old
// hook vocabulary was meant to make impossible, so the shape is validated where
// every other manifest invariant is.
//
// The task must also have caching disabled: a cache-collection result restored
// from cache would report bytes it never freed, and the mutation of
// machine-global state is unreproducible by definition.
func validateReservedCacheCommand(name string, cmd CommandDefinition, tasks map[string]TaskDefinition) []diag.Diagnostic {
	if !IsCacheCommand(name) {
		return nil
	}
	var diags []diag.Diagnostic
	field := "commands." + name
	if len(cmd.Run) != 1 {
		return append(diags, diag.Errorf("invalid-cache-command", field+".run",
			"reserved cache command %q must run exactly one pipeline step, got %d: core dispatches it as a single typed task",
			name, len(cmd.Run)))
	}
	taskName := cmd.Run[0].Task
	if taskName == "" {
		return diags // required-field is already reported by the step validation
	}
	task, ok := tasks[taskName]
	if !ok {
		// The generic step validation only WARNS on an unresolved task; for a
		// reserved cache command that warning is a silent no-collection state,
		// so it is an error here.
		return append(diags, diag.Errorf("invalid-cache-command", field+".run[0].task",
			"reserved cache command %q references task %q, which is not defined in this manifest; core would silently never invoke it",
			name, taskName))
	}
	if task.Cache.IsEnabled() {
		diags = append(diags, diag.Errorf("invalid-cache-command", field+".run[0].task",
			"reserved cache command %q must run a task with caching disabled (`\"cache\": false`): a restored collection result frees nothing",
			name))
	}
	return diags
}

// finalizerClaims records which finalizer already claimed each producer and
// each consumer inside ONE command's pipeline, so a second relation reaching
// for the same step is reported instead of overwriting the first.
//
// Both halves are exclusive for the same reason: a step participates in at most
// one invocation. Core resolves ONE invocation locator per step and one
// blocking/guard state per relation, so a step named by two relations would get
// one relation's private artifact tree while the other relation's confinement
// and setup-failure blocking silently applied to nothing.
type finalizerClaims struct {
	producers map[string]string
	consumers map[string]string
}

// validateFinalizerRelation checks the explicit producer/frontier relation.
// It does not add DAG edges: producer start is the trigger, and every listed
// consumer reaching a terminal state is the cleanup frontier.
func validateFinalizerRelation(
	field string,
	finalizer PipelineStep,
	stepsByID map[string]PipelineStep,
	finalizers map[string]bool,
	claimed finalizerClaims,
	tasks map[string]TaskDefinition,
) []diag.Diagnostic {
	relation := finalizer.Finalizes
	if relation == nil {
		return nil
	}

	var diags []diag.Diagnostic
	producerField := field + ".finalizes.producer"
	producer := strings.TrimSpace(relation.Producer)
	switch {
	case producer == "":
		diags = append(diags, diag.Errorf("required-field", producerField,
			"finalizer producer is required"))
	case producer == finalizer.ID:
		diags = append(diags, diag.Errorf("invalid-finalizer-relation", producerField,
			"a finalizer cannot name itself as its producer"))
	case finalizers[producer]:
		diags = append(diags, diag.Errorf("invalid-finalizer-relation", producerField,
			"producer %q is itself a finalizer", producer))
	case stepsByID[producer].ID == "":
		diags = append(diags, diag.Errorf("unresolved-finalizer-step", producerField,
			"producer step %q is not defined in this command", producer))
	default:
		producerTask := tasks[stepsByID[producer].Task]
		hasInvocationOutput := false
		if producerTask.Declares != nil {
			for _, output := range producerTask.Declares.Outputs {
				if output.EffectiveScope() == OutputScopeInvocation {
					hasInvocationOutput = true
					break
				}
			}
		}
		if !hasInvocationOutput {
			diags = append(diags, diag.Errorf("finalizer-producer-without-invocation-output", producerField,
				"producer %q must use a task that declares at least one invocation-scoped output", producer))
		}
		if existing := claimed.producers[producer]; existing != "" {
			diags = append(diags, diag.Errorf("duplicate-finalizer", producerField,
				"producer %q is already finalized by %q; exactly one finalizer may be armed", producer, existing))
		} else {
			claimed.producers[producer] = finalizer.ID
		}
	}

	if len(relation.Consumers) == 0 {
		diags = append(diags, diag.Errorf("required-field", field+".finalizes.consumers",
			"finalizer consumers must name the complete terminal frontier"))
		return diags
	}
	seen := make(map[string]bool, len(relation.Consumers))
	for i, authored := range relation.Consumers {
		consumerField := fmt.Sprintf("%s.finalizes.consumers[%d]", field, i)
		consumer := strings.TrimSpace(authored)
		switch {
		case consumer == "":
			diags = append(diags, diag.Errorf("required-field", consumerField,
				"finalizer consumer is required"))
		case seen[consumer]:
			diags = append(diags, diag.Errorf("duplicate-finalizer-consumer", consumerField,
				"consumer %q is listed more than once", consumer))
		case consumer == producer:
			diags = append(diags, diag.Errorf("invalid-finalizer-relation", consumerField,
				"producer %q cannot also be its own consumer frontier", producer))
		case consumer == finalizer.ID || finalizers[consumer]:
			diags = append(diags, diag.Errorf("invalid-finalizer-relation", consumerField,
				"consumer %q cannot be a finalizer", consumer))
		case stepsByID[consumer].ID == "":
			diags = append(diags, diag.Errorf("unresolved-finalizer-step", consumerField,
				"consumer step %q is not defined in this command", consumer))
		case claimed.consumers[consumer] != "":
			diags = append(diags, diag.Errorf("shared-finalizer-consumer", consumerField,
				"consumer %q is already in the frontier of finalizer %q and cannot also be in %q's; "+
					"a step belongs to at most one finalizes relation, because core resolves one "+
					"invocation locator per step and would hand it one relation's private artifact "+
					"tree while the other relation's confinement and blocking applied to nothing",
				consumer, claimed.consumers[consumer], finalizer.ID))
		case producer != "" && stepsByID[producer].ID != "" &&
			!stepDependsTransitively(stepsByID, consumer, producer):
			diags = append(diags, diag.Errorf("invalid-finalizer-frontier", consumerField,
				"consumer %q must be downstream of producer %q", consumer, producer))
		}
		if consumer != "" {
			seen[consumer] = true
			if claimed.consumers[consumer] == "" {
				claimed.consumers[consumer] = finalizer.ID
			}
		}
	}
	return diags
}

func stepDependsTransitively(stepsByID map[string]PipelineStep, stepID, dependencyID string) bool {
	seen := make(map[string]bool, len(stepsByID))
	var visit func(string) bool
	visit = func(current string) bool {
		if seen[current] {
			return false
		}
		seen[current] = true
		for _, dependency := range stepsByID[current].DependsOn {
			if isExternalRef(dependency) {
				continue
			}
			if dependency == dependencyID || visit(dependency) {
				return true
			}
		}
		return false
	}
	return visit(stepID)
}

func validateToolDefinition(name string, tool ToolDefinition) []diag.Diagnostic {
	field := "tools." + name
	var diags []diag.Diagnostic
	if !isNamespacedToolName(name) {
		diags = append(diags, diag.Errorf("invalid-tool-name", field,
			"tool name %q must be namespaced (for example, putnami.search)", name))
	}
	if strings.TrimSpace(tool.Description) == "" {
		diags = append(diags, diag.Errorf("required-field", field+".description", "tool description is required"))
	}
	if strings.TrimSpace(tool.Command) == "" {
		diags = append(diags, diag.Errorf("required-field", field+".command", "tool command is required"))
	}
	if tool.TimeoutMs < 0 {
		diags = append(diags, diag.Errorf("invalid-value", field+".timeoutMs", "tool timeoutMs cannot be negative"))
	}

	var schema map[string]any
	if len(tool.InputSchema) == 0 {
		diags = append(diags, diag.Errorf("required-field", field+".inputSchema", "tool inputSchema is required"))
	} else if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema == nil {
		if err != nil {
			diags = append(diags, diag.Errorf("invalid-schema", field+".inputSchema", "tool inputSchema must be a JSON object: %v", err))
		} else {
			diags = append(diags, diag.Errorf("invalid-schema", field+".inputSchema", "tool inputSchema must be a JSON object"))
		}
	}

	if tool.Annotations == nil {
		diags = append(diags, diag.Errorf("required-field", field+".annotations", "tool annotations are required"))
	} else {
		for _, annotation := range []struct {
			name  string
			value *bool
		}{
			{"readOnlyHint", tool.Annotations.ReadOnlyHint},
			{"destructiveHint", tool.Annotations.DestructiveHint},
			{"idempotentHint", tool.Annotations.IdempotentHint},
			{"openWorldHint", tool.Annotations.OpenWorldHint},
		} {
			if annotation.value == nil {
				diags = append(diags, diag.Errorf("required-field", field+".annotations."+annotation.name,
					"tool %s is required", annotation.name))
			}
		}
	}

	contract, ok := tool.Meta["putnami.dev/contract"].(map[string]any)
	if !ok {
		diags = append(diags, diag.Errorf("required-field", field+"._meta.putnami.dev/contract",
			"tool contract metadata is required"))
		return diags
	}
	access, accessOK := contract["access"].(string)
	readOnly, readOnlyOK := contract["readOnly"].(bool)
	if !accessOK || (access != "read" && access != "mutating") {
		diags = append(diags, diag.Errorf("invalid-enum", field+"._meta.putnami.dev/contract.access",
			"contract access must be read or mutating"))
	}
	if !readOnlyOK {
		diags = append(diags, diag.Errorf("required-field", field+"._meta.putnami.dev/contract.readOnly",
			"contract readOnly is required"))
	} else if accessOK && readOnly != (access == "read") {
		diags = append(diags, diag.Errorf("invalid-value", field+"._meta.putnami.dev/contract.readOnly",
			"contract readOnly must agree with contract access"))
	}
	if _, ok := contract["supportsDryRun"].(bool); !ok {
		diags = append(diags, diag.Errorf("required-field", field+"._meta.putnami.dev/contract.supportsDryRun",
			"contract supportsDryRun is required"))
	}
	if tool.Annotations != nil && tool.Annotations.ReadOnlyHint != nil && readOnlyOK && *tool.Annotations.ReadOnlyHint != readOnly {
		diags = append(diags, diag.Errorf("invalid-value", field+".annotations.readOnlyHint",
			"readOnlyHint must agree with contract readOnly"))
	}
	return diags
}

func isNamespacedToolName(name string) bool {
	if strings.Count(name, ".") == 0 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// ValidateNoReservedFlagShadows checks only the manifest flag surfaces for
// collisions with globally reserved CLI flags. It is intentionally narrower than
// ValidateManifest so normal extension loading can enforce the reserved-name
// contract without rejecting older manifests for unrelated structural issues.
func ValidateNoReservedFlagShadows(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	var diags []diag.Diagnostic

	cmdNames := sortedKeys(m.Commands)
	for _, name := range cmdNames {
		cmd := m.Commands[name]
		diags = append(diags, validateFlagDefinitions(fmt.Sprintf("commands.%s.flags", name), cmd.Flags)...)
	}

	groupNames := sortedKeys(m.CommandGroups)
	for _, groupName := range groupNames {
		group := m.CommandGroups[groupName]
		groupField := fmt.Sprintf("commandGroups.%s", groupName)
		diags = append(diags, validateFlagDefinitions(groupField+".flags", group.Flags)...)
		diags = append(diags, validateSubcommandFlagDefinitions(group.Subcommands, groupField+".subcommands")...)
	}

	return diags
}

// validateResourceRefs checks that each write/read resource declares a
// non-empty id and a recognized scope.
func validateResourceRefs(field string, refs []ResourceRef) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, ref := range refs {
		entry := fmt.Sprintf("%s[%d]", field, i)
		if ref.ID == "" {
			diags = append(diags, diag.Errorf("required-field", entry+".id", "resource id is required"))
		}
		switch ref.Scope {
		case "", ResourceScopeProject, ResourceScopeWorkspace:
		default:
			diags = append(diags, diag.Errorf("invalid-enum", entry+".scope",
				"invalid resource scope %q; must be one of: project, workspace", ref.Scope))
		}
	}
	return diags
}

// validateResourceClaims checks a task's named budget claims (TaskDefinition.
// Resources). A claim must name a resource and must be a POSITIVE number of
// units: zero and negative are authoring mistakes rather than a way to say "no
// claim", and admitting them silently would leave a task looking budgeted while
// nothing ever gates it. Names stay opaque — this protocol closes no vocabulary
// of resources, so no name is checked against a list.
func validateResourceClaims(field string, claims map[string]int) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, name := range sortedKeys(claims) {
		if strings.TrimSpace(name) == "" {
			diags = append(diags, diag.Errorf("required-field", field, "resource name is required"))
			continue
		}
		if claims[name] < 1 {
			diags = append(diags, diag.Errorf("invalid-value", fmt.Sprintf("%s.%s", field, name),
				"resource claim must be at least 1 unit, got %d", claims[name]))
		}
	}
	return diags
}

// validateBatchProjectsParam checks the parameter a batch policy reads its
// project cap from. The name must be usable as written, and the task must not
// key its cache on it: the cap only decides how ready jobs share one
// invocation, so keying on it would split the task cache for a value that
// cannot change a result. The CLI keys a task on its declared inputs and cache
// key params, on every flag of a command that runs it, and on its literal step
// bindings, so none of those may carry the name. Kebab and camel spellings
// of one name are the same parameter, because the CLI delivers both.
func validateBatchProjectsParam(m *Manifest, taskName string, task TaskDefinition, field string) []diag.Diagnostic {
	name := task.Batchable.MaxProjectsParam
	if name == "" {
		return nil
	}
	path := field + ".batchable.maxProjectsParam"
	if strings.TrimSpace(name) != name || strings.ContainsAny(name, " \t\r\n") {
		return []diag.Diagnostic{diag.Errorf("invalid-value", path,
			"batchable maxProjectsParam %q must be a parameter name without whitespace", name)}
	}
	keyed := sortedKeys(task.Inputs)
	if task.Cache != nil && task.Cache.Key != nil {
		keyed = append(keyed, task.Cache.Key.Params...)
	}
	for _, other := range keyed {
		if sameParamName(other, name) {
			return []diag.Diagnostic{diag.Errorf("invalid-value", path,
				"batchable maxProjectsParam %q is also a cache input of this task (%q); a batch cap only groups jobs and must never key a task",
				name, other)}
		}
	}
	for _, commandName := range sortedKeys(m.Commands) {
		command := m.Commands[commandName]
		for _, step := range command.Run {
			if step.Task != taskName {
				continue
			}
			for _, flag := range sortedKeys(command.Flags) {
				if sameParamName(flag, name) {
					return []diag.Diagnostic{diag.Errorf("invalid-value", path,
						"batchable maxProjectsParam %q is also a flag of command %q, which runs this task; the CLI keys a task on every flag of its command, and a batch cap must never key a task",
						name, commandName)}
				}
			}
			for _, binding := range sortedKeys(step.With) {
				if sameParamName(binding, name) {
					return []diag.Diagnostic{diag.Errorf("invalid-value", path,
						"batchable maxProjectsParam %q is also bound by step %q of command %q; the CLI keys a task on its literal step bindings, and a batch cap must never key a task",
						name, step.ID, commandName)}
				}
			}
		}
	}
	return nil
}

// sameParamName reports whether two parameter spellings reach a task as one
// parameter. The CLI delivers every name as written and adds a camelCase alias
// for a kebab-case one, so "batch-max-projects" and "batchMaxProjects" are one
// parameter while "BatchMaxProjects" is another.
func sameParamName(a, b string) bool {
	return canonicalParamName(a) == canonicalParamName(b)
}

// canonicalParamName copies the CLI's canonicalParamName and kebabToCamel
// (tooling/cli/internal/jobs/context.go), the rule it applies when it projects
// parameter aliases. This package cannot import the CLI, so the rule is
// copied; keep the two in step.
func canonicalParamName(name string) string {
	if !strings.Contains(name, "-") {
		return name
	}
	parts := strings.Split(name, "-")
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > 0 {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func validateFlagDefinitions(field string, flags map[string]FlagDefinition) []diag.Diagnostic {
	var diags []diag.Diagnostic
	flagNames := sortedKeys(flags)
	for _, name := range flagNames {
		def := flags[name]
		if err := protocolcli.ValidateNoReservedShadow(name); err != nil {
			diags = append(diags, diag.Errorf("reserved-global-flag", fmt.Sprintf("%s.%s", field, name), "%v", err))
		}
		if def.Short != "" {
			if err := protocolcli.ValidateNoReservedShadow(def.Short); err != nil {
				diags = append(diags, diag.Errorf("reserved-global-flag", fmt.Sprintf("%s.%s.short", field, name), "%v", err))
			}
		}
	}
	return diags
}

func validateSubcommands(m *Manifest, subs map[string]SubcommandDefinition, field, inheritedCommand string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	subNames := sortedKeys(subs)
	for _, subName := range subNames {
		sub := subs[subName]
		subField := fmt.Sprintf("%s.%s", field, subName)
		commandName := inheritedCommand
		if sub.Command != "" {
			commandName = sub.Command
			if _, ok := m.Commands[sub.Command]; !ok {
				diags = append(diags, diag.Errorf("unresolved-command", subField+".command",
					"subcommand references undefined command %q", sub.Command))
			}
		}
		if commandName == "" {
			diags = append(diags, diag.Errorf("required-field", subField+".command", "subcommand command is required"))
		}
		switch sub.Workspace {
		case "", SubcommandWorkspaceRequired:
		case SubcommandWorkspaceOptional:
			if !sub.Interactive {
				diags = append(diags, diag.Errorf("invalid-workspace-requirement", subField+".workspace",
					"subcommand workspace %q requires \"interactive\": true; only an interactive subcommand runs outside a workspace",
					SubcommandWorkspaceOptional))
			}
		default:
			diags = append(diags, diag.Errorf("invalid-enum", subField+".workspace",
				"subcommand workspace %q is not one of %q, %q",
				sub.Workspace, SubcommandWorkspaceRequired, SubcommandWorkspaceOptional))
		}
		for i, positional := range sub.Positionals {
			if positional.Name == "" {
				diags = append(diags, diag.Errorf("required-field", fmt.Sprintf("%s.positionals[%d].name", subField, i), "positional name is required"))
			}
		}
		for i, example := range sub.Examples {
			if example.Command == "" {
				diags = append(diags, diag.Errorf("required-field", fmt.Sprintf("%s.examples[%d].command", subField, i), "example command is required"))
			}
		}
		if len(sub.Subcommands) > 0 {
			diags = append(diags, validateSubcommands(m, sub.Subcommands, subField+".subcommands", commandName)...)
		}
	}
	return diags
}

func validateSubcommandFlagDefinitions(subs map[string]SubcommandDefinition, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	subNames := sortedKeys(subs)
	for _, subName := range subNames {
		sub := subs[subName]
		subField := fmt.Sprintf("%s.%s", field, subName)
		diags = append(diags, validateFlagDefinitions(subField+".flags", sub.Flags)...)
		if len(sub.Subcommands) > 0 {
			diags = append(diags, validateSubcommandFlagDefinitions(sub.Subcommands, subField+".subcommands")...)
		}
	}
	return diags
}

// NormalizeManifest applies canonical defaults to a parsed manifest for
// deterministic output. It modifies the manifest in place.
func NormalizeManifest(m *Manifest) {
	if m == nil {
		return
	}

	// Ensure Commands map is initialized.
	if m.Commands == nil {
		m.Commands = make(map[string]CommandDefinition)
	}

	// Ensure Tasks map is initialized.
	if m.Tasks == nil {
		m.Tasks = make(map[string]TaskDefinition)
	}

	// Sort extension dependency keys for deterministic output.
	if m.ExtensionDeps.List == nil {
		m.ExtensionDeps.List = make(map[string]string)
	}

	// Normalize every task: derived cache keys, sorted declared effects,
	// cleaned declared paths. One shared implementation with the task-contract
	// digest (NormalizeTask), so the strict path and TaskContractDigest can
	// never disagree about canonical form.
	for name, task := range m.Tasks {
		NormalizeTask(&task)
		m.Tasks[name] = task
	}

	// The lifecycle sections carry pattern SETS whose authoring order means
	// nothing, and the runtime artifact digest and the workspace snapshot
	// digest are taken over the parsed declaration. Canonicalizing them here is
	// what keeps two spellings of one declaration from becoming two digests.
	NormalizeRuntime(m)
	NormalizeWorkspaceAdapter(m)
}

// ValidatePipelineDAG checks that pipeline steps form an acyclic graph.
// It returns diagnostics for any cycles found.
func ValidatePipelineDAG(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}

	var diags []diag.Diagnostic
	dagCmdNames := sortedKeys(m.Commands)
	for _, cmdName := range dagCmdNames {
		cmd := m.Commands[cmdName]
		if len(cmd.Run) == 0 {
			continue
		}

		stepIdx := make(map[string]int, len(cmd.Run))
		for i, step := range cmd.Run {
			stepIdx[step.ID] = i
		}

		const (
			white = 0
			gray  = 1
			black = 2
		)
		color := make([]int, len(cmd.Run))

		var visit func(i int) bool
		visit = func(i int) bool {
			color[i] = gray
			for _, dep := range cmd.Run[i].DependsOn {
				if isExternalRef(dep) {
					continue
				}
				j, ok := stepIdx[dep]
				if !ok {
					continue
				}
				if color[j] == gray {
					diags = append(diags, diag.Errorf("pipeline-cycle",
						fmt.Sprintf("commands.%s.run", cmdName),
						"pipeline has a cycle: %s → %s", cmd.Run[i].ID, dep))
					return false
				}
				if color[j] == white {
					if !visit(j) {
						return false
					}
				}
			}
			color[i] = black
			return true
		}

		for i := range cmd.Run {
			if color[i] == white {
				if !visit(i) {
					break
				}
			}
		}
	}

	return diags
}

// ValidateSchemaRefs checks that task inputSchemaRef and outputSchemaRef values
// reference schemas defined in the contracts section.
func ValidateSchemaRefs(m *Manifest) []diag.Diagnostic {
	if m == nil || m.Contracts == nil || m.Contracts.Schemas == nil {
		return nil
	}

	var diags []diag.Diagnostic
	schemaTaskNames := sortedKeys(m.Tasks)
	for _, name := range schemaTaskNames {
		task := m.Tasks[name]
		field := fmt.Sprintf("tasks.%s", name)
		if task.InputSchemaRef != "" {
			if _, ok := m.Contracts.Schemas[task.InputSchemaRef]; !ok {
				diags = append(diags, diag.Errorf("unresolved-schema-ref",
					field+".inputSchemaRef",
					"references undefined schema %q", task.InputSchemaRef))
			}
		}
		if task.OutputSchemaRef != "" {
			if _, ok := m.Contracts.Schemas[task.OutputSchemaRef]; !ok {
				diags = append(diags, diag.Errorf("unresolved-schema-ref",
					field+".outputSchemaRef",
					"references undefined schema %q", task.OutputSchemaRef))
			}
		}
	}

	return diags
}

// sortedKeys returns the keys of a map in sorted order for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isExternalRef returns true if the dependency reference is cross-project.
// External refs start with ^ (upstream), / (workspace), or * (all).
func isExternalRef(ref string) bool {
	if len(ref) == 0 {
		return false
	}
	c := ref[0]
	return c == '^' || c == '/' || c == '*'
}

// FullValidateManifest runs all validation phases on a parsed manifest:
// structural validation, the v3 task contract (ValidateTaskContracts, run as
// part of ValidateManifest), pipeline DAG checks, and schema reference checks.
// This is the comprehensive protocol-level validation entry point — the
// authoring surface (`putnami dev extension validate`, the extension SDK's
// build-time gate) and the package-time gate, not the tolerant load path.
func FullValidateManifest(m *Manifest) []diag.Diagnostic {
	diags := ValidateManifest(m)
	if diag.HasErrors(diags) {
		return diags
	}
	diags = append(diags, ValidatePipelineDAG(m)...)
	diags = append(diags, ValidateSchemaRefs(m)...)
	return diags
}

// ParseAndValidateManifest combines strict parsing and validation in one call.
// Returns the manifest (if parsing succeeded), and all diagnostics from both
// parse and validation phases.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	vDiags := ValidateManifest(m)
	diags = append(diags, vDiags...)

	NormalizeManifest(m)

	return m, diags
}

// StrictLoadManifest reads a file and applies strict parse, validate, and
// normalize. This is the recommended entry point for consumers that want
// full contract enforcement.
func StrictLoadManifest(path string, data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		// Prefix diagnostics with the file path for context.
		for i := range diags {
			diags[i].Message = fmt.Sprintf("%s: %s", path, diags[i].Message)
		}
	}
	return m, diags
}
