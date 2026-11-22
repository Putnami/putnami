package docexamples

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Manifest names the markdown documents whose examples are checked, by
// slash-separated patterns relative to the workspace root. A pattern's `**`
// segment matches any number of directories; the other segments follow
// path.Match.
type Manifest struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// ReadManifest reads a manifest file.
func ReadManifest(file string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(file)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("%s: %w", file, err)
	}
	if len(manifest.Include) == 0 {
		return manifest, fmt.Errorf("%s: include names no document", file)
	}
	return manifest, nil
}

// Documents returns the documents under root the manifest names, as sorted
// slash-separated paths relative to root. An include pattern that matches no
// document is an error, so a moved document does not leave the check silently.
func (m Manifest) Documents(root string) ([]string, error) {
	found := map[string]bool{}
	for _, pattern := range m.Include {
		matched := false
		base := staticPrefix(pattern)
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(base)), func(file string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name := entry.Name(); name == "node_modules" || (strings.HasPrefix(name, ".") && file != root) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if Match(pattern, rel) {
				matched = true
				if !m.excluded(rel) {
					found[rel] = true
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if !matched {
			return nil, fmt.Errorf("the pattern %q matches no document", pattern)
		}
	}
	documents := make([]string, 0, len(found))
	for document := range found {
		documents = append(documents, document)
	}
	slices.Sort(documents)
	return documents, nil
}

func (m Manifest) excluded(rel string) bool {
	for _, pattern := range m.Exclude {
		if Match(pattern, rel) {
			return true
		}
	}
	return false
}

// staticPrefix returns the directories of a pattern before its first
// wildcard segment.
func staticPrefix(pattern string) string {
	segments := strings.Split(pattern, "/")
	var prefix []string
	for _, segment := range segments[:len(segments)-1] {
		if strings.ContainsAny(segment, "*?[") {
			break
		}
		prefix = append(prefix, segment)
	}
	return strings.Join(prefix, "/")
}

// Match reports whether a slash-separated path matches a pattern whose `**`
// segment matches zero or more directories.
func Match(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}
