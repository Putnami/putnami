package extension

import (
	"sort"

	proto "go.putnami.dev/protocol/extension"
)

// Vocabulary is the verb vocabulary of a set of loaded extensions. Their
// vocabularies merge in extension-name order, each in its declared order, and
// the first declaration of a verb wins whole: its description, aliases,
// default traits and related commands.
type Vocabulary struct {
	// Verbs are the merged verbs, in merge order.
	Verbs []VerbDefinition
	// Aliases are the aliases that apply, in merge order. Each resolves to a
	// verb of Verbs.
	Aliases []VerbAlias
}

// VerbAlias is one alias of a merged vocabulary.
type VerbAlias struct {
	Name string
	Verb string
}

// MergeVocabulary merges the verb vocabularies of extensions. An alias applies
// when no earlier alias claimed its name and it names no verb, no command or
// command group of a loaded extension, and no name reserved reports: the
// caller's own command roots. reserved may be nil.
func MergeVocabulary(extensions []*ExtensionDescription, reserved func(string) bool) Vocabulary {
	ordered := make([]*ExtensionDescription, 0, len(extensions))
	for _, ext := range extensions {
		if ext != nil {
			ordered = append(ordered, ext)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	var vocabulary Vocabulary
	verbs := make(map[string]bool)
	for _, ext := range ordered {
		for _, verb := range ext.Verbs {
			if verb.Name == "" || verbs[verb.Name] {
				continue
			}
			verbs[verb.Name] = true
			vocabulary.Verbs = append(vocabulary.Verbs, verb)
		}
	}

	taken := func(name string) bool {
		if verbs[name] || (reserved != nil && reserved(name)) {
			return true
		}
		for _, ext := range ordered {
			_, command := ext.Commands[name]
			_, group := ext.CommandGroups[name]
			if command || group {
				return true
			}
		}
		return false
	}
	claimed := make(map[string]bool)
	for _, verb := range vocabulary.Verbs {
		for _, alias := range verb.Aliases {
			if alias == "" || claimed[alias] || taken(alias) {
				continue
			}
			claimed[alias] = true
			vocabulary.Aliases = append(vocabulary.Aliases, VerbAlias{Name: alias, Verb: verb.Name})
		}
	}
	return vocabulary
}

// Verb returns the merged verb named name.
func (v Vocabulary) Verb(name string) (VerbDefinition, bool) {
	for _, verb := range v.Verbs {
		if verb.Name == name {
			return verb, true
		}
	}
	return VerbDefinition{}, false
}

// AliasMap returns the aliases as a map from alias to verb, or nil when the
// vocabulary has none.
func (v Vocabulary) AliasMap() map[string]string {
	if len(v.Aliases) == 0 {
		return nil
	}
	aliases := make(map[string]string, len(v.Aliases))
	for _, alias := range v.Aliases {
		aliases[alias.Name] = alias.Verb
	}
	return aliases
}

// DefaultTraits returns the traits a job of command takes when its manifest
// declares none: the default traits of its verb in the vocabulary, or, for a
// verb the vocabulary does not give traits, the well-known verb's defaults.
func (v Vocabulary) DefaultTraits(command string) CommandTraits {
	if verb, ok := v.Verb(proto.BaseCommandName(command)); ok && verb.Traits != nil {
		return *verb.Traits
	}
	return proto.EffectiveTraits(command, nil)
}

// ApplyVocabularyTraits gives every job of extensions whose manifest declared
// no traits the default traits of its verb in their merged vocabulary. Resolve
// sees one manifest at a time, so this pass runs once discovery has loaded
// them all. Without a vocabulary it changes nothing.
func ApplyVocabularyTraits(extensions []*ExtensionDescription) {
	vocabulary := MergeVocabulary(extensions, nil)
	if len(vocabulary.Verbs) == 0 {
		return
	}
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		for _, job := range ext.Jobs {
			if job != nil && !job.TraitsDeclared {
				job.Traits = vocabulary.DefaultTraits(job.Name)
			}
		}
	}
}
