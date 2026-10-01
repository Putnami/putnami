package jobs

import (
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const sessionCommandDependencyPrefix = "!"

// emitUpstreamJobs iteratively creates pipeline step jobs for upstream
// projects referenced by ^ dependencies. This ensures that when building
// a single project, all its transitive upstream dependencies get the
// necessary pipeline steps (e.g., build~generate) planned.
func emitUpstreamJobs(
	allJobs *[]*ScheduledJob,
	ws *workspace.Workspace,
	jobMap map[string][]*extension.JobDefinition,
	disabledJobs, disabledExts map[string]bool,
	extensions []*extension.ExtensionDescription,
	commandParams map[string]any,
	cache *planCache,
) error {
	// Track which project:jobName combinations exist
	existing := make(map[string]bool)
	for _, j := range *allJobs {
		existing[j.Key()] = true
	}

	// Iterate until no new jobs are added (handles transitive deps)
	for {
		type need struct {
			cmdName string
			stepID  string
			depName string
		}
		var needs []need

		// Build the lookup index once per fixpoint iteration. *allJobs only
		// grows at the end of the iteration, so this snapshot is valid for the
		// whole needs-collection pass and replaces the per-query index rebuild.
		index := buildPlannedIndex(*allJobs)

		for _, job := range *allJobs {
			if job.JobDef.DependsOn == nil {
				continue
			}
			cmdName := job.CommandName()
			for _, dep := range job.JobDef.DependsOn {
				if !strings.HasPrefix(dep, "^") {
					continue
				}
				stepID := dep[1:]
				// Emission is gated on the SAME exact identity that resolution
				// uses. The legacy "<project>:<stepID>" key probe that used to
				// sit beside this one suppressed emission on a job that could
				// never provide the step anyway.
				for _, depName := range ws.Graph.DependenciesOf(job.Project.ID) {
					if len(index.matchingKeys(depName, cmdName, stepID)) > 0 {
						continue
					}
					needs = append(needs, need{cmdName: cmdName, stepID: stepID, depName: depName})
				}
			}
		}

		if len(needs) == 0 {
			break
		}

		added := false
		for _, n := range needs {
			proj := ws.ProjectByID(n.depName)
			if proj == nil {
				continue
			}

			jobDefs := matchJobs(n.cmdName, proj, jobMap, disabledJobs, disabledExts, ws.Root, cache)
			jobDefs = excludeWorkspaceOnceJobs(jobDefs)
			if len(jobDefs) == 0 {
				continue
			}
			if err := extension.ValidateCommandFlags(n.cmdName, jobDefs); err != nil {
				return err
			}
			namespace := len(jobDefs) > 1

			for _, jobDef := range jobDefs {
				ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
				if ext == nil {
					continue
				}

				// A non-pipeline command job has no step to reference. It used
				// to be emitted here whenever its DISPLAY NAME happened to
				// equal the referenced step id — the emission half of the
				// stringly resolution B2b removes, and the only way a ^ref
				// could bind to a job that provides no such step.
				if len(jobDef.PipelineSteps) == 0 {
					continue
				}

				// Collect the target step and all its intra-pipeline dependencies.
				// When an upstream step like "npm" depends on "types" within the same
				// pipeline, we must also emit "types" or it will be an unresolvable
				// dependency that causes the step to be canceled.
				stepsToExpand := collectPipelineStepClosure(jobDef, n.cmdName, n.depName, n.stepID, ext, namespace, existing)
				if len(stepsToExpand) == 0 {
					continue
				}

				mergedParams := mergeCommandParams(ws, proj, ext, n.cmdName, jobDef, commandParams)
				opts := pipelineExpansionOptions(ws, proj, ext, namespace, cache)
				expanded, err := extension.ExpandPipelineWithOptions(n.cmdName, jobDef, stepsToExpand, ext, mergedParams, opts)
				if err != nil {
					return fmt.Errorf("expand upstream step %s for %s: %w", n.stepID, n.depName, err)
				}
				if len(expanded) == 0 {
					continue
				}

				for _, e := range expanded {
					s := e.Step
					sj := &ScheduledJob{
						Project:   proj,
						Extension: ext,
						JobDef:    e.JobDef,
						Step:      &s,
					}
					if existing[sj.Key()] {
						continue
					}
					*allJobs = append(*allJobs, sj)
					existing[sj.Key()] = true
					added = true
				}
			}
		}

		if !added {
			break
		}
	}

	return nil
}

// collectPipelineStepClosure returns the target step (stepID) together with the
// transitive closure of its intra-pipeline dependencies, found by BFS over each
// step's DependsOn. Steps already planned for depName (present in existing) are
// skipped, and nil is returned when stepID is not part of jobDef's pipeline.
// Extracted from emitUpstreamJobs to keep that function's nesting shallow.
func collectPipelineStepClosure(
	jobDef *extension.JobDefinition,
	cmdName, depName, stepID string,
	ext *extension.ExtensionDescription,
	namespace bool,
	existing map[string]bool,
) []extension.PipelineStep {
	stepIndex := make(map[string]int, len(jobDef.PipelineSteps))
	for i, step := range jobDef.PipelineSteps {
		stepIndex[step.ID] = i
	}
	if _, ok := stepIndex[stepID]; !ok {
		return nil
	}

	var stepsToExpand []extension.PipelineStep
	visited := make(map[string]bool)
	queue := []string{stepID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true

		idx, ok := stepIndex[id]
		if !ok {
			continue
		}
		step := jobDef.PipelineSteps[idx]
		stepName := extension.StepJobName(cmdName, id)
		if namespace {
			stepName = extension.NamespacedStepJobName(cmdName, ext.Name, id)
		}
		if existing[depName+":"+stepName] {
			continue
		}
		stepsToExpand = append(stepsToExpand, step)

		// Enqueue intra-pipeline dependencies.
		for _, dep := range step.DependsOn {
			if !extension.IsExternalRef(dep) && !visited[dep] {
				queue = append(queue, dep)
			}
		}
	}
	return stepsToExpand
}

// emitCommandDeps plans same-project prerequisite commands declared via
// CommandDependsOn.
// For example, when publish declares dependsOn: ["build", "package"], this
// auto-plans build and package for each project that has publish jobs,
// then wires up the dependency so all publish steps wait for the prerequisite
// command's leaf steps to complete. Session-scoped dependencies ("!build") are
// handled separately by emitSessionCommandDeps.
func emitCommandDeps(
	allJobs *[]*ScheduledJob,
	ws *workspace.Workspace,
	jobMap map[string][]*extension.JobDefinition,
	disabledJobs, disabledExts map[string]bool,
	extensions []*extension.ExtensionDescription,
	commandParams map[string]any,
	cache *planCache,
) error {
	// Collect (project, depCommand) pairs from existing jobs
	type projCmd struct {
		proj    *workspace.Project
		cmdName string
	}
	needed := make(map[string][]projCmd) // depCommand → projects needing it

	existing := make(map[string]bool)
	for _, j := range *allJobs {
		existing[j.Key()] = true
	}

	for _, job := range *allJobs {
		if len(job.JobDef.CommandDependsOn) == 0 {
			continue
		}
		for _, rawDep := range job.JobDef.CommandDependsOn {
			depCmd, sessionScoped := commandDependencyTarget(rawDep)
			if sessionScoped || depCmd == "" {
				continue
			}
			// Check if we already queued this
			alreadyQueued := false
			for _, pc := range needed[depCmd] {
				if pc.proj.ID == job.Project.ID {
					alreadyQueued = true
					break
				}
			}
			if !alreadyQueued {
				needed[depCmd] = append(needed[depCmd], projCmd{proj: job.Project, cmdName: depCmd})
			}
		}
	}

	if len(needed) == 0 {
		return nil
	}

	// Iterate the prerequisite commands in sorted order (as
	// emitSessionCommandDeps already does): plan node ORDER is part of what
	// makes two Plans over one workspace comparable, and Go randomizes map
	// iteration.
	depCmds := make([]string, 0, len(needed))
	for depCmd := range needed {
		depCmds = append(depCmds, depCmd)
	}
	sort.Strings(depCmds)

	for _, depCmd := range depCmds {
		for _, pc := range needed[depCmd] {
			jobDefs := matchJobs(depCmd, pc.proj, jobMap, disabledJobs, disabledExts, ws.Root, cache)
			jobDefs = excludeWorkspaceOnceJobs(jobDefs)
			if len(jobDefs) == 0 {
				continue
			}
			if err := extension.ValidateCommandFlags(depCmd, jobDefs); err != nil {
				return err
			}
			namespace := len(jobDefs) > 1

			for _, jobDef := range jobDefs {
				ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
				if ext == nil {
					continue
				}

				planned, err := expandJob(ws, pc.proj, ext, depCmd, jobDef, commandParams, namespace, cache)
				if err != nil {
					return err
				}
				for _, sj := range planned {
					if !existing[sj.Key()] {
						*allJobs = append(*allJobs, sj)
						existing[sj.Key()] = true
					}
				}
			}
		}
	}

	return nil
}

// emitSessionCommandDeps plans prerequisite commands declared as "!command".
// Unlike same-project command dependencies, a session barrier plans the
// prerequisite command across the current project selection. Resolution later
// wires each dependent root step to every leaf of that command in the final
// plan, so side-effecting commands such as publish/deploy can wait for the
// whole session gate instead of only their own project.
func emitSessionCommandDeps(
	allJobs *[]*ScheduledJob,
	ws *workspace.Workspace,
	projects []*workspace.Project,
	jobMap map[string][]*extension.JobDefinition,
	disabledJobs, disabledExts map[string]bool,
	extensions []*extension.ExtensionDescription,
	commandParams map[string]any,
	cache *planCache,
) error {
	needed := make(map[string]bool)
	for _, job := range *allJobs {
		for _, rawDep := range job.JobDef.CommandDependsOn {
			depCmd, sessionScoped := commandDependencyTarget(rawDep)
			if sessionScoped && depCmd != "" {
				needed[depCmd] = true
			}
		}
	}
	if len(needed) == 0 {
		return nil
	}

	existing := make(map[string]bool, len(*allJobs))
	for _, job := range *allJobs {
		existing[job.Key()] = true
	}

	depCmds := make([]string, 0, len(needed))
	for depCmd := range needed {
		depCmds = append(depCmds, depCmd)
	}
	sort.Strings(depCmds)

	for _, depCmd := range depCmds {
		workspaceJobs := matchWorkspaceOnceJobs(depCmd, jobMap, disabledJobs, disabledExts)
		if err := extension.ValidateCommandFlags(depCmd, workspaceJobs); err != nil {
			return err
		}
		for _, jobDef := range workspaceJobs {
			ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
			if ext == nil {
				continue
			}
			planned, err := expandJob(ws, workspaceOnceProject(ws), ext, depCmd, jobDef, commandParams, len(workspaceJobs) > 1, cache)
			if err != nil {
				return err
			}
			attachSelectedProjects(planned, projects)
			for _, sj := range planned {
				if !existing[sj.Key()] {
					*allJobs = append(*allJobs, sj)
					existing[sj.Key()] = true
				}
			}
		}

		for _, proj := range projects {
			jobDefs := matchJobs(depCmd, proj, jobMap, disabledJobs, disabledExts, ws.Root, cache)
			jobDefs = excludeWorkspaceOnceJobs(jobDefs)
			if len(jobDefs) == 0 {
				continue
			}
			if err := extension.ValidateCommandFlags(depCmd, jobDefs); err != nil {
				return err
			}

			namespace := len(jobDefs) > 1
			for _, jobDef := range jobDefs {
				ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
				if ext == nil {
					continue
				}

				planned, err := expandJob(ws, proj, ext, depCmd, jobDef, commandParams, namespace, cache)
				if err != nil {
					return err
				}
				for _, sj := range planned {
					if !existing[sj.Key()] {
						*allJobs = append(*allJobs, sj)
						existing[sj.Key()] = true
					}
				}
			}
		}
	}

	return nil
}

// sessionPrerequisiteRelation is one active, dependent-command-owned
// prerequisite after its selector and per-project policy have been resolved.
// Params are invocation-local overrides for the prerequisite command only.
type sessionPrerequisiteRelation struct {
	owner           *ScheduledJob
	definition      extension.SessionPrerequisiteDefinition
	projects        []*workspace.Project
	paramsByProject map[string]extension.ParamMap
}

type sessionPrerequisiteResolution struct {
	active                   bool
	relation                 sessionPrerequisiteRelation
	selectionIDs             []string
	projectIf                map[string]bool
	canonicalParamsByProject map[string]extension.ParamMap
}

// resolveSessionPrerequisiteForOwner evaluates every observable part of one
// command-level relation for one functional root. activeSessionPrerequisites
// compares these values across all roots before it de-duplicates the relation.
func resolveSessionPrerequisiteForOwner(
	ws *workspace.Workspace,
	fallbackProjects []*workspace.Project,
	commandParams extension.ParamMap,
	owner *ScheduledJob,
	index int,
) (sessionPrerequisiteResolution, error) {
	definition := owner.JobDef.SessionPrerequisites[index]
	ownerCommand := owner.CommandName()
	ownerDef := owner.Extension.Jobs[ownerCommand]
	if ownerDef == nil {
		ownerDef = owner.JobDef
	}
	ownerParams := mergeCommandParams(ws, owner.Project, owner.Extension, ownerCommand, ownerDef, commandParams)
	applyBoundParams(ownerParams, owner.JobDef.BoundParams)
	if definition.Command == "" || definition.Command == ownerCommand {
		return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite %d has invalid command %q", owner.Key(), index, definition.Command)
	}
	for _, gate := range definition.DependsOn {
		if gate == "" || gate == definition.Command || gate == ownerCommand {
			return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite %q has cyclic or empty gate %q", owner.Key(), definition.Command, gate)
		}
	}
	if !extension.ValidateExpressionSyntax(definition.If) {
		return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite %q has invalid if expression %q", owner.Key(), definition.Command, definition.If)
	}
	if !extension.ValidateExpressionSyntax(definition.ProjectIf) {
		return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite %q has invalid projectIf expression %q", owner.Key(), definition.Command, definition.ProjectIf)
	}
	resolved := sessionPrerequisiteResolution{
		active: extension.EvaluateExpression(definition.If, &extension.WhenContext{Params: ownerParams}),
		relation: sessionPrerequisiteRelation{
			owner:           owner,
			definition:      definition,
			paramsByProject: make(map[string]extension.ParamMap),
		},
		projectIf:                make(map[string]bool),
		canonicalParamsByProject: make(map[string]extension.ParamMap),
	}
	if !resolved.active {
		return resolved, nil
	}

	// Keep implicit selection local to its owning invocation. Project commands
	// carry their project; workspace-once commands carry their resolved union.
	selected := fallbackProjects
	if len(owner.SelectedProjects) > 0 {
		selected = owner.SelectedProjects
	} else if owner.JobDef.Activation != workspaceOnceActivation && owner.Project != nil {
		selected = []*workspace.Project{owner.Project}
	}
	if definition.ProjectsFromParam != "" {
		raw, present := ownerParams[definition.ProjectsFromParam]
		if present && raw != nil {
			selector, ok := raw.(string)
			if !ok {
				return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite projects parameter %q must be a string", owner.Key(), definition.ProjectsFromParam)
			}
			if strings.TrimSpace(selector) != "" {
				selected = workspace.ResolveTarget(ws, selector, ws.Root, ws.ScopeIndex)
				if len(selected) == 0 {
					return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite selector %q matched no projects", owner.Key(), selector)
				}
			}
		}
	}
	for _, project := range selected {
		if project != nil {
			resolved.selectionIDs = append(resolved.selectionIDs, project.ID)
		}
	}
	resolved.selectionIDs = dedupeSorted(resolved.selectionIDs)

	paramNames := make([]string, 0, len(definition.Params))
	for name := range definition.Params {
		paramNames = append(paramNames, name)
	}
	sort.Strings(paramNames)
	for _, project := range selected {
		if project == nil {
			continue
		}
		projectParams := mergeCommandParams(ws, project, owner.Extension, ownerCommand, ownerDef, commandParams)
		applyBoundParams(projectParams, owner.JobDef.BoundParams)
		eligible := extension.EvaluateExpression(definition.ProjectIf, &extension.WhenContext{Params: projectParams})
		resolved.projectIf[project.ID] = eligible
		if !eligible {
			continue
		}
		overrides := make(extension.ParamMap)
		for _, name := range paramNames {
			binding := definition.Params[name]
			hasValue := binding.Value != nil
			hasProjectParam := binding.FromProjectParam != ""
			if hasValue == hasProjectParam {
				return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite parameter %q must declare exactly one of value or fromProjectParam", owner.Key(), name)
			}
			if hasValue {
				overrides[name] = binding.Value
				continue
			}
			path := strings.TrimPrefix(binding.FromProjectParam, "params.")
			var value any = projectParams
			present := path != ""
			for _, segment := range strings.Split(path, ".") {
				object, ok := value.(extension.ParamMap)
				if !ok {
					present = false
					break
				}
				value, present = object[segment]
				if !present {
					break
				}
			}
			if !present {
				return sessionPrerequisiteResolution{}, fmt.Errorf("%s session prerequisite %q parameter %q fromProjectParam %q was not found for project %q",
					owner.Key(), definition.Command, name, binding.FromProjectParam, project.ID)
			}
			overrides[name] = value
		}
		resolved.relation.projects = append(resolved.relation.projects, project)
		if len(overrides) > 0 {
			resolved.relation.paramsByProject[project.ID] = overrides
			resolved.canonicalParamsByProject[project.ID], _ = projectParamAliases(overrides)
		}
	}
	return resolved, nil
}

// activeSessionPrerequisites resolves command-level conditions, optional
// selection overrides, per-project policy, and param bindings without planning
// any jobs. Re-running it at the fixpoint and again before edge resolution is
// deterministic because all of its inputs are immutable plan facts.
func activeSessionPrerequisites(
	jobs []*ScheduledJob,
	ws *workspace.Workspace,
	fallbackProjects []*workspace.Project,
	commandParams extension.ParamMap,
) ([]sessionPrerequisiteRelation, error) {
	seen := make(map[string]bool)
	var relations []sessionPrerequisiteRelation
	for _, owner := range jobs {
		if owner == nil || owner.Extension == nil || !isCommandRoot(owner) || len(owner.JobDef.SessionPrerequisites) == 0 {
			continue
		}
		ownerCommand := owner.CommandName()
		for i := range owner.JobDef.SessionPrerequisites {
			key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", owner.Project.ID, owner.Extension.Name, ownerCommand, i)
			if seen[key] {
				continue
			}
			var roots []*ScheduledJob
			for _, candidate := range jobs {
				if candidate == nil || candidate.Project == nil || candidate.Extension == nil || !isCommandRoot(candidate) ||
					candidate.Project.ID != owner.Project.ID || candidate.Extension.Name != owner.Extension.Name ||
					candidate.CommandName() != ownerCommand || len(candidate.JobDef.SessionPrerequisites) <= i {
					continue
				}
				roots = append(roots, candidate)
			}
			sort.Slice(roots, func(left, right int) bool { return roots[left].Key() < roots[right].Key() })
			baseline, err := resolveSessionPrerequisiteForOwner(ws, fallbackProjects, commandParams, roots[0], i)
			if err != nil {
				return nil, err
			}
			for _, root := range roots[1:] {
				candidate, candidateErr := resolveSessionPrerequisiteForOwner(ws, fallbackProjects, commandParams, root, i)
				if candidateErr != nil {
					return nil, candidateErr
				}
				compatible := baseline.active == candidate.active
				if compatible && baseline.active {
					compatible = reflect.DeepEqual(baseline.selectionIDs, candidate.selectionIDs) &&
						reflect.DeepEqual(baseline.projectIf, candidate.projectIf) &&
						reflect.DeepEqual(baseline.canonicalParamsByProject, candidate.canonicalParamsByProject)
				}
				if compatible {
					continue
				}
				leftParams, _ := projectParamAliases(roots[0].JobDef.BoundParams)
				rightParams, _ := projectParamAliases(root.JobDef.BoundParams)
				paramSet := make(map[string]struct{}, len(leftParams)+len(rightParams))
				for name := range leftParams {
					paramSet[name] = struct{}{}
				}
				for name := range rightParams {
					paramSet[name] = struct{}{}
				}
				var differing []string
				for name := range paramSet {
					leftValue, leftOK := leftParams[name]
					rightValue, rightOK := rightParams[name]
					if leftOK != rightOK || !reflect.DeepEqual(leftValue, rightValue) {
						differing = append(differing, name)
					}
				}
				sort.Strings(differing)
				detail := "resolved policy"
				if len(differing) > 0 {
					detail = strings.Join(differing, ",")
				}
				return nil, fmt.Errorf("command %q session prerequisite %d resolves incompatibly across command roots %s and %s: %s",
					ownerCommand, i, roots[0].Key(), root.Key(), detail)
			}
			seen[key] = true
			if !baseline.active {
				continue
			}
			baseline.relation.owner = roots[0]
			relations = append(relations, baseline.relation)
		}
	}
	return relations, nil
}

// emitSessionPrerequisites plans each active prerequisite and every declared
// gate across the prerequisite's resolved project selection.
func emitSessionPrerequisites(
	allJobs *[]*ScheduledJob,
	ws *workspace.Workspace,
	projects []*workspace.Project,
	explicitCommands map[string]bool,
	jobMap map[string][]*extension.JobDefinition,
	disabledJobs, disabledExts map[string]bool,
	extensions []*extension.ExtensionDescription,
	commandParams extension.ParamMap,
	cache *planCache,
) (bool, error) {
	relations, err := activeSessionPrerequisites(*allJobs, ws, projects, commandParams)
	if err != nil {
		return false, err
	}
	changed := false
	for _, relation := range relations {
		if len(relation.projects) == 0 {
			continue
		}
		for _, gate := range relation.definition.DependsOn {
			if len(jobMap[gate]) == 0 {
				return false, fmt.Errorf("%s session prerequisite %q has unknown gate %q", relation.owner.Key(), relation.definition.Command, gate)
			}
		}
		if len(relation.definition.Params) > 0 && explicitCommands[relation.definition.Command] {
			return false, fmt.Errorf("%s session prerequisite %q has local parameter bindings but that command was explicitly requested in the same plan", relation.owner.Key(), relation.definition.Command)
		}
		commandChanged, err := emitCommandForSelection(allJobs, ws, relation.definition.Command, relation.projects,
			jobMap, disabledJobs, disabledExts, extensions, commandParams, relation.definition.Params, relation.paramsByProject, cache)
		if err != nil {
			return false, err
		}
		changed = changed || commandChanged
		for _, gate := range relation.definition.DependsOn {
			gateChanged, err := emitCommandForSelection(allJobs, ws, gate, relation.projects,
				jobMap, disabledJobs, disabledExts, extensions, commandParams, nil, nil, cache)
			if err != nil {
				return false, err
			}
			changed = changed || gateChanged
		}
	}
	return changed, nil
}

// emitCommandForSelection plans all contributors to one command. Per-project
// overrides affect both plan-time `if` conditions and the eventual task
// context, but only for jobs introduced by this prerequisite relation.
func emitCommandForSelection(
	allJobs *[]*ScheduledJob,
	ws *workspace.Workspace,
	command string,
	projects []*workspace.Project,
	jobMap map[string][]*extension.JobDefinition,
	disabledJobs, disabledExts map[string]bool,
	extensions []*extension.ExtensionDescription,
	commandParams extension.ParamMap,
	bindings map[string]extension.SessionPrerequisiteParamBinding,
	paramsByProject map[string]extension.ParamMap,
	cache *planCache,
) (bool, error) {
	changed := false
	workspaceJobs := matchWorkspaceOnceJobs(command, jobMap, disabledJobs, disabledExts)
	var workspaceOverrides extension.ParamMap
	if len(workspaceJobs) > 0 && len(bindings) > 0 && len(projects) > 0 {
		if len(projects) > 1 {
			names := make([]string, 0, len(bindings))
			for name := range bindings {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if bindings[name].FromProjectParam != "" {
					return false, fmt.Errorf("session prerequisite command %q workspace-once parameter %q uses fromProjectParam across multiple projects", command, name)
				}
			}
		}
		workspaceOverrides = paramsByProject[projects[0].ID]
		for _, project := range projects[1:] {
			if !reflect.DeepEqual(workspaceOverrides, paramsByProject[project.ID]) {
				return false, fmt.Errorf("session prerequisite command %q has incompatible workspace-once local parameters across multiple projects", command)
			}
		}
	}
	if err := extension.ValidateCommandFlags(command, workspaceJobs); err != nil {
		return false, err
	}
	var workspaceCandidates []*ScheduledJob
	workspaceParams, _ := projectParamAliases(commandParams)
	if len(workspaceOverrides) > 0 {
		applyProjectedParamLayer(workspaceParams, workspaceOverrides)
	}
	for _, jobDef := range workspaceJobs {
		ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
		if ext == nil {
			continue
		}
		planned, err := expandJob(ws, workspaceOnceProject(ws), ext, command, jobDef, workspaceParams, len(workspaceJobs) > 1, cache)
		if err != nil {
			return false, err
		}
		workspaceCandidates = append(workspaceCandidates, planned...)
	}
	if len(workspaceJobs) > 0 {
		workspaceChanged, err := mergeSessionPrerequisiteCandidates(allJobs, command, workspaceOnceProject(ws).ID,
			workspaceCandidates, workspaceOverrides, projects)
		if err != nil {
			return false, err
		}
		changed = changed || workspaceChanged
	}

	for _, project := range projects {
		jobDefs := excludeWorkspaceOnceJobs(matchJobs(command, project, jobMap, disabledJobs, disabledExts, ws.Root, cache))
		if len(jobDefs) == 0 {
			continue
		}
		if err := extension.ValidateCommandFlags(command, jobDefs); err != nil {
			return false, err
		}
		overrides := paramsByProject[project.ID]
		effectiveParams, _ := projectParamAliases(commandParams)
		if len(overrides) > 0 {
			applyProjectedParamLayer(effectiveParams, overrides)
		}
		var candidates []*ScheduledJob
		for _, jobDef := range jobDefs {
			ext := extension.FindExtensionByName(extensions, jobDef.ExtensionName)
			if ext == nil {
				continue
			}
			planned, err := expandJob(ws, project, ext, command, jobDef, effectiveParams, len(jobDefs) > 1, cache)
			if err != nil {
				return false, err
			}
			candidates = append(candidates, planned...)
		}
		projectChanged, err := mergeSessionPrerequisiteCandidates(allJobs, command, project.ID, candidates, overrides, nil)
		if err != nil {
			return false, err
		}
		changed = changed || projectChanged
	}
	return changed, nil
}

// mergeSessionPrerequisiteCandidates enforces one representation for one
// command invocation. Reuse is permitted only when the complete job set and
// task-local parameters agree; workspace selections are then unioned rather
// than discarded by first-wins key de-duplication.
func mergeSessionPrerequisiteCandidates(
	allJobs *[]*ScheduledJob,
	command, projectID string,
	candidates []*ScheduledJob,
	overrides extension.ParamMap,
	selectedProjects []*workspace.Project,
) (bool, error) {
	paramNames := make([]string, 0, len(overrides))
	for name := range overrides {
		paramNames = append(paramNames, name)
	}
	sort.Strings(paramNames)
	for _, candidate := range candidates {
		if len(overrides) == 0 {
			continue
		}
		definition := cloneJobDefinition(candidate.JobDef)
		definition.BoundParams = make(extension.ParamMap, len(candidate.JobDef.BoundParams)+len(overrides))
		for name, value := range candidate.JobDef.BoundParams {
			definition.BoundParams[name] = value
		}
		literalParams, literalSources := projectParamAliases(definition.BoundParams)
		overrideParams, overrideSources := projectParamAliases(overrides)
		canonicalNames := make([]string, 0, len(overrideParams))
		for name := range overrideParams {
			canonicalNames = append(canonicalNames, name)
		}
		sort.Strings(canonicalNames)
		for _, canonicalName := range canonicalNames {
			value := overrideParams[canonicalName]
			if literal, exists := literalParams[canonicalName]; exists && !reflect.DeepEqual(literal, value) {
				return false, fmt.Errorf("session prerequisite command %q local parameter %q conflicts with literal run[].with %q on %s", command, overrideSources[canonicalName], literalSources[canonicalName], candidate.Key())
			}
		}
		for _, name := range paramNames {
			value := overrides[name]
			definition.BoundParams[name] = value
		}
		candidate.JobDef = definition
	}

	existing := make(map[string]*ScheduledJob)
	for _, job := range *allJobs {
		if job != nil && job.Project != nil && job.Project.ID == projectID && job.CommandName() == command {
			existing[job.Key()] = job
		}
	}
	candidateByKey := make(map[string]*ScheduledJob, len(candidates))
	for _, candidate := range candidates {
		candidateByKey[candidate.Key()] = candidate
	}
	if len(existing) > 0 {
		return mergeExistingSessionPrerequisiteCandidates(existing, candidateByKey, command, projectID, paramNames, selectedProjects)
	}

	if len(selectedProjects) > 0 {
		attachSelectedProjects(candidates, selectedProjects)
	}
	*allJobs = append(*allJobs, candidates...)
	return len(candidates) > 0, nil
}

func mergeExistingSessionPrerequisiteCandidates(
	existing, candidates map[string]*ScheduledJob,
	command, projectID string,
	paramNames []string,
	selectedProjects []*workspace.Project,
) (bool, error) {
	detail := ""
	if len(paramNames) > 0 {
		detail = fmt.Sprintf(" local parameters %s", strings.Join(paramNames, ","))
	}
	incompatible := fmt.Errorf("session prerequisite command %q is already planned with an incompatible%s invocation for %q", command, detail, projectID)
	if len(existing) != len(candidates) {
		return false, incompatible
	}
	for key, candidate := range candidates {
		planned := existing[key]
		if planned == nil {
			return false, incompatible
		}
		plannedParams, _ := projectParamAliases(planned.JobDef.BoundParams)
		candidateParams, _ := projectParamAliases(candidate.JobDef.BoundParams)
		if !reflect.DeepEqual(plannedParams, candidateParams) {
			return false, incompatible
		}
	}
	if len(selectedProjects) == 0 {
		return false, nil
	}
	changed := false
	for _, job := range existing {
		byID := make(map[string]*workspace.Project)
		for _, project := range append(job.SelectedProjects, selectedProjects...) {
			if project != nil {
				byID[project.ID] = project
			}
		}
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		next := make([]*workspace.Project, 0, len(ids))
		for _, id := range ids {
			next = append(next, byID[id])
		}
		if !reflect.DeepEqual(job.SelectedProjects, next) {
			job.SelectedProjects = next
			changed = true
		}
	}
	return changed, nil
}

// ownerGateLeafFrontier computes each owner's functional gate frontier: the
// deduplicated union of every gate leaf its relations declared over their own
// selections (plus the workspace-once project). Each relation's own leaves are
// recomputed identically in the wiring loop; the frontier only widens what a
// root will wait FOR, never which gates a relation declares — policy, no-op
// bookkeeping, and the provenance checks stay per relation.
func ownerGateLeafFrontier(
	relations []sessionPrerequisiteRelation,
	leaves map[string][]string,
	workspaceID string,
) map[string][]string {
	frontier := make(map[string][]string)
	for _, relation := range relations {
		if len(relation.projects) == 0 {
			continue
		}
		included := make(map[string]bool, len(relation.projects)+1)
		included[workspaceID] = true
		for _, project := range relation.projects {
			included[project.ID] = true
		}
		ownerKey := relation.owner.Key()
		for _, gate := range relation.definition.DependsOn {
			for projectID := range included {
				frontier[ownerKey] = append(frontier[ownerKey], leaves[projectID+":"+gate]...)
			}
		}
	}
	for ownerKey := range frontier {
		frontier[ownerKey] = dedupeSorted(frontier[ownerKey])
	}
	return frontier
}

// resolveSessionPrerequisites turns the policy into functional DAG edges:
// every prerequisite root waits for every declared gate leaf, and every owner
// root waits for every prerequisite leaf. Functional dependencies make failure
// propagation the scheduler's ordinary behavior rather than a second policy.
//
// One owner has ONE functional gate frontier. Sibling relations of the same
// owner may declare gates over disjoint selections (deploy gates its apps'
// publish while a packageAsDependency relation gates a dependency-packaged
// base project); a prerequisite root then waits for the UNION of every gate
// leaf its owner's relations declared, not only its own relation's. Without
// the union, one relation's gate leaves are invisible to a sibling's jobs, and
// a capability contract that quantifies over the FINAL DAG ("every protected
// job after every leaf of each required command") is unsatisfiable the moment
// two relations contribute leaves for the same command over different
// projects: the sibling's protected jobs sit beside — not after — the new
// leaves, and authorization fails closed.
func resolveSessionPrerequisites(
	jobs []*ScheduledJob,
	ws *workspace.Workspace,
	projects []*workspace.Project,
	commandParams extension.ParamMap,
) error {
	relations, err := activeSessionPrerequisites(jobs, ws, projects, commandParams)
	if err != nil {
		return err
	}
	leaves := computeCommandLeaves(jobs)
	workspaceID := workspaceOnceProject(ws).ID
	gatePolicyByJob := make(map[string][]string)
	noopGatesByJob := make(map[string]map[string]bool)
	gatePolicyProjectsByJob := make(map[string]map[string]bool)

	ownerGateLeaves := ownerGateLeafFrontier(relations, leaves, workspaceID)
	plannedByKey := jobsByPlanKey(jobs)

	for _, relation := range relations {
		if len(relation.projects) == 0 {
			continue
		}
		included := make(map[string]bool, len(relation.projects)+1)
		included[workspaceID] = true
		for _, project := range relation.projects {
			included[project.ID] = true
		}

		var gateLeaves, noopGates []string
		for _, gate := range relation.definition.DependsOn {
			var commandGateLeaves []string
			for projectID := range included {
				commandGateLeaves = append(commandGateLeaves, leaves[projectID+":"+gate]...)
			}
			commandGateLeaves = dedupeSorted(commandGateLeaves)
			if len(commandGateLeaves) == 0 {
				noopGates = append(noopGates, gate)
				continue
			}
			gateLeaves = append(gateLeaves, commandGateLeaves...)
		}
		gateLeaves = dedupeSorted(gateLeaves)
		noopGates = dedupeSorted(noopGates)
		gatePolicy := dedupeSorted(append([]string(nil), relation.definition.DependsOn...))
		relationNoopGates := make(map[string]bool, len(noopGates))
		for _, gate := range noopGates {
			relationNoopGates[gate] = true
		}
		for _, job := range jobs {
			if job.CommandName() != relation.definition.Command || !included[job.Project.ID] {
				continue
			}
			key := job.Key()
			if existingPolicy, seen := gatePolicyByJob[key]; seen {
				if !reflect.DeepEqual(existingPolicy, gatePolicy) {
					return fmt.Errorf(
						"session prerequisite workspace-once command %q reuses %s with incompatible gate policy %v versus %v",
						relation.definition.Command, key, existingPolicy, gatePolicy,
					)
				}
				for gate := range noopGatesByJob[key] {
					if !relationNoopGates[gate] {
						delete(noopGatesByJob[key], gate)
					}
				}
			} else {
				gatePolicyByJob[key] = gatePolicy
				noopGatesByJob[key] = relationNoopGates
			}
			if gatePolicyProjectsByJob[key] == nil {
				gatePolicyProjectsByJob[key] = make(map[string]bool)
			}
			for _, project := range relation.projects {
				if project != nil {
					gatePolicyProjectsByJob[key][project.ID] = true
				}
			}
			if isCommandRoot(job) {
				frontier := ownerGateLeaves[relation.owner.Key()]
				if len(frontier) == 0 {
					frontier = gateLeaves
				}
				// A root never waits for its own command's leaves: a sibling
				// relation may legally gate on THIS relation's command, and a
				// root waiting for a same-command leaf would self-reference or
				// cycle inside the frontier it is part of.
				filtered := make([]string, 0, len(frontier))
				for _, leaf := range frontier {
					if leaf == key {
						continue
					}
					if leafJob := plannedByKey[leaf]; leafJob != nil &&
						leafJob.CommandName() == relation.definition.Command {
						continue
					}
					filtered = append(filtered, leaf)
				}
				job.DependsOn = dedupeSorted(append(job.DependsOn, filtered...))
			}
		}

		var prerequisiteLeaves []string
		for projectID := range included {
			prerequisiteLeaves = append(prerequisiteLeaves, leaves[projectID+":"+relation.definition.Command]...)
		}
		prerequisiteLeaves = dedupeSorted(prerequisiteLeaves)
		if len(prerequisiteLeaves) == 0 {
			return fmt.Errorf("%s session prerequisite %q planned no jobs", relation.owner.Key(), relation.definition.Command)
		}
		for _, job := range jobs {
			if job.Project.ID == relation.owner.Project.ID && job.Extension != nil &&
				job.Extension.Name == relation.owner.Extension.Name && job.CommandName() == relation.owner.CommandName() && isCommandRoot(job) {
				job.DependsOn = dedupeSorted(append(job.DependsOn, prerequisiteLeaves...))
			}
		}
	}
	return finalizeSessionPrerequisiteNoopGates(jobs, gatePolicyByJob, noopGatesByJob, gatePolicyProjectsByJob)
}

func finalizeSessionPrerequisiteNoopGates(
	jobs []*ScheduledJob,
	gatePolicyByJob map[string][]string,
	noopGatesByJob, gatePolicyProjectsByJob map[string]map[string]bool,
) error {
	for _, job := range jobs {
		key := job.Key()
		noopSet, seen := noopGatesByJob[key]
		if !seen {
			continue
		}
		if len(gatePolicyByJob[key]) > 0 && len(job.SelectedProjects) > 0 {
			var uncovered []string
			for _, project := range job.SelectedProjects {
				if project != nil && !gatePolicyProjectsByJob[key][project.ID] {
					uncovered = append(uncovered, project.ID)
				}
			}
			uncovered = dedupeSorted(uncovered)
			if len(uncovered) > 0 {
				return fmt.Errorf(
					"session prerequisite workspace-once command %q reuses %s with selected projects %s outside its gate policy provenance",
					job.CommandName(), key, strings.Join(uncovered, ","),
				)
			}
		}
		job.SessionPrerequisiteNoopGates = job.SessionPrerequisiteNoopGates[:0]
		for gate := range noopSet {
			job.SessionPrerequisiteNoopGates = append(job.SessionPrerequisiteNoopGates, gate)
		}
		sort.Strings(job.SessionPrerequisiteNoopGates)
	}
	return nil
}

func isCommandRoot(job *ScheduledJob) bool {
	if isFinalizerJob(job) {
		return false
	}
	for _, dependency := range job.JobDef.DependsOn {
		if !extension.IsExternalRef(dependency) {
			return false
		}
	}
	return true
}

// resolveExternalDeps resolves ^, /, * dependency references and command-level
// deps.
//
// Every external reference resolves by EXACT identity — the (command, step) a
// job IS, never a derived display string, plan name or synthesized key. A
// reference that matches nothing exactly is legal and contributes no edge:
// `^generate` simply has no upstream provider in a project
// whose dependencies do not run that step. A reference that matches nothing
// exactly but WOULD have bound through one of the removed spellings is an
// error, not a silent rebind and not a silent drop — see legacyReferenceLines.
func resolveExternalDeps(jobs []*ScheduledJob, ws *workspace.Workspace) error {
	// Build the lookup index once: by key, by project, and by identity name
	// (the last drives the "*" wildcard branch below without rescanning jobs).
	idx := buildPlannedIndex(jobs)

	// Pre-compute leaf steps per (project, command) for command-level deps.
	// A leaf step is one that no other step in the same command depends on.
	commandLeaves := computeCommandLeaves(jobs)

	// Resolve command-level dependencies: each root step of a command
	// (step with no intra-pipeline deps) gets dependencies on the leaf
	// steps of each prerequisite command in the same project.
	resolveCommandDeps(jobs, commandLeaves)
	resolveSessionCommandDeps(jobs, commandLeaves)

	var legacy []legacyReference
	for _, job := range jobs {
		if job.JobDef.DependsOn == nil {
			continue
		}

		cmdName := job.CommandName()

		resolved := append([]string(nil), job.DependsOn...)
		for _, dep := range job.JobDef.DependsOn {
			if !extension.IsExternalRef(dep) {
				// Intra-pipeline: qualify with project ID
				resolved = append(resolved, job.Project.ID+":"+dep)
				continue
			}

			switch {
			case strings.HasPrefix(dep, "^"):
				// ^ = upstream dependencies
				stepID := dep[1:]
				want := exactIdentityDescription(cmdName, stepID)
				for _, depName := range ws.Graph.DependenciesOf(job.Project.ID) {
					keys := idx.matchingKeys(depName, cmdName, stepID)
					if len(keys) == 0 {
						legacy = appendLegacyReference(legacy, job, dep, want, idx.legacyKeys(depName, cmdName, stepID))
						continue
					}
					resolved = append(resolved, keys...)
				}

			case strings.HasPrefix(dep, "/"):
				// / = workspace-level job
				stepID := dep[1:]
				keys := idx.matchingKeys(ws.Name, cmdName, stepID)
				if len(keys) == 0 {
					legacy = appendLegacyReference(legacy, job, dep,
						exactIdentityDescription(cmdName, stepID), idx.legacyKeys(ws.Name, cmdName, stepID))
					break
				}
				resolved = append(resolved, keys...)

			case strings.HasPrefix(dep, "*"):
				// * = all projects providing this step or command. byStepName
				// indexes every job under the command it belongs to and the
				// step it is, so a single lookup replaces a full scan;
				// self-references and duplicates are pruned by dedupeSorted on
				// resolved below.
				stepID := dep[1:]
				matched := 0
				for _, j := range idx.byStepName[stepID] {
					if j.Key() == job.Key() {
						continue
					}
					matched++
					resolved = append(resolved, j.Key())
				}
				if matched == 0 {
					legacy = appendLegacyReference(legacy, job, dep,
						fmt.Sprintf("command or step %q", stepID), idx.legacyWildcardKeys(job, stepID))
				}
			}
		}

		job.DependsOn = dedupeSorted(resolved)
	}

	if lines := legacyReferenceLines(legacy, maxDAGDiagnosticLines); len(lines) > 0 {
		return fmt.Errorf("dependency references resolve only by a removed legacy spelling:\n%s",
			strings.Join(lines, "\n"))
	}

	return nil
}

// orderContractClients orders a generated client's jobs after the provider jobs
// that produce what those jobs read, as ORDERING edges alone.
//
// The edge carries one input, the contract, and the client's identity already
// states it: its committed manifest's contractSha256 is folded into its
// project metadata digest, which every one of its cache keys asks for. The
// provider's sources are read by no action of the client, so they must not
// reach its keys — and DependsOn is a cache-key edge, because
// computeJobCacheHashWith folds every DependsOn key into the dependent's key.
// SerializeAfter is the ordering-only family: it sequences execution, never
// contributes to a key, and a failed predecessor does not cancel the
// successor. That is exactly the relation here — the client reads a file that
// is committed whether or not the provider's job succeeds, and it must not
// read it while the provider's generator is rewriting it.
//
// Two rules name the provider jobs, and both decide from the plan alone.
//
// The step rule is the `^` rule applied to the contract provider instead of
// the declared dependencies: a step orders after the provider's step of the
// same name. A client's `generate` waits for the provider's `generate`, a Go
// client's `describe` for the provider's `describe`.
//
// The tree rule orders EVERY job of the client after every provider job that
// can rewrite the client's directory. Such a job's task declares a durable
// directory output whose path its run reports (pathFrom): the provider's
// configuration chooses that directory, so the plan knows the writer and not
// the path. Every job of the client reads its own directory, and the writer's
// drift reference snapshots it, so no client job runs while such a writer
// runs. The rule decides reach from the output's root. A project-rooted output
// lies strictly inside the provider's directory, so it reaches the client only
// when one of the two directories contains the other. A workspace-rooted
// output reaches any client. The command-output and invocation roots lie
// outside every project and reach none. A client kept outside its provider's
// directory therefore waits for no project-rooted writer. A file output never
// qualifies: a generated client is a directory.
//
// An unplanned provider contributes no edge — there is nothing to wait for —
// and nothing is emitted into the plan for it, so an ordering relation never
// widens a selection. A finalizer is on neither end of an edge: the invocation
// runtime dispatches it, not the DAG.
//
// An edge is added only when the provider's job does not already run after the
// client's, so a provider that imports its own generated client keeps a plan
// that is a DAG: the client jobs its writer depends on already run before it,
// through the writer's dependencies, and the others wait. The check is over every edge the plan holds —
// functional, write-serialization, and the contract edges already added — so
// the pass runs after serializeWriteResources, and nothing later adds an edge.
func orderContractClients(jobs []*ScheduledJob, ws *workspace.Workspace) {
	idx := buildPlannedIndex(jobs)
	predecessors := make(map[string][]string, len(jobs))
	inPlan := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		inPlan[job.Key()] = true
	}
	for _, job := range jobs {
		var edges []string
		for _, key := range append(append([]string(nil), job.DependsOn...), job.SerializeAfter...) {
			if inPlan[key] {
				edges = append(edges, key)
			}
		}
		predecessors[job.Key()] = edges
	}
	// runsBefore reports whether candidate must already run before key, so
	// ordering key before candidate would close a cycle.
	runsBefore := func(candidate, key string) bool {
		seen := map[string]bool{key: true}
		stack := append([]string(nil), predecessors[key]...)
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if current == candidate {
				return true
			}
			if seen[current] {
				continue
			}
			seen[current] = true
			stack = append(stack, predecessors[current]...)
		}
		return false
	}
	// nested reports whether one of two workspace-relative directories contains
	// the other, "." being the workspace root.
	nested := func(a, b string) bool {
		a, b = path.Clean(a), path.Clean(b)
		return a == "." || b == "." || a == b ||
			strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
	}
	// writesClientTree applies the tree rule to one provider job.
	writesClientTree := func(providerJob, client *ScheduledJob) bool {
		declaration := taskDeclarationOf(providerJob)
		if declaration == nil || isFinalizerJob(providerJob) {
			return false
		}
		for _, output := range declaration.Outputs {
			if output.PathFrom == "" || output.Kind != extension.OutputKindDirectory {
				continue
			}
			switch output.EffectiveRoot() {
			case extension.OutputRootWorkspace:
				return true
			case extension.OutputRootProject:
				if nested(projectRelPath(providerJob), projectRelPath(client)) {
					return true
				}
			}
		}
		return false
	}

	for _, job := range jobs {
		if job.JobDef == nil || isFinalizerJob(job) {
			continue
		}
		providerID := ws.Graph.ContractProviderOf(job.Project.ID)
		if providerID == "" {
			continue
		}
		cmdName := job.CommandName()
		var candidates []string
		for _, dep := range job.JobDef.DependsOn {
			if strings.HasPrefix(dep, "^") {
				candidates = append(candidates, idx.matchingKeys(providerID, cmdName, dep[1:])...)
			}
		}
		for _, providerJob := range idx.byProject[providerID] {
			if writesClientTree(providerJob, job) {
				candidates = append(candidates, providerJob.Key())
			}
		}
		if len(candidates) == 0 {
			continue
		}
		key := job.Key()
		functional := make(map[string]bool, len(job.DependsOn))
		for _, dep := range job.DependsOn {
			functional[dep] = true
		}
		var extra []string
		for _, providerKey := range dedupeSorted(candidates) {
			if providerKey == key || functional[providerKey] || runsBefore(key, providerKey) {
				continue
			}
			extra = append(extra, providerKey)
			predecessors[key] = append(predecessors[key], providerKey)
		}
		if len(extra) == 0 {
			continue
		}
		job.SerializeAfter = dedupeSorted(append(job.SerializeAfter, extra...))
	}
}

