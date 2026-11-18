package planner

import (
	"path/filepath"
	"slices"
	"sort"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const workspaceOnceActivation = "workspace-once"

func matchJobs(
	cmdName string,
	proj *workspace.Project,
	jobMap map[string][]*extension.JobDefinition,
	disabledJobs, disabledExts map[string]bool,
	workspaceRoot string,
	cache *planCache,
) []*extension.JobDefinition {
	candidates := jobMap[cmdName]
	if len(candidates) == 0 {
		return nil
	}

	// Library projects have no runnable entrypoint — skip commands whose
	// traits require one (serve and run by verb default).
	if proj.Type == "library" {
		requiresRunnable := false
		for _, def := range candidates {
			if def.Traits.RequiresRunnable {
				requiresRunnable = true
				break
			}
		}
		if requiresRunnable {
			return nil
		}
	}

	// Build per-project sets once for O(1) lookups
	var projDisabledJobs map[string]bool
	if proj.Config != nil && proj.Config.Disable != nil {
		projDisabledJobs = toSet(proj.Config.Disable.Jobs)
	}
	publishSet := toSet(proj.Publish)

	// Filter and sort by priority
	var valid []*extension.JobDefinition
	for _, job := range candidates {
		// Check workspace-level disabled
		if disabledJobs[cmdName] || disabledJobs[job.ExtensionName+":"+cmdName] {
			continue
		}
		if disabledExts[job.ExtensionName] {
			continue
		}

		// Check project-level disabled jobs
		if projDisabledJobs[cmdName] || projDisabledJobs[job.ExtensionName+":"+cmdName] {
			continue
		}

		// Check channel
		if job.Channel != "" && len(publishSet) > 0 {
			if !publishSet[job.Channel] {
				continue
			}
		}

		// Check activation files
		matchedActivationFiles := false
		if len(job.ActivationFiles) > 0 && workspaceRoot != "" {
			projRoot := filepath.Join(workspaceRoot, proj.Path)
			if !hasActivationFiles(projRoot, job.ActivationFiles, cache) {
				continue
			}
			matchedActivationFiles = true
		}

		// The project must use the job's extension (workspace.UsesJobExtension).
		if !workspace.UsesJobExtension(proj, job, matchedActivationFiles) {
			continue
		}

		valid = append(valid, job)
	}

	if len(valid) == 0 {
		return nil
	}

	// Sort by: 1) priority descending, 2) project extension match, 3) activation specificity.
	projRoot := ""
	if workspaceRoot != "" {
		projRoot = filepath.Join(workspaceRoot, proj.Path)
	}
	extSet := make(map[string]bool, len(proj.Extensions))
	for _, ext := range proj.Extensions {
		extSet[ext] = true
	}

	// Pre-compute activation results to avoid repeated filesystem walks
	// during sort comparisons. With a shared *planCache, repeated calls for
	// the same (extension, project) reuse the result across (project, command)
	// pairs instead of re-walking once per command.
	activationCache := make(map[string]bool)
	if projRoot != "" {
		for _, job := range valid {
			if _, ok := activationCache[job.ExtensionName]; !ok {
				activationCache[job.ExtensionName] = extensionActivatesForProject(job.ExtensionName, projRoot, jobMap, cache)
			}
		}
	}

	sort.SliceStable(valid, func(i, j int) bool {
		a, b := valid[i], valid[j]

		// Primary: higher priority wins
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}

		// Secondary: prefer extensions the project lists (proj.Extensions)
		aMatch := extSet[a.ExtensionName]
		bMatch := extSet[b.ExtensionName]
		if aMatch != bMatch {
			return aMatch
		}

		// Tertiary: prefer extensions that have other commands with matching
		// activation files for this project (indicates the extension is designed
		// for this project type)
		if projRoot != "" {
			aAct := activationCache[a.ExtensionName]
			bAct := activationCache[b.ExtensionName]
			if aAct != bAct {
				return aAct
			}
		}

		if a.ExtensionName != b.ExtensionName {
			return a.ExtensionName < b.ExtensionName
		}
		return a.Name < b.Name
	})

	return withProjectCommandDeps(valid, proj, cmdName)
}

// withProjectCommandDeps returns the job definitions a project plans for a
// command, each carrying, after the dependsOn its manifest declares, the
// commands the project's putnami.json names under commands.<command>.dependsOn
// (CLI ADR 0097). A definition is copied, never edited, since every project
// shares the manifest's; without such entries the definitions are returned as
// they are.
func withProjectCommandDeps(defs []*extension.JobDefinition, proj *workspace.Project, cmdName string) []*extension.JobDefinition {
	if proj == nil || proj.Config == nil {
		return defs
	}
	declared := proj.Config.Commands[cmdName].DependsOn
	if len(declared) == 0 {
		return defs
	}
	out := make([]*extension.JobDefinition, len(defs))
	for i, def := range defs {
		deps := slices.Clone(def.CommandDependsOn)
		for _, dep := range declared {
			if !slices.Contains(deps, dep) {
				deps = append(deps, dep)
			}
		}
		clone := cloneJobDefinition(def)
		clone.CommandDependsOn = deps
		out[i] = clone
	}
	return out
}

