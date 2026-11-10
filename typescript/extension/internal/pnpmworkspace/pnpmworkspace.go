// Package pnpmworkspace reads and edits the two members of pnpm-workspace.yaml
// that a TypeScript workspace install owns: the `packages` list of members and
// the `catalog`/`catalogs` mappings that back the catalog: protocol.
//
// It is not a YAML parser. It reads the block and flow forms pnpm documents
// for those keys, at the top level of a one-document file, and it edits by
// replacing or inserting lines: every line it does not own stays byte for byte,
// comments included. A shape it cannot read is an error, never a guess.
package pnpmworkspace

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// File is the name of the pnpm workspace file at the workspace root.
const File = "pnpm-workspace.yaml"

// Catalogs are the catalog entries the file declares: Default backs the bare
// catalog: protocol, Named backs catalog:<name>. A nil Default means the file
// declares no `catalog` key.
type Catalogs struct {
	Default map[string]string
	Named   map[string]map[string]string
}

// document is the file split into lines, with the line ending it uses.
type document struct {
	lines []string
	eol   string
	// final reports whether the last line ended with a line ending.
	final bool
}

func parseDocument(text string) document {
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	if text == "" {
		return document{eol: eol, final: true}
	}
	final := strings.HasSuffix(text, "\n")
	body := strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
	return document{lines: strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n"), eol: eol, final: final}
}

func (d document) String() string {
	if len(d.lines) == 0 {
		return ""
	}
	out := strings.Join(d.lines, d.eol)
	if d.final {
		out += d.eol
	}
	return out
}

// indentOf returns the number of leading spaces of s. A tab in the
// indentation is an error YAML forbids, reported by the callers that read it.
func indentOf(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

// blank reports whether a line holds nothing but spaces or a comment.
func blank(s string) bool {
	trimmed := strings.TrimSpace(s)
	return trimmed == "" || strings.HasPrefix(trimmed, "#")
}

// topKey reads a top-level `key: value` line and returns the key and the raw
// value after the colon, comment included. ok is false for any other line.
func topKey(s string) (key, rest string, ok bool) {
	if s == "" || s[0] == ' ' || s[0] == '\t' || blank(s) || s == "---" || s == "..." {
		return "", "", false
	}
	return mappingKey(s)
}

// mappingKey reads `key: value` at the start of s (after its indentation),
// where key is plain, single-quoted or double-quoted.
func mappingKey(s string) (key, rest string, ok bool) {
	body := strings.TrimLeft(s, " ")
	if body == "" {
		return "", "", false
	}
	switch body[0] {
	case '\'', '"':
		value, n, err := quoted(body)
		if err != nil {
			return "", "", false
		}
		after := body[n:]
		if !strings.HasPrefix(after, ":") {
			return "", "", false
		}
		return value, after[1:], true
	case '-', '#', '[', '{':
		return "", "", false
	}
	for i := 0; i < len(body); i++ {
		if body[i] == ':' && (i+1 == len(body) || body[i+1] == ' ' || body[i+1] == '\t') {
			return strings.TrimSpace(body[:i]), body[i+1:], true
		}
	}
	return "", "", false
}

// quoted reads the quoted scalar at the start of s and returns its value and
// the number of bytes it took.
func quoted(s string) (string, int, error) {
	if s == "" {
		return "", 0, errors.New("empty scalar")
	}
	switch s[0] {
	case '\'':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '\'' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return b.String(), i + 1, nil
		}
		return "", 0, fmt.Errorf("unterminated quoted scalar %s", s)
	case '"':
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				value, err := strconv.Unquote(s[:i+1])
				if err != nil {
					return "", 0, fmt.Errorf("quoted scalar %s: %w", s[:i+1], err)
				}
				return value, i + 1, nil
			}
		}
		return "", 0, fmt.Errorf("unterminated quoted scalar %s", s)
	}
	return "", 0, fmt.Errorf("%s is not quoted", s)
}

