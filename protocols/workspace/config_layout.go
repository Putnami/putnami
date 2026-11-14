package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// ConfigLineWidth is the column a short array or object must fit in to stay
// on one line: the default line width of Biome and Prettier.
const ConfigLineWidth = 80

// RenderConfigFile renders doc, a value that encodes as a JSON object, as the
// content of a configuration file that replaces existing. The file keeps the
// layout of existing: a value whose content is unchanged keeps its bytes,
// members keep their order, and the file keeps its indentation and line
// endings. A changed array of scalars that fits in ConfigLineWidth columns is
// written on one line, unless existing writes such an array one item per
// line. Content equal to existing answers existing itself. With no existing
// file, or one that is not a JSON object, it renders a new file.
func RenderConfigFile(doc any, existing []byte) ([]byte, error) {
	return RenderConfigFileBeside(doc, existing, nil)
}

// RenderConfigFileAt is RenderConfigFile for the file at path, which holds
// existing: a new file is indented as the package.json beside it.
func RenderConfigFileAt(path string, doc any, existing []byte) ([]byte, error) {
	var beside []byte
	if len(existing) == 0 {
		beside, _ = os.ReadFile(filepath.Join(filepath.Dir(path), "package.json"))
	}
	return RenderConfigFileBeside(doc, existing, beside)
}

// RenderConfigFileBeside is RenderConfigFile where a new file is indented as
// beside, the content of a JSON file next to it such as its package.json, and
// with two spaces when beside states no indentation.
func RenderConfigFileBeside(doc any, existing, beside []byte) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	want, err := parseLayout(buf.Bytes())
	if err != nil {
		return nil, err
	}
	if want.kind != '{' {
		return nil, errors.New("a configuration file holds a JSON object")
	}
	l := layout{indent: "  ", newline: "\n", width: ConfigLineWidth}
	var have *layoutNode
	if json.Valid(existing) {
		if node, err := parseLayout(existing); err == nil && node.kind == '{' {
			have = node
		}
	}
	if have != nil {
		if sameContent(want, have) {
			return existing, nil
		}
		if unit := indentUnit(existing); unit != "" {
			l.indent = unit
		}
		if bytes.Contains(existing, []byte("\r\n")) {
			l.newline = "\r\n"
		}
		l.expandsShortArrays = expandsShortArrays(have, l)
	} else if unit := indentUnit(beside); unit != "" {
		l.indent = unit
	}
	var out bytes.Buffer
	l.render(&out, want, have, 0, 0, false)
	out.WriteString(l.newline)
	return out.Bytes(), nil
}

// layoutNode is one JSON value with the bytes it was read from.
type layoutNode struct {
	kind     byte // '{', '[', '"' or 'v' for a number or a literal
	raw      []byte
	expanded bool // a line break follows the opening bracket
	col      int  // column of the value's first byte
	comma    bool // a comma follows the value
	keys     [][]byte
	names    []string
	vals     []*layoutNode
}

func parseLayout(data []byte) (*layoutNode, error) {
	p := &layoutParser{data: data}
	p.space()
	node, err := p.value()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.pos != len(p.data) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return node, nil
}

type layoutParser struct {
	data []byte
	pos  int
}

func (p *layoutParser) space() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *layoutParser) column(at int) int {
	start := bytes.LastIndexByte(p.data[:at], '\n') + 1
	return width(string(p.data[start:at]))
}

func (p *layoutParser) value() (*layoutNode, error) {
	if p.pos >= len(p.data) {
		return nil, errors.New("unexpected end of JSON")
	}
	start := p.pos
	node := &layoutNode{col: p.column(start)}
	switch c := p.data[p.pos]; c {
	case '{', '[':
		node.kind = c
		closing := byte('}')
		if c == '[' {
			closing = ']'
		}
		p.pos++
		node.expanded = bytes.ContainsAny(p.data[p.pos:p.skipSpace()], "\n")
		p.space()
		if p.pos < len(p.data) && p.data[p.pos] == closing {
			p.pos++
			break
		}
		for {
			if c == '{' {
				keyStart := p.pos
				if err := p.str(); err != nil {
					return nil, err
				}
				key := p.data[keyStart:p.pos]
				var name string
				if err := json.Unmarshal(key, &name); err != nil {
					return nil, err
				}
				node.keys = append(node.keys, key)
				node.names = append(node.names, name)
				p.space()
				if p.pos >= len(p.data) || p.data[p.pos] != ':' {
					return nil, errors.New("expected a colon after an object key")
				}
				p.pos++
				p.space()
			}
			child, err := p.value()
			if err != nil {
				return nil, err
			}
			node.vals = append(node.vals, child)
			p.space()
			if p.pos >= len(p.data) {
				return nil, errors.New("unexpected end of JSON")
			}
			if p.data[p.pos] == ',' {
				child.comma = true
				p.pos++
				p.space()
				continue
			}
			if p.data[p.pos] != closing {
				return nil, errors.New("expected a comma or a closing bracket")
			}
			p.pos++
			break
		}
	case '"':
		node.kind = '"'
		if err := p.str(); err != nil {
			return nil, err
		}
	default:
		node.kind = 'v'
		for p.pos < len(p.data) && strings.IndexByte("+-.0123456789eEtrufalsn", p.data[p.pos]) >= 0 {
			p.pos++
		}
		if p.pos == start {
			return nil, errors.New("invalid JSON value")
		}
	}
	node.raw = p.data[start:p.pos]
	return node, nil
}

