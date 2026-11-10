package rush

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

	detect "go.putnami.dev/tooling/observe/detect/rush"
	"go.putnami.dev/tooling/stacks"
)

// sourceEntry is the entry an import gives a project whose package.json
// names a build output: the TypeScript extension builds and resolves a
// project from its sources (D-066).
const sourceEntry = "./src/index.ts"

// rootTSConfig is the TypeScript configuration of the workspace root, which
// the TypeScript extension's install writes.
const rootTSConfig = "tsconfig.json"

// conventionWrite is one file an import writes to adopt Putnami's TypeScript
// conventions.
type conventionWrite struct {
	// file is the workspace-relative file.
	file string
	// before and after are its bytes; before is nil for a new file.
	before, after []byte
	// what says what the write does, for the warning that lists it.
	what string
}

// importConventions answers the files an import writes for the projects, and
// a warning for each manifest it cannot edit: the exports of a package.json
// whose main or types names a file outside src/, unless rush.json publishes
// the project, and a tsconfig.json that extends the root's where a project
// has none.
func importConventions(root string, projects []detect.Project) ([]conventionWrite, []string, error) {
	published, err := publishedProjects(root)
	if err != nil {
		return nil, nil, err
	}
	var writes []conventionWrite
	var warnings []string
	for _, p := range projects {
		dir := filepath.Join(root, filepath.FromSlash(p.Folder))
		manifest := path.Join(p.Folder, "package.json")
		data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a project manifest under the workspace root
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		if data != nil {
			after, reason, err := withSourceExports(dir, data)
			switch {
			case err != nil:
				return nil, nil, fmt.Errorf("%s: %w", manifest, err)
			case after != nil && published[p.Folder]:
				warnings = append(warnings, fmt.Sprintf("%s keeps its entry: rush.json publishes %s, so its main stays the build output its consumers read, which Putnami's TypeScript build does not write, and the projects that depend on it cannot resolve it; add \"exports\": {\".\": %q} by hand when no consumer outside the repository needs that entry, or adopt the repository in delegate mode", manifest, p.Name, sourceEntry))
			case after != nil:
				writes = append(writes, conventionWrite{file: manifest, before: data, after: after, what: "exports " + sourceEntry + " added, so its dependents build against its sources"})
			case reason != "":
				warnings = append(warnings, fmt.Sprintf("%s keeps its entry: %s; add \"exports\": {\".\": %q} by hand so its dependents build against its sources", manifest, reason, sourceEntry))
			}
		}
		config := filepath.Join(dir, rootTSConfig)
		if _, err := os.Stat(config); errors.Is(err, fs.ErrNotExist) {
			up := strings.Repeat("../", strings.Count(p.Folder, "/")+1)
			body := fmt.Sprintf("{\n  \"extends\": %q\n}\n", up+rootTSConfig)
			writes = append(writes, conventionWrite{file: path.Join(p.Folder, rootTSConfig), after: []byte(body), what: "written, extending the root's"})
		}
	}
	return writes, warnings, nil
}

// publishedProjects answers the folders of the projects rush.json publishes:
// shouldPublish, or a version policy.
func publishedProjects(root string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(root, detect.Config)) //nolint:gosec // rush.json at the workspace root
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Projects []struct {
			Folder        string `json:"projectFolder"`
			ShouldPublish bool   `json:"shouldPublish"`
			VersionPolicy string `json:"versionPolicyName"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(detect.StripComments(data), &cfg); err != nil {
		return nil, fmt.Errorf("rush.json cannot be parsed: %w", err)
	}
	out := map[string]bool{}
	for _, p := range cfg.Projects {
		if p.ShouldPublish || p.VersionPolicy != "" {
			out[path.Clean(strings.TrimPrefix(p.Folder, "./"))] = true
		}
	}
	return out, nil
}

// entryKeys are the top-level members of a package.json that name its entry.
var entryKeys = []string{"main", "types", "typings"}

// memberLine is a line that holds one member of a JSON object, alone, with
// its indentation, and an optional trailing comma.
var memberLine = regexp.MustCompile(`^([ \t]+)"[^"]+"\s*:\s*"[^"]*"(,?)[ \t]*$`)

// withSourceExports answers the package.json data with exports sourceEntry
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
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(sourceEntry))); err != nil {
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
	member := fmt.Sprintf("%s\"exports\": {\n%s%s\".\": %q\n%s}", indent, indent, indent, sourceEntry, indent)
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

// applyConventions writes the files, each replaced through a temporary file
// beside it, and lists each in a warning, or with dry set adds their diffs.
func applyConventions(root string, writes []conventionWrite, result *stacks.GraphResult, dry bool) error {
	for _, w := range writes {
		if dry {
			result.Diffs = append(result.Diffs, stacks.GraphDiff{File: w.file, Diff: stacks.LineDiff(string(w.before), string(w.after))})
			continue
		}
		if err := writeAtomic(filepath.Join(root, filepath.FromSlash(w.file)), w.after); err != nil {
			return err
		}
		result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %s", w.file, w.what))
	}
	return nil
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