// scalar reads the value of a key or a sequence item: a quoted scalar, or a
// plain one up to a comment. empty is true when nothing but a comment follows.
func scalar(raw string) (value string, empty bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", true, nil
	}
	if raw[0] == '\'' || raw[0] == '"' {
		value, n, err := quoted(raw)
		if err != nil {
			return "", false, err
		}
		if rest := strings.TrimSpace(raw[n:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return "", false, fmt.Errorf("unexpected %q after %s", rest, raw[:n])
		}
		return value, false, nil
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return raw, false, nil
}

// flowItems splits the inside of a flow collection on its top-level commas,
// outside quotes.
func flowItems(inner string) ([]string, error) {
	var items []string
	var current strings.Builder
	quote := byte(0)
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case quote == '\'' && c == '\'':
			if i+1 < len(inner) && inner[i+1] == '\'' {
				current.WriteByte(c)
				i++
			} else {
				quote = 0
			}
		case quote == '"' && c == '\\':
			current.WriteByte(c)
			if i+1 < len(inner) {
				i++
				c = inner[i]
			}
		case quote == '"' && c == '"':
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == 0 && (c == '[' || c == '{' || c == ']' || c == '}'):
			return nil, errors.New("nested flow collections are not supported")
		case quote == 0 && c == ',':
			items = append(items, current.String())
			current.Reset()
			continue
		}
		current.WriteByte(c)
	}
	if quote != 0 {
		return nil, errors.New("unterminated quoted scalar in a flow collection")
	}
	if last := strings.TrimSpace(current.String()); last != "" {
		items = append(items, current.String())
	}
	return items, nil
}

// flowInner returns the inside of a one-line flow collection opened by open,
// with its trailing comment dropped. ok is false when raw is no such
// collection.
func flowInner(raw string, open, close byte) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != open {
		return "", false, nil
	}
	end := strings.LastIndexByte(raw, close)
	if end < 0 {
		return "", true, fmt.Errorf("a flow collection that spans lines is not supported: %s", raw)
	}
	if rest := strings.TrimSpace(raw[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", true, fmt.Errorf("unexpected %q after %s", rest, raw[:end+1])
	}
	return raw[1:end], true, nil
}

// block is the span of a top-level key: its line, and the lines after it that
// belong to its value. end is one past the last line that holds a value, so
// blank and comment lines that follow the value stay outside it.
type block struct {
	key   int
	end   int
	value string
}

// findTop returns the block of the top-level key name. found is false when
// the file declares no such key.
func findTop(d document, name string) (block, bool) {
	for i, s := range d.lines {
		key, rest, ok := topKey(s)
		if !ok || key != name {
			continue
		}
		b := block{key: i, end: i + 1, value: rest}
		for j := i + 1; j < len(d.lines); j++ {
			next := d.lines[j]
			if blank(next) {
				continue
			}
			if next[0] == ' ' || next[0] == '\t' || strings.HasPrefix(next, "- ") || next == "-" {
				b.end = j + 1
				continue
			}
			break
		}
		return b, true
	}
	return block{}, false
}

// Packages returns the `packages` list of text. found is false when the file
// declares no `packages` key.
func Packages(text string) (packages []string, found bool, err error) {
	d := parseDocument(text)
	b, found := findTop(d, "packages")
	if !found {
		return nil, false, nil
	}
	list, err := sequence(d, b)
	if err != nil {
		return nil, true, fmt.Errorf("%s packages: %w", File, err)
	}
	return list, true, nil
}

