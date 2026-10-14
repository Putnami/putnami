package extension

import (
	"slices"

	extproto "go.putnami.dev/protocol/extension"
)

// CommandSideEffects returns what a run of command mutates outside the
// workspace, as the loaded extensions declare it: the distinct non-empty
// sideEffects traits ("cloud", "registry") of every loaded job whose base
// command name is command's, sorted. An empty result means the command changes
// nothing outside the workspace.
//
// The answer is the declared trait, never the command name. Discovery resolved
// each job's traits (extproto.EffectiveTraits), so a manifest that declares
// sideEffects on a verb of its own counts exactly as the default of publish
// does, and a manifest that declares traits for a well-known verb owns the
// answer for it: declared traits replace the verb's defaults wholesale. A
// command several extensions declare carries the side effects of all of them,
// because a run schedules all of them.
//
// A command no loaded extension declares falls back to the well-known verb's
// default traits. That keeps publish and deploy classified where the extension
// that provides them is not installed, and where discovery failed and
// extensions is nil: an unreadable manifest never removes a side effect.
func CommandSideEffects(extensions []*ExtensionDescription, command string) []string {
	name := extproto.BaseCommandName(command)
	declared := false
	var effects []string
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		for jobName, job := range ext.Jobs {
			if job == nil || extproto.BaseCommandName(jobName) != name {
				continue
			}
			declared = true
			if effect := job.Traits.SideEffects; effect != extproto.SideEffectsNone && !slices.Contains(effects, effect) {
				effects = append(effects, effect)
			}
		}
	}
	if !declared {
		if effect := extproto.EffectiveTraits(name, nil).SideEffects; effect != extproto.SideEffectsNone {
			effects = append(effects, effect)
		}
	}
	slices.Sort(effects)
	return effects
}
