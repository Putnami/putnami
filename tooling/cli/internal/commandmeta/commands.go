package commandmeta

// Alias describes a command shortcut that help and completion present: a
// vocabulary alias of a loaded extension, or a workspace alias.
type Alias struct {
	Name    string
	Command string
}

// ReservesVocabularyAlias reports whether a vocabulary alias may not take name
// in a workspace whose aliases are workspace: a command path of the catalog
// has it, or a workspace alias does. The core declares no alias of its own.
func ReservesVocabularyAlias(name string, workspace map[string]string) bool {
	if _, ok := Lookup(name); ok {
		return true
	}
	_, mine := workspace[name]
	return mine
}
