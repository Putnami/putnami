package workspace

import (
	"fmt"
	"strings"
)

// SelectorKind is the vocabulary every Putnami document uses to select
// projects. One grammar serves the workspace config, the CI document, and the
// command line, so a selection means the same thing wherever it is authored.
type SelectorKind string

const (
	// SelectorTag selects every project carrying a tag.
	SelectorTag SelectorKind = "tag"
	// SelectorGroup selects the members of a declared group.
	SelectorGroup SelectorKind = "group"
	// SelectorScope selects every project under a scope path.
	SelectorScope SelectorKind = "scope"
	// SelectorProject selects exactly one project by its id.
	SelectorProject SelectorKind = "project"
)

// Selector is one parsed selection expression.
type Selector struct {
	// Kind is what the expression selects by.
	Kind SelectorKind
	// Value is the tag, group name, scope path, or project id.
	Value string
}

// ParseSelector reads a selection expression. "tag:<tag>", "group:<group>" and
// "scope:<path>" are the three qualified forms; anything else is a project id,
// which is why the vocabulary is closed — a fourth prefix invented downstream
// would silently parse as a project nobody can find.
func ParseSelector(raw string) (Selector, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Selector{}, fmt.Errorf("empty selector")
	}
	for _, kind := range []SelectorKind{SelectorTag, SelectorGroup, SelectorScope} {
		prefix := string(kind) + ":"
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		if value == "" {
			return Selector{}, fmt.Errorf("selector %q names no %s", raw, kind)
		}
		return Selector{Kind: kind, Value: value}, nil
	}
	return Selector{Kind: SelectorProject, Value: trimmed}, nil
}

// String renders a selector back to its authored form: the qualified spelling
// for a tag, group or scope, and the bare id for a project.
func (s Selector) String() string {
	if s.Kind == SelectorProject || s.Kind == "" {
		return s.Value
	}
	return string(s.Kind) + ":" + s.Value
}
