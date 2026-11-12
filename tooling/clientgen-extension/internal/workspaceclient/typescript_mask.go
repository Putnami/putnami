package workspaceclient

import "strings"

// tsCodeMask returns content with every comment, string, template, regular
// expression and JSX text replaced by spaces, so a pattern matched on the
// result matches code only.
func tsCodeMask(content string) string {
	masker := typeScriptMasker{source: content, output: []byte(content)}
	masker.scanCode(0, false)
	return string(masker.output)
}

type typeScriptMasker struct {
	source string
	output []byte
}

func (m *typeScriptMasker) scanCode(start int, stopAtBrace bool) int {
	braceDepth := 0
	for index := start; index < len(m.source); {
		switch {
		case index+1 < len(m.source) && m.source[index:index+2] == "//":
			index = m.maskLineComment(index)
		case index+1 < len(m.source) && m.source[index:index+2] == "/*":
			index = m.maskBlockComment(index)
		case m.source[index] == '\'' || m.source[index] == '"':
			index = m.maskQuoted(index, m.source[index])
		case m.source[index] == '`':
			index = m.maskTemplate(index)
		case m.source[index] == '/' && m.regexCanStart(index):
			index = m.maskRegularExpression(index)
		case m.source[index] == '<':
			if next, ok := m.maskJSXElement(index); ok {
				index = next
			} else {
				index++
			}
		case m.source[index] == '{':
			braceDepth++
			index++
		case m.source[index] == '}':
			if stopAtBrace && braceDepth == 0 {
				return index
			}
			if braceDepth > 0 {
				braceDepth--
			}
			index++
		default:
			index++
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) maskLineComment(start int) int {
	index := start
	for index < len(m.source) && m.source[index] != '\n' {
		m.output[index] = ' '
		index++
	}
	return index
}

func (m *typeScriptMasker) maskBlockComment(start int) int {
	index := start
	for index < len(m.source) {
		m.maskByte(index)
		if index+1 < len(m.source) && m.source[index:index+2] == "*/" {
			m.maskByte(index + 1)
			return index + 2
		}
		index++
	}
	return index
}

func (m *typeScriptMasker) maskQuoted(start int, quote byte) int {
	escaped := false
	for index := start; index < len(m.source); index++ {
		character := m.source[index]
		m.maskByte(index)
		if index == start {
			continue
		}
		if escaped {
			escaped = false
		} else if character == '\\' {
			escaped = true
		} else if character == quote {
			return index + 1
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) maskTemplate(start int) int {
	m.maskByte(start)
	for index := start + 1; index < len(m.source); {
		switch {
		case m.source[index] == '\\':
			m.maskByte(index)
			if index+1 < len(m.source) {
				m.maskByte(index + 1)
			}
			index += 2
		case m.source[index] == '`':
			m.maskByte(index)
			return index + 1
		case index+1 < len(m.source) && m.source[index:index+2] == "${":
			m.maskByte(index)
			m.maskByte(index + 1)
			closing := m.scanCode(index+2, true)
			if closing >= len(m.source) {
				return closing
			}
			m.maskByte(closing)
			index = closing + 1
		default:
			m.maskByte(index)
			index++
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) regexCanStart(index int) bool {
	previous := index - 1
	for previous >= 0 && (m.output[previous] == ' ' || m.output[previous] == '\t' || m.output[previous] == '\r' || m.output[previous] == '\n') {
		previous--
	}
	if previous < 0 || strings.ContainsRune("([{:;,=!?&|+-*%^~<>", rune(m.output[previous])) {
		return true
	}
	if m.output[previous] == '>' && previous > 0 && m.output[previous-1] == '=' {
		return true
	}
	end := previous + 1
	for previous >= 0 && (m.output[previous] == '_' || m.output[previous] == '$' ||
		m.output[previous] >= 'a' && m.output[previous] <= 'z' || m.output[previous] >= 'A' && m.output[previous] <= 'Z') {
		previous--
	}
	switch string(m.output[previous+1 : end]) {
	case "await", "case", "delete", "in", "instanceof", "of", "return", "throw", "typeof", "void", "yield":
		return true
	default:
		return false
	}
}

func (m *typeScriptMasker) maskRegularExpression(start int) int {
	inClass := false
	escaped := false
	for index := start; index < len(m.source); index++ {
		character := m.source[index]
		m.maskByte(index)
		if index == start {
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '[' {
			inClass = true
		} else if character == ']' {
			inClass = false
		} else if character == '/' && !inClass {
			index++
			for index < len(m.source) && ((m.source[index] >= 'a' && m.source[index] <= 'z') || (m.source[index] >= 'A' && m.source[index] <= 'Z')) {
				m.maskByte(index)
				index++
			}
			return index
		} else if character == '\n' || character == '\r' {
			return index
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) maskJSXElement(start int) (int, bool) {
	nameStart := start + 1
	if nameStart >= len(m.source) || !isJSXNameStart(m.source[nameStart]) || !m.jsxCanStart(start) {
		return start, false
	}
	nameEnd := nameStart + 1
	for nameEnd < len(m.source) && isJSXNamePart(m.source[nameEnd]) {
		nameEnd++
	}
	name := m.source[nameStart:nameEnd]
	index, selfClosing, ok := m.maskJSXTag(start, nameEnd)
	if !ok || selfClosing {
		return index, ok
	}
	for index < len(m.source) {
		switch {
		case strings.HasPrefix(m.source[index:], "</"+name):
			return m.maskJSXClosingTag(index), true
		case m.source[index] == '<':
			if next, nested := m.maskJSXElement(index); nested {
				index = next
			} else {
				m.maskByte(index)
				index++
			}
		case m.source[index] == '{':
			m.maskByte(index)
			closing := m.scanCode(index+1, true)
			if closing >= len(m.source) {
				return closing, true
			}
			m.maskByte(closing)
			index = closing + 1
		default:
			m.maskByte(index)
			index++
		}
	}
	return index, true
}

func (m *typeScriptMasker) jsxCanStart(start int) bool {
	previous := start - 1
	for previous >= 0 && (m.output[previous] == ' ' || m.output[previous] == '\t' || m.output[previous] == '\r' || m.output[previous] == '\n') {
		previous--
	}
	return previous < 0 || strings.ContainsRune("=([{,:;>!&|?", rune(m.output[previous]))
}

func (m *typeScriptMasker) maskJSXTag(start, index int) (int, bool, bool) {
	for position := start; position < index; position++ {
		m.maskByte(position)
	}
	for index < len(m.source) {
		switch {
		case m.source[index] == '\'' || m.source[index] == '"':
			index = m.maskQuoted(index, m.source[index])
		case m.source[index] == '{':
			m.maskByte(index)
			closing := m.scanCode(index+1, true)
			if closing >= len(m.source) {
				return closing, false, false
			}
			m.maskByte(closing)
			index = closing + 1
		case m.source[index] == '>':
			selfClosing := index > start && m.source[index-1] == '/'
			m.maskByte(index)
			return index + 1, selfClosing, true
		default:
			m.maskByte(index)
			index++
		}
	}
	return index, false, false
}

func (m *typeScriptMasker) maskJSXClosingTag(start int) int {
	index := start
	for index < len(m.source) {
		m.maskByte(index)
		if m.source[index] == '>' {
			return index + 1
		}
		index++
	}
	return index
}

func (m *typeScriptMasker) maskByte(index int) {
	if m.output[index] != '\n' && m.output[index] != '\r' {
		m.output[index] = ' '
	}
}

func isJSXNameStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func isJSXNamePart(character byte) bool {
	return isJSXNameStart(character) || character >= '0' && character <= '9' || character == '-' || character == '.' || character == ':'
}
