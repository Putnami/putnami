// Package yaml reads the YAML that the Dart detector and the pubspec.lock
// reader need: pubspec.yaml, pubspec.lock and melos.yaml.
//
// It reads the first document of a block-style YAML 1.2 file into maps:
// mappings are Map, sequences []any, and scalars string, or nil for an empty
// or null value. Booleans and numbers stay the text the document writes: no
// reader of observe computes with them. Flow collections, quoted scalars,
// block scalars, multi-line plain scalars, comments, tags and anchors read;
// an alias, a complex key and a tab in indentation are errors that name their
// line.
package yaml

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Map is a YAML mapping.
type Map = map[string]any

// Parse reads the first document of data. An empty document is nil.
func Parse(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("the document is not UTF-8")
	}
	lines, err := split(string(data))
	if err != nil {
		return nil, err
	}
	p := &parser{lines: lines}
	l, ok := p.peek()
	if !ok {
		return nil, nil
	}
	value, err := p.node(l.indent)
	if err != nil {
		return nil, err
	}
	if l, ok := p.peek(); ok {
		return nil, l.errorf("unexpected content at indentation %d", l.indent)
	}
	return value, nil
}

// Get walks doc through the mapping keys and returns the value it reaches,
// nil when a key is missing or a value on the way is not a mapping.
func Get(doc any, keys ...string) any {
	for _, key := range keys {
		m, ok := doc.(Map)
		if !ok {
			return nil
		}
		doc = m[key]
	}
	return doc
}

// String returns the string Get reaches, "" for any other value.
func String(doc any, keys ...string) string {
	s, _ := Get(doc, keys...).(string)
	return s
}