// sequence reads the list value of b: a flow list on its key line, or the
// block items under it.
func sequence(d document, b block) ([]string, error) {
	inner, isFlow, err := flowInner(b.value, '[', ']')
	if err != nil {
		return nil, err
	}
	if isFlow {
		items, err := flowItems(inner)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			value, empty, err := scalar(item)
			if err != nil {
				return nil, err
			}
			if !empty {
				out = append(out, value)
			}
		}
		return out, nil
	}
	if _, empty, err := scalar(b.value); err != nil || !empty {
		return nil, fmt.Errorf("is not a list: %s", strings.TrimSpace(b.value))
	}
	var out []string
	for _, s := range d.lines[b.key+1 : b.end] {
		if blank(s) {
			continue
		}
		body := strings.TrimLeft(s, " ")
		if strings.HasPrefix(s, "\t") || !(strings.HasPrefix(body, "- ") || body == "-") {
			return nil, fmt.Errorf("is not a list of strings: %s", strings.TrimSpace(s))
		}
		value, empty, err := scalar(strings.TrimPrefix(body, "-"))
		if err != nil {
			return nil, err
		}
		if empty {
			return nil, fmt.Errorf("holds an empty item: %s", strings.TrimSpace(s))
		}
		out = append(out, value)
	}
	return out, nil
}

// SetPackages returns text with its `packages` list replaced by desired, in
// block form. A list in block form keeps the indentation of its first item. A
// file without the key gains it as its first line; an empty desired list is
// written `packages: []`. Every other line is kept.
func SetPackages(text string, desired []string) string {
	d := parseDocument(text)
	itemPrefix := "  - "
	var replaced []string
	if len(desired) == 0 {
		replaced = []string{"packages: []"}
	} else {
		replaced = []string{"packages:"}
	}
	b, found := findTop(d, "packages")
	if found {
		for _, s := range d.lines[b.key+1 : b.end] {
			if blank(s) {
				continue
			}
			body := strings.TrimLeft(s, " ")
			if strings.HasPrefix(body, "-") {
				itemPrefix = s[:indentOf(s)] + "- "
			}
			break
		}
	}
	for _, path := range desired {
		replaced = append(replaced, itemPrefix+renderScalar(path))
	}
	if !found {
		d.lines = append(replaced, d.lines...)
		d.final = true
		return d.String()
	}
	lines := make([]string, 0, len(d.lines)-(b.end-b.key)+len(replaced))
	lines = append(lines, d.lines[:b.key]...)
	lines = append(lines, replaced...)
	lines = append(lines, d.lines[b.end:]...)
	d.lines = lines
	return d.String()
}

// ReadCatalogs returns the `catalog` and `catalogs` entries of text.
func ReadCatalogs(text string) (Catalogs, error) {
	d := parseDocument(text)
	var c Catalogs
	if b, found := findTop(d, "catalog"); found {
		entries, err := mapping(d, b.key, b.end, b.value)
		if err != nil {
			return Catalogs{}, fmt.Errorf("%s catalog: %w", File, err)
		}
		c.Default = map[string]string{}
		for _, e := range entries {
			c.Default[e.key] = e.value
		}
	}
	if b, found := findTop(d, "catalogs"); found {
		if _, empty, err := scalar(b.value); err != nil || !empty {
			if inner, isFlow, ferr := flowInner(b.value, '{', '}'); ferr == nil && isFlow && strings.TrimSpace(inner) == "" {
				return c, nil
			}
			return Catalogs{}, fmt.Errorf("%s catalogs is not a block mapping of catalogs: %s", File, strings.TrimSpace(b.value))
		}
		children, err := childBlocks(d, b)
		if err != nil {
			return Catalogs{}, fmt.Errorf("%s catalogs: %w", File, err)
		}
		c.Named = map[string]map[string]string{}
		for _, child := range children {
			entries, err := mapping(d, child.key, child.end, child.value)
			if err != nil {
				return Catalogs{}, fmt.Errorf("%s catalogs.%s: %w", File, child.name, err)
			}
			cat := map[string]string{}
			for _, e := range entries {
				cat[e.key] = e.value
			}
			c.Named[child.name] = cat
		}
	}
	return c, nil
}

// entry is one `key: value` line of a block mapping.
type entry struct {
	key, value string
	line       int
	indent     int
}

