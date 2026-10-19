package extension

import (
	extproto "go.putnami.dev/protocol/extension"
)

// CommandConsumesReleaseSet reports whether a run of command binds a release
// set and an environment: whether a loaded job whose base command name is
// command's carries the consumesReleaseSet trait. A planned job answers the
// same question by its own JobDef.Traits.ConsumesReleaseSet.
//
// A job's traits are the ones its manifest declares, or its verb's default
// traits in the merged vocabulary when it declares none. The core names no
// command that consumes a release set, so a command that no loaded extension
// declares, or a run whose discovery failed and passes nil extensions,
// consumes none.
func CommandConsumesReleaseSet(extensions []*ExtensionDescription, command string) bool {
	name := extproto.BaseCommandName(command)
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		for jobName, job := range ext.Jobs {
			if job != nil && job.Traits.ConsumesReleaseSet && extproto.BaseCommandName(jobName) == name {
				return true
			}
		}
	}
	return false
}
