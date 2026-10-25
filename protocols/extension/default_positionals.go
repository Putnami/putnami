package extension

// DeclaresDefaultPositionals reports whether a command group of the manifest
// names a default subcommand that declares positionals, the vocabulary of
// protocolcli.DefaultPositionalsContract. A reader at that contract runs the
// default for `putnami <group> <word>` when the word names no subcommand of
// the group, with the word as the default's first argument. A default that
// names no subcommand of its group declares nothing: strict validation refuses
// it on its own.
func DeclaresDefaultPositionals(m *Manifest) bool {
	if m == nil {
		return false
	}
	for _, group := range m.CommandGroups {
		if group.Default == "" {
			continue
		}
		if sub, ok := group.Subcommands[group.Default]; ok && len(sub.Positionals) > 0 {
			return true
		}
	}
	return false
}
