package extension

import (
	"slices"

	extproto "go.putnami.dev/protocol/extension"
)

// CommandSideEffects returns what a run of command mutates outside the
// workspace: the distinct non-empty sideEffects traits ("cloud", "registry")
// of every loaded job whose base command name is command's, sorted. An empty
// result means the command changes nothing outside the workspace.
//
// It reads command traits only. The effects a task declares (declares.effects)
// are read by scheduler.HasExternalEffects.
//
// A job's traits are the ones its manifest declares, or its verb's default
// traits in the merged vocabulary when it declares none. The core gives no
// command a side effect of its own, so a command that no loaded extension
// declares, or a run whose discovery failed and passes nil extensions, has
// none. A command several extensions declare carries the side effects of all
// of them, because a run schedules all of them.
func CommandSideEffects(extensions []*ExtensionDescription, command string) []string {
	name := extproto.BaseCommandName(command)
	var effects []string
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		for jobName, job := range ext.Jobs {
			if job == nil || extproto.BaseCommandName(jobName) != name {
				continue
			}
			if effect := job.Traits.SideEffects; effect != extproto.SideEffectsNone && !slices.Contains(effects, effect) {
				effects = append(effects, effect)
			}
		}
	}
	slices.Sort(effects)
	return effects
}
