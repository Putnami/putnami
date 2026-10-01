package jobs

import (
	"path/filepath"
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

		// Check extension dependency: the project must list the extension in its
		// dependencies or extensions, unless activation files matched or the
		// project IS the extension (self-publish)
		// or the command has workspace activation.
		if !isProjectExtensionDep(proj, job, matchedActivationFiles) {
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

		// Secondary: prefer extensions listed in the project's devDependencies
		// or putnami.json extensions field
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

	return valid
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

// isProjectExtensionDep returns true if the project should use jobs from the
// given extension. A project qualifies if:
//  1. The command has workspace activation (activates for all projects), OR
//  2. The command's activation files matched the project, OR
//  3. The extension is listed in the project's Dependencies or Extensions, OR
//  4. The project IS the extension itself (self-publish exception).
//
// This matches the TypeScript SDK's filter in job.planning.ts.
func isProjectExtensionDep(proj *workspace.Project, job *extension.JobDefinition, matchedActivationFiles bool) bool {
	extName := job.ExtensionName
	extID := ""
	if job.ExtensionPath != "" {
		extID = workspace.ProjectIDFromPath(job.ExtensionPath)
	}
	cmdName := job.Name

	// Workspace-activated commands skip the dependency check
	if job.Activation == "workspace" || job.Activation == workspaceOnceActivation {
		return true
	}

	if matchedActivationFiles && cmdName == "config-extract" {
		return true
	}

	// Self-publish: extension projects can always package/publish themselves
	if proj.Name == extName && (cmdName == "publish" || cmdName == "package") {
		return true
	}

	// Check explicit dependencies and extensions.
	// Match against both the extension's publish name and its /path ID.
	for _, dep := range proj.Dependencies {
		if dep == extName || (extID != "" && dep == extID) {
			return true
		}
	}
	for _, ext := range proj.Extensions {
		if ext == extName || (extID != "" && ext == extID) {
			return true
		}
	}

	return false
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
