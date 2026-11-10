package turbo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ProjectSchema is the schema a putnami.json the sync creates names.
const ProjectSchema = "https://putnami.dev/schemas/putnami-project.json"

// skippedDirs are never walked: build outputs, installed packages and caches.
var skippedDirs = map[string]bool{"dist": true, "node_modules": true, "coverage": true}

// readProject reads the putnami.json at file as a JSON object, with its
// bytes; a missing file reads as an empty object and nil bytes.
func readProject(file string) (map[string]any, []byte, error) {
	doc := map[string]any{}
	existing, err := os.ReadFile(file) //nolint:gosec // a project file under the workspace root
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(existing))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("not a JSON object: %w", err)
	}
	return doc, existing, nil
}

// encodeProject renders doc as the sync writes it: two-space indented, with
// sorted keys and a final newline.
func encodeProject(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// replaceFile replaces file with data through a temporary file beside it.
func replaceFile(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+wsproto.ConfigFilename+".*")
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

// stringList reads a JSON array of strings; any other value reads as empty.
func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// setList sets key to list, or removes it when list is empty.
func setList(doc map[string]any, key string, list []string) {
	if len(list) == 0 {
		delete(doc, key)
		return
	}
	doc[key] = list
}

func sortedUnique(list []string) []string {
	slices.Sort(list)
	return slices.Compact(list)
}

// markedProjects lists, sorted, the workspace-relative directories under root
// whose putnami.json carries the turbo tag the sync writes.
func markedProjects(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(entry.Name(), ".") || skippedDirs[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != wsproto.ConfigFilename {
			return nil
		}
		doc, _, err := readProject(p)
		if err != nil || !slices.Contains(stringList(doc["tags"]), Tag) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil || rel == "." {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(out)
	return out, err
}

// junitReports reads the JUnit patterns the test task override of a
// putnami.json names.
func junitReports(doc map[string]any) []string {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[ExtensionName].(map[string]any)
	tasks, _ := layer["tasks"].(map[string]any)
	test, _ := tasks["test"].(map[string]any)
	reports, _ := test["reports"].(map[string]any)
	return stringList(reports["junit"])
}

// clone deep-copies a decoded JSON object, so the members the sync owns can be
// compared before and after it edits them.
func clone(doc map[string]any) map[string]any {
	data, _ := json.Marshal(doc)
	out := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}
