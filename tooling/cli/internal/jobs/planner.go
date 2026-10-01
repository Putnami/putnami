// Package jobs handles job planning, scheduling, execution, and caching.
package jobs

import (
	"fmt"
	"sort"

	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Plan computes the full execution plan for the given commands and projects.
// It resolves extensions, expands pipelines, and builds the dependency graph.
func Plan(
	ws *workspace.Workspace,
	commands []string,
	projects []*workspace.Project,
	extensions []*extension.ExtensionDescription,
	commandParams map[string]any,
	disabledJobs []string,
	disabledExtensions []string,
) ([]*ScheduledJob, error) {
	jobMap := extension.BuildJobMap(extensions)
	commands = extension.ExpandAlsoRunCommands(commands, jobMap)
	explicitCommands := toSet(commands)
	disabledJobSet := toSet(disabledJobs)
	disabledExtSet := toSet(disabledExtensions)

	// cache is shared by every matchJob (direct and via dependency resolution)
	// so activation-file probes for the same (extension, project) pair are
	// computed once per Plan invocation.
	cache := newPlanCache()
	recordEffectiveCommand := func(key, command string, params extension.ParamMap) {
		if cache.effectiveCommands[key] == nil {
			cache.effectiveCommands[key] = make(map[string]bool)
		}
		cache.effectiveCommands[key][command] = true
		if cache.effectiveCommandParams[key] == nil {
			cache.effectiveCommandParams[key] = make(map[string]extension.ParamMap)
		}
		cache.effectiveCommandParams[key][command] = params
	}

	// Resolve command availability and params before expanding any pipeline so
	// conditions are independent of command order. Scope the facts by provider
	// and selected project: a disabled or unmatched test, another provider's
	// test, and an unselected dependency are not replacement evidence.
	workspaceProject := workspaceOnceProject(ws)
	for _, cmdName := range commands {
		for _, jobDef := range matchWorkspaceOnceJobs(cmdName, jobMap, disabledJobSet, disabledExtSet) {
			ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
			if ext == nil {
				continue
			}
			key := workspaceProject.ID + "\x00" + jobDef.ExtensionName
			params := mergeCommandParams(ws, workspaceProject, ext, cmdName, jobDef, commandParams)
			recordEffectiveCommand(key, cmdName, params)
		}
		for _, proj := range projects {
			jobDefs := matchJobs(cmdName, proj, jobMap, disabledJobSet, disabledExtSet, ws.Root, cache)
			for _, jobDef := range excludeWorkspaceOnceJobs(jobDefs) {
				ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
				if ext == nil {
					continue
				}
				key := proj.ID + "\x00" + jobDef.ExtensionName
				params := mergeCommandParams(ws, proj, ext, cmdName, jobDef, commandParams)
				recordEffectiveCommand(key, cmdName, params)
			}
		}
	}

	var allJobs []*ScheduledJob

	for _, cmdName := range commands {
		workspaceJobs := matchWorkspaceOnceJobs(cmdName, jobMap, disabledJobSet, disabledExtSet)
		if err := extension.ValidateCommandFlags(cmdName, workspaceJobs); err != nil {
			return nil, err
		}
		for _, jobDef := range workspaceJobs {
			ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
			if ext == nil {
				continue
			}
			proj := workspaceOnceProject(ws)
			planned, err := expandJob(ws, proj, ext, cmdName, jobDef, commandParams, len(workspaceJobs) > 1, cache)
			if err != nil {
				return nil, err
			}
			attachSelectedProjects(planned, projects)
			allJobs = append(allJobs, planned...)
		}

		for _, proj := range projects {
			// Find all matching extension jobs for this project/command.
			jobDefs := matchJobs(cmdName, proj, jobMap, disabledJobSet, disabledExtSet, ws.Root, cache)
			jobDefs = excludeWorkspaceOnceJobs(jobDefs)
			if len(jobDefs) == 0 {
				continue
			}
			if err := extension.ValidateCommandFlags(cmdName, jobDefs); err != nil {
				return nil, err
			}

			namespace := len(jobDefs) > 1
			for _, jobDef := range jobDefs {
				ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
				if ext == nil {
					continue
				}

				planned, err := expandJob(ws, proj, ext, cmdName, jobDef, commandParams, namespace, cache)
				if err != nil {
					return nil, err
				}
				allJobs = append(allJobs, planned...)
			}
		}
	}

	// Expand implicit jobs to a fixpoint. ^ references can introduce upstream
	// project steps; command dependencies can introduce prerequisite commands;
	// session barriers (!command) can introduce jobs across the selected scope.
	// Newly-added jobs may themselves carry any of those relationships.
	for {
		countBefore := len(allJobs)
		prerequisiteChanged := false

		if err := emitUpstreamJobs(&allJobs, ws, jobMap, disabledJobSet, disabledExtSet, extensions, commandParams, cache); err != nil {
			return nil, err
		}
		if err := emitCommandDeps(&allJobs, ws, jobMap, disabledJobSet, disabledExtSet, extensions, commandParams, cache); err != nil {
			return nil, err
		}
		if err := emitSessionCommandDeps(&allJobs, ws, projects, jobMap, disabledJobSet, disabledExtSet, extensions, commandParams, cache); err != nil {
			return nil, err
		}
		var err error
		prerequisiteChanged, err = emitSessionPrerequisites(&allJobs, ws, projects, explicitCommands, jobMap, disabledJobSet, disabledExtSet, extensions, commandParams, cache)
		if err != nil {
			return nil, err
		}

		if len(allJobs) == countBefore && !prerequisiteChanged {
			break
		}
	}
	if err := resolveSessionPrerequisites(allJobs, ws, projects, commandParams); err != nil {
		return nil, err
	}

	// Resolve cross-project dependencies (^, /, *)
	if err := resolveExternalDeps(allJobs, ws); err != nil {
		return nil, err
	}
	serializeWriteResources(allJobs)
	orderContractClients(allJobs, ws)

	if err := validatePlanDAG(allJobs); err != nil {
		return nil, err
	}
	if err := validatePlanContract(allJobs, ws); err != nil {
		return nil, err
	}
	if err := validateBatchProjectLimits(allJobs, ws, commandParams); err != nil {
		return nil, err
	}
	if err := validatePlanProducers(allJobs); err != nil {
		return nil, err
	}

	// Resolve the plan's finalizes relations before identities are stamped. It
	// attaches each consumer's producer, which is a cache-key input, so a key
	// computed outside a scheduler run — PrecomputeKeys for remote
	// negotiation, `putnami plan` — sees the same value execution does.
	// It also rejects a plan that puts one step in two relations' frontiers,
	// which has no execution: a step receives exactly one invocation locator.
	if err := validateInvocationFrontiers(allJobs); err != nil {
		return nil, err
	}

	// Group the content-identical nodes several commands scheduled over one
	// manifest task so the scheduler runs each group once. It adds no edge
	// and moves no key — see plan_shared.go — so it is safe here, after the
	// graph is final and before identities are stamped.
	attachSharedExecutions(ws, allJobs, commandParams)

	attachIdentities(allJobs)
	return allJobs, nil
}

func attachSelectedProjects(planned []*ScheduledJob, projects []*workspace.Project) {
	if len(planned) == 0 || len(projects) == 0 {
		return
	}
	selected := append([]*workspace.Project(nil), projects...)
	for _, job := range planned {
		job.SelectedProjects = selected
	}
}

// ResolvedRunSelection builds the job context contract's `selection` member
// from the facts the run's selection stage resolved: the mode it ran in, the
// baseline `--impacted` measured against and the tier that produced it, and the
// projects it finally selected.
//
// Project ids are sorted here, once, because the contract requires it and
// because two runs over one workspace must produce byte-identical documents.
// `scoped` is derived rather than passed: a narrowed run is scoped and the
// whole-workspace projection is not, and a caller that could disagree with that
// would be the second definition of "narrowed".
func ResolvedRunSelection(
	mode, baseline, baselineSource string,
	projects []*workspace.Project,
) *protocoljob.Selection {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		if project != nil {
			ids = append(ids, project.ID)
		}
	}
	sort.Strings(ids)
	return &protocoljob.Selection{
		Mode:           mode,
		Scoped:         mode != protocoljob.SelectionModeAll,
		Baseline:       baseline,
		BaselineSource: baselineSource,
		ProjectIDs:     ids,
	}
}

