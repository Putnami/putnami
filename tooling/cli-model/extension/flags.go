package extension

import (
	"fmt"
	"reflect"
	"sort"
)

// MergeCommandFlags returns the deterministic union of flags declared by the
// active job definitions for a command. Re-declaring the same flag is allowed
// when its behavioral definition matches; a differing Description is tolerated
// (see FlagDefsCompatible) because independently-released extensions routinely
// word a shared flag slightly differently and that must not break the command.
func MergeCommandFlags(commandName string, jobs []*JobDefinition) (map[string]FlagDefinition, error) {
	if len(jobs) == 0 {
		return nil, nil
	}

	sorted := sortedJobDefinitions(jobs)

	merged := make(map[string]FlagDefinition)
	owners := make(map[string]string)
	for _, job := range sorted {
		flagNames := make([]string, 0, len(job.Flags))
		for name := range job.Flags {
			flagNames = append(flagNames, name)
		}
		sort.Strings(flagNames)

		for _, name := range flagNames {
			def := job.Flags[name]
			if existing, ok := merged[name]; ok {
				if !FlagDefsCompatible(existing, def) {
					return nil, fmt.Errorf(
						"command %q flag --%s has incompatible definitions in %s and %s",
						commandName,
						name,
						owners[name],
						job.ExtensionName,
					)
				}
				continue
			}
			merged[name] = def
			owners[name] = job.ExtensionName
		}
	}

	if len(merged) == 0 {
		return nil, nil
	}
	return merged, nil
}

// FlagDefsCompatible reports whether two declarations of the same flag can
// coexist behind one CLI token. Every behavioral field (type, short alias,
// default, required, choices) must match, but a differing Description is
// tolerated: it is human-facing help text, and requiring byte-identical
// descriptions across independently-versioned extensions made cosmetic wording
// drift on a shared flag (e.g. `publish --stable` in @putnami/cloud vs
// @putnami/go) fail the entire command. The first-declared description wins
// (MergeCommandFlags keeps the existing entry).
//
// Exported for the CLI's parse pass, which asks the same question ACROSS the
// tasks of a comma-composed invocation (`putnami build,publish --stable`) —
// MergeCommandFlags only ever asked it within one command.
func FlagDefsCompatible(a, b FlagDefinition) bool {
	a.Description = ""
	b.Description = ""
	return reflect.DeepEqual(a, b)
}

// CollectCommandFlags returns a deterministic, non-validating union of flags
// declared by job definitions for help and shell completion. Execution keeps
// using MergeCommandFlags so incompatible shared flags still fail before jobs
// run, but discovery surfaces the flag name instead of hiding the whole command
// surface.
func CollectCommandFlags(jobs []*JobDefinition) map[string]FlagDefinition {
	if len(jobs) == 0 {
		return nil
	}

	merged := make(map[string]FlagDefinition)
	for _, job := range sortedJobDefinitions(jobs) {
		flagNames := make([]string, 0, len(job.Flags))
		for name := range job.Flags {
			flagNames = append(flagNames, name)
		}
		sort.Strings(flagNames)

		for _, name := range flagNames {
			if _, exists := merged[name]; exists {
				continue
			}
			merged[name] = job.Flags[name]
		}
	}

	if len(merged) == 0 {
		return nil
	}
	return merged
}

// MergeFlagLayers merges flag layers in increasing order of precedence: a later
// layer overrides an earlier one on a name collision. Returns nil when every
// layer is empty. Resolves a group subcommand's effective flag surface — flat
// command target (base), group shared flags, then the subcommand's own flags.
func MergeFlagLayers(layers ...map[string]FlagDefinition) map[string]FlagDefinition {
	merged := make(map[string]FlagDefinition)
	for _, layer := range layers {
		for name, def := range layer {
			merged[name] = def
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// ValidateCommandFlags reports whether a command's active contributors can
// share one CLI flag surface.
func ValidateCommandFlags(commandName string, jobs []*JobDefinition) error {
	_, err := MergeCommandFlags(commandName, jobs)
	return err
}

func sortedJobDefinitions(jobs []*JobDefinition) []*JobDefinition {
	sorted := append([]*JobDefinition(nil), jobs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ExtensionName != sorted[j].ExtensionName {
			return sorted[i].ExtensionName < sorted[j].ExtensionName
		}
		return sorted[i].Name < sorted[j].Name
	})
	return sorted
}
