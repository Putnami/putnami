package cli

import (
	"slices"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// helpScope is the set of commands a workspace's loaded extensions declare.
// Help inside a workspace lists a job command, an alias or a global flag only
// when an extension there gives it meaning: a workspace with no deploying
// extension shows no deploy, and one with no cache provider no --cache-trust.
//
// A nil scope hides nothing. That is the help outside a workspace, and the
// help of a workspace where an extension did not load: its commands are
// unknown, and help must not hide what it would provide.
type helpScope struct {
	commands map[string]bool
}

// newHelpScope returns the scope of the workspace at wsRoot, or nil outside a
// workspace and when an extension there did not load.
func newHelpScope(wsRoot string, cfg *wsproto.Config) *helpScope {
	if wsRoot == "" {
		return nil
	}
	result, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPathsForRoot(wsRoot))
	if err != nil || len(result.Skipped) > 0 {
		return nil
	}
	scope := &helpScope{commands: map[string]bool{}}
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

// declares reports whether help lists command: always outside a workspace,
// and inside one only when a loaded extension declares it.
func (s *helpScope) declares(command string) bool {
	return s == nil || s.commands[command]
}

// listsJobCommand reports whether help lists path. A path that is not a job
// command is a built-in, which the core always provides.
func (s *helpScope) listsJobCommand(path string) bool {
	if command, ok := commandmeta.Lookup(path); !ok || command.Kind != commandmeta.KindJob {
		return true
	}
	return s.declares(path)
}

// listsFlag reports whether help lists a global flag: one the core gives its
// meaning, or one whose provider a loaded extension declares.
func (s *helpScope) listsFlag(flag FlagInfo) bool {
	return len(flag.Providers) == 0 || slices.ContainsFunc(flag.Providers, s.declares)
}

// jobCommands returns the public job commands help lists.
func (s *helpScope) jobCommands() []CommandInfo {
	out := make([]CommandInfo, 0, len(jobCommands.Commands))
	for _, command := range jobCommands.Commands {
		if s.declares(command.Name) {
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

// aliases returns the built-in aliases help lists: those whose command it lists.
func (s *helpScope) aliases() []commandmeta.Alias {
	all := commandmeta.BuiltinAliasList()
	out := make([]commandmeta.Alias, 0, len(all))
	for _, alias := range all {
		if s.listsJobCommand(alias.Command) {
			out = append(out, alias)
		}
	}
	return out
}
