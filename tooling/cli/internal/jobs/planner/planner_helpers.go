package planner

import (
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// planCache owns invocation-stable planner facts and memoizes the filesystem
// probes repeated while matching every (project, command) pair. Activation
// files and the effective command facts don't change during a Plan invocation,
// so the same answers are shared by every pipeline expansion.
type planCache struct {
	// effectiveCommands is keyed by project ID + NUL + provider name. Each value
	// is the immutable subset of requested root commands that actually matched
	// that provider on that selected project.
	effectiveCommands map[string]map[string]bool
	// effectiveCommandParams uses the same key, then command name, to retain the
	// fully resolved parameter bag for each effective command.
	effectiveCommandParams map[string]map[string]extension.ParamMap

	// extActivates is keyed by extName + "\x00" + projRoot and memoizes the
	// "any job of this extension that activates in this project?" answer.
	extActivates map[string]bool

	// hasFiles is keyed by projRoot + "\x00" + joined patterns and memoizes
	// the filesystem-glob result. Patterns are joined with "\x01" — both
	// separators are illegal in filesystem paths and pattern globs, so no
	// real key can collide with another.
	hasFiles map[string]bool
	// projectRoots is the loaded workspace's complete project inventory. A
	// recursive activation probe may enter ordinary source directories, but
	// not a nested project that owns its own files.
	projectRoots map[string]bool

	// fileContains is keyed by absolutePath + "\x00" + substring and memoizes
	// step-activation content probes (e.g. "does this go.mod require the app
	// framework?") so the same file is read at most once per Plan.
	fileContains map[string]bool

	// closureRoots is keyed by project ID and memoizes the absolute roots of a
	// project's dependency closure, so a closureFiles gate walks the graph once
	// per project instead of once per (project, command, step).
	closureRoots map[string][]string

	// ownedChangedFiles is keyed by project ID and holds the run's changed files
	// each project owns, project-relative (workspace.OwnedChangedFiles). A
	// changedFiles gate and a context binding read it; it is nil when the run
	// measured no diff, so a gated step plans nowhere and a binding binds
	// nothing.
	ownedChangedFiles map[string][]string
}

// newPlanCache returns an empty cache; callers do not need to know its shape.
func newPlanCache() *planCache {
	return &planCache{
		effectiveCommands:      make(map[string]map[string]bool),
		effectiveCommandParams: make(map[string]map[string]extension.ParamMap),
		extActivates:           make(map[string]bool),
		hasFiles:               make(map[string]bool),
		projectRoots:           make(map[string]bool),
		fileContains:           make(map[string]bool),
		closureRoots:           make(map[string][]string),
	}
}

func hasActivationFiles(projRoot string, patterns []string, cache *planCache) bool {
	if cache != nil {
		key := projRoot + "\x00" + strings.Join(patterns, "\x01")
		if v, ok := cache.hasFiles[key]; ok {
			return v
		}
		v := hasActivationFilesUncached(projRoot, patterns, cache.projectRoots)
		cache.hasFiles[key] = v
		return v
	}
	return hasActivationFilesUncached(projRoot, patterns, nil)
}

func hasActivationFilesUncached(projRoot string, patterns []string, projectRoots map[string]bool) bool {
	return workspace.HasActivationFiles(projRoot, patterns, projectRoots)
}

// mergeCommandParams builds the full resolved params for plan-time `if` evaluation.
// Merge order matches the TS SDK's evalJobOptions and BuildJobContext:
// workspace config defaults → manifest flag defaults → project options → CLI flags.
func mergeCommandParams(
	ws *workspace.Workspace,
	proj *workspace.Project,
	ext *extension.ExtensionDescription,
	cmdName string,
	jobDef *extension.JobDefinition,
	commandParams map[string]any,
) map[string]any {
	var configDefaults map[string]any
	if ws.Config != nil {
		configDefaults = ws.Config.GetCommandDefaults(cmdName, ext.Name)
	}

	// Commands with the injectPublishChannels trait (package/publish by
	// verb default) auto-activate the project's declared channels, so
	// --go/--docker CLI flags aren't required.
	var publishChannels []string
	if jobDef.Traits.InjectPublishChannels {
		publishChannels = proj.Publish
	}

	return cachekey.MergeParamLayers(configDefaults, jobDef, nil, proj, ext.Name, cmdName, publishChannels, commandParams)
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}
