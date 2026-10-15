package extension

import (
	"fmt"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// VerbDefinition is one verb of an extension's verb vocabulary
// (Manifest.Verbs). A vocabulary declares verbs and runs nothing: the jobs of
// a verb stay the commands each extension declares under its name.
//
// The CLI merges the vocabularies of the loaded extensions in extension-name
// order, and the first declaration of a verb wins whole. Help lists the verbs
// in that order, completion offers them, and alias resolution reads their
// aliases before the core's. A job whose manifest declares no traits takes the
// verb's default traits; a verb that declares none leaves the well-known
// verb's defaults (WellKnownVerbs) in place.
type VerbDefinition struct {
	// Name is the command name the verb gives meaning to.
	Name string `json:"name"`
	// Description is the one-line description help and completion show.
	Description string `json:"description"`
	// Aliases are the short names that resolve to the verb. An alias never
	// shadows a verb, a command or a command group: the CLI drops one that
	// would.
	Aliases []string `json:"aliases,omitempty"`
	// Traits are the default traits of every job of the verb whose manifest
	// declares none. A declared object replaces the defaults wholesale; it may
	// not change the side effects WellKnownVerbs gives the verb.
	Traits *CommandTraits `json:"traits,omitempty"`
	// Related are the commands `putnami help <verb>` suggests, in order.
	Related []string `json:"related,omitempty"`
}

// DeclaresVerbs reports whether the manifest declares a verb vocabulary. The
// section's presence is what counts: it is contract surface, and a manifest
// that declares it requires protocolcli.VerbVocabularyContract.
func (m *Manifest) DeclaresVerbs() bool {
	return m != nil && m.Verbs != nil
}

var (
	verbNameFormat    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	verbAliasFormat   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)
	verbRelatedFormat = regexp.MustCompile(`^[a-z][a-z0-9-]*( [a-z][a-z0-9-]*)*$`)
)

// ValidateVerbs checks the verb vocabulary of one manifest: a non-empty list
// of uniquely named, described verbs, whose aliases are unique and name no verb
// or command of the manifest, whose traits use the side-effect vocabulary and
// keep the side effects WellKnownVerbs gives a verb, and whose related commands
// are command paths other than the verb itself. It is inert for a manifest that
// declares no vocabulary.
func ValidateVerbs(m *Manifest) []diag.Diagnostic {
	if !m.DeclaresVerbs() {
		return nil
	}
	if len(m.Verbs) == 0 {
		return []diag.Diagnostic{diag.Errorf("empty-verbs", "verbs", "a verb vocabulary declares at least one verb")}
	}
	var diags []diag.Diagnostic
	verbs := make(map[string]int, len(m.Verbs))
	for i, verb := range m.Verbs {
		if _, seen := verbs[verb.Name]; !seen {
			verbs[verb.Name] = i
		}
	}
	aliases := make(map[string]string)
	for i, verb := range m.Verbs {
		field := fmt.Sprintf("verbs[%d]", i)
		switch {
		case verb.Name == "":
			diags = append(diags, diag.Errorf("required-field", field+".name", "verb name is required"))
		case !verbNameFormat.MatchString(verb.Name):
			diags = append(diags, diag.Errorf("invalid-verb-name", field+".name",
				"verb name %q must be lowercase letters, digits and hyphens, starting with a letter", verb.Name))
		case verbs[verb.Name] != i:
			diags = append(diags, diag.Errorf("duplicate-verb", field+".name",
				"verb %q is declared twice (also verbs[%d])", verb.Name, verbs[verb.Name]))
		}
		if strings.TrimSpace(verb.Description) == "" {
			diags = append(diags, diag.Errorf("required-field", field+".description", "verb description is required"))
		}
		for j, alias := range verb.Aliases {
			aliasField := fmt.Sprintf("%s.aliases[%d]", field, j)
			_, isVerb := verbs[alias]
			_, isCommand := m.Commands[alias]
			_, isGroup := m.CommandGroups[alias]
			switch {
			case !verbAliasFormat.MatchString(alias):
				diags = append(diags, diag.Errorf("invalid-verb-alias", aliasField,
					"alias %q must be letters, digits and hyphens, starting with a letter", alias))
			case aliases[alias] != "":
				diags = append(diags, diag.Errorf("duplicate-verb-alias", aliasField,
					"alias %q is declared twice (also %s)", alias, aliases[alias]))
			case isVerb || isCommand || isGroup:
				diags = append(diags, diag.Errorf("duplicate-verb-alias", aliasField,
					"alias %q names a verb, command or command group of this manifest", alias))
			default:
				aliases[alias] = aliasField
			}
		}
		if verb.Traits != nil {
			switch wellKnown := WellKnownVerbs[verb.Name].Traits.SideEffects; verb.Traits.SideEffects {
			case SideEffectsNone, SideEffectsRegistry, SideEffectsCloud:
				if wellKnown != SideEffectsNone && verb.Traits.SideEffects != wellKnown {
					diags = append(diags, diag.Errorf("invalid-verb-traits", field+".traits.sideEffects",
						"verb %q must keep the side effects %q the core gives it", verb.Name, wellKnown))
				}
			default:
				diags = append(diags, diag.Errorf("invalid-enum", field+".traits.sideEffects",
					"sideEffects %q must be %q or %q", verb.Traits.SideEffects, SideEffectsRegistry, SideEffectsCloud))
			}
		}
		related := make(map[string]bool, len(verb.Related))
		for j, command := range verb.Related {
			relatedField := fmt.Sprintf("%s.related[%d]", field, j)
			switch {
			case !verbRelatedFormat.MatchString(command):
				diags = append(diags, diag.Errorf("invalid-verb-related", relatedField,
					"related command %q must be a command path of lowercase words", command))
			case command == verb.Name:
				diags = append(diags, diag.Errorf("invalid-verb-related", relatedField,
					"verb %q cannot be related to itself", verb.Name))
			case related[command]:
				diags = append(diags, diag.Errorf("invalid-verb-related", relatedField,
					"related command %q is repeated", command))
			default:
				related[command] = true
			}
		}
	}
	return diags
}
