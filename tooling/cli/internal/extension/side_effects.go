package extension

import (
	"slices"

	extproto "go.putnami.dev/protocol/extension"
)

// CommandSideEffects returns what a run of command mutates outside the
// workspace: the distinct non-empty sideEffects traits ("cloud", "registry")
// of every loaded job whose base command name is command's, together with the
// well-known verb's default, sorted. An empty result means the command changes
// nothing outside the workspace.
//
// It reads command traits only. The effects a task declares (declares.effects)
// are read by jobs.HasExternalEffects.
//
// Declared traits add side effects and never remove one: a manifest that
// declares sideEffects on a verb of its own counts exactly as the default of
// publish does, and the well-known verb's default always stays, so no manifest
// removes registry from publish or cloud from deploy. A command several
// extensions declare carries the side effects of all of them, because a run
// schedules all of them. The default also stands where the extension that
// provides the verb is not installed, and where discovery failed and
// extensions is nil: an unreadable manifest never removes a side effect.
func CommandSideEffects(extensions []*ExtensionDescription, command string) []string {
	name := extproto.BaseCommandName(command)
	var effects []string
	if effect := extproto.EffectiveTraits(name, nil).SideEffects; effect != extproto.SideEffectsNone {
		effects = append(effects, effect)
	}
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