// legacyReference is one dependency reference that no longer resolves, together
// with the plan keys a removed stringly fallback would have bound it to. It
// exists so the removal fails loudly at plan time instead of quietly dropping
// an edge the manifest author believed in.
type legacyReference struct {
	job *ScheduledJob
	ref string
	// want describes the exact identity the reference was looked up under, so
	// the diagnostic states what the manifest would have to provide.
	want  string
	would []string
}

// exactIdentityDescription names the identity a step-scoped reference resolves
// under.
func exactIdentityDescription(cmdName, stepID string) string {
	return fmt.Sprintf("command %q, step %q", cmdName, stepID)
}

func appendLegacyReference(acc []legacyReference, job *ScheduledJob, ref, want string, would []string) []legacyReference {
	if len(would) == 0 {
		return acc
	}
	return append(acc, legacyReference{job: job, ref: ref, want: want, would: would})
}

// legacyReferenceLines renders the rejection, naming the referencing job, the
// reference as written, and every job the removed spelling would have bound —
// everything the manifest owner needs to respell the reference exactly.
func legacyReferenceLines(refs []legacyReference, limit int) []string {
	if len(refs) == 0 {
		return nil
	}
	var lines []string
	for _, ref := range refs {
		if len(lines) >= limit {
			lines = append(lines, fmt.Sprintf("  ... and %d more legacy references", len(refs)-limit))
			break
		}
		lines = append(lines, fmt.Sprintf("  %s depends on %q, which no job provides as (%s); the removed name match would have bound %s",
			ref.job.Key(), ref.ref, ref.want, strings.Join(ref.would, ", ")))
	}
	return lines
}

