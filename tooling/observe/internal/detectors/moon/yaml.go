package moon

import (
	"fmt"
	"strconv"
	"strings"
)

// topLevel reads the value of one top-level key of a YAML document. It is not
// a YAML parser: it reads the lines of that key only, so the rest of the
// document may use any YAML. The value is read as block or flow mappings
// (map[string]any), sequences ([]any) and scalars (string, the text the
// document writes after unquoting); an empty value is nil. Anchors, aliases
// and tags in that value are an error. Only the first document of a stream is
// read. found is false when the document has no such key.
func topLevel(text, key string) (value any, found bool, err error) {
	lines := yamlLines(text)
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if l.text == "..." || l.text == "---" && i > 0 {
			break
		}
		if l.indent != 0 || l.text == "---" {
			continue
		}
		k, rest, ok := splitKey(l.text)
		if !ok || k != key {
			continue
		}
		end := i + 1
		if rest != "" && (rest[0] == '[' || rest[0] == '{') {
			for joined := rest; !balanced(joined) && end < len(lines); end++ {
				joined += " " + lines[end].text
			}
		}
		for end < len(lines) && (lines[end].indent > 0 || strings.HasPrefix(lines[end].text, "-") && lines[end].text != "---") {
			end++
		}
		p := &yamlParser{lines: lines[i+1 : end]}
		value, err := p.entryValue(rest, 0, l.num)
		if err != nil {
			return nil, true, err
		}
		if p.next < len(p.lines) {
			return nil, true, fmt.Errorf("line %d: unexpected %q", p.lines[p.next].num, p.lines[p.next].text)
		}
		return value, true, nil
	}
	return nil, false, nil
}

// yamlLine is one line that holds content: its indentation, its text without
// the indentation and the comment, and its 1-based number.
type yamlLine struct {
	indent int
	text   string
	num    int
	raw    string
}

func yamlLines(text string) []yamlLine {
	var out []yamlLine
	for n, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimLeft(raw, " ")
		content := stripComment(trimmed)
		if strings.TrimSpace(content) == "" {
			continue
		}
		out = append(out, yamlLine{indent: len(raw) - len(trimmed), text: strings.TrimSpace(content), num: n + 1, raw: raw})
	}
	return out
}

type yamlParser struct {
	lines []yamlLine
	next  int
}

// block reads the block node that starts at the next line, whose indentation
// is indent.
func (p *yamlParser) block(indent int) (any, error) {
	l := p.lines[p.next]
	if isSequenceItem(l.text) {
		return p.sequence(indent)
	}
	if _, _, ok := splitKey(l.text); ok {
		return p.mapping(indent)
	}
	p.next++
	return p.inline(l.text, l.num)
}

func (p *yamlParser) mapping(indent int) (any, error) {
	out := map[string]any{}
	for p.next < len(p.lines) && p.lines[p.next].indent == indent {
		l := p.lines[p.next]
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: %q is not a key", l.num, l.text)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: key %q appears twice", l.num, key)
		}
		p.next++
		value, err := p.entryValue(rest, indent, l.num)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// entryValue reads the value of a mapping entry whose key sits at indent and
// whose text after the colon is rest.
func (p *yamlParser) entryValue(rest string, indent, num int) (any, error) {
	if rest == "" {
		if p.next >= len(p.lines) {
			return nil, nil
		}
		next := p.lines[p.next]
		if next.indent > indent || next.indent == indent && isSequenceItem(next.text) {
			return p.block(next.indent)
		}
		return nil, nil
	}
	if rest[0] == '|' || rest[0] == '>' {
		var text []string
		for p.next < len(p.lines) && p.lines[p.next].indent > indent {
			text = append(text, strings.TrimSpace(p.lines[p.next].raw))
			p.next++
		}
		return strings.Join(text, "\n"), nil
	}
	return p.inline(rest, num)
}

