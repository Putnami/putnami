package jobs

import (
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

var projectWalkExcludedDirs = map[string]bool{
	".git": true, ".putnami": true, ".venv": true,
	"dist": true, "node_modules": true, "vendor": true,
}

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

	// fileContains is keyed by absolutePath + "\x00" + substring and memoizes
	// step-activation content probes (e.g. "does this go.mod require the app
	// framework?") so the same file is read at most once per Plan.
	fileContains map[string]bool

	// closureRoots is keyed by project ID and memoizes the absolute roots of a
	// project's dependency closure, so a closureFiles gate walks the graph once
	// per project instead of once per (project, command, step).
	closureRoots map[string][]string
}

// newPlanCache returns an empty cache; callers do not need to know its shape.
func newPlanCache() *planCache {
	return &planCache{
		effectiveCommands:      make(map[string]map[string]bool),
		effectiveCommandParams: make(map[string]map[string]extension.ParamMap),
		extActivates:           make(map[string]bool),
		hasFiles:               make(map[string]bool),
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
		v := hasActivationFilesUncached(projRoot, patterns)
		cache.hasFiles[key] = v
		return v
	}
	return hasActivationFilesUncached(projRoot, patterns)
}

func hasActivationFilesUncached(projRoot string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(pattern, "**") {
			if matchDoubleStarGlob(projRoot, pattern) {
				return true
			}
		} else {
			matches, _ := filepath.Glob(filepath.Join(projRoot, pattern))
			if len(matches) > 0 {
				return true
			}
		}
	}
	return false
}

// matchDoubleStarGlob handles glob patterns containing ** (recursive match).
// Go's filepath.Glob does not support **. This function walks the directory
// tree and matches each file against the non-** suffix of the pattern.
func matchDoubleStarGlob(root, pattern string) bool {
	// Split on "**/" or "**" to get the suffix pattern.
	// e.g. "**/*.test.ts" → suffix "*.test.ts"
	// e.g. "src/**/*.go"  → prefix "src", suffix "*.go"
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.TrimSuffix(parts[0], string(filepath.Separator))
	suffix := strings.TrimPrefix(parts[1], string(filepath.Separator))
	// Also handle forward slashes (JSON manifests use them)
	suffix = strings.TrimPrefix(suffix, "/")

	searchRoot := root
	if prefix != "" {
		searchRoot = filepath.Join(root, prefix)
	}

	found := false
	_ = filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			if found {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			// Skip common non-project directories for performance
			if projectWalkExcludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Match the filename against the suffix pattern
		matched, _ := filepath.Match(suffix, d.Name())
		if matched {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
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

	return mergeParamLayers(configDefaults, jobDef, nil, proj, ext.Name, cmdName, publishChannels, commandParams)
}

// mergeParamLayers merges the layered param sources shared by plan-time `if`
// evaluation (mergeCommandParams) and the job context file (BuildJobContext)
// into a single bag, lowest to highest priority:
//
//  1. Manifest flag defaults
//  2. Workspace config defaults (configDefaults)
//  3. Job definition defaults (jobDefaults, nil to skip)
//  4. Project-level options (options.{cmd}, options.{ext}, options.{ext}:{cmd})
//  5. Publish channels forced to true (publishChannels, nil to skip)
//  6. CLI flags (commandParams)
func mergeParamLayers(
	configDefaults map[string]any,
	jobDef *extension.JobDefinition,
	jobDefaults map[string]string,
	proj *workspace.Project,
	extName, cmdName string,
	publishChannels []string,
	commandParams map[string]any,
) map[string]any {
	params := make(map[string]any)

	// Manifest flag defaults (lowest priority — overridden by everything else)
	flagDefaults := make(extension.ParamMap)
	if jobDef.Flags != nil {
		for name, def := range jobDef.Flags {
			if def.Default != nil {
				flagDefaults[name] = def.Default
			}
		}
	}
	applyProjectedParamLayer(params, flagDefaults)

	// Workspace config defaults (options.*, options.{cmd}, options.{ext}, options.{ext}:{cmd})
	applyProjectedParamLayer(params, configDefaults)

	resolvedJobDefaults := make(extension.ParamMap, len(jobDefaults))
	for k, v := range jobDefaults {
		resolvedJobDefaults[k] = v
	}
	applyProjectedParamLayer(params, resolvedJobDefaults)

	// Project-level options (options.{cmd}, options.{ext}, options.{ext}:{cmd})
	if proj.Config != nil && proj.Config.Options != nil {
		if projCmdOpts, ok := proj.Config.Options[cmdName]; ok {
			applyProjectedParamLayer(params, projCmdOpts)
		}
		if projExtOpts, ok := proj.Config.Options[extName]; ok {
			applyProjectedParamLayer(params, projExtOpts)
		}
		if projExtCmdOpts, ok := proj.Config.Options[extName+":"+cmdName]; ok {
			applyProjectedParamLayer(params, projExtCmdOpts)
		}
	}

	publishDefaults := make(extension.ParamMap, len(publishChannels))
	for _, ch := range publishChannels {
		publishDefaults[ch] = true
	}
	applyProjectedParamLayer(params, publishDefaults)

	// CLI flags (highest priority)
	applyProjectedParamLayer(params, commandParams)

	return params
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}
