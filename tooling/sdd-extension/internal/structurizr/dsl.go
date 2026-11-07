package structurizr

import (
	"fmt"
	"strings"
)

// The DSL reader covers the statements that describe a model. It follows the
// Structurizr DSL language reference: keywords are case-insensitive, a
// statement ends at a newline, a block opens with "{" at the end of its
// statement, and comments start with "//", "#" (as the first token of a line)
// or "/*".

type tokenKind int

const (
	tokenWord tokenKind = iota
	tokenString
	tokenOpen
	tokenClose
	tokenNewline
)

type token struct {
	kind  tokenKind
	value string
	line  int
}

func tokenize(source, file string) ([]token, error) {
	var tokens []token
	line := 1
	lineStart := true
	runes := []rune(source)
	emit := func(kind tokenKind, value string) {
		tokens = append(tokens, token{kind: kind, value: value, line: line})
		lineStart = kind == tokenNewline
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\n':
			emit(tokenNewline, "")
			line++
		case r == '\\' && (i+1 < len(runes) && (runes[i+1] == '\n' || (runes[i+1] == '\r' && i+2 < len(runes) && runes[i+2] == '\n'))):
			// Line continuation: the statement goes on on the next line.
			for runes[i] != '\n' {
				i++
			}
			line++
		case r == ' ' || r == '\t' || r == '\r':
		case r == '/' && i+1 < len(runes) && runes[i+1] == '/':
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
		case r == '#' && lineStart:
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
		case r == '/' && i+1 < len(runes) && runes[i+1] == '*':
			start := line
			i += 2
			for ; i < len(runes) && !(runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '/'); i++ {
				if runes[i] == '\n' {
					line++
				}
			}
			if i >= len(runes) {
				return nil, &Error{File: file, Line: start, Message: "unterminated /* comment"}
			}
			i++
		case r == '"':
			start := line
			var value strings.Builder
			i++
			for ; i < len(runes) && runes[i] != '"'; i++ {
				if runes[i] == '\\' && i+1 < len(runes) && runes[i+1] == '"' {
					i++
				}
				if runes[i] == '\n' {
					return nil, &Error{File: file, Line: start, Message: "unterminated string"}
				}
				value.WriteRune(runes[i])
			}
			if i >= len(runes) {
				return nil, &Error{File: file, Line: start, Message: "unterminated string"}
			}
			emit(tokenString, value.String())
		case r == '{':
			emit(tokenOpen, "{")
		case r == '}':
			emit(tokenClose, "}")
		default:
			var value strings.Builder
			for ; i < len(runes); i++ {
				c := runes[i]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '{' || c == '}' || c == '"' {
					break
				}
				value.WriteRune(c)
			}
			i--
			emit(tokenWord, value.String())
		}
	}
	emit(tokenNewline, "")
	return tokens, nil
}

type statement struct {
	tokens []token
	line   int
	block  bool
}

func (s statement) keyword() string {
	if len(s.tokens) == 0 {
		return ""
	}
	return strings.ToLower(s.tokens[0].value)
}

type rawRelationship struct {
	source, destination string
	scope               *Element
	description         string
	technology          string
	line                int
}

type dslParser struct {
	file      string
	tokens    []token
	pos       int
	workspace *Workspace
	refs      map[string][]*Element
	pending   []rawRelationship
}

// ParseDSL reads a Structurizr DSL workspace. file names the source in errors.
func ParseDSL(data []byte, file string) (*Workspace, error) {
	tokens, err := tokenize(string(data), file)
	if err != nil {
		return nil, err
	}
	p := &dslParser{file: file, tokens: tokens, workspace: &Workspace{}, refs: map[string][]*Element{}}
	if err := p.parseTop(); err != nil {
		return nil, err
	}
	if err := p.resolveRelationships(); err != nil {
		return nil, err
	}
	return p.workspace, nil
}

func (p *dslParser) errorf(line int, format string, args ...any) error {
	return &Error{File: p.file, Line: line, Message: fmt.Sprintf(format, args...)}
}

func (p *dslParser) unsupported(s statement) error {
	return p.errorf(s.line,
		"unsupported DSL statement %q; export the workspace as JSON with the Structurizr CLI and import that file", s.tokens[0].value)
}

// next reads the next statement, or reports a closing brace (closed) or the
// end of the input (eof).
func (p *dslParser) next() (s statement, closed, eof bool) {
	for p.pos < len(p.tokens) && p.tokens[p.pos].kind == tokenNewline {
		p.pos++
	}
	if p.pos >= len(p.tokens) {
		return statement{}, false, true
	}
	if p.tokens[p.pos].kind == tokenClose {
		p.pos++
		return statement{}, true, false
	}
	s.line = p.tokens[p.pos].line
	for p.pos < len(p.tokens) {
		t := p.tokens[p.pos]
		switch t.kind {
		case tokenNewline:
			p.pos++
			return s, false, false
		case tokenClose:
			return s, false, false
		case tokenOpen:
			p.pos++
			s.block = true
			return s, false, false
		}
		s.tokens = append(s.tokens, t)
		p.pos++
	}
	return s, false, false
}

