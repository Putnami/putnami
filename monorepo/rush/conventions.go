package rush

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
}

// importConventions answers the files an import writes for the projects: the
// exports of a package.json whose main or types names a file outside src/,
// and a tsconfig.json that extends the root's where a project has none.
func importConventions(root string, projects []detect.Project) ([]conventionWrite, error) {
	var writes []conventionWrite
	for _, p := range projects {
		dir := filepath.Join(root, filepath.FromSlash(p.Folder))
		manifest := path.Join(p.Folder, "package.json")
		data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a project manifest under the workspace root
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if data != nil {
			after, err := withSourceExports(dir, data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", manifest, err)
			}
			if after != nil {
				writes = append(writes, conventionWrite{file: manifest, before: data, after: after})
			}
		}
		config := filepath.Join(dir, rootTSConfig)
		if _, err := os.Stat(config); errors.Is(err, fs.ErrNotExist) {
			up := strings.Repeat("../", strings.Count(p.Folder, "/")+1)
			body := fmt.Sprintf("{\n  \"extends\": %q\n}\n", up+rootTSConfig)
			writes = append(writes, conventionWrite{file: path.Join(p.Folder, rootTSConfig), after: []byte(body)})
		}
	}
	return writes, nil
}

// mainLine finds the top-level "main", "types" or "typings" member of a
// two-space or tab indented package.json, with its indentation.
var mainLine = regexp.MustCompile(`(?m)^([ \t]+)"(main|types|typings)"\s*:\s*"[^"]*",?[ \t]*$`)

// withSourceExports answers the package.json data with exports sourceEntry
// added after its main member, or nil when it keeps its own entry: it
// declares exports, names no main or types outside src/, or the project holds
// no src/index.ts. Every other byte is kept.
func withSourceExports(dir string, data []byte) ([]byte, error) {
	var manifest struct {
		Main    string          `json:"main"`
		Types   string          `json:"types"`
		Typings string          `json:"typings"`
		Exports json.RawMessage `json:"exports"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if manifest.Exports != nil {
		return nil, nil
	}
	outside := false
	for _, entry := range []string{manifest.Main, manifest.Types, manifest.Typings} {
		if entry != "" && !strings.HasPrefix(path.Clean(entry), "src/") {
			outside = true
		}
	}
	if !outside {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(sourceEntry))); err != nil {
		return nil, nil
	}
	loc := mainLine.FindSubmatchIndex(data)
	if loc == nil {
		return nil, nil
	}
	indent := string(data[loc[2]:loc[3]])
	line := data[loc[0]:loc[1]]
	var out bytes.Buffer
	out.Write(data[:loc[0]])
	if bytes.HasSuffix(bytes.TrimRight(line, " \t"), []byte(",")) {
		out.Write(line)
		fmt.Fprintf(&out, "\n%s\"exports\": {\n%s%s\".\": %q\n%s},", indent, indent, indent, sourceEntry, indent)
	} else {
		out.Write(bytes.TrimRight(line, " \t"))
		fmt.Fprintf(&out, ",\n%s\"exports\": {\n%s%s\".\": %q\n%s}", indent, indent, indent, sourceEntry, indent)
	}
	out.Write(data[loc[1]:])
	if !json.Valid(out.Bytes()) {
		return nil, nil
	}
	return out.Bytes(), nil
}

// applyConventions writes the files, or with dry set adds their diffs, and
// adds each to the result's Written.
func applyConventions(root string, writes []conventionWrite, result *stacks.GraphResult, dry bool) error {
	for _, w := range writes {
		result.Written = append(result.Written, w.file)
		if dry {
			result.Diffs = append(result.Diffs, stacks.GraphDiff{File: w.file, Diff: stacks.LineDiff(string(w.before), string(w.after))})
			continue
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(w.file)), w.after, 0o644); err != nil { //nolint:gosec // a tracked project file
			return err
		}
	}
	return nil
}