func matchWorkspaceOnceJobs(
	cmdName string,
	jobMap map[string][]*extension.JobDefinition,
	disabledJobs, disabledExts map[string]bool,
) []*extension.JobDefinition {
	candidates := jobMap[cmdName]
	if len(candidates) == 0 {
		return nil
	}

	var valid []*extension.JobDefinition
	for _, job := range candidates {
		if job.Activation != workspaceOnceActivation {
			continue
		}
		if disabledJobs[cmdName] || disabledJobs[job.ExtensionName+":"+cmdName] {
			continue
		}
		if disabledExts[job.ExtensionName] {
			continue
		}
		valid = append(valid, job)
	}
	if len(valid) == 0 {
		return nil
	}

	sort.SliceStable(valid, func(i, j int) bool {
		a, b := valid[i], valid[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.ExtensionName != b.ExtensionName {
			return a.ExtensionName < b.ExtensionName
		}
		return a.Name < b.Name
	})
	return valid
}

// extensionActivatesForProject checks whether an extension has any command
// with activation files that match the given project directory. The cache
// (if non-nil) collapses repeated probes for the same (extension, project)
// pair across all matchJob calls within a single Plan.
func extensionActivatesForProject(extName, projRoot string, jobMap map[string][]*extension.JobDefinition, cache *planCache) bool {
	if cache != nil {
		key := extName + "\x00" + projRoot
		if v, ok := cache.extActivates[key]; ok {
			return v
		}
		v := extensionActivatesForProjectUncached(extName, projRoot, jobMap, cache)
		cache.extActivates[key] = v
		return v
	}
	return extensionActivatesForProjectUncached(extName, projRoot, jobMap, cache)
}

func extensionActivatesForProjectUncached(extName, projRoot string, jobMap map[string][]*extension.JobDefinition, cache *planCache) bool {
	for _, jobs := range jobMap {
		for _, job := range jobs {
			if job.ExtensionName != extName {
				continue
			}
			if len(job.ActivationFiles) > 0 && hasActivationFiles(projRoot, job.ActivationFiles, cache) {
				return true
			}
		}
	}
	return false
}

// UnusedExtension is an extension that declares a job for some of a run's
// commands, and the selected projects that do not use it.
type UnusedExtension struct {
	Name     string
	Commands []string
	Projects []*workspace.Project
}

// UnusedExtensions explains an empty plan whose commands a loaded extension
// does declare: a project plans the jobs of an extension only once it uses
// that extension (workspace.UsesJobExtension), which for most projects means
// listing it in the `extensions` of its putnami.json. It returns, in name
// order, each extension serving one of commands that some of projects do not
// use, with the commands in the order given and those projects in the order
// given.
//
// A job the workspace or the project disables serves nothing. A project that
// already uses an extension serving a command is never named for it: its plan
// is empty for another reason, such as an activation file or a library that
// cannot serve, and no extension to add would fix it.
func UnusedExtensions(commands []string, projects []*workspace.Project, extensions []*extension.ExtensionDescription, disabledJobs, disabledExtensions []string) []UnusedExtension {
	jobMap := extension.BuildJobMap(extensions)
	disabledJobSet, disabledExtSet := toSet(disabledJobs), toSet(disabledExtensions)
	byName := map[string]*UnusedExtension{}
	for _, command := range commands {
		for _, project := range projects {
			if project == nil {
				continue
			}
			var projectDisabled map[string]bool
			if project.Config != nil && project.Config.Disable != nil {
				projectDisabled = toSet(project.Config.Disable.Jobs)
			}
			var serving []*extension.JobDefinition
			used := false
			for _, job := range jobMap[command] {
				if job == nil || job.ExtensionName == "" || disabledExtSet[job.ExtensionName] ||
					disabledJobSet[command] || disabledJobSet[job.ExtensionName+":"+command] ||
					projectDisabled[command] || projectDisabled[job.ExtensionName+":"+command] {
					continue
				}
				if workspace.UsesJobExtension(project, job, false) {
					used = true
					break
				}
				serving = append(serving, job)
			}
			if used {
				continue
			}
			for _, job := range serving {
				unused := byName[job.ExtensionName]
				if unused == nil {
					unused = &UnusedExtension{Name: job.ExtensionName}
					byName[job.ExtensionName] = unused
				}
				if !slices.Contains(unused.Commands, command) {
					unused.Commands = append(unused.Commands, command)
				}
				if !slices.Contains(unused.Projects, project) {
					unused.Projects = append(unused.Projects, project)
				}
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	unused := make([]UnusedExtension, 0, len(names))
	for _, name := range names {
		unused = append(unused, *byName[name])
	}
	return unused
}
