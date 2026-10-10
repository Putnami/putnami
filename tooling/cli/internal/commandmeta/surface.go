package commandmeta

import protocolcli "go.putnami.dev/protocol/cli"

// Surface returns the CLI's command surface (go.putnami.dev/protocol/cli
// CommandSurface): every command a user can type, with the flags and
// positionals the catalog declares for it, and the global flags.
//
// A command is every catalog path, an alternate spelling (DetailFrom)
// included, and every built-in alias ("b" for "build"). A path that shares
// another path's help also shares its flags and positionals, as ResolveFlags
// and Detail resolve them. Job commands declare no catalog flags: the flags
// they accept come from extensions, so the core surface lists the commands
// alone. A command flag carries its values only when the parser enforces them
// (Flag.Values). A global flag carries none, because GlobalFlag.Values lists
// the candidates completion offers, not a closed set.
func Surface() (protocolcli.CommandSurface, error) {
	commands := make([]protocolcli.CommandSurfaceCommand, 0, len(catalog)+len(builtinAliases))
	for _, command := range catalog {
		commands = append(commands, surfaceCommand(command.Path, command.Path))
	}
	for _, alias := range builtinAliases {
		commands = append(commands, surfaceCommand(alias.Name, alias.Command))
	}
	globals := make([]protocolcli.CommandSurfaceFlag, 0, len(globalFlags))
	for _, flag := range globalFlags {
		globals = append(globals, protocolcli.CommandSurfaceFlag{Long: flag.Long, Short: flag.Short, Type: surfaceFlagType(flag.Type)})
	}
	return protocolcli.NewCommandSurface(globals, commands)
}

// surfaceCommand is the surface entry typed as path, for the catalog entry
// target it runs.
func surfaceCommand(path, target string) protocolcli.CommandSurfaceCommand {
	entry := protocolcli.CommandSurfaceCommand{Path: path}
	command, ok := Lookup(target)
	if !ok {
		return entry
	}
	flags, _ := ResolveFlags(target)
	for _, flag := range flags {
		entry.Flags = append(entry.Flags, protocolcli.CommandSurfaceFlag{
			Long:   flag.Long,
			Short:  flag.Short,
			Type:   surfaceFlagType(flag.Type),
			Values: append([]string(nil), flag.Values...),
		})
	}
	positionals := command.Positionals
	if command.DetailFrom != "" {
		if source, ok := Lookup(command.DetailFrom); ok {
			positionals = source.Positionals
		}
	}
	for _, positional := range positionals {
		entry.Positionals = append(entry.Positionals, protocolcli.CommandSurfacePositional{Name: positional.Name, Required: positional.Required})
	}
	return entry
}

func surfaceFlagType(flagType FlagType) string {
	if flagType == FlagValue {
		return protocolcli.CommandFlagValue
	}
	return protocolcli.CommandFlagBool
}