func (p *layoutParser) skipSpace() int {
	end := p.pos
	for end < len(p.data) && strings.IndexByte(" \t\n\r", p.data[end]) >= 0 {
		end++
	}
	return end
}

func (p *layoutParser) str() error {
	if p.pos >= len(p.data) || p.data[p.pos] != '"' {
		return errors.New("expected a string")
	}
	for p.pos++; p.pos < len(p.data); p.pos++ {
		switch p.data[p.pos] {
		case '\\':
			p.pos++
		case '"':
			p.pos++
			return nil
		}
	}
	return errors.New("unterminated string")
}

// sameContent reports whether two values hold the same content, whatever
// their layout, member order or string escapes.
func sameContent(a, b *layoutNode) bool {
	if a.kind != b.kind || len(a.vals) != len(b.vals) {
		return false
	}
	switch a.kind {
	case '"':
		var x, y string
		return json.Unmarshal(a.raw, &x) == nil && json.Unmarshal(b.raw, &y) == nil && x == y
	case 'v':
		return bytes.Equal(a.raw, b.raw)
	case '[':
		for i := range a.vals {
			if !sameContent(a.vals[i], b.vals[i]) {
				return false
			}
		}
		return true
	}
	for i, name := range a.names {
		j := slices.Index(b.names, name)
		if j < 0 || !sameContent(a.vals[i], b.vals[j]) {
			return false
		}
	}
	return true
}

// layout is how a file writes its values.
type layout struct {
	indent  string
	newline string
	width   int
	// expandsShortArrays: the file writes an array that fits on one line one
	// item per line, and no array on one line.
	expandsShortArrays bool
}

// indentUnit is the leading whitespace of the first indented line of a JSON
// document, or "" when it has none.
func indentUnit(data []byte) string {
	for line := range strings.Lines(string(data)) {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == line || strings.TrimSpace(trimmed) == "" {
			continue
		}
		unit := line[:len(line)-len(trimmed)]
		if strings.Trim(unit, " ") == "" || strings.Trim(unit, "\t") == "" {
			return unit
		}
		return ""
	}
	return ""
}

// expandsShortArrays reports whether the file writes an array of scalars that
// would fit on one line one item per line, and writes no array of several
// items on one line.
func expandsShortArrays(root *layoutNode, l layout) bool {
	expanded, inline := false, false
	var walk func(n *layoutNode)
	walk = func(n *layoutNode) {
		if n.kind == '[' && len(n.vals) > 0 && scalars(n) {
			switch {
			case !n.expanded:
				inline = true
			case n.col+width(flatArray(n))+boolInt(n.comma) <= l.width:
				expanded = true
			}
		}
		for _, v := range n.vals {
			walk(v)
		}
	}
	walk(root)
	return expanded && !inline
}

// keepsExpanded reports whether an existing array stays one item per line.
func (l layout) keepsExpanded(have *layoutNode) bool {
	if have == nil || len(have.vals) == 0 {
		return l.expandsShortArrays
	}
	if have.kind != '[' || !have.expanded || len(have.vals) == 0 || !scalars(have) {
		return false
	}
	return have.col+width(flatArray(have))+boolInt(have.comma) <= l.width
}

func scalars(n *layoutNode) bool {
	for _, v := range n.vals {
		if (v.kind == '{' || v.kind == '[') && len(v.vals) > 0 {
			return false
		}
	}
	return true
}

