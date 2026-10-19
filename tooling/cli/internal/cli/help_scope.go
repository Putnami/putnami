package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// helpScope is the set of commands a workspace's loaded extensions declare.
// The core declares no job command and no alias, so help lists a job command,
// an alias or a provider-bound global flag only when a loaded extension gives
// it meaning: a workspace with no deploying extension shows no deploy, and one
// with no cache provider no --cache-trust.
//
// A nil scope is the help outside a workspace, or where the extensions cannot
// be read: no extension is loaded, so it lists none of them. A scope without
// commands is the help of a workspace where an extension did not load: its
// commands are unknown, so help hides nothing it would provide.
//
// The verb vocabulary of the loaded extensions comes first: its verbs lead the
// job commands in its order, its aliases are the only ones help lists, and a
// verb's related commands replace the catalog's.
type helpScope struct {
	commands   map[string]bool
	listed     []CommandInfo
	vocabulary extension.Vocabulary
}

// newHelpScope returns the scope of the workspace at wsRoot, or nil outside a
// workspace.
func newHelpScope(wsRoot string, cfg *wsproto.Config) *helpScope {
	scope, _ := discoverHelpScope(wsRoot, cfg)
	return scope
}

// discoverHelpScope returns the scope of the workspace at wsRoot and the
// extensions that loaded there, so one discovery serves a help page's scope
// and its job flags.
func discoverHelpScope(wsRoot string, cfg *wsproto.Config) (*helpScope, []*extension.ExtensionDescription) {
	if wsRoot == "" {
		return nil, nil
	}
	result, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPathsForRoot(wsRoot))
	if err != nil {
		return nil, nil
	}
	return helpScopeOf(result, workspaceAliases(cfg)), result.Extensions
}

// workspaceAliases returns the aliases a workspace configuration declares.
func workspaceAliases(cfg *wsproto.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	return cfg.Aliases
}

// helpScopeOf returns the scope a discovery gives in a workspace whose aliases
// are user. When an extension did not load, the scope hides nothing and keeps
// the vocabulary and commands of those that did.
func helpScopeOf(result *extension.DiscoveryResult, user map[string]string) *helpScope {
	scope := &helpScope{vocabulary: commandVocabulary(result.Extensions, user), listed: listedCommands(result.Extensions)}
	if len(result.Skipped) > 0 {
		return scope
	}
	scope.commands = map[string]bool{}
	for _, ext := range result.Extensions {
		if ext == nil {
			continue
		}
		for name := range ext.Commands {
			scope.commands[name] = true
		}
	}
	return scope
}

// listedCommands returns, by name, the commands of extensions that help lists
// after the vocabulary's verbs: the ones a user types as a job command. It
// leaves out an internal command, a provider command, which the core calls, a
// command an extension's command group carries, which the user types as a
// subcommand of the group, and a command named like a path of the catalog,
// which runs the core's command instead. A command several extensions declare
// takes the description of the first, in extension-name order, that gives one.
func listedCommands(extensions []*extension.ExtensionDescription) []CommandInfo {
	ordered := make([]*extension.ExtensionDescription, 0, len(extensions))
	for _, ext := range extensions {
		if ext != nil {
			ordered = append(ordered, ext)
		}
	}
	slices.SortStableFunc(ordered, func(a, b *extension.ExtensionDescription) int { return strings.Compare(a.Name, b.Name) })
	descriptions := map[string]string{}
	for _, ext := range ordered {
		carried, providers := groupCommands(ext.CommandGroups), extension.ProviderCommands(ext)
		for name, description := range ext.Commands {
			if _, core := commandmeta.Lookup(name); core || ext.CommandVisibility[name] == "internal" ||
				carried[name] || slices.Contains(providers, name) {
				continue
			}
			if current, seen := descriptions[name]; !seen || current == "" {
				descriptions[name] = description
			}
		}
	}
	out := make([]CommandInfo, 0, len(descriptions))
	for _, name := range slices.Sorted(maps.Keys(descriptions)) {
		out = append(out, CommandInfo{Name: name, Description: strings.TrimSuffix(descriptions[name], ".")})
	}
	return out
}

// groupCommands returns the flat commands the subcommands of groups route to,
// nested subcommands included.
func groupCommands(groups map[string]extension.CommandGroupDefinition) map[string]bool {
	carried := map[string]bool{}
	var walk func(map[string]extension.SubcommandDefinition)
	walk = func(subcommands map[string]extension.SubcommandDefinition) {
		for _, subcommand := range subcommands {
			if subcommand.Command != "" {
				carried[subcommand.Command] = true
			}
			walk(subcommand.Subcommands)
		}
	}
	for _, group := range groups {
		walk(group.Subcommands)
	}
	return carried
}