// mapping reads the mapping value whose key line is at keyLine and whose
// block ends before end: a flow mapping on the key line, or one entry per
// line under it at one indentation.
func mapping(d document, keyLine, end int, value string) ([]entry, error) {
	inner, isFlow, err := flowInner(value, '{', '}')
	if err != nil {
		return nil, err
	}
	if isFlow {
		items, err := flowItems(inner)
		if err != nil {
			return nil, err
		}
		var out []entry
		for _, item := range items {
			key, rest, ok := mappingKey(strings.TrimSpace(item))
			if !ok {
				return nil, fmt.Errorf("is not a mapping of strings: %s", strings.TrimSpace(item))
			}
			v, empty, err := scalar(rest)
			if err != nil {
				return nil, err
			}
			if empty {
				return nil, fmt.Errorf("%s has no version", key)
			}
			out = append(out, entry{key: key, value: v, line: keyLine})
		}
		return out, nil
	}
	if _, empty, err := scalar(value); err != nil || !empty {
		return nil, fmt.Errorf("is not a mapping: %s", strings.TrimSpace(value))
	}
	var out []entry
	indent := -1
	for i := keyLine + 1; i < end; i++ {
		s := d.lines[i]
		if blank(s) {
			continue
		}
		if strings.HasPrefix(s, "\t") {
			return nil, fmt.Errorf("is indented with a tab: %s", strings.TrimSpace(s))
		}
		n := indentOf(s)
		if indent < 0 {
			indent = n
		}
		if n != indent {
			return nil, fmt.Errorf("is not a flat mapping of strings: %s", strings.TrimSpace(s))
		}
		key, rest, ok := mappingKey(s)
		if !ok {
			return nil, fmt.Errorf("is not a mapping of strings: %s", strings.TrimSpace(s))
		}
		v, empty, err := scalar(rest)
		if err != nil {
			return nil, err
		}
		if empty {
			return nil, fmt.Errorf("%s has no version", key)
		}
		out = append(out, entry{key: key, value: v, line: i, indent: n})
	}
	return out, nil
}

// child is one named catalog under `catalogs`.
type child struct {
	name   string
	key    int
	end    int
	value  string
	indent int
}

// childBlocks returns the named catalogs under the `catalogs` block b, each
// with the span of its entries.
func childBlocks(d document, b block) ([]child, error) {
	var out []child
	indent := -1
	for i := b.key + 1; i < b.end; i++ {
		s := d.lines[i]
		if blank(s) {
			continue
		}
		if strings.HasPrefix(s, "\t") {
			return nil, fmt.Errorf("is indented with a tab: %s", strings.TrimSpace(s))
		}
		n := indentOf(s)
		if indent < 0 {
			indent = n
		}
		if n < indent {
			return nil, fmt.Errorf("is not a mapping of catalogs: %s", strings.TrimSpace(s))
		}
		if n > indent {
			if len(out) == 0 {
				return nil, fmt.Errorf("is not a mapping of catalogs: %s", strings.TrimSpace(s))
			}
			out[len(out)-1].end = i + 1
			continue
		}
		name, rest, ok := mappingKey(s)
		if !ok {
			return nil, fmt.Errorf("is not a mapping of catalogs: %s", strings.TrimSpace(s))
		}
		out = append(out, child{name: name, key: i, end: i + 1, value: rest, indent: n})
	}
	return out, nil
}

// Seed is one catalog entry to add: Catalog "" selects the default catalog.
type Seed struct {
	Catalog string
	Name    string
	Version string
}

// AddCatalogEntries returns text with each seed added to the catalog it
// selects, after the catalog's last entry, at the indentation its entries
// use. A missing `catalog`, `catalogs` or named catalog is appended at the end
// of the file. An entry the catalog already holds is left as it is. A catalog
// written as a non-empty flow mapping is an error: its entries are not lines
// an entry can be added after.
func AddCatalogEntries(text string, seeds []Seed) (string, error) {
	for _, seed := range seeds {
		var err error
		text, err = addCatalogEntry(text, seed)
		if err != nil {
			return "", err
		}
	}
	return text, nil
}

