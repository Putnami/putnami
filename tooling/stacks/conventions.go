package stacks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// SourceEntry is the entry an import gives a project whose package.json
// names a build output that its build no longer writes: the TypeScript
// extension's Bun build resolves a project from its sources (D-066).
const SourceEntry = "./src/index.ts"

// rootTSConfig is the TypeScript configuration of the workspace root, which
// the TypeScript extension's install writes.
const rootTSConfig = "tsconfig.json"

// ConventionProject is a project an import hands to the TypeScript
// extension, with what D-066 reads of it.
type ConventionProject struct {
	// Dir is the workspace-relative project directory.
	Dir string
	// Name is the project's name, for the warnings.
	Name string
	// BunBuild reports a build the import moves to the TypeScript
	// extension's Bun build, which writes nothing in the project: a build
	// that runs tsc writes the output its main names, and so does one that
	// stays on the tool.
	BunBuild bool
	// PublishedBy, when set, names what publishes the project, such as
	// rush.json: its main stays the entry its consumers read.
	PublishedBy string
}

// ConventionWrite is one file an import writes to adopt Putnami's
// TypeScript conventions.
type ConventionWrite struct {
	// File is the workspace-relative file.
	File string
	// Before and After are its bytes; Before is nil for a new file.
	Before, After []byte
	// What says what the write does, for the note that lists it.
	What string
}

// ImportConventions answers the files an import writes for the projects
// (D-066), and a warning for each manifest it does not edit: the exports of
// a package.json whose main or types names a file outside src/, when the
// project's build moves to the Bun build and nothing publishes it, and a
// tsconfig.json that extends the root's where a project holds no
// tsconfig*.json. A project that holds another tsconfig*.json keeps its
// configuration, which its test runner or its linter may read (ADR 0017).
func ImportConventions(root string, projects []ConventionProject) ([]ConventionWrite, []string, error) {
	var writes []ConventionWrite
	var warnings []string
	for _, p := range projects {
		dir := filepath.Join(root, filepath.FromSlash(p.Dir))
		manifest := path.Join(p.Dir, "package.json")
		data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a project manifest under the workspace root
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		if data != nil && p.BunBuild {
			after, reason, err := withSourceExports(dir, data)
			switch {
			case err != nil:
				return nil, nil, fmt.Errorf("%s: %w", manifest, err)
			case after != nil && p.PublishedBy != "":
				warnings = append(warnings, fmt.Sprintf("%s keeps its entry: %s publishes %s, so its main stays the build output its consumers read, which Putnami's Bun build does not write, and the projects that depend on it cannot resolve it; add \"exports\": {\".\": %q} by hand when no consumer outside the repository needs that entry, or keep its build on the tool", manifest, p.PublishedBy, p.Name, SourceEntry))
			case after != nil:
				writes = append(writes, ConventionWrite{File: manifest, Before: data, After: after, What: "exports " + SourceEntry + " added, so its dependents build against its sources"})
			case reason != "":
				warnings = append(warnings, fmt.Sprintf("%s keeps its entry: %s; add \"exports\": {\".\": %q} by hand so its dependents build against its sources", manifest, reason, SourceEntry))
			}
		}
		if configs, _ := filepath.Glob(filepath.Join(dir, "tsconfig*.json")); len(configs) == 0 {
			up := strings.Repeat("../", strings.Count(p.Dir, "/")+1)
			body := fmt.Sprintf("{\n  \"extends\": %q\n}\n", up+rootTSConfig)
			writes = append(writes, ConventionWrite{File: path.Join(p.Dir, rootTSConfig), After: []byte(body), What: "written, extending the root's"})
		}
	}
	return writes, warnings, nil
}

