// Package toml reads the TOML that the Python detector and the Python
// lockfile readers need: pyproject.toml, uv.lock and poetry.lock.
//
// It reads TOML 1.0 documents into maps: tables and inline tables are Table,
// arrays are []any, an array of tables is []Table, strings are string and
// booleans bool. Numbers, dates and times stay the text the document writes,
// as a string: no reader of observe computes with them. A document that does
// not read is an error that names its line.
package toml

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Table is a TOML table.
type Table = map[string]any

// Parse reads a TOML document.
func Parse(data []byte) (Table, error) {
	p := &parser{s: string(data), line: 1}
	root := Table{}
	current := root
	for {
		p.skipBlank()
		if p.eof() {
			return root, nil
		}
		if p.peek() == '[' {
			array := strings.HasPrefix(p.s[p.i:], "[[")
			if array {
				p.i += 2
			} else {
				p.i++
			}
			p.skipSpaces()
			keys, err := p.key()
			if err != nil {
				return nil, err
			}
			p.skipSpaces()
			closing := "]"
			if array {
				closing = "]]"
			}
			if !strings.HasPrefix(p.s[p.i:], closing) {
				return nil, p.errorf("a table header does not end with %s", closing)
			}
			p.i += len(closing)
			if current, err = p.header(root, keys, array); err != nil {
				return nil, err
			}
		} else {
			keys, err := p.key()
			if err != nil {
				return nil, err
			}
			p.skipSpaces()
			if p.eof() || p.peek() != '=' {
				return nil, p.errorf("the key %s is not followed by =", strings.Join(keys, "."))
			}
			p.i++
			p.skipSpaces()
			value, err := p.value()
			if err != nil {
				return nil, err
			}
			if err := p.set(current, keys, value); err != nil {
				return nil, err
			}
		}
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

// Tables returns the tables of an array of tables, or of an array of inline
// tables; another value has none.
func Tables(v any) []Table {
	switch list := v.(type) {
	case []Table:
		return list
	case []any:
		var out []Table
		for _, item := range list {
			if t, ok := item.(Table); ok {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

// Get returns the value at a dotted path of nested tables, or nil.
func Get(t Table, keys ...string) any {
	var v any = t
	for _, key := range keys {
		table, ok := v.(Table)
		if !ok {
			return nil
		}
		v = table[key]
	}
	return v
}

// String returns the string at a dotted path, or "".
func String(t Table, keys ...string) string {
	s, _ := Get(t, keys...).(string)
	return s
}

// Strings returns the strings of the array at a dotted path.
func Strings(t Table, keys ...string) []string {
	list, _ := Get(t, keys...).([]any)
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

type parser struct {
	s    string
	i    int
	line int
}

func (p *parser) eof() bool  { return p.i >= len(p.s) }
func (p *parser) peek() byte { return p.s[p.i] }

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("line %d: %s", p.line, fmt.Sprintf(format, args...))
}

// skipSpaces skips spaces and tabs.
func (p *parser) skipSpaces() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.i++
	}
}

// skipBlank skips whitespace, newlines and comments.
func (p *parser) skipBlank() {
	for !p.eof() {
		switch p.peek() {
		case ' ', '\t', '\r':
			p.i++
		case '\n':
			p.i++
			p.line++
		case '#':
			p.skipComment()
		default:
			return
		}
	}
}

func (p *parser) skipComment() {
	for !p.eof() && p.peek() != '\n' {
		p.i++
	}
}

// endOfLine accepts spaces and a comment up to the end of the line.
func (p *parser) endOfLine() error {
	p.skipSpaces()
	if !p.eof() && p.peek() == '#' {
		p.skipComment()
	}
	if !p.eof() && p.peek() == '\r' {
		p.i++
	}
	if p.eof() {
		return nil
	}
	if p.peek() != '\n' {
		return p.errorf("unexpected %q after a value", p.peek())
	}
	return nil
}

// key reads a dotted key: bare, basic or literal parts joined by dots.
func (p *parser) key() ([]string, error) {
	var keys []string
	for {
		p.skipSpaces()
		if p.eof() {
			return nil, p.errorf("a key is missing")
		}
		var part string
		switch c := p.peek(); {
		case c == '"':
			s, err := p.basicString()
			if err != nil {
				return nil, err
			}
			part = s
		case c == '\'':
			s, err := p.literalString()
			if err != nil {
				return nil, err
			}
			part = s
		default:
			start := p.i
			for !p.eof() && isBare(p.peek()) {
				p.i++
			}
			if p.i == start {
				return nil, p.errorf("a key is missing")
			}
			part = p.s[start:p.i]
		}
		keys = append(keys, part)
		p.skipSpaces()
		if p.eof() || p.peek() != '.' {
			return keys, nil
		}
		p.i++
	}
}

func isBare(c byte) bool {
	return c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// header opens the table a [table] or [[array]] header names.
func (p *parser) header(root Table, keys []string, array bool) (Table, error) {
	parent, err := p.descend(root, keys[:len(keys)-1])
	if err != nil {
		return nil, err
	}
	last := keys[len(keys)-1]
	if array {
		existing, found := parent[last]
		list, ok := existing.([]Table)
		if found && !ok {
			return nil, p.errorf("%s is not an array of tables", strings.Join(keys, "."))
		}
		t := Table{}
		parent[last] = append(list, t)
		return t, nil
	}
	return p.child(parent, last, keys)
}

// descend walks the tables of keys from t, creating the missing ones. An
// array of tables descends into its last table.
func (p *parser) descend(t Table, keys []string) (Table, error) {
	for i, key := range keys {
		next, err := p.child(t, key, keys[:i+1])
		if err != nil {
			return nil, err
		}
		t = next
	}
	return t, nil
}

func (p *parser) child(t Table, key string, path []string) (Table, error) {
	switch existing := t[key].(type) {
	case nil:
		child := Table{}
		t[key] = child
		return child, nil
	case Table:
		return existing, nil
	case []Table:
		if len(existing) > 0 {
			return existing[len(existing)-1], nil
		}
	}
	return nil, p.errorf("%s is not a table", strings.Join(path, "."))
}

// set assigns a value to a dotted key of the current table.
func (p *parser) set(t Table, keys []string, value any) error {
	parent, err := p.descend(t, keys[:len(keys)-1])
	if err != nil {
		return err
	}
	last := keys[len(keys)-1]
	if _, taken := parent[last]; taken {
		return p.errorf("the key %s is defined twice", strings.Join(keys, "."))
	}
	parent[last] = value
	return nil
}

// value reads one value.
func (p *parser) value() (any, error) {
	if p.eof() {
		return nil, p.errorf("a value is missing")
	}
	switch rest := p.s[p.i:]; {
	case strings.HasPrefix(rest, `"""`):
		return p.multilineString(`"""`, true)
	case strings.HasPrefix(rest, "'''"):
		return p.multilineString("'''", false)
	case rest[0] == '"':
		return p.basicString()
	case rest[0] == '\'':
		return p.literalString()
	case rest[0] == '[':
		return p.array()
	case rest[0] == '{':
		return p.inlineTable()
	}
	start := p.i
	for !p.eof() && !strings.ContainsRune(",]}\n#\r", rune(p.peek())) {
		p.i++
	}
	token := strings.TrimSpace(p.s[start:p.i])
	switch token {
	case "":
		return nil, p.errorf("a value is missing")
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return token, nil
}

func (p *parser) array() ([]any, error) {
	p.i++ // [
	list := []any{}
	for {
		p.skipBlank()
		if p.eof() {
			return nil, p.errorf("an array is not closed")
		}
		if p.peek() == ']' {
			p.i++
			return list, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		list = append(list, v)
		p.skipBlank()
		if p.eof() {
			return nil, p.errorf("an array is not closed")
		}
		switch p.peek() {
		case ',':
			p.i++
		case ']':
		default:
			return nil, p.errorf("unexpected %q in an array", p.peek())
		}
	}
}

// inlineTable reads { key = value, ... }; newlines inside are accepted, as
// TOML 1.1 allows.
func (p *parser) inlineTable() (Table, error) {
	p.i++ // {
	t := Table{}
	for {
		p.skipBlank()
		if p.eof() {
			return nil, p.errorf("an inline table is not closed")
		}
		if p.peek() == '}' {
			p.i++
			return t, nil
		}
		keys, err := p.key()
		if err != nil {
			return nil, err
		}
		p.skipSpaces()
		if p.eof() || p.peek() != '=' {
			return nil, p.errorf("the key %s is not followed by =", strings.Join(keys, "."))
		}
		p.i++
		p.skipSpaces()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if err := p.set(t, keys, v); err != nil {
			return nil, err
		}
		p.skipBlank()
		if p.eof() {
			return nil, p.errorf("an inline table is not closed")
		}
		switch p.peek() {
		case ',':
			p.i++
		case '}':
		default:
			return nil, p.errorf("unexpected %q in an inline table", p.peek())
		}
	}
}

func (p *parser) literalString() (string, error) {
	p.i++ // '
	start := p.i
	for !p.eof() && p.peek() != '\'' {
		if p.peek() == '\n' {
			return "", p.errorf("a string is not closed")
		}
		p.i++
	}
	if p.eof() {
		return "", p.errorf("a string is not closed")
	}
	s := p.s[start:p.i]
	p.i++
	return s, nil
}

func (p *parser) basicString() (string, error) {
	p.i++ // "
	var b strings.Builder
	for {
		if p.eof() || p.peek() == '\n' {
			return "", p.errorf("a string is not closed")
		}
		c := p.peek()
		if c == '"' {
			p.i++
			return b.String(), nil
		}
		if c == '\\' {
			if err := p.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		b.WriteByte(c)
		p.i++
	}
}

// multilineString reads a """ or ”' string. A newline right after the
// opening quotes is dropped; in a basic one, a backslash at the end of a line
// joins it to the next non-blank character.
func (p *parser) multilineString(quotes string, basic bool) (string, error) {
	p.i += len(quotes)
	if strings.HasPrefix(p.s[p.i:], "\r\n") {
		p.i += 2
		p.line++
	} else if !p.eof() && p.peek() == '\n' {
		p.i++
		p.line++
	}
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.errorf("a multi-line string is not closed")
		}
		if strings.HasPrefix(p.s[p.i:], quotes) {
			// Up to two quotes before the closing ones belong to the string.
			end := p.i + len(quotes)
			for extra := 0; extra < 2 && end < len(p.s) && p.s[end] == quotes[0]; extra++ {
				end++
			}
			b.WriteString(p.s[p.i : end-len(quotes)])
			p.i = end
			return b.String(), nil
		}
		c := p.peek()
		if c == '\n' {
			p.line++
		}
		if basic && c == '\\' {
			rest := strings.TrimLeft(p.s[p.i+1:], " \t\r")
			if strings.HasPrefix(rest, "\n") {
				p.i++
				for !p.eof() && strings.ContainsRune(" \t\r\n", rune(p.peek())) {
					if p.peek() == '\n' {
						p.line++
					}
					p.i++
				}
				continue
			}
			if err := p.escape(&b); err != nil {
				return "", err
			}
			continue
		}
		b.WriteByte(c)
		p.i++
	}
}

func (p *parser) escape(b *strings.Builder) error {
	if p.i+1 >= len(p.s) {
		return p.errorf("a string is not closed")
	}
	c := p.s[p.i+1]
	p.i += 2
	switch c {
	case '"', '\\':
		b.WriteByte(c)
	case 'n':
		b.WriteByte('\n')
	case 't':
		b.WriteByte('\t')
	case 'r':
		b.WriteByte('\r')
	case 'b':
		b.WriteByte('\b')
	case 'f':
		b.WriteByte('\f')
	case 'e':
		b.WriteByte(0x1b)
	case 'u', 'U':
		size := 4
		if c == 'U' {
			size = 8
		}
		if p.i+size > len(p.s) {
			return p.errorf("a \\%c escape is cut short", c)
		}
		r, ok := hexRune(p.s[p.i : p.i+size])
		if !ok {
			return p.errorf("the \\%c escape %s is not a code point", c, p.s[p.i:p.i+size])
		}
		b.WriteRune(r)
		p.i += size
	default:
		return p.errorf("the escape \\%c is not TOML", c)
	}
	return nil
}

// hexRune reads the hexadecimal digits of a \u or \U escape as a code point.
func hexRune(digits string) (rune, bool) {
	var r rune
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, false
		}
		if r > utf8.MaxRune>>4 {
			return 0, false
		}
		r = r<<4 | d
	}
	return r, utf8.ValidRune(r)
}