// computeCommandLeaves finds the leaf steps (steps not depended on by
// any other step in the same command+project) for each (project, command).
// Returns a map of "projectID:commandName" → list of leaf job keys.
func computeCommandLeaves(jobs []*ScheduledJob) map[string][]string {
	// Group jobs by project+command
	type cmdKey struct {
		projectID string
		cmdName   string
	}
	groups := make(map[cmdKey][]*ScheduledJob)
	for _, j := range jobs {
		if isFinalizerJob(j) {
			continue
		}
		cmd := j.CommandName()
		k := cmdKey{projectID: j.Project.ID, cmdName: cmd}
		groups[k] = append(groups[k], j)
	}

	result := make(map[string][]string)
	for k, groupJobs := range groups {
		// Collect all step IDs that are depended on by other steps
		dependedOn := make(map[string]bool)
		for _, j := range groupJobs {
			for _, dep := range j.JobDef.DependsOn {
				if !extension.IsExternalRef(dep) {
					dependedOn[dep] = true
				}
			}
		}

		// Leaves are steps not in the dependedOn set
		mapKey := k.projectID + ":" + k.cmdName
		for _, j := range groupJobs {
			if !dependedOn[j.PlanName()] {
				result[mapKey] = append(result[mapKey], j.Key())
			}
		}
	}

	return result
}

