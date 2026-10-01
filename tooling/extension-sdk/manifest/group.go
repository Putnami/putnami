// Command-group authoring: the interactive half of the manifest.
//
// A flat command is one name and one pipeline, which a struct literal states
// clearly. A command group is a TREE — a group, its shared flags, its
// subcommands, and each subcommand's own flags, positionals and examples — and
// a struct literal for it is three nested map composites deep before anything
// is said. What gets lost in there is what the protocol actually requires: a
// subcommand must route to a flat command in the SAME manifest (`command`), its
// positionals are ORDERED while everything around them is an unordered map, and
// a group with no subcommands is not a group at all.
//
// The constructors here make that shape read as what it is. Positionals stay a
// list because their order is the argument order; everything else is named. And
// nothing is validated as members are added: a subcommand's command target is
// only resolvable against the whole manifest, so the verdict belongs to Build
// like every other one — the protocol's own rules
// (extension.ValidateManifest → validateSubcommands), never a second copy here.

package manifest

import (
	proto "go.putnami.dev/protocol/extension"
)

// GroupOption configures one command group.
type GroupOption func(*proto.CommandGroupDefinition)

// SubcommandOption configures one subcommand of a command group.
type SubcommandOption func(*proto.SubcommandDefinition)

// CommandGroup adds a structured command group — `putnami <group> <sub>`, the
// shape `putnami cloud login` uses. A later call with the same name replaces the
// earlier one, exactly like Command, so a generator can compose a manifest
// without checking what it already wrote.
//
// The description is the group's one-line help text; pass "" to leave it out.
func (b *Builder) CommandGroup(name, description string, opts ...GroupOption) *Builder {
	group := proto.CommandGroupDefinition{
		Description: description,
		Subcommands: map[string]proto.SubcommandDefinition{},
	}
	for _, opt := range opts {
		opt(&group)
	}
	if b.manifest.CommandGroups == nil {
		b.manifest.CommandGroups = map[string]proto.CommandGroupDefinition{}
	}
	b.manifest.CommandGroups[name] = group
	return b
}

// GroupFlag declares a flag shared by every subcommand of the group, so a
// cross-cutting selector (--env) is written once instead of on each subcommand.
// A subcommand's own flag of the same name wins.
//
// Reserved global flag names (--output, --dry-run, …) are rejected by Build:
// the CLI consumes them before the extension is reached, so a manifest that
// declared one would describe a flag its binary can never receive.
func GroupFlag(name string, flag proto.FlagDefinition) GroupOption {
	return func(g *proto.CommandGroupDefinition) {
		if g.Flags == nil {
			g.Flags = map[string]proto.FlagDefinition{}
		}
		g.Flags[name] = flag
	}
}

// DefaultSubcommand names the subcommand `putnami <group>` runs when no
// subcommand word follows; `putnami <group> --help` still prints the group
// help. Build rejects a name the group does not declare.
func DefaultSubcommand(name string) GroupOption {
	return func(g *proto.CommandGroupDefinition) { g.Default = name }
}

// Subcommand adds one subcommand to the group. The description is its one-line
// help text; leaving it "" falls back to the description of the flat command it
// runs.
//
// The protocol also lets a subcommand nest further subcommands. Nothing here
// builds that shape yet — no first-party extension needs it — so a nested tree
// is still authored by assigning proto.SubcommandDefinition.Subcommands
// directly.
func Subcommand(name, description string, opts ...SubcommandOption) GroupOption {
	return func(g *proto.CommandGroupDefinition) {
		sub := proto.SubcommandDefinition{Description: description}
		for _, opt := range opts {
			opt(&sub)
		}
		if g.Subcommands == nil {
			g.Subcommands = map[string]proto.SubcommandDefinition{}
		}
		g.Subcommands[name] = sub
	}
}

// Runs names the flat command, in the same manifest, that the subcommand routes
// to. It is what carries the pipeline, so flags and steps are declared once and
// the group is a naming surface over them. Build rejects a name no command in
// the manifest defines.
func Runs(command string) SubcommandOption {
	return func(s *proto.SubcommandDefinition) { s.Command = command }
}

// Interactive marks the subcommand as command UX rather than job UX: the live
// renderer is bypassed, the subprocess inherits the terminal's stdio, and it
// receives PUTNAMI_INTERACTIVE=1. It is what a subcommand that writes its own
// result document — or prompts — needs, since a renderer-driven run owns stdout.
func Interactive() SubcommandOption {
	return func(s *proto.SubcommandDefinition) { s.Interactive = true }
}

// WorkspaceRequirement states whether the subcommand needs a workspace:
// proto.SubcommandWorkspaceRequired, the meaning of an absent value, or
// proto.SubcommandWorkspaceOptional, which also runs it outside any workspace
// from an extension pinned in the user scope. Build rejects optional on a
// subcommand that is not Interactive, and any other value.
func WorkspaceRequirement(requirement string) SubcommandOption {
	return func(s *proto.SubcommandDefinition) { s.Workspace = requirement }
}

// SubcommandFlag declares a flag owned by this subcommand. It overrides a group
// flag and a flat-command flag of the same name, and is subject to the same
// reserved-name rule as GroupFlag.
func SubcommandFlag(name string, flag proto.FlagDefinition) SubcommandOption {
	return func(s *proto.SubcommandDefinition) {
		if s.Flags == nil {
			s.Flags = map[string]proto.FlagDefinition{}
		}
		s.Flags[name] = flag
	}
}

// Positional declares an optional positional argument. Declaration order IS
// argument order, which is why positionals are the one part of a subcommand
// that stays a list.
func Positional(name string) SubcommandOption {
	return appendPositional(proto.PositionalDefinition{Name: name})
}

// RequiredPositional declares a positional argument the subcommand cannot run
// without. The requirement is help and completion metadata: arity is enforced
// by the extension binary, which owns the error text a user reads.
func RequiredPositional(name string) SubcommandOption {
	return appendPositional(proto.PositionalDefinition{Name: name, Required: true})
}

func appendPositional(positional proto.PositionalDefinition) SubcommandOption {
	return func(s *proto.SubcommandDefinition) {
		s.Positionals = append(s.Positionals, positional)
	}
}

// Example adds one runnable example to the subcommand's help, in declaration
// order. The description glosses what the line does; pass "" for none.
func Example(command, description string) SubcommandOption {
	return func(s *proto.SubcommandDefinition) {
		s.Examples = append(s.Examples, proto.ExampleDefinition{
			Command:     command,
			Description: description,
		})
	}
}
