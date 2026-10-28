package extension

import (
	"slices"

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

// CommandPublishes reports whether a run of command publishes to a registry:
// whether a loaded job whose base command name is command's declares the
// registry side effect. A planned job answers the same question by its own
// JobDef.Traits.SideEffects. A command that no loaded extension declares, or a
// run whose discovery failed and passes nil extensions, publishes nothing.
func CommandPublishes(extensions []*ExtensionDescription, command string) bool {
	return slices.Contains(CommandSideEffects(extensions, command), extproto.SideEffectsRegistry)
}

// MemberPackageCommand names the command that packages a release-set member:
// the one same-project prerequisite of the publisher's publishCommand that
// the package publisher declares with the injectPublishChannels trait and a
// pipeline step whose id is packageStep. It returns "" when no command, or
// more than one, matches, so the coordinator and the selection fingerprint
// refuse the member instead of guessing.
//
// A step id alone cannot name the command: one extension can run the same
// step in several commands, such as a package and a validate that both check
// a member, or a build and a package that both describe it.
func MemberPackageCommand(extensions []*ExtensionDescription, publisher, packagePublisher, publishCommand, packageStep string) string {
	var publishes, packages *ExtensionDescription
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		if ext.Name == publisher {
			publishes = ext
		}
		if ext.Name == packagePublisher {
			packages = ext
		}
	}
	if publishes == nil || packages == nil || publishes.Jobs[publishCommand] == nil {
		return ""
	}
	found := ""
	for _, dependency := range publishes.Jobs[publishCommand].CommandDependsOn {
		parsed := ParseCommandDependency(dependency)
		if parsed.Session {
			continue
		}
		command := parsed.Command
		job := packages.Jobs[command]
		if command == publishCommand || job == nil || !job.Traits.InjectPublishChannels || command == found {
			continue
		}
		if !slices.ContainsFunc(job.PipelineSteps, func(step PipelineStep) bool { return step.ID == packageStep }) {
			continue
		}
		if found != "" {
			return ""
		}
		found = command
	}
	return found
}