// resolveCommandDeps wires command-level dependsOn into job-level deps.
// For each job whose command has CommandDependsOn, its root steps (steps
// with no intra-pipeline deps) get dependencies on the leaf steps of
// each prerequisite command in the same project.
func resolveCommandDeps(jobs []*ScheduledJob, leaves map[string][]string) {
	for _, job := range jobs {
		if len(job.JobDef.CommandDependsOn) == 0 {
			continue
		}

		// Only apply to root steps (no intra-pipeline deps)
		hasIntraDeps := false
		for _, dep := range job.JobDef.DependsOn {
			if !extension.IsExternalRef(dep) {
				hasIntraDeps = true
				break
			}
		}
		if hasIntraDeps {
			continue
		}

		for _, rawDep := range job.JobDef.CommandDependsOn {
			depCmd, sessionScoped := commandDependencyTarget(rawDep)
			if sessionScoped || depCmd == "" {
				continue
			}
			mapKey := job.Project.ID + ":" + depCmd
			job.DependsOn = append(job.DependsOn, leaves[mapKey]...)
		}
		job.DependsOn = dedupeSorted(job.DependsOn)
	}
}

// resolveSessionCommandDeps wires "!command" dependencies into job-level deps.
// For each root step of the dependent command, it waits for all leaf jobs of
// the prerequisite command that are present anywhere in the plan.
func resolveSessionCommandDeps(jobs []*ScheduledJob, leaves map[string][]string) {
	sessionLeaves := make(map[string][]string)
	for projectCommand, keys := range leaves {
		_, cmdName, ok := strings.Cut(projectCommand, ":")
		if !ok || cmdName == "" {
			continue
		}
		sessionLeaves[cmdName] = append(sessionLeaves[cmdName], keys...)
	}
	for cmdName, keys := range sessionLeaves {
		sessionLeaves[cmdName] = dedupeSorted(keys)
	}

	for _, job := range jobs {
		if len(job.JobDef.CommandDependsOn) == 0 {
			continue
		}

		hasIntraDeps := false
		for _, dep := range job.JobDef.DependsOn {
			if !extension.IsExternalRef(dep) {
				hasIntraDeps = true
				break
			}
		}
		if hasIntraDeps {
			continue
		}

		for _, rawDep := range job.JobDef.CommandDependsOn {
			depCmd, sessionScoped := commandDependencyTarget(rawDep)
			if !sessionScoped || depCmd == "" {
				continue
			}
			for _, depKey := range sessionLeaves[depCmd] {
				if depKey != job.Key() {
					job.DependsOn = append(job.DependsOn, depKey)
				}
			}
		}
		job.DependsOn = dedupeSorted(job.DependsOn)
	}
}