// skipBlock consumes the body of a block whose "{" was just read.
func (p *dslParser) skipBlock(line int) error {
	depth := 1
	for ; p.pos < len(p.tokens); p.pos++ {
		switch p.tokens[p.pos].kind {
		case tokenOpen:
			depth++
		case tokenClose:
			depth--
			if depth == 0 {
				p.pos++
				return nil
			}
		}
	}
	return p.errorf(line, "block is never closed")
}

func (p *dslParser) parseTop() error {
	s, closed, eof := p.next()
	switch {
	case eof:
		return p.errorf(1, "no workspace block")
	case closed:
		return p.errorf(s.line, "unexpected }")
	case s.keyword() != "workspace" || !s.block:
		return p.errorf(s.line, "a DSL workspace starts with `workspace {`")
	case len(s.tokens) > 1 && strings.EqualFold(s.tokens[1].value, "extends"):
		return p.unsupported(statement{tokens: s.tokens[1:], line: s.line})
	}
	if len(s.tokens) > 1 {
		p.workspace.Name = s.tokens[1].value
	}
	if err := p.parseWorkspace(); err != nil {
		return err
	}
	if trailing, _, eof := p.next(); !eof {
		return p.errorf(trailing.line, "content after the workspace block")
	}
	return nil
}

func (p *dslParser) parseWorkspace() error {
	for {
		s, closed, eof := p.next()
		if eof {
			return p.errorf(len(p.tokens), "the workspace block is never closed")
		}
		if closed {
			return nil
		}
		switch s.keyword() {
		case "model":
			if !s.block {
				return p.errorf(s.line, "model needs a block")
			}
			if err := p.parseModel(nil); err != nil {
				return err
			}
		case "views", "configuration", "properties":
			if s.block {
				if err := p.skipBlock(s.line); err != nil {
					return err
				}
			}
		case "name", "description", "!identifiers", "!docs", "!adrs", "!impliedrelationships":
			if s.block {
				return p.unsupported(s)
			}
		default:
			return p.unsupported(s)
		}
	}
}

// parseModel reads the model block or the body of an element. scope is the
// element whose body is read, nil for the model.
func (p *dslParser) parseModel(scope *Element) error {
	for {
		s, closed, eof := p.next()
		if eof {
			return p.errorf(len(p.tokens), "a block is never closed")
		}
		if closed {
			return nil
		}
		if err := p.parseStatement(scope, s); err != nil {
			return err
		}
	}
}

func (p *dslParser) parseStatement(scope *Element, s statement) error {
	tokens := s.tokens
	if len(tokens) == 0 {
		return p.errorf(s.line, "a block must follow its statement on the same line")
	}
	identifier := ""
	if len(tokens) >= 3 && tokens[1].kind == tokenWord && tokens[1].value == "=" {
		identifier = tokens[0].value
		tokens = tokens[2:]
	}
	if index := arrowIndex(tokens); index >= 0 {
		return p.parseRelationship(scope, tokens, index, s)
	}
	keyword := strings.ToLower(tokens[0].value)
	body := statement{tokens: tokens, line: s.line, block: s.block}
	switch keyword {
	case "person", "softwaresystem", "container", "component", "element":
		return p.parseElement(scope, identifier, keyword, body)
	}
	if identifier != "" {
		return p.unsupported(body)
	}
	switch keyword {
	case "group", "enterprise":
		if !s.block {
			return p.errorf(s.line, "%s needs a block", tokens[0].value)
		}
		return p.parseModel(scope)
	case "deploymentenvironment", "perspectives", "properties", "!docs", "!adrs", "!identifiers", "!impliedrelationships", "url":
		if keyword == "properties" && scope != nil && s.block {
			return p.parseProperties(scope)
		}
		if s.block {
			return p.skipBlock(s.line)
		}
		return nil
	case "description", "technology", "tags":
		if scope == nil || s.block {
			return p.unsupported(body)
		}
		values := stringArgs(tokens[1:])
		switch keyword {
		case "description":
			scope.Description = strings.Join(values, " ")
		case "technology":
			scope.Technology = strings.Join(values, " ")
		case "tags":
			for _, value := range values {
				scope.Tags = append(scope.Tags, splitTags(value)...)
			}
		}
		return nil
	}
	return p.unsupported(body)
}