func (p *yamlParser) sequence(indent int) (any, error) {
	out := []any{}
	for p.next < len(p.lines) && p.lines[p.next].indent == indent && isSequenceItem(p.lines[p.next].text) {
		l := p.lines[p.next]
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			p.next++
			if p.next < len(p.lines) && p.lines[p.next].indent > indent {
				value, err := p.block(p.lines[p.next].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			} else {
				out = append(out, nil)
			}
			continue
		}
		// The item's content starts a node at its own column, so a compact
		// mapping continues on the lines indented to that column.
		column := indent + len(l.text) - len(rest)
		p.lines[p.next] = yamlLine{indent: column, text: rest, num: l.num, raw: l.raw}
		value, err := p.block(column)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// inline reads a scalar or a flow collection; a flow collection may continue
// on the next lines until its brackets close.
func (p *yamlParser) inline(text string, num int) (any, error) {
	if text[0] == '[' || text[0] == '{' {
		for !balanced(text) && p.next < len(p.lines) {
			text += " " + p.lines[p.next].text
			p.next++
		}
		f := flowReader{text: text, num: num}
		value, err := f.value()
		if err != nil {
			return nil, err
		}
		f.space()
		if f.pos != len(f.text) {
			return nil, fmt.Errorf("line %d: unexpected %q after a flow collection", num, f.text[f.pos:])
		}
		return value, nil
	}
	return scalar(text, num)
}

// scalar unquotes a plain, single-quoted or double-quoted scalar.
func scalar(text string, num int) (any, error) {
	switch text[0] {
	case '&', '*', '!', '%', '@', '`':
		return nil, fmt.Errorf("line %d: %q is not supported", num, text)
	case '\'':
		if len(text) < 2 || text[len(text)-1] != '\'' {
			return nil, fmt.Errorf("line %d: unterminated quoted scalar %s", num, text)
		}
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil
	case '"':
		value, err := strconv.Unquote(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: quoted scalar %s does not read: %w", num, text, err)
		}
		return value, nil
	}
	if text == "~" || text == "null" {
		return nil, nil
	}
	return text, nil
}

// flowReader reads a flow collection: [a, b] and {k: v}, nested, with quoted
// or plain scalars.
type flowReader struct {
	text string
	pos  int
	num  int
}

func (f *flowReader) space() {
	for f.pos < len(f.text) && f.text[f.pos] == ' ' {
		f.pos++
	}
}

func (f *flowReader) fail(format string, args ...any) error {
	return fmt.Errorf("line %d: %s in %q", f.num, fmt.Sprintf(format, args...), f.text)
}

func (f *flowReader) value() (any, error) {
	f.space()
	if f.pos >= len(f.text) {
		return nil, f.fail("missing value")
	}
	switch f.text[f.pos] {
	case '[':
		f.pos++
		out := []any{}
		for {
			f.space()
			if f.pos < len(f.text) && f.text[f.pos] == ']' {
				f.pos++
				return out, nil
			}
			item, err := f.value()
			if err != nil {
				return nil, err
			}
			out = append(out, item)
			if err := f.separator(']'); err != nil {
				return nil, err
			}
		}
	case '{':
		f.pos++
		out := map[string]any{}
		for {
			f.space()
			if f.pos < len(f.text) && f.text[f.pos] == '}' {
				f.pos++
				return out, nil
			}
			key, err := f.scalar(true)
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, f.fail("a key is not a string")
			}
			f.space()
			if f.pos >= len(f.text) || f.text[f.pos] != ':' {
				return nil, f.fail("missing : after key %q", k)
			}
			f.pos++
			item, err := f.value()
			if err != nil {
				return nil, err
			}
			out[k] = item
			if err := f.separator('}'); err != nil {
				return nil, err
			}
		}
	}
	return f.scalar(false)
}

// separator reads the comma between two flow items, or leaves the closing
// bracket for the caller.
func (f *flowReader) separator(closing byte) error {
	f.space()
	if f.pos >= len(f.text) {
		return f.fail("unclosed flow collection")
	}
	switch f.text[f.pos] {
	case ',':
		f.pos++
		return nil
	case closing:
		return nil
	}
	return f.fail("unexpected %q", f.text[f.pos:])
}

func (f *flowReader) scalar(key bool) (any, error) {
	start := f.pos
	if f.pos < len(f.text) && (f.text[f.pos] == '\'' || f.text[f.pos] == '"') {
		quote := f.text[f.pos]
		f.pos++
		for f.pos < len(f.text) {
			c := f.text[f.pos]
			switch {
			case quote == '"' && c == '\\':
				f.pos += 2
				continue
			case c == quote && quote == '\'' && f.pos+1 < len(f.text) && f.text[f.pos+1] == '\'':
				f.pos += 2
				continue
			case c == quote:
				f.pos++
				return scalar(f.text[start:f.pos], f.num)
			}
			f.pos++
		}
		return nil, f.fail("unterminated quoted scalar")
	}
	for f.pos < len(f.text) {
		c := f.text[f.pos]
		if c == ',' || c == ']' || c == '}' || c == '[' || c == '{' || key && c == ':' {
			break
		}
		if c == ':' && (f.pos+1 == len(f.text) || f.text[f.pos+1] == ' ') {
			break
		}
		f.pos++
	}
	text := strings.TrimSpace(f.text[start:f.pos])
	if text == "" {
		return nil, f.fail("missing value")
	}
	return scalar(text, f.num)
}

// splitKey splits a mapping entry "key: rest". ok is false when the text is
// no mapping entry.
func splitKey(text string) (key, rest string, ok bool) {
	if text == "" || isSequenceItem(text) || text[0] == '[' || text[0] == '{' {
		return "", "", false
	}
	if text[0] == '\'' || text[0] == '"' {
		end := strings.IndexByte(text[1:], text[0])
		if end < 0 {
			return "", "", false
		}
		key, after := text[1:end+1], strings.TrimLeft(text[end+2:], " ")
		rest, found := strings.CutPrefix(after, ":")
		if !found || rest != "" && rest[0] != ' ' {
			return "", "", false
		}
		return key, strings.TrimSpace(rest), true
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ':' && (i+1 == len(text) || text[i+1] == ' ') {
			return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+1:]), true
		}
	}
	return "", "", false
}

// opensQuote reports whether the quote at i starts a quoted scalar: it
// follows the start of the text, a space or a flow or mapping indicator, not
// a letter of a plain scalar such as don't.
func opensQuote(text string, i int) bool {
	return i == 0 || strings.IndexByte(" \t[{,:-", text[i-1]) >= 0
}

func isSequenceItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

// balanced reports whether the flow brackets of text close, outside quotes.
func balanced(text string) bool {
	depth := 0
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case (c == '\'' || c == '"') && opensQuote(text, i):
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		}
	}
	return depth <= 0
}

// stripComment removes a comment that starts outside quotes.
func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case (c == '"' || c == '\'') && opensQuote(line, i):
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return strings.TrimRight(line, " \t")
}