// PackagePublished answers what publishes the package in dir: its
// package.json when it is not private, which npm then publishes; empty for a
// private package or a directory without a readable package.json.
func PackagePublished(root, dir string) string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "package.json")) //nolint:gosec // a project manifest under the workspace root
	if err != nil {
		return ""
	}
	var manifest struct {
		Private bool `json:"private"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.Private {
		return ""
	}
	return "its package.json, which is not private,"
}

// entryKeys are the top-level members of a package.json that name its entry.
var entryKeys = []string{"main", "types", "typings"}

// memberLine is a line that holds one member of a JSON object, alone, with
// its indentation, and an optional trailing comma.
var memberLine = regexp.MustCompile(`^([ \t]+)"[^"]+"\s*:\s*"[^"]*"(,?)[ \t]*$`)

// withSourceExports answers the package.json data with exports SourceEntry
// added after its top-level main, types or typings member, or nil when it
// keeps its own entry: it declares exports, names no entry outside src/, or
// the project holds no src/index.ts. A manifest it would edit but cannot,
// such as one whose entry shares its line with another member, gives a
// reason instead. Every other byte is kept.
func withSourceExports(dir string, data []byte) ([]byte, string, error) {
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, "", fmt.Errorf("not JSON: %w", err)
	}
	if _, ok := manifest["exports"]; ok {
		return nil, "", nil
	}
	outside := false
	for _, key := range entryKeys {
		var entry string
		if json.Unmarshal(manifest[key], &entry) == nil && entry != "" && !strings.HasPrefix(path.Clean(entry), "src/") {
			outside = true
		}
	}
	if !outside {
		return nil, "", nil
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(SourceEntry))); err != nil {
		return nil, "", nil
	}
	end, err := topLevelEntryEnd(data)
	if err != nil {
		return nil, "", fmt.Errorf("not JSON: %w", err)
	}
	if bytes.Contains(data, []byte("\r\n")) {
		return nil, "its lines end with CRLF", nil
	}
	start := bytes.LastIndexByte(data[:end], '\n') + 1
	stop := bytes.IndexByte(data[end:], '\n')
	if stop < 0 {
		return nil, "its entry is on its last line", nil
	}
	stop += end
	line := data[start:stop]
	m := memberLine.FindSubmatch(line)
	if m == nil {
		return nil, "its entry shares its line with another member", nil
	}
	indent := string(m[1])
	member := fmt.Sprintf("%s\"exports\": {\n%s%s\".\": %q\n%s}", indent, indent, indent, SourceEntry, indent)
	var out bytes.Buffer
	out.Write(data[:start])
	if len(m[2]) > 0 {
		out.Write(line)
		out.WriteString("\n" + member + ",")
	} else {
		out.Write(bytes.TrimRight(line, " \t"))
		out.WriteString(",\n" + member)
	}
	out.Write(data[stop:])
	if !json.Valid(out.Bytes()) {
		return nil, "the edit would not be valid JSON", nil
	}
	return out.Bytes(), "", nil
}

// topLevelEntryEnd answers the offset just after the value of the last
// top-level main, types or typings member of the object in data, so a
// member of a nested object, such as publishConfig.main, is never edited.
func topLevelEntryEnd(data []byte) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	depth, expectKey, found := 0, false, -1
	var key string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				depth++
				expectKey = depth == 1
			case '[':
				depth++
			case '}', ']':
				depth--
				expectKey = depth == 1
			}
			continue
		case string:
			if depth == 1 && expectKey {
				key, expectKey = t, false
				continue
			}
		}
		if depth == 1 && !expectKey {
			for _, k := range entryKeys {
				if key == k {
					found = int(dec.InputOffset())
				}
			}
			expectKey = true
		}
	}
	if found < 0 {
		return 0, errors.New("no top-level entry member")
	}
	return found, nil
}

// ApplyConventions writes the files, each replaced through a temporary file
// beside it, and answers a note that lists each; with dry set it writes
// nothing and answers each as a change.
func ApplyConventions(root string, writes []ConventionWrite, dry bool) ([]FileChange, []string, error) {
	var changes []FileChange
	var notes []string
	for _, w := range writes {
		if dry {
			changes = append(changes, FileChange{File: w.File, Before: w.Before, After: w.After})
			continue
		}
		if err := writeAtomic(filepath.Join(root, filepath.FromSlash(w.File)), w.After); err != nil {
			return nil, nil, err
		}
		notes = append(notes, fmt.Sprintf("%s: %s", w.File, w.What))
	}
	return changes, notes, nil
}

// writeAtomic replaces file with data through a temporary file beside it.
func writeAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644) //nolint:gosec // a tracked project file
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), file)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}
