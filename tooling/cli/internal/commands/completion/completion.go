package completion

import (
	"path/filepath"
	"sort"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/template"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

type completionValue struct {
	Value       string
	Description string
}

// completionContext gathers dynamic workspace info for completion scripts.
//
// The built-in half — structured command names, their descriptions, their
// subcommands, their flags, and the global flags — is SEEDED FROM THE COMMAND
// CATALOG (internal/commandmeta) rather than retyped here. Before that change
// it was a hand-maintained copy of the registry that had stopped
// being updated: eight registered commands never tab-completed in any shell and
// eleven documented subcommands were never offered, and each of the three
// generators carried its own wording of the same flag descriptions.
//
// The maps stay mutable because the DYNAMIC half is layered on top: extension
// command groups add structured names and nested subcommand paths, and extension
// jobs add flags (gatherCompletionContext below).
type completionContext struct {
	commands             []string          // all known job command names
	commandDesc          map[string]string // job command descriptions
	projects             []string          // all project names
	projectIDs           []string          // all project IDs (e.g. "/typescript/frameworks/application")
	projectBasenames     []string          // project path basenames (e.g. "application", "http") — resolvable short-form targets
	aliases              []completionValue // project aliases and groups (bare words that resolve to projects)
	structured           []string          // structured command names
	structuredDesc       map[string]string
	subcommands          map[string][]string
	knownSubcommandPaths map[string]bool
	structuredFlags      map[string][]StructuredFlagInfo
	structuredFlagValues map[string][]completionValue
	globalFlags          []commandmeta.GlobalFlag // CLI-wide flags, from the catalog
}

func gatherCompletionContext(wsRoot string, cfg *wsproto.Config) completionContext {
	ctx := completionContext{
		structuredDesc:       map[string]string{},
		subcommands:          map[string][]string{},
		knownSubcommandPaths: map[string]bool{},
		structuredFlags:      map[string][]StructuredFlagInfo{},
		structuredFlagValues: map[string][]completionValue{
			"workspace init --extension": {
				{Value: "ts", Description: "TypeScript"},
				{Value: "go", Description: "Go"},
				{Value: "py", Description: "Python"},
			},
		},
		globalFlags: commandmeta.GlobalFlags(),
		commandDesc: map[string]string{},
	}
	addCatalogCompletions(&ctx)
	for _, command := range commandmeta.PublicJobCommands() {
		addCompletionCommand(&ctx, command.Name, command.Description)
	}

	// Add dynamic commands from extensions
	if wsRoot != "" {
		projectPaths := []string{}
		ws, _ := workspace.Load(wsRoot)
		if ws != nil {
			for _, p := range ws.Projects {
				ctx.projects = append(ctx.projects, p.Name)
				ctx.projectIDs = append(ctx.projectIDs, p.ID)
			}

			projectPaths = make([]string, len(ws.Projects))
			for i, p := range ws.Projects {
				projectPaths[i] = p.Path
			}

			// Collect project aliases and groups from workspace config.
			if ws.Config != nil {
				for alias, target := range ws.Config.ProjectAliases {
					ctx.aliases = append(ctx.aliases, completionValue{Value: alias, Description: target})
				}
				for group, pattern := range ws.Config.Groups {
					ctx.aliases = append(ctx.aliases, completionValue{Value: group, Description: pattern})
				}
			}

			// Collect project path basenames as short-form targets (e.g.
			// "http", "application"). ResolveTarget resolves these via
			// filepath.Base(Path), but they appear in neither the names nor
			// the IDs list, so completion must offer them explicitly. Dedup
			// against names and aliases so the combined target list has no
			// duplicate entries.
			seen := make(map[string]bool, len(ctx.projects)+len(ctx.aliases))
			for _, name := range ctx.projects {
				seen[name] = true
			}
			for _, a := range ctx.aliases {
				seen[a.Value] = true
			}
			for _, p := range ws.Projects {
				base := filepath.Base(p.Path)
				if base == "" || base == "." || base == string(filepath.Separator) || seen[base] {
					continue
				}
				seen[base] = true
				ctx.projectBasenames = append(ctx.projectBasenames, base)
			}
		}

		extensions, _ := extension.DiscoverExtensions(wsRoot, cfg, projectPaths)
		for _, ext := range extensions {
			for name, description := range ext.Commands {
				if ext.CommandVisibility[name] == "internal" {
					continue
				}
				addCompletionCommand(&ctx, name, description)
			}
		}
		jobMap := extension.BuildJobMap(extensions)
		for name, jobs := range jobMap {
			publicJobs := publicCompletionJobs(jobs)
			if len(publicJobs) == 0 {
				continue
			}
			if flags := extension.CollectCommandFlags(publicJobs); len(flags) > 0 {
				ctx.structuredFlags[name] = mergeStructuredCompletionFlags(
					ctx.structuredFlags[name],
					manifestFlagsToStructured(flags),
				)
			}
		}
		addExtensionCommandGroupCompletions(&ctx, extensions)
		templates, _ := template.DiscoverTemplates(wsRoot, projectPaths)
		for _, tpl := range templates {
			ctx.structuredFlagValues["projects create --template"] = appendUniqueCompletionValue(
				ctx.structuredFlagValues["projects create --template"],
				completionValue{Value: tpl.Name, Description: tpl.Description},
			)
		}
	}

	finalizeCompletionContext(&ctx)
	addCommandAliasCompletions(&ctx)

	sort.Strings(ctx.commands)
	sort.Strings(ctx.projects)
	sort.Strings(ctx.projectIDs)
	sort.Strings(ctx.projectBasenames)
	sort.Slice(ctx.aliases, func(i, j int) bool {
		return ctx.aliases[i].Value < ctx.aliases[j].Value
	})
	for key := range ctx.structuredFlagValues {
		sort.Slice(ctx.structuredFlagValues[key], func(i, j int) bool {
			return ctx.structuredFlagValues[key][i].Value < ctx.structuredFlagValues[key][j].Value
		})
	}

	return ctx
}

// addCatalogCompletions seeds the built-in structured vocabulary from the
// command catalog: every structured root becomes a first-word candidate
// described by its Summary, every deeper path becomes a subcommand of its
// parent, and every path that declares flags gets them offered. Nothing here is
// filtered — a registered command absent from completion is the defect A1b
// removed, so the projection is total by construction.
func addCatalogCompletions(ctx *completionContext) {
	for _, root := range commandmeta.StructuredRoots() {
		ctx.structured = shared.AppendUnique(ctx.structured, root.Path)
		if root.Summary != "" {
			ctx.structuredDesc[root.Path] = root.Summary
		}
	}
	for _, command := range commandmeta.StructuredCommands() {
		for _, sub := range commandmeta.Subcommands(command.Path) {
			ctx.subcommands[command.Path] = shared.AppendUnique(ctx.subcommands[command.Path], sub)
		}
		// Detail, not command.Flags: an alternate spelling inherits the flags of
		// the path it shares help with ("init" ≡ "workspace init"), so completion
		// and `putnami help <path>` offer the same set.
		if detail, ok := commandmeta.Detail(command.Path); ok && len(detail.Flags) > 0 {
			ctx.structuredFlags[command.Path] = mergeStructuredCompletionFlags(
				ctx.structuredFlags[command.Path],
				structuredFlagInfos(detail.Flags),
			)
		}
	}
}

func publicCompletionJobs(jobs []*extension.JobDefinition) []*extension.JobDefinition {
	if len(jobs) == 0 {
		return nil
	}
	publicJobs := make([]*extension.JobDefinition, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || job.Visibility == "internal" {
			continue
		}
		publicJobs = append(publicJobs, job)
	}
	return publicJobs
}

func addCompletionCommand(ctx *completionContext, name, description string) {
	if name == "" {
		return
	}
	ctx.commands = shared.AppendUnique(ctx.commands, name)
	if ctx.commandDesc == nil {
		ctx.commandDesc = map[string]string{}
	}
	if description != "" && ctx.commandDesc[name] == "" {
		ctx.commandDesc[name] = description
	}
}

func addCommandAliasCompletions(ctx *completionContext) {
	for _, alias := range commandmeta.BuiltinAliasList() {
		addCompletionCommand(ctx, alias.Name, "Alias for "+alias.Command)
		if flags := ctx.structuredFlags[alias.Command]; len(flags) > 0 {
			ctx.structuredFlags[alias.Name] = mergeStructuredCompletionFlags(ctx.structuredFlags[alias.Name], flags)
		}
		if ctx.knownSubcommandPaths[alias.Command] {
			ctx.knownSubcommandPaths[alias.Name] = true
		}
	}
}

func addExtensionCommandGroupCompletions(ctx *completionContext, extensions []*extension.ExtensionDescription) {
	for _, ext := range extensions {
		for groupName, group := range ext.CommandGroups {
			ctx.structured = shared.AppendUnique(ctx.structured, groupName)
			if group.Description != "" && ctx.structuredDesc[groupName] == "" {
				ctx.structuredDesc[groupName] = group.Description
			}
			// Seed the inherited-flags layer with the group's shared flags so
			// they cascade to every (including nested) subcommand's completion.
			// The recursive merge yields command < group < subcommand precedence,
			// matching help rendering and the runtime param path.
			addExtensionSubcommandCompletions(ctx, ext, groupName, group.Subcommands, "", group.Flags)
		}
	}
}

func addExtensionSubcommandCompletions(
	ctx *completionContext,
	ext *extension.ExtensionDescription,
	parentPath string,
	subcommands map[string]extension.SubcommandDefinition,
	inheritedCommand string,
	inheritedFlags map[string]extension.FlagDefinition,
) {
	for name, sub := range subcommands {
		childPath := parentPath + " " + name
		ctx.subcommands[parentPath] = shared.AppendUnique(ctx.subcommands[parentPath], name)

		commandName := inheritedCommand
		if sub.Command != "" {
			commandName = sub.Command
		}

		localFlags := mergeManifestFlags(inheritedFlags, sub.Flags)
		effectiveFlags := mergeManifestFlags(commandFlags(ext, commandName), localFlags)
		if len(effectiveFlags) > 0 {
			ctx.structuredFlags[childPath] = mergeStructuredCompletionFlags(
				ctx.structuredFlags[childPath],
				manifestFlagsToStructured(effectiveFlags),
			)
		}

		if len(sub.Subcommands) > 0 {
			addExtensionSubcommandCompletions(ctx, ext, childPath, sub.Subcommands, commandName, localFlags)
		}
	}
}

func commandFlags(ext *extension.ExtensionDescription, commandName string) map[string]extension.FlagDefinition {
	if ext == nil || commandName == "" {
		return nil
	}
	job := ext.Jobs[commandName]
	if job == nil {
		return nil
	}
	return job.Flags
}

func mergeManifestFlags(
	first map[string]extension.FlagDefinition,
	second map[string]extension.FlagDefinition,
) map[string]extension.FlagDefinition {
	if len(first) == 0 && len(second) == 0 {
		return nil
	}
	merged := make(map[string]extension.FlagDefinition, len(first)+len(second))
	for name, flag := range first {
		merged[name] = flag
	}
	for name, flag := range second {
		merged[name] = flag
	}
	return merged
}

func manifestFlagsToStructured(flags map[string]extension.FlagDefinition) []StructuredFlagInfo {
	if len(flags) == 0 {
		return nil
	}
	names := make([]string, 0, len(flags))
	for name := range flags {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]StructuredFlagInfo, 0, len(names))
	for _, name := range names {
		flag := flags[name]
		info := StructuredFlagInfo{
			Long:        "--" + name,
			Description: flag.Description,
		}
		if flag.Type != "" && flag.Type != "boolean" {
			info.ValueName = "<value>"
		}
		out = append(out, info)
	}
	return out
}