// Strings returns the strings of a sequence of scalars. ok is false when value
// is not a sequence or holds another value; nil is an empty sequence.
func Strings(value any) (out []string, ok bool) {
	if value == nil {
		return nil, true
	}
	list, isList := value.([]any)
	if !isList {
		return nil, false
	}
	for _, item := range list {
		s, isString := item.(string)
		if !isString {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// line is one line of the document: its indentation in spaces, its content
// without the indentation and a trailing comment, and the raw text block
// scalars read.
type line struct {
	number int
	indent int
	text   string
	raw    string
}

func (l line) errorf(format string, args ...any) error {
	return fmt.Errorf("line %d: %s", l.number, fmt.Sprintf(format, args...))
}

// split cuts the first document into lines. A blank or comment-only line has
// an empty text.
func split(doc string) ([]line, error) {
	doc = strings.TrimPrefix(doc, "\ufeff")
	var out []line
	content := false
	for i, raw := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimLeft(raw, " ")
		indent := len(raw) - len(trimmed)
		l := line{number: i + 1, indent: indent, raw: raw}
		if indent == 0 {
			switch {
			case strings.HasPrefix(raw, "%") && !content:
				continue
			case raw == "---" || strings.HasPrefix(raw, "--- "):
				if content {
					return out, nil
				}
				rest := strings.TrimSpace(strings.TrimPrefix(raw, "---"))
				if stripComment(rest) == "" {
					continue
				}
				return nil, l.errorf("content on the document start line is not supported")
			case raw == "...":
				return out, nil
			}
		}
		l.text = stripComment(trimmed)
		if l.text != "" {
			if strings.HasPrefix(trimmed, "\t") {
				return nil, l.errorf("a tab indents the line")
			}
			content = true
		}
		out = append(out, l)
	}
	return out, nil
}

type parser struct {
	lines []line
	pos   int
}

// peek returns the next line with content, skipping blank ones.
func (p *parser) peek() (line, bool) {
	for p.pos < len(p.lines) && p.lines[p.pos].text == "" {
		p.pos++
	}
	if p.pos == len(p.lines) {
		return line{}, false
	}
	return p.lines[p.pos], true
}

// node reads the node whose first line is the next one, at indent.
func (p *parser) node(indent int) (any, error) {
	l, ok := p.peek()
	if !ok || l.indent < indent {
		return nil, nil
	}
	if isItem(l.text) {
		return p.sequence(l.indent)
	}
	if _, _, isKey, err := splitKey(l); err != nil {
		return nil, err
	} else if isKey {
		return p.mapping(l.indent)
	}
	p.pos++
	return p.inline(l, l.text, l.indent-1)
}

func (p *parser) mapping(indent int) (Map, error) {
	out := Map{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return out, nil
		}
		if l.indent > indent {
			return nil, l.errorf("unexpected indentation %d inside a mapping at %d", l.indent, indent)
		}
		if isItem(l.text) {
			return nil, l.errorf("a sequence item where a mapping key is expected")
		}
		key, rest, isKey, err := splitKey(l)
		if err != nil {
			return nil, err
		}
		if !isKey {
			return nil, l.errorf("a mapping key is expected")
		}
		p.pos++
		value, err := p.value(l, indent, rest)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
}

// value reads the value of a mapping key at indent, whose line holds rest
// after the colon.
func (p *parser) value(l line, indent int, rest string) (any, error) {
	rest = stripProperties(rest)
	if rest == "" {
		next, ok := p.peek()
		switch {
		case !ok:
			return nil, nil
		case next.indent > indent:
			return p.node(next.indent)
		case next.indent == indent && isItem(next.text):
			return p.sequence(indent)
		}
		return nil, nil
	}
	if rest[0] == '|' || rest[0] == '>' {
		return p.block(l, indent, rest)
	}
	return p.inline(l, rest, indent)
}

func (p *parser) sequence(indent int) ([]any, error) {
	out := []any{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return out, nil
		}
		if l.indent > indent {
			return nil, l.errorf("unexpected indentation %d inside a sequence at %d", l.indent, indent)
		}
		if !isItem(l.text) {
			return out, nil
		}
		after := l.text[1:]
		rest := strings.TrimLeft(after, " \t")
		rest = stripProperties(rest)
		var item any
		var err error
		switch {
		case rest == "":
			p.pos++
			if next, ok := p.peek(); ok && next.indent > indent {
				item, err = p.node(next.indent)
			}
		case rest[0] == '|' || rest[0] == '>':
			p.pos++
			item, err = p.block(l, indent, rest)
		default:
			// The item's content is a node at its own column: the rest of the
			// line takes the place of the item line.
			column := indent + 1 + len(after) - len(strings.TrimLeft(after, " \t"))
			p.lines[p.pos] = line{number: l.number, indent: column, text: rest, raw: l.raw}
			item, err = p.node(column)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
}

// inline reads a scalar or a flow collection that starts on l with text. A
// flow collection may continue on the next lines; a plain scalar continues
// on the next lines indented past parent.
func (p *parser) inline(l line, text string, parent int) (any, error) {
	if text[0] == '[' || text[0] == '{' {
		for !balanced(text) {
			if p.pos == len(p.lines) {
				return nil, l.errorf("an unterminated flow collection")
			}
			if next := p.lines[p.pos].text; next != "" {
				text += " " + next
			}
			p.pos++
		}
		f := &flow{text: text}
		value, err := f.value(false)
		if err != nil {
			return nil, l.errorf("%v", err)
		}
		f.space()
		if f.i != len(f.text) {
			return nil, l.errorf("unexpected text after a flow collection")
		}
		return value, nil
	}
	for {
		next, ok := p.peek()
		if !ok || next.indent <= parent || isQuoted(text) && closed(text) {
			break
		}
		if _, _, isKey, _ := splitKey(next); isKey && !isQuoted(text) {
			return nil, next.errorf("unexpected indentation %d after a scalar", next.indent)
		}
		text += " " + next.text
		p.pos++
	}
	value, err := scalar(text)
	if err != nil {
		return nil, l.errorf("%v", err)
	}
	return value, nil
}

// block reads a literal (|) or folded (>) block scalar whose header is on l,
// with the lines indented past parent.
func (p *parser) block(l line, parent int, header string) (any, error) {
	style := header[0]
	chomp := byte(0)
	explicit := 0
	for _, c := range header[1:] {
		switch {
		case c == '-' || c == '+':
			chomp = byte(c)
		case c >= '1' && c <= '9':
			explicit = int(c - '0')
		case c == ' ':
		default:
			return nil, l.errorf("an invalid block scalar header %q", header)
		}
	}
	var body []string
	indent := 0
	if explicit > 0 {
		indent = parent + explicit
	}
	for p.pos < len(p.lines) {
		raw := p.lines[p.pos].raw
		blank := strings.TrimSpace(raw) == ""
		rawIndent := len(raw) - len(strings.TrimLeft(raw, " "))
		if !blank && rawIndent <= parent {
			break
		}
		if !blank && indent == 0 {
			indent = rawIndent
		}
		if blank {
			body = append(body, "")
		} else if rawIndent < indent {
			return nil, p.lines[p.pos].errorf("a block scalar line is less indented than its first line")
		} else {
			body = append(body, raw[indent:])
		}
		p.pos++
	}
	trailing := 0
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
		trailing++
	}
	var text string
	if style == '|' {
		text = strings.Join(body, "\n")
	} else {
		var b strings.Builder
		for i, s := range body {
			switch {
			case s == "":
				b.WriteByte('\n')
				continue
			case i == 0 || body[i-1] == "":
			case strings.HasPrefix(s, " ") || strings.HasPrefix(body[i-1], " "):
				b.WriteByte('\n')
			default:
				b.WriteByte(' ')
			}
			b.WriteString(s)
		}
		text = b.String()
	}
	switch {
	case len(body) == 0:
	case chomp == '-':
	case chomp == '+':
		text += strings.Repeat("\n", trailing+1)
	default:
		text += "\n"
	}
	return text, nil
}

// isItem reports whether a line's text starts a sequence item.
func isItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ") || strings.HasPrefix(text, "-\t")
}

// splitKey splits "key: rest". isKey is false for a line that is no mapping
// entry.
func splitKey(l line) (key, rest string, isKey bool, err error) {
	text := l.text
	switch text[0] {
	case '[', '{', '|', '>', '*':
		return "", "", false, nil
	case '?':
		if text == "?" || strings.HasPrefix(text, "? ") {
			return "", "", false, l.errorf("a complex mapping key is not supported")
		}
	case '"', '\'':
		end := quoteEnd(text)
		if end < 0 {
			return "", "", false, nil
		}
		after := strings.TrimLeft(text[end+1:], " ")
		if after != ":" && !strings.HasPrefix(after, ": ") && !strings.HasPrefix(after, ":\t") {
			return "", "", false, nil
		}
		value, err := scalar(text[:end+1])
		if err != nil {
			return "", "", false, l.errorf("%v", err)
		}
		key, _ = value.(string)
		return key, strings.Trim(after[1:], " \t"), true, nil
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ':' && (i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\t') {
			value, err := scalar(strings.TrimSpace(text[:i]))
			if err != nil {
				return "", "", false, l.errorf("%v", err)
			}
			key, _ = value.(string)
			return key, strings.Trim(text[i+1:], " \t"), true, nil
		}
	}
	return "", "", false, nil
}

// stripProperties drops the tag and the anchor that may lead a node.
func stripProperties(text string) string {
	for text != "" && (text[0] == '!' || text[0] == '&') {
		cut := strings.IndexByte(text, ' ')
		if cut < 0 {
			return ""
		}
		text = strings.TrimLeft(text[cut:], " ")
	}
	return text
}

// scalar reads one scalar: double-quoted, single-quoted or plain. An empty,
// ~ or null scalar is nil.
func scalar(text string) (any, error) {
	text = stripProperties(strings.TrimSpace(text))
	switch text {
	case "", "~", "null", "Null", "NULL":
		return nil, nil
	}
	switch text[0] {
	case '*':
		return nil, errors.New("an alias is not supported")
	case '"':
		if !closed(text) {
			return nil, errors.New("an unterminated double-quoted scalar")
		}
		return unescape(text[1 : len(text)-1])
	case '\'':
		if !closed(text) {
			return nil, errors.New("an unterminated single-quoted scalar")
		}
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil
	}
	return text, nil
}

func isQuoted(text string) bool {
	return text != "" && (text[0] == '"' || text[0] == '\'')
}

// closed reports whether a quoted text ends with its closing quote.
func closed(text string) bool {
	return quoteEnd(text) == len(text)-1
}

// quoteEnd returns the index of the quote that closes the scalar text starts
// with, -1 when there is none.
func quoteEnd(text string) int {
	quote := text[0]
	for i := 1; i < len(text); i++ {
		switch {
		case quote == '"' && text[i] == '\\':
			i++
		case text[i] == quote && quote == '\'' && i+1 < len(text) && text[i+1] == '\'':
			i++
		case text[i] == quote:
			return i
		}
	}
	return -1
}

// escapes are the one-character escapes of a double-quoted scalar, and
// escapeWidths the hexadecimal digits of the code point ones.
var (
	escapes = map[byte]string{
		'0': "\x00", 'a': "\a", 'b': "\b", 't': "\t", '\t': "\t", 'n': "\n", 'v': "\v", 'f': "\f", 'r': "\r", 'e': "\x1b",
		' ': " ", '"': `"`, '/': "/", '\\': `\`, 'N': "\u0085", '_': "\u00a0", 'L': "\u2028", 'P': "\u2029",
	}
	escapeWidths = map[byte]int{'x': 2, 'u': 4, 'U': 8}
)

// unescape reads the escapes of a double-quoted scalar.
func unescape(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i == len(s) {
			return "", errors.New("a double-quoted scalar ends with a backslash")
		}
		if v, ok := escapes[s[i]]; ok {
			b.WriteString(v)
			continue
		}
		width := escapeWidths[s[i]]
		if width == 0 || i+1+width > len(s) {
			return "", fmt.Errorf("an unknown escape \\%c", s[i])
		}
		code, err := strconv.ParseUint(s[i+1:i+1+width], 16, 32)
		if err != nil {
			return "", fmt.Errorf("an invalid escape \\%c%s", s[i], s[i+1:i+1+width])
		}
		b.WriteRune(rune(code))
		i += width
	}
	return b.String(), nil
}

// stripComment removes a comment that starts outside quotes, and the spaces
// before it. A quote opens a scalar only where a scalar can start.
func stripComment(text string) string {
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case (c == '"' || c == '\'') && (i == 0 || strings.IndexByte(" \t[{,:-?", text[i-1]) >= 0):
			end := quoteEnd(text[i:])
			if end < 0 {
				return strings.TrimRight(text, " \t")
			}
			i += end
		case c == '#' && (i == 0 || text[i-1] == ' ' || text[i-1] == '\t'):
			return strings.TrimRight(text[:i], " \t")
		}
	}
	return strings.TrimRight(text, " \t")
}

// balanced reports whether the brackets of a flow collection close.
func balanced(text string) bool {
	depth := 0
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case '"', '\'':
			end := quoteEnd(text[i:])
			if end < 0 {
				return false
			}
			i += end
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return depth <= 0
}

// flow reads a flow collection.
type flow struct {
	text string
	i    int
}

func (f *flow) space() {
	for f.i < len(f.text) && f.text[f.i] == ' ' {
		f.i++
	}
}

// value reads one flow node. A key stops at a colon.
func (f *flow) value(key bool) (any, error) {
	f.space()
	if f.i == len(f.text) {
		return nil, errors.New("a flow collection ends early")
	}
	switch f.text[f.i] {
	case '[':
		return f.sequence()
	case '{':
		return f.mapping()
	case '"', '\'':
		end := quoteEnd(f.text[f.i:])
		if end < 0 {
			return nil, errors.New("an unterminated quoted scalar")
		}
		text := f.text[f.i : f.i+end+1]
		f.i += end + 1
		return scalar(text)
	}
	start := f.i
	for f.i < len(f.text) {
		c := f.text[f.i]
		if c == ',' || c == ']' || c == '}' {
			break
		}
		if key && c == ':' && (f.i+1 == len(f.text) || strings.IndexByte(" ,]}", f.text[f.i+1]) >= 0) {
			break
		}
		f.i++
	}
	return scalar(f.text[start:f.i])
}

func (f *flow) sequence() ([]any, error) {
	f.i++
	out := []any{}
	for {
		f.space()
		if f.i < len(f.text) && f.text[f.i] == ']' {
			f.i++
			return out, nil
		}
		item, err := f.value(false)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		if err := f.separator(']'); err != nil {
			return nil, err
		}
	}
}

func (f *flow) mapping() (Map, error) {
	f.i++
	out := Map{}
	for {
		f.space()
		if f.i < len(f.text) && f.text[f.i] == '}' {
			f.i++
			return out, nil
		}
		k, err := f.value(true)
		if err != nil {
			return nil, err
		}
		key, _ := k.(string)
		f.space()
		var v any
		if f.i < len(f.text) && f.text[f.i] == ':' {
			f.i++
			f.space()
			if f.i < len(f.text) && f.text[f.i] != ',' && f.text[f.i] != '}' {
				if v, err = f.value(false); err != nil {
					return nil, err
				}
			}
		}
		out[key] = v
		if err := f.separator('}'); err != nil {
			return nil, err
		}
	}
}

// separator reads the comma after an entry, or leaves the closing bracket.
func (f *flow) separator(end byte) error {
	f.space()
	switch {
	case f.i == len(f.text):
		return errors.New("a flow collection is not closed")
	case f.text[f.i] == ',':
		f.i++
		return nil
	case f.text[f.i] == end:
		return nil
	}
	return fmt.Errorf("unexpected %q in a flow collection", f.text[f.i])
}
