package cachekey

import (
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

var ProjectWalkExcludedDirs = map[string]bool{
	".git": true, ".putnami": true, ".venv": true,
	"dist": true, "node_modules": true, "vendor": true,
}

// MergeParamLayers merges the layered param sources shared by plan-time `if`
// evaluation (mergeCommandParams) and the job context file (BuildJobContext)
// into a single bag, lowest to highest priority:
//
//  1. Manifest flag defaults
//  2. Workspace config defaults (configDefaults)
//  3. Job definition defaults (jobDefaults, nil to skip)
//  4. Project-level options (options.{cmd}, options.{ext}, options.{ext}:{cmd})
//  5. Publish channels forced to true (publishChannels, nil to skip)
//  6. CLI flags (commandParams)
func MergeParamLayers(
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
	ApplyProjectedParamLayer(params, flagDefaults)

	// Workspace config defaults (options.*, options.{cmd}, options.{ext}, options.{ext}:{cmd})
	ApplyProjectedParamLayer(params, configDefaults)

	resolvedJobDefaults := make(extension.ParamMap, len(jobDefaults))
	for k, v := range jobDefaults {
		resolvedJobDefaults[k] = v
	}
	ApplyProjectedParamLayer(params, resolvedJobDefaults)

	// Project-level options (options.{cmd}, options.{ext}, options.{ext}:{cmd})
	if proj.Config != nil && proj.Config.Options != nil {
		if projCmdOpts, ok := proj.Config.Options[cmdName]; ok {
			ApplyProjectedParamLayer(params, projCmdOpts)
		}
		if projExtOpts, ok := proj.Config.Options[extName]; ok {
			ApplyProjectedParamLayer(params, projExtOpts)
		}
		if projExtCmdOpts, ok := proj.Config.Options[extName+":"+cmdName]; ok {
			ApplyProjectedParamLayer(params, projExtCmdOpts)
		}
	}

	publishDefaults := make(extension.ParamMap, len(publishChannels))
	for _, ch := range publishChannels {
		publishDefaults[ch] = true
	}
	ApplyProjectedParamLayer(params, publishDefaults)

	// CLI flags (highest priority)
	ApplyProjectedParamLayer(params, commandParams)

	return params
}