func mergeStructuredCompletionFlags(first, second []StructuredFlagInfo) []StructuredFlagInfo {
	if len(first) == 0 {
		return second
	}
	if len(second) == 0 {
		return first
	}
	byName := make(map[string]StructuredFlagInfo, len(first)+len(second))
	for _, flag := range first {
		byName[flag.Long] = flag
	}
	for _, flag := range second {
		byName[flag.Long] = flag
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]StructuredFlagInfo, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func finalizeCompletionContext(ctx *completionContext) {
	for path, subs := range ctx.subcommands {
		if path != "" {
			ctx.knownSubcommandPaths[path] = true
		}
		sort.Strings(subs)
		ctx.subcommands[path] = subs
		for _, sub := range subs {
			ctx.knownSubcommandPaths[path+" "+sub] = true
		}
	}
	for path, flags := range ctx.structuredFlags {
		if path != "" {
			ctx.knownSubcommandPaths[path] = true
		}
		sort.Slice(flags, func(i, j int) bool {
			return flags[i].Long < flags[j].Long
		})
		ctx.structuredFlags[path] = flags
	}
	sort.Strings(ctx.structured)
}

// globalFlagNames returns the "--long" spellings of every global flag, in
// catalog order, for the shells that offer names without descriptions.
func (c completionContext) globalFlagNames() []string {
	names := make([]string, 0, len(c.globalFlags))
	for _, flag := range c.globalFlags {
		names = append(names, flag.Long)
	}
	return names
}

// globalFlagValues returns the candidate values a global flag accepts, so a
// shell that completes them inline does not retype the vocabulary.
func (c completionContext) globalFlagValues(long string) []string {
	for _, flag := range c.globalFlags {
		if flag.Long == long {
			return flag.Values
		}
	}
	return nil
}

func aliasNames(aliases []completionValue) []string {
	names := make([]string, len(aliases))
	for i, a := range aliases {
		names[i] = a.Value
	}
	return names
}

func appendUniqueCompletionValue(values []completionValue, value completionValue) []completionValue {
	for i, existing := range values {
		if existing.Value != value.Value {
			continue
		}
		if values[i].Description == "" {
			values[i].Description = value.Description
		}
		return values
	}
	return append(values, value)
}