func flatArray(n *layoutNode) string {
	parts := make([]string, len(n.vals))
	for i, v := range n.vals {
		parts[i] = string(v.raw)
		if v.kind == '{' || v.kind == '[' {
			parts[i] = string(v.kind) + string(closer(v.kind))
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func closer(kind byte) byte {
	if kind == '{' {
		return '}'
	}
	return ']'
}

func width(s string) int {
	return utf8.RuneCountInString(s) + strings.Count(s, "\t")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// render writes want at depth, its first byte at column col, guided by have,
// the value it replaces, or nil. comma states that a comma follows the value.
func (l layout) render(out *bytes.Buffer, want, have *layoutNode, depth, col int, comma bool) {
	if have != nil && sameContent(want, have) {
		out.Write(have.raw)
		return
	}
	if have != nil && have.kind != want.kind {
		have = nil
	}
	switch want.kind {
	case '{':
		l.renderObject(out, want, have, depth, col, comma)
	case '[':
		l.renderArray(out, want, have, depth, col, comma)
	default:
		out.Write(want.raw)
	}
}

func (l layout) renderObject(out *bytes.Buffer, want, have *layoutNode, depth, col int, comma bool) {
	if len(want.vals) == 0 {
		out.WriteString("{}")
		return
	}
	order := memberOrder(want, have)
	keys := make([][]byte, len(order))
	haves := make([]*layoutNode, len(order))
	for i, idx := range order {
		keys[i] = want.keys[idx]
		if have != nil {
			if j := slices.Index(have.names, want.names[idx]); j >= 0 {
				keys[i], haves[i] = have.keys[j], have.vals[j]
			}
		}
	}
	if have != nil && !have.expanded {
		var flat bytes.Buffer
		flat.WriteString("{ ")
		for i, idx := range order {
			if i > 0 {
				flat.WriteString(", ")
			}
			flat.Write(keys[i])
			flat.WriteString(": ")
			l.render(&flat, want.vals[idx], haves[i], depth, col+width(flat.String()), i < len(order)-1)
		}
		flat.WriteString(" }")
		if !bytes.Contains(flat.Bytes(), []byte("\n")) && col+width(flat.String())+boolInt(comma) <= l.width {
			out.Write(flat.Bytes())
			return
		}
	}
	inner := strings.Repeat(l.indent, depth+1)
	out.WriteString("{" + l.newline)
	for i, idx := range order {
		out.WriteString(inner)
		out.Write(keys[i])
		out.WriteString(": ")
		last := i == len(order)-1
		l.render(out, want.vals[idx], haves[i], depth+1, width(inner)+width(string(keys[i]))+2, !last)
		if !last {
			out.WriteByte(',')
		}
		out.WriteString(l.newline)
	}
	out.WriteString(strings.Repeat(l.indent, depth) + "}")
}

// memberOrder answers the indexes of want's members in the order the file
// writes them: the members have holds in its order, and each new member after
// the member want places before it.
func memberOrder(want, have *layoutNode) []int {
	if have == nil {
		order := make([]int, len(want.names))
		for i := range order {
			order[i] = i
		}
		return order
	}
	var order []int
	for _, name := range have.names {
		if i := slices.Index(want.names, name); i >= 0 && !slices.Contains(order, i) {
			order = append(order, i)
		}
	}
	for i, name := range want.names {
		if slices.Contains(have.names, name) {
			continue
		}
		at := 0
		for prev := i - 1; prev >= 0; prev-- {
			if pos := slices.Index(order, prev); pos >= 0 {
				at = pos + 1
				break
			}
		}
		order = slices.Insert(order, at, i)
	}
	return order
}

func (l layout) renderArray(out *bytes.Buffer, want, have *layoutNode, depth, col int, comma bool) {
	if len(want.vals) == 0 {
		out.WriteString("[]")
		return
	}
	haves := matchItems(want, have)
	if scalars(want) && !l.keepsExpanded(have) {
		flat := flatArray(want)
		if col+width(flat)+boolInt(comma) <= l.width {
			out.WriteString(flat)
			return
		}
	}
	inner := strings.Repeat(l.indent, depth+1)
	out.WriteString("[" + l.newline)
	for i, item := range want.vals {
		out.WriteString(inner)
		last := i == len(want.vals)-1
		l.render(out, item, haves[i], depth+1, width(inner), !last)
		if !last {
			out.WriteByte(',')
		}
		out.WriteString(l.newline)
	}
	out.WriteString(strings.Repeat(l.indent, depth) + "]")
}

// matchItems pairs each item of want with the item of have it replaces: an
// item with the same content, else the item at the same index.
func matchItems(want, have *layoutNode) []*layoutNode {
	haves := make([]*layoutNode, len(want.vals))
	if have == nil {
		return haves
	}
	used := make([]bool, len(have.vals))
	for i, item := range want.vals {
		for j, old := range have.vals {
			if !used[j] && sameContent(item, old) {
				haves[i], used[j] = old, true
				break
			}
		}
	}
	for i := range want.vals {
		if haves[i] == nil && i < len(have.vals) && !used[i] {
			haves[i], used[i] = have.vals[i], true
		}
	}
	return haves
}
