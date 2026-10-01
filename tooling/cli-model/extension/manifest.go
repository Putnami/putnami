package extension

import (
	"slices"
	"sort"

	proto "go.putnami.dev/protocol/extension"
)

// LoadManifest reads and parses a putnami.extension.json file, negotiating the
// manifest's declared cliContract against this CLI's contract version. Since
// contract 3 the negotiation has no recoverable arm: a manifest that does not
// declare this CLI's contract does not load. See the protocol package for the
// enforce/reject matrix.
func LoadManifest(path string) (*Manifest, error) {
	return proto.LoadManifest(path)
}

// Resolve converts a raw manifest into an ExtensionDescription ready for
// use by the planner. It expands template variables and builds the job map.
func Resolve(manifest *Manifest, extPath string) *ExtensionDescription {
	desc := &ExtensionDescription{
		Name:              manifest.Name,
		Version:           manifest.Version,
		Path:              extPath,
		Commands:          make(map[string]string),
		CommandVisibility: make(map[string]string),
		CommandGroups:     manifest.CommandGroups,
		Jobs:              make(map[string]*JobDefinition),
		ExtDeps:           manifest.ExtensionDeps,
		Hooks:             manifest.Hooks,
		AutoServe:         manifest.AutoServe == nil || *manifest.AutoServe,
		Tasks:             manifest.Tasks,
		Contracts:         manifest.Contracts,
		Tools:             manifest.Tools,
		Runtime:           manifest.Runtime,
		Workspace:         manifest.Workspace,
		Ecosystems:        manifest.Ecosystems,
		Uses:              manifest.Uses,
	}

	for cmdName, cmd := range manifest.Commands {
		desc.Commands[cmdName] = cmd.Description
		desc.CommandVisibility[cmdName] = cmd.Visibility

		job := &JobDefinition{
			ExtensionName:        manifest.Name,
			Name:                 cmdName,
			Visibility:           cmd.Visibility,
			Kind:                 "command",
			ActivationFiles:      cmd.ActivationFiles,
			Activation:           cmd.Activation,
			Channel:              cmd.Channel,
			CommandDependsOn:     cmd.DependsOn,
			SessionPrerequisites: cmd.SessionPrerequisites,
			AlsoRuns:             cmd.AlsoRuns,
			Flags:                cmd.Flags,
			Priority:             cmd.Priority,
			Quiet:                cmd.Quiet,
			Traits:               proto.EffectiveTraits(cmdName, cmd.Traits),
			PipelineSteps:        cmd.Run,
			PipelineOutputs:      cmd.Outputs,
		}

		if cmd.Defaults != nil {
			job.Defaults = make(map[string]string)
			for k, v := range cmd.Defaults {
				if s, ok := v.(string); ok {
					job.Defaults[k] = s
				}
			}
		}

		// For single-step pipelines without a named task, populate command/args from the task.
		if len(cmd.Run) == 1 {
			taskName := cmd.Run[0].Task
			if task, ok := manifest.Tasks[taskName]; ok {
				job.Command = task.Command
				job.Args = task.Args
				job.Cwd = task.Cwd
				job.Env = task.Env
				job.Toolchains = runtimeToolchainRefs(manifest.Runtime, task.Toolchains)
				job.TimeoutMs = task.TimeoutMs
				job.ContractDigest = proto.TaskContractDigest(task)
				job.Writes = task.Writes
				job.Reads = task.Reads
				job.Resources = task.Resources
				job.Cache = task.Cache.IsEnabled()
				job.Batchable = task.Batchable
				// v2: derive cache key from task.Inputs
				if len(task.Inputs) > 0 {
					derivedKey := DeriveTaskCacheKey(task.Inputs)
					job.FilePatterns = derivedKey.Files
					policy := &TaskCachePolicy{Key: &derivedKey}
					if task.Cache != nil {
						policy.Enabled = task.Cache.Enabled
						policy.Deterministic = task.Cache.Deterministic
						policy.VersionAware = task.Cache.VersionAware
						policy.NoOutput = task.Cache.NoOutput
					}
					job.TaskCachePolicy = policy
				} else if task.Cache != nil {
					job.TaskCachePolicy = task.Cache
					if task.Cache.Key != nil {
						job.FilePatterns = task.Cache.Key.Files
					}
				}
			}
		}

		desc.Jobs[cmdName] = job
	}

	return desc
}

// runtimeToolchainRefs merges a task's own requirements with the ones every
// task of the runtime carries, then sorts and drops repeats. The two lists
// legitimately overlap — a task may restate a run-wide requirement — and a
// repeat would resolve the same toolchain twice, publish its environment twice,
// and name it twice in the cache identity.
func runtimeToolchainRefs(runtime *RuntimeDefinition, task []string) []string {
	if runtime == nil && len(task) == 0 {
		return nil
	}
	refs := append([]string(nil), task...)
	if runtime != nil {
		refs = append(refs, runtime.RunToolchains...)
	}
	sort.Strings(refs)
	return slices.Compact(refs)
}

// ExpandTemplateVars replaces template variables in a string.
func ExpandTemplateVars(s string, vars map[string]string) string {
	return proto.ExpandTemplateVars(s, vars)
}

// BuildTemplateVars builds the standard template variable map.
func BuildTemplateVars(workspaceRoot, projectRoot, extensionRoot, outputRoot string) map[string]string {
	return proto.BuildTemplateVars(workspaceRoot, projectRoot, extensionRoot, outputRoot)
}
