package commandmeta

import proto "go.putnami.dev/protocol/extension"

// Alias describes a built-in command shortcut.
type Alias struct {
	Name    string
	Command string
}

// JobCommand describes a public job command shown in help and completion.
type JobCommand struct {
	Name        string
	Description string
}

var builtinAliases = []Alias{
	{Name: "b", Command: "build"},
	{Name: "t", Command: "test"},
	{Name: "l", Command: "lint"},
	{Name: "s", Command: "serve"},
	{Name: "f", Command: "format"},
	{Name: "p", Command: "publish"},
	{Name: "P", Command: "publish"},
	{Name: "d", Command: "deploy"},
	{Name: "D", Command: "deploy"},
}

// BuiltinAliases returns a copy of the built-in alias map.
func BuiltinAliases() map[string]string {
	aliases := make(map[string]string, len(builtinAliases))
	for _, alias := range builtinAliases {
		aliases[alias.Name] = alias.Command
	}
	return aliases
}

// BuiltinAliasList returns built-in aliases in display order.
func BuiltinAliasList() []Alias {
	return append([]Alias(nil), builtinAliases...)
}

// PublicJobCommands returns the protocol-level public job vocabulary in catalog
// order, described by the protocol where it describes the verb and by the
// catalog Summary otherwise. Internal orchestration verbs intentionally stay out
// of the user-facing command list, which is why the catalog — not
// proto.WellKnownVerbs — decides membership and order.
func PublicJobCommands() []JobCommand {
	out := make([]JobCommand, 0, len(catalog))
	for _, command := range catalog {
		if command.Kind != KindJob {
			continue
		}
		desc := command.Summary
		if spec, ok := proto.WellKnownVerbs[command.Path]; ok && spec.Description != "" {
			desc = spec.Description
		}
		out = append(out, JobCommand{Name: command.Path, Description: desc})
	}
	return out
}