func commandDependencyTarget(dep string) (target string, sessionScoped bool) {
	if strings.HasPrefix(dep, sessionCommandDependencyPrefix) {
		return strings.TrimPrefix(dep, sessionCommandDependencyPrefix), true
	}
	return dep, false
}

// plannedIndex provides O(1) lookups over a set of scheduled jobs by full key,
// by project ID, and by the identity names a reference may name. It is built
// once and reused across dependency-resolution queries so the planner stops
// rescanning the whole job list (and re-deriving job-name strings) on every
// query.
type plannedIndex struct {
	byKey     map[string]*ScheduledJob
	byProject map[string][]*ScheduledJob
	// byStepName indexes each job under its IDENTITY names only: the command
	// it belongs to and the step it is. These are the two things a reference is
	// allowed to name.
	byStepName map[string][]*ScheduledJob
	// byLegacyName indexes each job under its DERIVED spellings (display name,
	// plan name). Nothing resolves through it — it exists so a reference that
	// used to bind through one of those spellings can be reported instead of
	// silently losing its edge.
	byLegacyName map[string][]*ScheduledJob
}

func buildPlannedIndex(jobs []*ScheduledJob) *plannedIndex {
	idx := &plannedIndex{
		byKey:        make(map[string]*ScheduledJob, len(jobs)),
		byProject:    make(map[string][]*ScheduledJob),
		byStepName:   make(map[string][]*ScheduledJob),
		byLegacyName: make(map[string][]*ScheduledJob),
	}
	for _, job := range jobs {
		idx.byKey[job.Key()] = job
		pid := job.Project.ID
		idx.byProject[pid] = append(idx.byProject[pid], job)
		indexJobNames(idx.byStepName, job, job.CommandName(), job.StepID())
		indexJobNames(idx.byLegacyName, job, job.DisplayName(), job.PlanName())
	}
	return idx
}

