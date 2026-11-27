package git

import "bytes"

// The flags of wildmatch, as Git's wildmatch.h names them.
const (
	wmCaseFold = 1
	wmPathname = 2
)

// The internal answers of dowild.
const (
	wmMatch           = 0
	wmNoMatch         = 1
	wmAbortAll        = -1
	wmAbortToStarStar = -2
)

// wildmatch reports whether text matches pattern, as Git's wildmatch does
// (wildmatch.c of Git 2.43, ported line by line): the matcher every ignore
// rule goes through.
func wildmatch(pattern, text []byte, flags int) bool {
	return dowild(pattern, text, flags) == wmMatch
}

// at returns s[i], or 0 past its end: Git's strings end with a NUL.
func at(s []byte, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func dowild(pattern, text []byte, flags int) int {
	p, t := 0, 0
	for ; at(pattern, p) != 0; t, p = t+1, p+1 {
		pch := pattern[p]
		tch := at(text, t)
		if tch == 0 && pch != '*' {
			return wmAbortAll
		}
		if flags&wmCaseFold != 0 && isUpper(tch) {
			tch = toLower(tch)
		}
		if flags&wmCaseFold != 0 && isUpper(pch) {
			pch = toLower(pch)
		}
		switch pch {
		case '\\':
			// Literal match with the following character; past the end, the
			// NUL never equals a text character.
			p++
			pch = at(pattern, p)
			if tch != pch {
				return wmNoMatch
			}
			continue
		case '?':
			if flags&wmPathname != 0 && tch == '/' {
				return wmNoMatch
			}
			continue
		case '*':
			var matchSlash bool
			p++
			if at(pattern, p) == '*' {
				prevP := p
				for {
					p++
					if at(pattern, p) != '*' {
						break
					}
				}
				if flags&wmPathname == 0 {
					matchSlash = true
				} else if (prevP < 2 || pattern[prevP-2] == '/') &&
					(at(pattern, p) == 0 || at(pattern, p) == '/' ||
						(at(pattern, p) == '\\' && at(pattern, p+1) == '/')) {
					if at(pattern, p) == '/' && dowild(pattern[p+1:], text[t:], flags) == wmMatch {
						return wmMatch
					}
					matchSlash = true
				} else {
					matchSlash = false
				}
			} else {
				matchSlash = flags&wmPathname == 0
			}
			if at(pattern, p) == 0 {
				if !matchSlash && bytes.IndexByte(text[t:], '/') >= 0 {
					return wmAbortToStarStar
				}
				return wmMatch
			} else if !matchSlash && pattern[p] == '/' {
				slash := bytes.IndexByte(text[t:], '/')
				if slash < 0 {
					return wmAbortAll
				}
				t += slash
				// The slash is consumed by the loop.
				continue
			}
			for {
				if tch == 0 {
					break
				}
				if !isGlobSpecial(pattern[p]) {
					pch = pattern[p]
					if flags&wmCaseFold != 0 && isUpper(pch) {
						pch = toLower(pch)
					}
					for {
						tch = at(text, t)
						if tch == 0 || (!matchSlash && tch == '/') {
							break
						}
						if flags&wmCaseFold != 0 && isUpper(tch) {
							tch = toLower(tch)
						}
						if tch == pch {
							break
						}
						t++
					}
					if tch != pch {
						if matchSlash {
							return wmAbortAll
						}
						return wmAbortToStarStar
					}
				}
				matched := dowild(pattern[p:], text[t:], flags)
				if matched != wmNoMatch {
					if !matchSlash || matched != wmAbortToStarStar {
						return matched
					}
				} else if !matchSlash && tch == '/' {
					return wmAbortToStarStar
				}
				t++
				tch = at(text, t)
			}
			return wmAbortAll
		case '[':
			p++
			pch = at(pattern, p)
			if pch == '^' {
				pch = '!'
			}
			negated := pch == '!'
			if negated {
				p++
				pch = at(pattern, p)
			}
			var prevCh byte
			matched := false
			for {
				if pch == 0 {
					return wmAbortAll
				}
				if pch == '\\' {
					p++
					pch = at(pattern, p)
					if pch == 0 {
						return wmAbortAll
					}
					if tch == pch {
						matched = true
					}
				} else if pch == '-' && prevCh != 0 && at(pattern, p+1) != 0 && at(pattern, p+1) != ']' {
					p++
					pch = pattern[p]
					if pch == '\\' {
						p++
						pch = at(pattern, p)
						if pch == 0 {
							return wmAbortAll
						}
					}
					if tch <= pch && tch >= prevCh {
						matched = true
					} else if flags&wmCaseFold != 0 && isLower(tch) {
						upper := toUpper(tch)
						if upper <= pch && upper >= prevCh {
							matched = true
						}
					}
					pch = 0
				} else if pch == '[' && at(pattern, p+1) == ':' {
					p += 2
					s := p
					for at(pattern, p) != 0 && pattern[p] != ']' {
						p++
					}
					pch = at(pattern, p)
					if pch == 0 {
						return wmAbortAll
					}
					i := p - s - 1
					if i < 0 || pattern[p-1] != ':' {
						// No ":]": a normal set that holds '['.
						p = s - 2
						pch = '['
						if tch == pch {
							matched = true
						}
						prevCh = pch
						p++
						pch = at(pattern, p)
						if pch == ']' {
							break
						}
						continue
					}
					class, ok := charClass(string(pattern[s:s+i]), tch, flags)
					if !ok {
						return wmAbortAll
					}
					if class {
						matched = true
					}
					pch = 0
				} else if tch == pch {
					matched = true
				}
				prevCh = pch
				p++
				pch = at(pattern, p)
				if pch == ']' {
					break
				}
			}
			if matched == negated || (flags&wmPathname != 0 && tch == '/') {
				return wmNoMatch
			}
			continue
		default:
			if tch != pch {
				return wmNoMatch
			}
			continue
		}
	}
	if t < len(text) {
		return wmNoMatch
	}
	return wmMatch
}

// charClass answers a [:name:] class for c, and false for a name Git does not
// know.
func charClass(name string, c byte, flags int) (bool, bool) {
	switch name {
	case "alnum":
		return isAlpha(c) || isDigit(c), true
	case "alpha":
		return isAlpha(c), true
	case "blank":
		return c == ' ' || c == '\t', true
	case "cntrl":
		return c < 0x20 || c == 0x7f, true
	case "digit":
		return isDigit(c), true
	case "graph":
		return c > 0x20 && c < 0x7f, true
	case "lower":
		return isLower(c), true
	case "print":
		return c >= 0x20 && c <= 0x7e, true
	case "punct":
		return c > 0x20 && c < 0x7f && !isAlpha(c) && !isDigit(c), true
	case "space":
		return c == ' ' || c == '\t' || c == '\n' || c == '\r', true
	case "upper":
		return isUpper(c) || (flags&wmCaseFold != 0 && isLower(c)), true
	case "xdigit":
		return isDigit(c) || (c|0x20) >= 'a' && (c|0x20) <= 'f', true
	}
	return false, false
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func toLower(c byte) byte {
	if isUpper(c) {
		return c | 0x20
	}
	return c
}
func toUpper(c byte) byte {
	if isLower(c) {
		return c &^ 0x20
	}
	return c
}

// isGlobSpecial reports the characters Git's ctype marks GIT_GLOB_SPECIAL.
func isGlobSpecial(c byte) bool { return c == '*' || c == '?' || c == '[' || c == '\\' }
