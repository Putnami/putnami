package git

import (
	"bytes"
	"strings"
)

// The flags of one ignore pattern (dir.h).
const (
	patternNoDir     = 1
	patternEndsWith  = 4
	patternMustBeDir = 8
	patternNegative  = 16
)

// pattern is one ignore rule, parsed as Git's parse_path_pattern does.
type pattern struct {
	text       []byte
	noWildcard int
	flags      int
	base       string // the directory of its file, with its trailing slash; "" at the root
}

// patternList is the rules of one file, in its order.
type patternList []pattern

// parsePatterns parses the content of an ignore file whose rules apply at
// base, as Git's add_patterns_from_buffer does: a UTF-8 BOM is skipped, a
// line ends at a newline with a carriage return before it dropped, then at its
// first NUL, an empty line and a comment are skipped, and trailing unescaped
// spaces are trimmed.
func parsePatterns(content []byte, base string) patternList {
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	if len(content) == 0 {
		return nil
	}
	content = append(append([]byte(nil), content...), '\n')
	var list patternList
	start := 0
	for i, c := range content {
		if c != '\n' {
			continue
		}
		line := content[start:i]
		start = i + 1
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		if line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		// Git holds the line as a C string: it ends at its first NUL.
		if nul := bytes.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		line = trimTrailingSpaces(line)
		list = append(list, parsePattern(line, base))
	}
	return list
}

// trimTrailingSpaces drops the trailing spaces no backslash escapes.
func trimTrailingSpaces(line []byte) []byte {
	lastSpace := -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			if lastSpace < 0 {
				lastSpace = i
			}
		case '\\':
			i++
			if i >= len(line) {
				return line
			}
			lastSpace = -1
		default:
			lastSpace = -1
		}
	}
	if lastSpace >= 0 {
		return line[:lastSpace]
	}
	return line
}

func parsePattern(line []byte, base string) pattern {
	p := pattern{base: base}
	if len(line) > 0 && line[0] == '!' {
		p.flags |= patternNegative
		line = line[1:]
	}
	if len(line) > 0 && line[len(line)-1] == '/' {
		line = line[:len(line)-1]
		p.flags |= patternMustBeDir
	}
	if bytes.IndexByte(line, '/') < 0 {
		p.flags |= patternNoDir
	}
	p.noWildcard = simpleLength(line)
	if len(line) > 0 && line[0] == '*' && simpleLength(line[1:]) == len(line)-1 {
		p.flags |= patternEndsWith
	}
	p.text = line
	return p
}

// versionSensitive reports whether a rule of the list is one whose meaning
// Git changed after 2.43: a path rule whose literal prefix does not end with
// a slash and is followed by "**", such as `foo**/bar`, which Git 2.52 no
// longer lets match `foobar` or `foo/x/bar` (match_pathname). The reader
// steps aside, so each Git answers by its own reading.
func (l patternList) versionSensitive() bool {
	for _, p := range l {
		if p.flags&patternNoDir != 0 {
			continue
		}
		text, prefix := p.text, p.noWildcard
		if len(text) > 0 && text[0] == '/' {
			text, prefix = text[1:], prefix-1
		}
		if prefix > 0 && text[prefix-1] != '/' && bytes.HasPrefix(text[prefix:], []byte("**")) {
			return true
		}
	}
	return false
}

// simpleLength is the length of the leading part of s without a glob
// special character.
func simpleLength(s []byte) int {
	for i, c := range s {
		if isGlobSpecial(c) {
			return i
		}
	}
	return len(s)
}

// matcher answers whether paths are excluded, with the case folding of the
// repository (core.ignoreCase).
type matcher struct {
	ignoreCase bool
}

func (m matcher) equal(a, b []byte) bool {
	if m.ignoreCase {
		return asciiEqualFold(a, b)
	}
	return bytes.Equal(a, b)
}

func (m matcher) flags() int {
	if m.ignoreCase {
		return wmCaseFold
	}
	return 0
}

// lastMatch returns the last rule of list that matches path, a slash path
// from the work tree's root, and whether one does (last_matching_pattern_from_list).
func (m matcher) lastMatch(list patternList, path string, isDir bool) (pattern, bool) {
	basename := path[strings.LastIndexByte(path, '/')+1:]
	for i := len(list) - 1; i >= 0; i-- {
		p := list[i]
		if p.flags&patternMustBeDir != 0 && !isDir {
			continue
		}
		if p.flags&patternNoDir != 0 {
			if m.matchBasename([]byte(basename), p) {
				return p, true
			}
			continue
		}
		if m.matchPathname([]byte(path), p) {
			return p, true
		}
	}
	return pattern{}, false
}

func (m matcher) matchBasename(basename []byte, p pattern) bool {
	switch {
	case p.noWildcard == len(p.text):
		return m.equal(p.text, basename)
	case p.flags&patternEndsWith != 0:
		suffix := p.text[1:]
		return len(suffix) <= len(basename) && m.equal(suffix, basename[len(basename)-len(suffix):])
	default:
		return wildmatch(p.text, basename, m.flags())
	}
}

func (m matcher) matchPathname(path []byte, p pattern) bool {
	text, prefix := p.text, p.noWildcard
	if len(text) > 0 && text[0] == '/' {
		text = text[1:]
		prefix--
	}
	baseLen := len(p.base)
	if baseLen > 0 {
		baseLen-- // without its trailing slash
	}
	if len(path) < baseLen+1 || (baseLen > 0 && path[baseLen] != '/') || !m.equal(path[:baseLen], []byte(p.base[:baseLen])) {
		return false
	}
	name := path
	if baseLen > 0 {
		name = path[baseLen+1:]
	}
	if prefix > 0 {
		if prefix > len(name) || !m.equal(text[:prefix], name[:prefix]) {
			return false
		}
		text, name = text[prefix:], name[prefix:]
		if len(text) == 0 && len(name) == 0 {
			return true
		}
	}
	return wildmatch(text, name, m.flags()|wmPathname)
}

func asciiEqualFold(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if toLower(a[i]) != toLower(b[i]) {
			return false
		}
	}
	return true
}