// indexJobNames adds job to index under each distinct non-empty name. An empty
// name is skipped deliberately: a non-pipeline job has no step id, and matching
// it by "no name" is the accidental binding this slice removes.
func indexJobNames(index map[string][]*ScheduledJob, job *ScheduledJob, names ...string) {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		index[name] = append(index[name], job)
	}
}

// matchingKeys returns the keys of jobs in the given project that provide the
// named step or command, by EXACT identity: the job's command is cmdName and
// the job IS step stepID. No display name, plan name or synthesized key form
// participates — those are how a reference silently binds to the wrong job.
func (idx *plannedIndex) matchingKeys(projectID, cmdName, stepID string) []string {
	var keys []string
	for _, job := range idx.byProject[projectID] {
		if job.CommandName() == cmdName && job.StepID() == stepID {
			keys = append(keys, job.Key())
		}
	}
	return dedupeSorted(keys)
}

// legacyKeys returns the plan keys a REMOVED spelling would have bound for a
// reference that matched nothing exactly: a job in the same project whose
// display or plan name equals stepID, or the synthesized "<project>:<command>~
// <step>" key. Purely diagnostic — the caller turns a non-empty result into a
// plan-time error.
func (idx *plannedIndex) legacyKeys(projectID, cmdName, stepID string) []string {
	var keys []string
	for _, job := range idx.byLegacyName[stepID] {
		if job.Project.ID == projectID {
			keys = append(keys, job.Key())
		}
	}
	if key := projectID + ":" + extension.StepJobName(cmdName, stepID); idx.byKey[key] != nil {
		keys = append(keys, key)
	}
	return dedupeSorted(keys)
}

// legacyWildcardKeys is legacyKeys for the "*" reference, which spans projects:
// it reports the jobs a derived-name wildcard match would have pulled in.
func (idx *plannedIndex) legacyWildcardKeys(job *ScheduledJob, stepID string) []string {
	var keys []string
	for _, candidate := range idx.byLegacyName[stepID] {
		if candidate.Key() == job.Key() {
			continue
		}
		keys = append(keys, candidate.Key())
	}
	return dedupeSorted(keys)
}
