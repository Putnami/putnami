package lockdiff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Bun's lockfiles: the text lockfile Bun 1.2 and later write, and the binary
// one before it, which is not read.
const (
	BunLock       = "bun.lock"
	BunLockBinary = "bun.lockb"
)

// ReadBunLock reads a bun.lock, the JSON with trailing commas Bun 1.2 and
// later write: each entry of packages names its package and version in the
// first member of its array, name@version, under a key that is its install
// path. A workspace, link, file or root entry is the repository's own code
// and is left out.
func ReadBunLock(data []byte) (Packages, error) {
	var lock struct {
		Packages map[string][]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(stripJSONC(data), &lock); err != nil {
		return nil, fmt.Errorf("bun.lock is not JSON: %w", err)
	}
	out := Packages{}
	for key, entry := range lock.Packages {
		if len(entry) == 0 {
			continue
		}
		var ident string
		if err := json.Unmarshal(entry[0], &ident); err != nil {
			return nil, fmt.Errorf("bun.lock: the package %s does not start with its name and version", key)
		}
		// A name holds no @ but its scope's leading one; a git or tarball
		// resolution may hold more after it.
		at := strings.IndexByte(ident[min(1, len(ident)):], '@') + 1
		if at <= 0 || at == len(ident)-1 {
			continue
		}
		name, version := ident[:at], ident[at+1:]
		if localBunSpec(version) {
			continue
		}
		out.add(name, version)
	}
	return out, nil
}

// localBunSpec reports whether a bun.lock resolution names the repository's
// own code: a workspace, link, file or root specifier.
func localBunSpec(s string) bool {
	for _, prefix := range []string{"workspace:", "link:", "file:", "root:"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// errUnterminated is a string or a block comment that does not end.
var errUnterminated = errors.New("unterminated string or comment")

// stripJSONC turns JSON with comments and trailing commas into JSON: each
// comment becomes spaces and each comma before a closing bracket or brace
// is dropped. A document it cannot scan is returned as it is, for the JSON
// decoder to refuse.
func stripJSONC(data []byte) []byte {
	out, err := scanJSONC(data)
	if err != nil {
		return data
	}
	return out
}

func scanJSONC(data []byte) ([]byte, error) {
	out := make([]byte, 0, len(data))
	comma := -1 // the index in out of a comma not yet followed by a value
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '"':
			end := i + 1
			for ; end < len(data) && data[end] != '"'; end++ {
				if data[end] == '\\' {
					end++
				}
			}
			if end >= len(data) {
				return nil, errUnterminated
			}
			out = append(out, data[i:end+1]...)
			comma = -1
			i = end
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			i--
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			end := bytes.Index(data[i+2:], []byte("*/"))
			if end < 0 {
				return nil, errUnterminated
			}
			i += end + 3
		case c == ',':
			comma = len(out)
			out = append(out, c)
		case c == '}' || c == ']':
			if comma >= 0 {
				out[comma] = ' '
			}
			comma = -1
			out = append(out, c)
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			out = append(out, c)
		default:
			comma = -1
			out = append(out, c)
		}
	}
	return out, nil
}
