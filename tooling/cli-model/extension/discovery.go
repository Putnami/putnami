package extension

import (
	"sort"
)

// SkippedExtension records an extension that discovery found but could not
// load: its manifest exists yet failed to parse, or it requires a newer CLI
// contract than this putnami implements. Skips feed the provider gates and the
// release-archives guard so a failure three layers away can cite the root
// cause instead of reporting silent absence.
type SkippedExtension struct {
	// Ref is the reference discovery was resolving (config name, project
	// path, or devDependency name).
	Ref string
	// Name is the extension's best-known canonical name (package.json /
	// putnami.json, falling back to Ref).
	Name string
	// Path is the directory whose manifest failed to load.
	Path string
	// Version is the best-known installed version ("" when unknown).
	Version string
	// Reason is the load error.
	Reason error
}

// InstalledRemediation is appended to skew warnings for extensions loaded from
// the installed lock-pinned layout (.putnami/bin/extensions/…), where the fix
// is usually one command away.
const InstalledRemediation = "a newer published version may already fix this — run `putnami extensions update` or `putnami upgrade`"

// BuildJobMap creates a mapping from job name → list of job definitions.
// Multiple extensions can provide the same job (e.g., "build").
func BuildJobMap(extensions []*ExtensionDescription) map[string][]*JobDefinition {
	jobMap := make(map[string][]*JobDefinition)
	for _, ext := range extensions {
		for jobName, job := range ext.Jobs {
			jobMap[jobName] = append(jobMap[jobName], job)
		}
	}
	for jobName := range jobMap {
		sort.SliceStable(jobMap[jobName], func(i, j int) bool {
			a, b := jobMap[jobName][i], jobMap[jobName][j]
			if a.ExtensionName != b.ExtensionName {
				return a.ExtensionName < b.ExtensionName
			}
			return a.Name < b.Name
		})
	}
	return jobMap
}

// ExpandAlsoRunCommands widens a requested command list with every `alsoRuns`
// companion declared by a matching job definition. The expansion is a property
// of the manifest model, applied to the REQUEST before any activation is
// evaluated: a companion keeps its own activation, so a workspace-once
// companion still plans exactly once even when the requesting command
// activates nowhere. Breadth-first over a visited set — chains terminate,
// cycles cannot loop, and requesting a companion explicitly is a no-op.
func ExpandAlsoRunCommands(commands []string, jobMap map[string][]*JobDefinition) []string {
	visited := make(map[string]bool, len(commands))
	for _, name := range commands {
		visited[name] = true
	}
	expanded := append([]string(nil), commands...)
	for queue := append([]string(nil), commands...); len(queue) > 0; queue = queue[1:] {
		for _, jobDef := range jobMap[queue[0]] {
			for _, companion := range jobDef.AlsoRuns {
				if companion == "" || visited[companion] {
					continue
				}
				visited[companion] = true
				expanded = append(expanded, companion)
				queue = append(queue, companion)
			}
		}
	}
	return expanded
}

// FindExtensionByName returns the extension with the given name, or nil.
func FindExtensionByName(extensions []*ExtensionDescription, name string) *ExtensionDescription {
	for _, ext := range extensions {
		if ext.Name == name {
			return ext
		}
	}
	return nil
}