// AttachRunSelection stamps the run's resolved selection onto every planned
// node.
//
// EVERY node, unlike attachSelectedProjects above, which stamps only the ones
// that run once for the workspace: a project-scoped task reporting on the
// workspace has to know the run was narrowed just as much as a workspace-scoped
// one does. It runs after the plan is final because the selection is a fact
// about the RUN, not about the node — identical on all of them, so threading it
// through every construction site would buy nothing.
//
// Nothing stamped here reaches a cache key: the key builder reads named job
// fields, and this is not one of them (TestCacheKeyIgnoresTheRunSelection).
func AttachRunSelection(planned []*ScheduledJob, selection *protocoljob.Selection) {
	if selection == nil {
		return
	}
	for _, job := range planned {
		if job != nil {
			job.Selection = selection
		}
	}
}

func expandJob(
	ws *workspace.Workspace,
	proj *workspace.Project,
	ext *extension.ExtensionDescription,
	cmdName string,
	jobDef *extension.JobDefinition,
	commandParams map[string]any,
	namespace bool,
	cache *planCache,
) ([]*ScheduledJob, error) {
	if len(jobDef.PipelineSteps) == 0 {
		plannedDef := jobDef
		if namespace {
			plannedDef = cloneJobDefinition(jobDef)
			plannedDef.InternalName = extension.NamespacedCommandJobName(cmdName, ext.Name)
			plannedDef.CommandName = cmdName
		}
		return []*ScheduledJob{{
			Project:   proj,
			Extension: ext,
			JobDef:    plannedDef,
		}}, nil
	}

	// Merge params: workspace config defaults → manifest flag defaults →
	// project options → CLI flags. This matches the TS SDK's evalJobOptions
	// so plan-time `if` conditions see the full resolved params.
	mergedParams := mergeCommandParams(ws, proj, ext, cmdName, jobDef, commandParams)
	mergedParams, _ = projectParamAliases(mergedParams)
	opts := pipelineExpansionOptions(ws, proj, ext, namespace, cache)
	expanded, err := extension.ExpandPipelineWithOptions(cmdName, jobDef, jobDef.PipelineSteps, ext, mergedParams, opts)
	if err != nil {
		return nil, fmt.Errorf("expand pipeline for %s/%s: %w", proj.Name, cmdName, err)
	}

	planned := make([]*ScheduledJob, 0, len(expanded))
	for _, step := range expanded {
		s := step.Step
		planned = append(planned, &ScheduledJob{
			Project:   proj,
			Extension: ext,
			JobDef:    step.JobDef,
			Step:      &s,
		})
	}
	return planned, nil
}

func excludeWorkspaceOnceJobs(jobDefs []*extension.JobDefinition) []*extension.JobDefinition {
	if len(jobDefs) == 0 {
		return nil
	}
	out := make([]*extension.JobDefinition, 0, len(jobDefs))
	for _, jobDef := range jobDefs {
		if jobDef.Activation == workspaceOnceActivation {
			continue
		}
		out = append(out, jobDef)
	}
	return out
}

func cloneJobDefinition(jobDef *extension.JobDefinition) *extension.JobDefinition {
	if jobDef == nil {
		return nil
	}
	clone := *jobDef
	return &clone
}

func workspaceOnceProject(ws *workspace.Workspace) *workspace.Project {
	name := ws.Name
	if name == "" {
		name = "workspace"
	}
	return &workspace.Project{
		ID:   name,
		Name: name,
		Path: ".",
	}
}
