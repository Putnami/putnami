package stacks

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// ImportOption is the member of a delegated tool's workspace options that
// lists the projects a delegated workspace imports: project paths and globs.
// The same name is the parameter of the tool's sync job that outranks it.
const ImportOption = "import"

// ImportList selects the projects of a delegated workspace that its sync
// writes as import mode writes them. An entry matches a project's
// workspace-relative directory: a leading / is ignored, and *, ? and [...]
// match within one path segment, as path.Match does.
type ImportList struct {
	entries  []string
	patterns []string
	matched  []bool
}

// NewImportList checks each entry and returns the list that holds them, in
// their order. An empty entry, one that leaves the workspace or a malformed
// pattern is an error.
func NewImportList(entries []string) (*ImportList, error) {
	l := &ImportList{}
	for _, entry := range entries {
		pattern, err := importPattern(entry)
		if err != nil {
			return nil, err
		}
		l.entries = append(l.entries, entry)
		l.patterns = append(l.patterns, pattern)
		l.matched = append(l.matched, false)
	}
	return l, nil
}

func importPattern(entry string) (string, error) {
	trimmed := strings.TrimSpace(entry)
	pattern := path.Clean(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/"), "./"))
	if trimmed == "" || pattern == "." || pattern == ".." || strings.HasPrefix(pattern, "../") || strings.HasPrefix(pattern, "-") {
		return "", fmt.Errorf("the import entry %q names no project directory under the workspace root", entry)
	}
	if strings.Contains(pattern, "**") {
		return "", fmt.Errorf("the import entry %q uses **, which matches within one path segment only: list each level, such as packages/* or packages/*/*", entry)
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return "", fmt.Errorf("the import entry %q is no valid pattern: %w", entry, err)
	}
	return pattern, nil
}

// Selects reports whether an entry matches the workspace-relative directory,
// and records each entry that does. The workspace root is never selected.
func (l *ImportList) Selects(dir string) bool {
	if l == nil {
		return false
	}
	dir = path.Clean(strings.TrimPrefix(dir, "/"))
	if dir == "." {
		return false
	}
	selected := false
	for i, pattern := range l.patterns {
		if ok, _ := path.Match(pattern, dir); ok {
			l.matched[i] = true
			selected = true
		}
	}
	return selected
}

// Len is the number of entries.
func (l *ImportList) Len() int {
	if l == nil {
		return 0
	}
	return len(l.entries)
}

// Unmatched are the entries, as written and in their order, that matched no
// directory Selects was asked about.
func (l *ImportList) Unmatched() []string {
	if l == nil {
		return nil
	}
	var out []string
	for i, entry := range l.entries {
		if !l.matched[i] {
			out = append(out, entry)
		}
	}
	return out
}

// ImportEntries reads the import member of a tool's workspace option layer,
// as decoded from JSON: a list of strings, or absent. Any other value is an
// error that names the extension.
func ImportEntries(extension string, layer map[string]any) ([]string, error) {
	value, ok := layer[ImportOption]
	if !ok || value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("options[%q].%s is %s, not a list of project paths", extension, ImportOption, describeJSON(value))
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("options[%q].%s holds %s, not a project path", extension, ImportOption, describeJSON(item))
		}
		out = append(out, s)
	}
	return out, nil
}

// ImportParam reads the import parameter of a sync job: a JSON list of
// strings, or a comma-separated string, as a flag gives it. ok is false when
// the parameter is absent.
func ImportParam(raw json.RawMessage) (entries []string, ok bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if json.Unmarshal(raw, &entries) == nil {
		return entries, true, nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return nil, false, fmt.Errorf("the %s parameter is %s, not a list of project paths", ImportOption, raw)
	}
	entries = []string{}
	for _, entry := range strings.Split(text, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries, true, nil
}

func describeJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}
