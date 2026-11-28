package emit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// configResolver finds the tsconfig.json that governs a module the way Bun
// does, the nearest one in its directory or above it, and resolves the
// compiler options of its extends chain.
type configResolver struct {
	workspaceRoot string
	// byDir memoizes the governing config of a directory: its id, or "" when
	// its chain cannot be read.
	byDir map[string]string
	// options holds the merged compilerOptions of each resolved config id.
	options map[string]map[string]any
	// memberRoots are the closure's directories, whose files the task's key
	// reads.
	memberRoots []string
}

func newConfigResolver(workspaceRoot string, members []Member) *configResolver {
	r := &configResolver{
		workspaceRoot: filepath.Clean(workspaceRoot),
		byDir:         map[string]string{},
		options:       map[string]map[string]any{},
	}
	for _, m := range members {
		r.memberRoots = append(r.memberRoots, filepath.Clean(m.Root))
	}
	return r
}

// keyed reports whether the task's key reads file: a file of a closure member,
// or a tsconfig at the workspace root. A config outside them could change
// without moving the key, so its chain is not read.
func (r *configResolver) keyed(file string) bool {
	file = filepath.Clean(file)
	if filepath.Dir(file) == r.workspaceRoot {
		base := filepath.Base(file)
		return strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json")
	}
	for _, root := range r.memberRoots {
		if within(root, file) {
			return true
		}
	}
	return false
}

// governing returns the id of the config that governs the modules of dir, and
// false when a config governs them whose chain cannot be read, or when the
// search reaches a directory whose tsconfig.json the key does not read
// (keyedDir): those modules then key on their bytes. A directory no config
// governs gets the empty options, as Bun transpiles it.
func (r *configResolver) governing(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	if id, ok := r.byDir[dir]; ok {
		return id, id != ""
	}
	id := r.resolveDir(dir)
	r.byDir[dir] = id
	return id, id != ""
}

func (r *configResolver) resolveDir(dir string) string {
	if !r.keyedDir(dir) {
		// Whether this directory holds a tsconfig.json is not in the task's
		// key, so the search cannot tell which config governs.
		return ""
	}
	candidate := filepath.Join(dir, "tsconfig.json")
	if _, err := os.Stat(candidate); err == nil {
		options, ok := r.readChain(candidate, map[string]bool{})
		if !ok {
			return ""
		}
		encoded, _ := json.Marshal(options)
		sum := sha256.Sum256(encoded)
		id := hex.EncodeToString(sum[:8])
		r.options[id] = options
		return id
	}
	parent := filepath.Dir(dir)
	if dir == r.workspaceRoot || parent == dir || !within(r.workspaceRoot, parent) {
		r.options["none"] = map[string]any{}
		return "none"
	}
	id, _ := r.governing(parent)
	return id
}

// keyedDir reports whether the task's key reads which tsconfig.json dir
// holds: the workspace root, whose tsconfig*.json it reads, or a directory of
// a closure member, whose files it reads. A directory between a member and the
// workspace root is neither, so a search that reaches one governs nothing.
func (r *configResolver) keyedDir(dir string) bool {
	if dir == r.workspaceRoot {
		return true
	}
	for _, root := range r.memberRoots {
		if within(root, dir) {
			return true
		}
	}
	return false
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// readChain merges the compilerOptions of a config over those of the configs it
// extends. It answers false for a chain it cannot read: a file that does not
// parse, a cycle, a config the task's key does not read, or an extends that
// names a package, whose resolution is not this reader's to reproduce.
func (r *configResolver) readChain(file string, seen map[string]bool) (map[string]any, bool) {
	if seen[file] || !r.keyed(file) {
		return nil, false
	}
	seen[file] = true
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	var doc struct {
		Extends         json.RawMessage `json:"extends"`
		CompilerOptions map[string]any  `json:"compilerOptions"`
	}
	if err := json.Unmarshal(StripJSONC(data), &doc); err != nil {
		return nil, false
	}
	var bases []string
	if len(doc.Extends) > 0 {
		var one string
		if err := json.Unmarshal(doc.Extends, &one); err == nil {
			bases = []string{one}
		} else if err := json.Unmarshal(doc.Extends, &bases); err != nil {
			return nil, false
		}
	}
	merged := map[string]any{}
	for _, base := range bases {
		if !strings.HasPrefix(base, "./") && !strings.HasPrefix(base, "../") && !filepath.IsAbs(base) {
			return nil, false
		}
		target := base
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(file), filepath.FromSlash(base))
		}
		if !strings.HasSuffix(target, ".json") {
			if _, err := os.Stat(target); err != nil {
				target += ".json"
			}
		}
		options, ok := r.readChain(target, seen)
		if !ok {
			return nil, false
		}
		for k, v := range options {
			merged[k] = v
		}
	}
	for k, v := range doc.CompilerOptions {
		merged[k] = v
	}
	return merged, true
}

// StripJSONC removes the comments and trailing commas a tsconfig may hold,
// outside strings.
func StripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i++
		case c == ',':
			j := i + 1
			for j < len(data) && strings.ContainsRune(" \t\r\n", rune(data[j])) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}
