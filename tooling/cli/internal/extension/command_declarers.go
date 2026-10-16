package extension

import "slices"

// PublicDeclarers returns, per command, the sorted names of the extensions
// whose manifest declares it with a visibility other than internal. A command
// that no extension declares public has no entry, and a nil extension names no
// one.
func PublicDeclarers(commands []string, extensions []*ExtensionDescription) map[string][]string {
	declarers := make(map[string][]string, len(commands))
	for _, command := range commands {
		if _, done := declarers[command]; done {
			continue
		}
		var names []string
		for _, ext := range extensions {
			if ext == nil {
				continue
			}
			if _, declared := ext.Commands[command]; !declared || ext.CommandVisibility[command] == "internal" {
				continue
			}
			if !slices.Contains(names, ext.Name) {
				names = append(names, ext.Name)
			}
		}
		if len(names) > 0 {
			slices.Sort(names)
			declarers[command] = names
		}
	}
	return declarers
}
