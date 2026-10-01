package skipguard

import "strings"

type tokenKind int

const (
	identToken tokenKind = iota
	punctToken
	stringToken // text holds the literal's content, without quotes
	templateToken
	numberToken
	regexToken
)

type token struct {
	kind   tokenKind
	text   string
	line   int
	column int
}

func (t token) display() string {
	switch t.kind {
	case stringToken:
		return "'" + t.text + "'"
	case templateToken:
		if t.text != "" {
			return "`" + t.text + "`"
		}
		return "`…`"
	case regexToken:
		return "/…/"
	}
	return t.text
}

type comment struct {
	text       string // without the // or /* */ markers
	endLine    int
	standalone bool // no token ends on the line the comment starts
}

// punctuators lists the multi-character punctuators the scanner must not split,
// longest first.
var punctuators = []string{
	">>>=", "...", "===", "!==", "**=", "<<=", ">>=", ">>>", "&&=", "||=", "??=",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--", "+=", "-=",
	"*=", "/=", "%=", "&=", "|=", "^=", "**", "<<", ">>",
}

// regexKeywords are the keywords after which a slash starts a regular
// expression rather than a division.
var regexKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true, "new": true,
	"delete": true, "void": true, "throw": true, "case": true, "do": true, "else": true,
	"yield": true, "await": true,
}

type lexer struct {
	src      []byte
	pos      int
	line     int
	column   int
	tokens   []token
	comments []comment
	// lastLine is the line the last token ends on; 0 before the first token.
	lastLine int
	// braces tracks each open brace: false for a block or object brace, true
	// for the `${` of a template literal, whose `}` resumes the template.
	braces []bool
}

// lex splits src into tokens and comments. It never fails: an unterminated
// literal ends at the end of its line or of the file.
func lex(src []byte) ([]token, []comment) {
	l := &lexer{src: src, line: 1, column: 1}
	for l.pos < len(l.src) {
		l.next()
	}
	return l.tokens, l.comments
}

func (l *lexer) peek(offset int) byte {
	if l.pos+offset < len(l.src) {
		return l.src[l.pos+offset]
	}
	return 0
}

func (l *lexer) advance(n int) {
	for i := 0; i < n && l.pos < len(l.src); i++ {
		if l.src[l.pos] == '\n' {
			l.line++
			l.column = 1
		} else {
			l.column++
		}
		l.pos++
	}
}

func (l *lexer) emit(kind tokenKind, text string, line, column int) {
	l.tokens = append(l.tokens, token{kind: kind, text: text, line: line, column: column})
	l.lastLine = l.line
}

func (l *lexer) next() {
	c := l.peek(0)
	line, column := l.line, l.column
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
		l.advance(1)
	case c == '/' && l.peek(1) == '/':
		start := l.pos + 2
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.advance(1)
		}
		l.comments = append(l.comments, comment{text: string(l.src[start:l.pos]), endLine: line, standalone: l.lastLine < line})
	case c == '/' && l.peek(1) == '*':
		start := l.pos + 2
		l.advance(2)
		for l.pos < len(l.src) && !(l.peek(0) == '*' && l.peek(1) == '/') {
			l.advance(1)
		}
		end := l.pos
		l.advance(2)
		l.comments = append(l.comments, comment{text: strings.Trim(string(l.src[start:end]), "* \t\r\n"), endLine: l.line, standalone: l.lastLine < line})
	case c == '\'' || c == '"':
		l.advance(1)
		start := l.pos
		for l.pos < len(l.src) && l.src[l.pos] != c && l.src[l.pos] != '\n' {
			if l.src[l.pos] == '\\' {
				l.advance(1)
			}
			l.advance(1)
		}
		text := string(l.src[start:min(l.pos, len(l.src))])
		if l.peek(0) == c {
			l.advance(1)
		}
		l.emit(stringToken, text, line, column)
	case c == '`':
		l.advance(1)
		l.template(line, column, true)
	case c == '}' && len(l.braces) > 0 && l.braces[len(l.braces)-1]:
		l.braces = l.braces[:len(l.braces)-1]
		l.advance(1)
		l.template(line, column, false)
	case c == '/' && l.regexAllowed():
		l.regex(line, column)
	case isIdentStart(c):
		start := l.pos
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.advance(1)
		}
		l.emit(identToken, string(l.src[start:l.pos]), line, column)
	case c == '#' && isIdentStart(l.peek(1)):
		start := l.pos
		l.advance(1)
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.advance(1)
		}
		l.emit(identToken, string(l.src[start:l.pos]), line, column)
	case c >= '0' && c <= '9' || c == '.' && l.peek(1) >= '0' && l.peek(1) <= '9':
		start := l.pos
		for l.pos < len(l.src) && (isIdentPart(l.src[l.pos]) || l.src[l.pos] == '.') {
			l.advance(1)
		}
		l.emit(numberToken, string(l.src[start:l.pos]), line, column)
	default:
		for _, p := range punctuators {
			if strings.HasPrefix(string(l.src[l.pos:min(l.pos+len(p), len(l.src))]), p) {
				// `a?.5:b` is a ternary with a number, not an optional chain.
				if p == "?." && l.peek(2) >= '0' && l.peek(2) <= '9' {
					continue
				}
				l.advance(len(p))
				l.emit(punctToken, p, line, column)
				return
			}
		}
		if c == '{' {
			l.braces = append(l.braces, false)
		} else if c == '}' && len(l.braces) > 0 {
			l.braces = l.braces[:len(l.braces)-1]
		}
		l.advance(1)
		l.emit(punctToken, string(c), line, column)
	}
}

// template scans template literal text up to its closing backtick or the
// next `${`, whose expression is then lexed as ordinary tokens. A whole
// template without substitutions keeps its text, like a string.
func (l *lexer) template(line, column int, whole bool) {
	start := l.pos
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case '\\':
			l.advance(2)
			continue
		case '`':
			text := ""
			if whole {
				text = string(l.src[start:l.pos])
			}
			l.advance(1)
			l.emit(templateToken, text, line, column)
			return
		case '$':
			if l.peek(1) == '{' {
				l.advance(2)
				l.braces = append(l.braces, true)
				l.emit(templateToken, "", line, column)
				return
			}
		}
		l.advance(1)
	}
	l.emit(templateToken, "", line, column)
}

// regex scans a regular expression literal and its flags. A line break ends
// an unterminated one.
func (l *lexer) regex(line, column int) {
	l.advance(1)
	inClass := false
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		c := l.src[l.pos]
		if c == '\\' {
			l.advance(2)
			continue
		}
		l.advance(1)
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			break
		}
	}
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.advance(1)
	}
	l.emit(regexToken, "", line, column)
}

// regexAllowed reports whether a slash here starts a regular expression: it
// does unless the previous token ends an expression. A slash right after `<`
// opens a JSX closing tag (`</li>`), not a regex.
func (l *lexer) regexAllowed() bool {
	if len(l.tokens) == 0 {
		return true
	}
	prev := l.tokens[len(l.tokens)-1]
	switch prev.kind {
	case identToken:
		return regexKeywords[prev.text]
	case punctToken:
		return prev.text != ")" && prev.text != "]" && prev.text != "}" && prev.text != "++" && prev.text != "--" && prev.text != "<"
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}