// commandVocabulary merges the verb vocabularies of extensions for a workspace
// whose aliases are user. A vocabulary alias never takes a command path of the
// catalog, and a workspace alias of the same name hides it
// (commandmeta.ReservesVocabularyAlias).
func commandVocabulary(extensions []*extension.ExtensionDescription, user map[string]string) extension.Vocabulary {
	return extension.MergeVocabulary(extensions, func(name string) bool {
		return commandmeta.ReservesVocabularyAlias(name, user)
	})
}

// hiddenAliasWarnings returns a warning for each vocabulary alias a workspace
// alias of the same name hides, in merge order.
func hiddenAliasWarnings(extensions []*extension.ExtensionDescription, user map[string]string) []string {
	if len(user) == 0 {
		return nil
	}
	var warnings []string
	for _, alias := range commandVocabulary(extensions, nil).Aliases {
		if target, mine := user[alias.Name]; mine {
			warnings = append(warnings, fmt.Sprintf(
				"warning: the workspace alias %s (%s) hides the alias %s of %s that %s declares",
				alias.Name, target, alias.Name, alias.Verb, alias.Extension))
		}
	}
	return warnings
}

// declares reports whether a loaded extension declares command: never
// outside a workspace, and always where an extension did not load, whose
// commands are unknown.
func (s *helpScope) declares(command string) bool {
	return s != nil && (s.commands == nil || s.commands[command])
}

// verb returns the vocabulary's verb named command.
func (s *helpScope) verb(command string) (extension.VerbDefinition, bool) {
	if s == nil {
		return extension.VerbDefinition{}, false
	}
	return s.vocabulary.Verb(command)
}

// commandAliases returns the alias tables a command name resolves through in
// this scope: the vocabulary's, then user.
func (s *helpScope) commandAliases(user map[string]string) commandAliases {
	aliases := commandAliases{user: user}
	if s != nil {
		aliases.vocabulary = s.vocabulary.AliasMap()
	}
	return aliases
}

// listsJobCommand reports whether help lists path. A command of the catalog
// is always listed, because the core always provides it. Any other path (a
// verb of the vocabulary, a command of an extension) is listed only when a
// loaded extension declares it.
func (s *helpScope) listsJobCommand(path string) bool {
	if _, ok := commandmeta.Lookup(path); ok {
		return true
	}
	return s.declares(path)
}

// related returns the commands help suggests next to command: those of its
// verb when the vocabulary has it, the catalog's otherwise.
func (s *helpScope) related(command string) []string {
	if verb, ok := s.verb(command); ok {
		return verb.Related
	}
	return relatedCommands[command]
}

// listsFlag reports whether help lists a global flag: one the core gives its
// meaning, or one whose provider a loaded extension declares.
func (s *helpScope) listsFlag(flag FlagInfo) bool {
	return len(flag.Providers) == 0 || slices.ContainsFunc(flag.Providers, s.declares)
}

// jobCommands returns the job commands help lists: the verbs of the
// vocabulary in its order, then the other listed commands of the loaded
// extensions by name, each only when a loaded extension declares it.
func (s *helpScope) jobCommands() []CommandInfo {
	if s == nil {
		return nil
	}
	out := make([]CommandInfo, 0, len(s.vocabulary.Verbs)+len(s.listed))
	for _, verb := range s.vocabulary.Verbs {
		if s.declares(verb.Name) {
			out = append(out, CommandInfo{Name: verb.Name, Description: strings.TrimSuffix(verb.Description, ".")})
		}
	}
	for _, command := range s.listed {
		if _, isVerb := s.verb(command.Name); !isVerb && s.declares(command.Name) {
			out = append(out, command)
		}
	}
	return out
}

// flagCategories returns the global-flag categories help lists, dropping a
// category left empty.
func (s *helpScope) flagCategories() []FlagCategory {
	out := make([]FlagCategory, 0, len(allFlagCategories))
	for _, category := range allFlagCategories {
		flags := make([]FlagInfo, 0, len(category.Flags))
		for _, flag := range category.Flags {
			if s.listsFlag(flag) {
				flags = append(flags, flag)
			}
		}
		if len(flags) > 0 {
			out = append(out, FlagCategory{Name: category.Name, Flags: flags})
		}
	}
	return out
}

// aliases returns the aliases help lists: the vocabulary's whose verb a
// loaded extension declares. The core declares no alias.
func (s *helpScope) aliases() []commandmeta.Alias {
	if s == nil {
		return nil
	}
	out := make([]commandmeta.Alias, 0, len(s.vocabulary.Aliases))
	for _, alias := range s.vocabulary.Aliases {
		if s.listsJobCommand(alias.Verb) {
			out = append(out, commandmeta.Alias{Name: alias.Name, Command: alias.Verb})
		}
	}
	return out
}
