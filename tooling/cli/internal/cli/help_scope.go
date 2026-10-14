package cli

import (
	"fmt"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// helpScope is the set of commands a workspace's loaded extensions declare.
// Help inside a workspace lists a job command, an alias or a global flag only
// when an extension there gives it meaning: a workspace with no deploying
// extension shows no deploy, and one with no cache provider no --cache-trust.
//
// A nil scope hides nothing. That is the help outside a workspace. A scope
// without commands hides nothing either: it is the help of a workspace where an
// extension did not load, whose commands are unknown, and help must not hide
// what it would provide.
//
// The verb vocabulary of the loaded extensions comes first: its verbs lead the
// job commands in its order, its aliases lead the built-in ones, and a verb's
// related commands replace the catalog's.
type helpScope struct {
	commands   map[string]bool
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
// the vocabulary of those that did.
func helpScopeOf(result *extension.DiscoveryResult, user map[string]string) *helpScope {
	vocabulary := commandVocabulary(result.Extensions, user)
	if len(result.Skipped) > 0 {
		return &helpScope{vocabulary: vocabulary}
	}
	scope := &helpScope{commands: map[string]bool{}, vocabulary: vocabulary}
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

// commandVocabulary merges the verb vocabularies of extensions for a workspace
// whose aliases are user. A vocabulary alias never takes a command path of the
// catalog, structured or job, and a workspace alias of the same name hides it.
func commandVocabulary(extensions []*extension.ExtensionDescription, user map[string]string) extension.Vocabulary {
	return extension.MergeVocabulary(extensions, func(name string) bool {
		_, mine := user[name]
		return mine || isCatalogPath(name)
	})
}

// isCatalogPath reports whether name is a command path of the catalog.
func isCatalogPath(name string) bool {
	_, ok := commandmeta.Lookup(name)
	return ok
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

// declares reports whether help lists command: always outside a workspace,
// and inside one only when a loaded extension declares it.
func (s *helpScope) declares(command string) bool {
	return s == nil || s.commands == nil || s.commands[command]
}

// verb returns the vocabulary's verb named command.
func (s *helpScope) verb(command string) (extension.VerbDefinition, bool) {
	if s == nil {
		return extension.VerbDefinition{}, false
	}
	return s.vocabulary.Verb(command)
}

// commandAliases returns the alias tables a command name resolves through in
// this scope: the vocabulary's, the built-in ones, then user.
func (s *helpScope) commandAliases(user map[string]string) commandAliases {
	aliases := commandAliases{user: user}
	if s != nil {
		aliases.vocabulary = s.vocabulary.AliasMap()
	}
	return aliases
}

// listsJobCommand reports whether help lists path. A built-in command of the
// catalog is always listed, because the core always provides it. Any other
// path (a job command of the catalog, a verb of the vocabulary, a command of
// an extension) is listed only when a loaded extension declares it.
func (s *helpScope) listsJobCommand(path string) bool {
	if command, ok := commandmeta.Lookup(path); ok && command.Kind != commandmeta.KindJob {
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

// jobCommands returns the public job commands help lists: the verbs of the
// vocabulary in its order, then the catalog's job commands it does not name.
func (s *helpScope) jobCommands() []CommandInfo {
	var verbs []extension.VerbDefinition
	if s != nil {
		verbs = s.vocabulary.Verbs
	}
	out := make([]CommandInfo, 0, len(verbs)+len(jobCommands.Commands))
	for _, verb := range verbs {
		if s.declares(verb.Name) {
			out = append(out, CommandInfo{Name: verb.Name, Description: strings.TrimSuffix(verb.Description, ".")})
		}
	}
	for _, command := range jobCommands.Commands {
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

// aliases returns the aliases help lists, those whose command it lists: the
// vocabulary's, then the built-in ones whose name the vocabulary leaves free.
func (s *helpScope) aliases() []commandmeta.Alias {
	var vocabulary []extension.VerbAlias
	if s != nil {
		vocabulary = s.vocabulary.Aliases
	}
	builtin := commandmeta.BuiltinAliasList()
	out := make([]commandmeta.Alias, 0, len(vocabulary)+len(builtin))
	claimed := make(map[string]bool, len(vocabulary))
	for _, alias := range vocabulary {
		claimed[alias.Name] = true
		if s.listsJobCommand(alias.Verb) {
			out = append(out, commandmeta.Alias{Name: alias.Name, Command: alias.Verb})
		}
	}
	for _, alias := range builtin {
		if !claimed[alias.Name] && s.listsJobCommand(alias.Command) {
			out = append(out, alias)
		}
	}
	return out
}