func (p *dslParser) parseProperties(scope *Element) error {
	if scope.Properties == nil {
		scope.Properties = map[string]string{}
	}
	for {
		s, closed, eof := p.next()
		if eof {
			return p.errorf(len(p.tokens), "the properties block is never closed")
		}
		if closed {
			return nil
		}
		if len(s.tokens) != 2 || s.block {
			return p.errorf(s.line, "a property is one name and one value")
		}
		scope.Properties[s.tokens[0].value] = s.tokens[1].value
	}
}

var allowedParents = map[string]Kind{
	"container": KindSoftwareSystem,
	"component": KindContainer,
}

func (p *dslParser) parseElement(scope *Element, identifier, keyword string, s statement) error {
	kind := map[string]Kind{
		"person": KindPerson, "softwaresystem": KindSoftwareSystem,
		"container": KindContainer, "component": KindComponent, "element": KindOther,
	}[keyword]
	if want, nested := allowedParents[keyword]; nested {
		if scope == nil || scope.Kind != want {
			return p.errorf(s.line, "a %s must be declared inside a %s", kind, want)
		}
	} else if scope != nil && keyword != "element" {
		return p.errorf(s.line, "a %s cannot be declared inside a %s", kind, scope.Kind)
	}
	args := stringArgs(s.tokens[1:])
	if len(args) == 0 {
		return p.errorf(s.line, "%s needs a name", s.tokens[0].value)
	}
	element := &Element{Kind: kind, Name: args[0], Parent: scope, Line: s.line}
	at := func(index int) string {
		if index < len(args) {
			return args[index]
		}
		return ""
	}
	switch kind {
	case KindContainer, KindComponent:
		element.Description, element.Technology = at(1), at(2)
		element.Tags = splitTags(at(3))
	case KindOther:
		element.Description = at(2)
		element.Tags = splitTags(at(3))
	default:
		element.Description = at(1)
		element.Tags = splitTags(at(2))
	}
	if identifier != "" {
		element.Ref = identifier
		if scope != nil && scope.Ref != "" {
			element.Ref = scope.Ref + "." + identifier
		}
		p.refs[identifier] = append(p.refs[identifier], element)
		if element.Ref != identifier {
			p.refs[element.Ref] = append(p.refs[element.Ref], element)
		}
	}
	if scope != nil {
		scope.Children = append(scope.Children, element)
	}
	p.workspace.Elements = append(p.workspace.Elements, element)
	if s.block {
		return p.parseModel(element)
	}
	return nil
}

func (p *dslParser) parseRelationship(scope *Element, tokens []token, index int, s statement) error {
	source := "this"
	switch index {
	case 0:
		if scope == nil {
			return p.errorf(s.line, "a relationship with no source must be declared inside an element")
		}
	case 1:
		source = tokens[0].value
	default:
		return p.unsupported(s)
	}
	if len(tokens) <= index+1 {
		return p.errorf(s.line, "a relationship needs a destination")
	}
	args := stringArgs(tokens[index+2:])
	raw := rawRelationship{source: source, destination: tokens[index+1].value, scope: scope, line: s.line}
	if len(args) > 0 {
		raw.description = args[0]
	}
	if len(args) > 1 {
		raw.technology = args[1]
	}
	p.pending = append(p.pending, raw)
	if s.block {
		return p.skipBlock(s.line)
	}
	return nil
}

func (p *dslParser) resolve(name string, scope *Element, line int) (*Element, error) {
	if name == "this" {
		if scope == nil {
			return nil, p.errorf(line, "`this` is only valid inside an element")
		}
		return scope, nil
	}
	if name == "*" {
		return nil, p.errorf(line, "a relationship to `*` is not supported")
	}
	candidates := p.refs[name]
	switch len(candidates) {
	case 0:
		return nil, p.errorf(line, "identifier %q is not declared", name)
	case 1:
		return candidates[0], nil
	}
	return nil, p.errorf(line, "identifier %q names %d elements; use hierarchical identifiers", name, len(candidates))
}

func (p *dslParser) resolveRelationships() error {
	for _, raw := range p.pending {
		source, err := p.resolve(raw.source, raw.scope, raw.line)
		if err != nil {
			return err
		}
		destination, err := p.resolve(raw.destination, raw.scope, raw.line)
		if err != nil {
			return err
		}
		p.workspace.Relationships = append(p.workspace.Relationships, Relationship{
			Source: source, Destination: destination,
			Description: raw.description, Technology: raw.technology, Line: raw.line,
		})
	}
	return nil
}

func arrowIndex(tokens []token) int {
	for index, t := range tokens {
		if t.kind == tokenWord && t.value == "->" {
			return index
		}
	}
	return -1
}

func stringArgs(tokens []token) []string {
	values := make([]string, 0, len(tokens))
	for _, t := range tokens {
		values = append(values, t.value)
	}
	return values
}
