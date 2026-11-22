package planner

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ChannelGap is a selected project that a packaging command packages nothing
// for: every channel the command offers it is off.
type ChannelGap struct {
	Project *workspace.Project
	Command string
	// Channels are the channels the command offers the project, sorted: the
	// keys to turn on under options.package, or the flags to pass.
	Channels []string
}

// stepParamReference matches a `params.<name>` reference in a step condition
// and the character before it, so a negated reference can be told apart.
var stepParamReference = regexp.MustCompile(`(^|[^!A-Za-z0-9_.-])params\.([A-Za-z0-9_-]+)`)

// isPackagingJob reports whether job packages: it declares the
// injectPublishChannels trait and no registry side effect, so `publish` is
// not one.
func isPackagingJob(job *extension.JobDefinition) bool {
	return job != nil && job.Traits.InjectPublishChannels && job.Traits.SideEffects != extension.SideEffectsRegistry
}

// gatingParams returns the parameters a step condition reads without
// negation, such as docker in `params.docker && !params.image`, that may turn
// a packaging step on: a declared boolean flag, or a parameter the job does
// not declare, such as image, which an image project sets in its options. A
// declared flag of another type, such as a compiler choice, gates a build
// step, not a channel.
func gatingParams(job *extension.JobDefinition, condition string) []string {
	var names []string
	for _, match := range stepParamReference.FindAllStringSubmatch(condition, -1) {
		name := match[2]
		if flag, declared := job.Flags[name]; declared && flag.Type != "boolean" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// PackageChannels returns the channels of a packaging job, sorted: the
// declared boolean flags a step condition reads without negation, such as
// docker, the keys to set under options.package or the flags to pass. A flag
// no condition reads, such as dry-run, and a negated one are not channels. It
// returns nil for any other job, and for a packaging job that offers no
// channel, which packages whatever its steps make.
func PackageChannels(job *extension.JobDefinition) []string {
	if !isPackagingJob(job) {
		return nil
	}
	var channels []string
	for _, step := range job.PipelineSteps {
		for _, name := range gatingParams(job, step.If) {
			if _, declared := job.Flags[name]; declared && !slices.Contains(channels, name) {
				channels = append(channels, name)
			}
		}
	}
	sort.Strings(channels)
	return channels
}

// InactiveChannels returns, for each of commands and each of projects in the
// order given, the project that a packaging job of the command matches while
// no step of a matching packaging job that a gating parameter turns on would
// be planned, in the parameters the project resolves: its options, the
// channels its publish list injects and commandParams, the run's flags. The
// planner would plan no packaging step for such a project, or only the build
// steps that precede one, and the run would succeed having packaged nothing.
//
// A project that a packaging job offering no channel matches, such as one that
// always writes its member, or that no packaging job matches, is never
// returned.
func InactiveChannels(
	ws *workspace.Workspace,
	commands []string,
	projects []*workspace.Project,
	extensions []*extension.ExtensionDescription,
	commandParams extension.ParamMap,
	disabledJobs, disabledExtensions []string,
) []ChannelGap {
	jobMap := extension.BuildJobMap(extensions)
	disabledJobSet, disabledExtSet := toSet(disabledJobs), toSet(disabledExtensions)
	cache := newPlanCache()
	var gaps []ChannelGap
	for _, command := range commands {
		for _, project := range projects {
			if project == nil {
				continue
			}
			var offered []string
			on := false
			for _, job := range excludeWorkspaceOnceJobs(matchJobs(command, project, jobMap, disabledJobSet, disabledExtSet, ws.Root, cache)) {
				if !isPackagingJob(job) {
					continue
				}
				channels := PackageChannels(job)
				if len(channels) == 0 {
					// A packaging job that offers no channel packages what
					// its steps make whatever the options say.
					on = true
					continue
				}
				ext := extension.FindExtensionByName(extensions, job.ExtensionName)
				if ext == nil {
					continue
				}
				params := mergeCommandParams(ws, project, ext, command, job, commandParams)
				when := &extension.WhenContext{Params: params}
				for _, step := range job.PipelineSteps {
					if len(gatingParams(job, step.If)) > 0 && extension.EvaluateExpression(step.If, when) {
						on = true
					}
				}
				for _, channel := range channels {
					if !slices.Contains(offered, channel) {
						offered = append(offered, channel)
					}
				}
			}
			if on || len(offered) == 0 {
				continue
			}
			sort.Strings(offered)
			gaps = append(gaps, ChannelGap{Project: project, Command: command, Channels: offered})
		}
	}
	return gaps
}

// NamedProjects returns the IDs of the projects selector names one by one:
// a comma-separated entry of a --projects selector, or of a positional
// target, names a project when it spells the project's name, ID or path, or
// is "." inside it. A glob, an alias, a group or a directory entry names no
// project, even one that resolves to a single project, and neither does a
// subtraction.
func NamedProjects(ws *workspace.Workspace, selector string) map[string]bool {
	var named map[string]bool
	for _, entry := range strings.Split(selector, ",") {
		entry = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(entry), "./"), "/")
		if entry == "" || strings.HasPrefix(entry, "-") {
			continue
		}
		resolved := workspace.FilterProjects(ws, workspace.FilterOptions{
			Projects: entry, DirectTarget: true, ScopeIndex: ws.ScopeIndex,
		})
		if len(resolved) != 1 {
			continue
		}
		project := resolved[0]
		if entry != "." && entry != project.Name && entry != project.ID &&
			entry != project.Path && entry != "/"+project.Path {
			continue
		}
		if named == nil {
			named = make(map[string]bool)
		}
		named[project.ID] = true
	}
	return named
}

// ChannelGapMessages words gaps for the run: refusals name the projects of
// named, the ones the caller named, and warnings name the others. Gaps of one
// command that offer the same channels share one message, which names the
// command, its projects, the option that turns a channel on in a project's
// putnami.json and the flags.
func ChannelGapMessages(gaps []ChannelGap, named map[string]bool) (refusals, warnings []string) {
	type group struct {
		command, channels string
		refused, warned   []*workspace.Project
	}
	var groups []*group
	for _, gap := range gaps {
		channels := strings.Join(gap.Channels, ",")
		index := slices.IndexFunc(groups, func(g *group) bool { return g.command == gap.Command && g.channels == channels })
		if index < 0 {
			groups = append(groups, &group{command: gap.Command, channels: channels})
			index = len(groups) - 1
		}
		if named[gap.Project.ID] {
			groups[index].refused = append(groups[index].refused, gap.Project)
		} else {
			groups[index].warned = append(groups[index].warned, gap.Project)
		}
	}
	message := func(command string, channels []string, projects []*workspace.Project) string {
		ids := make([]string, 0, len(projects))
		for _, project := range projects {
			if len(ids) == 10 {
				ids = append(ids, "…")
				break
			}
			ids = append(ids, project.ID)
		}
		subject := "the project's"
		if len(projects) > 1 {
			subject = "each project's"
		}
		return fmt.Sprintf(
			"%s packages nothing for %s: no channel is on. Turn one on under options.%s in %s putnami.json, such as \"options\": {\"%s\": {\"%s\": true}}, or pass its flag: --%s",
			command, strings.Join(ids, ", "), command, subject, command, channels[0], strings.Join(channels, ", --"))
	}
	for _, g := range groups {
		channels := strings.Split(g.channels, ",")
		if len(g.refused) > 0 {
			refusals = append(refusals, message(g.command, channels, g.refused))
		}
		if len(g.warned) > 0 {
			warnings = append(warnings, message(g.command, channels, g.warned))
		}
	}
	return refusals, warnings
}