func addCatalogEntry(text string, seed Seed) (string, error) {
	current, err := ReadCatalogs(text)
	if err != nil {
		return "", err
	}
	if seed.Catalog == "" {
		if _, ok := current.Default[seed.Name]; ok {
			return text, nil
		}
	} else if _, ok := current.Named[seed.Catalog][seed.Name]; ok {
		return text, nil
	}
	d := parseDocument(text)
	entryLine := func(indent int) string {
		return strings.Repeat(" ", indent) + renderScalar(seed.Name) + ": " + renderScalar(seed.Version)
	}

	if seed.Catalog == "" {
		b, found := findTop(d, "catalog")
		if !found {
			d = appendLines(d, "catalog:", entryLine(2))
			return d.String(), nil
		}
		return insertEntry(d, b.key, b.end, b.value, 2, entryLine, "catalog")
	}

	b, found := findTop(d, "catalogs")
	if !found {
		d = appendLines(d, "catalogs:", "  "+renderScalar(seed.Catalog)+":", entryLine(4))
		return d.String(), nil
	}
	if inner, isFlow, _ := flowInner(b.value, '{', '}'); isFlow {
		if strings.TrimSpace(inner) != "" {
			return "", fmt.Errorf("%s catalogs is a flow mapping: write it as a block mapping so an entry can be added", File)
		}
		d.lines[b.key] = "catalogs:"
		d = insertLines(d, b.key+1, "  "+renderScalar(seed.Catalog)+":", entryLine(4))
		return d.String(), nil
	}
	children, err := childBlocks(d, b)
	if err != nil {
		return "", fmt.Errorf("%s catalogs: %w", File, err)
	}
	childIndent := 2
	for _, c := range children {
		childIndent = c.indent
		if c.name == seed.Catalog {
			return insertEntry(d, c.key, c.end, c.value, c.indent+2, entryLine, "catalogs."+c.name)
		}
	}
	d = insertLines(d, b.end, strings.Repeat(" ", childIndent)+renderScalar(seed.Catalog)+":", entryLine(childIndent+2))
	return d.String(), nil
}

// insertEntry adds an entry to the mapping whose key line is keyLine and whose
// block ends before end.
func insertEntry(d document, keyLine, end int, value string, defaultIndent int, entryLine func(int) string, label string) (string, error) {
	if inner, isFlow, _ := flowInner(value, '{', '}'); isFlow {
		if strings.TrimSpace(inner) != "" {
			return "", fmt.Errorf("%s %s is a flow mapping: write it as a block mapping so an entry can be added", File, label)
		}
		keyText := d.lines[keyLine]
		colon := len(keyText) - len(value)
		d.lines[keyLine] = strings.TrimRight(keyText[:colon+strings.IndexByte(value, '{')], " ")
		return insertLines(d, keyLine+1, entryLine(defaultIndent)).String(), nil
	}
	entries, err := mapping(d, keyLine, end, value)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", File, label, err)
	}
	if len(entries) == 0 {
		return insertLines(d, keyLine+1, entryLine(defaultIndent)).String(), nil
	}
	last := entries[len(entries)-1]
	return insertLines(d, last.line+1, entryLine(last.indent)).String(), nil
}

func insertLines(d document, at int, lines ...string) document {
	out := make([]string, 0, len(d.lines)+len(lines))
	out = append(out, d.lines[:at]...)
	out = append(out, lines...)
	out = append(out, d.lines[at:]...)
	d.lines = out
	if len(d.lines) == len(lines) {
		d.final = true
	}
	return d
}

func appendLines(d document, lines ...string) document {
	d.lines = append(d.lines, lines...)
	d.final = true
	return d
}

// renderScalar writes s as a plain scalar when YAML reads it back unchanged,
// and single-quoted otherwise.
func renderScalar(s string) string {
	if plainSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func plainSafe(s string) bool {
	if s == "" {
		return false
	}
	switch s {
	case "true", "false", "null", "~", "yes", "no", "on", "off":
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '/' || c == '^':
		case i > 0 && (c == '-' || c == '~' || c == '+' || c == '=' || c == '<' || c == '>' || c == '*' || c == '@'):
		default:
			return false
		}
	}
	return true
}
