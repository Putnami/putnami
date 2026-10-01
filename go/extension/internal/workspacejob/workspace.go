package workspacejob

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	pctx "go.putnami.dev/sdk/extension/context"
)

// WorkspaceConfigPath is the workspace configuration file: putnami.workspace.json,
// or the legacy .putnamirc.json when the former is missing.
func WorkspaceConfigPath(workspaceRoot string) string {
	current := filepath.Join(workspaceRoot, "putnami.workspace.json")
	if fileExists(current) {
		return current
	}
	return filepath.Join(workspaceRoot, ".putnamirc.json")
}

// GoProjects returns the workspace projects that hold a go.mod, in the order
// the workspace configuration declares them, duplicates included. A missing
// or unreadable configuration declares none.
func GoProjects(workspaceRoot string) []string {
	var projects []string
	for _, project := range declaredProjects(WorkspaceConfigPath(workspaceRoot)) {
		if fileExists(filepath.Join(workspaceRoot, project, "go.mod")) {
			projects = append(projects, project)
		}
	}
	return projects
}

// GoMembers returns the member paths that hold a go.mod, relative to
// workspaceRoot in slash form, sorted and without duplicates. members are the
// project paths of the workspace membership the CLI resolved from the
// configuration's includes (the job context's workspaceProjects). The
// workspace root itself and a path outside it are not members.
func GoMembers(workspaceRoot string, members []string) []string {
	seen := make(map[string]bool, len(members))
	var out []string
	for _, member := range members {
		rel := path.Clean(filepath.ToSlash(member))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) || filepath.IsAbs(member) || seen[rel] {
			continue
		}
		seen[rel] = true
		if fileExists(filepath.Join(workspaceRoot, filepath.FromSlash(rel), "go.mod")) {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// WorkspaceGoProjects returns the Go projects the Go jobs act on, and whether
// they come from the configuration's legacy "projects" member. That member
// wins when it names a Go project: the result is GoProjects, in declared order
// with duplicates. Otherwise the result is the Go members of the membership the
// CLI resolved, members reached through includes among them: GoMembers of
// members. The go.work sync, the module warm-up and deps-upgrade all act on
// this one answer.
func WorkspaceGoProjects(workspaceRoot string, members []string) (projects []string, legacy bool) {
	if declared := GoProjects(workspaceRoot); len(declared) > 0 {
		return declared, true
	}
	return GoMembers(workspaceRoot, members), false
}

// MemberPaths returns the project path of each reference in refs, in order:
// the paths of the membership a job context carries (workspaceProjects).
func MemberPaths(refs []pctx.ProjectRef) []string {
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		paths = append(paths, ref.Path)
	}
	return paths
}

// declaredProjects returns the entries of the configuration's "projects"
// member as the script's `jq -r '.projects // [] | .[]'` printed them: a
// string as is, a number or a boolean in its JSON spelling. null prints
// "null", which names no project with a go.mod unless one is called that.
func declaredProjects(configPath string) []string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var doc struct {
		Projects json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	values, ok := jsonIterate(doc.Projects)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := jsonScalarText(value); ok {
			out = append(out, text)
		}
	}
	return out
}

// jsonIterate returns the elements of a JSON array, or the member values of a
// JSON object in document order: what jq's `.[]` yields. Anything else yields
// nothing.
func jsonIterate(raw json.RawMessage) ([]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	switch raw[0] {
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, false
		}
		return values, true
	case '{':
		return objectValues(raw)
	default:
		return nil, false
	}
}

// objectValues returns the member values of a JSON object in document order.
func objectValues(raw json.RawMessage) ([]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	var values []json.RawMessage
	for decoder.More() {
		if _, err := decoder.Token(); err != nil {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

// jsonScalarText is the text `jq -r` prints for a scalar: a string without
// quotes, anything else in its JSON spelling. Objects and arrays report false.
func jsonScalarText(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	switch raw[0] {
	case '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", false
		}
		return text, true
	case '{', '[':
		return "", false
	default:
		return string(raw), true
	}
}

// UsesStandaloneMetadata reports whether the member owning goMod promises to
// resolve without the workspace's go.work: its putnami.json publishes a Go
// module or sets options["@putnami/go"].standalone.
//
// workspace-install warms these members' own build lists and deps-upgrade
// treats their go.mod and go.sum as release-lock surfaces, so both jobs read
// this one predicate. It answers what the script's jq expression
//
//	(((.publish // []) | index("go")) != null) or
//	((.options["@putnami/go"].standalone // false) == true)
//
// answered, including its edges: a publish string containing "go" counts, and
// a putnami.json the expression failed on is not standalone.
func UsesStandaloneMetadata(goMod string) bool {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(goMod), "putnami.json"))
	if err != nil {
		return false
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		return false
	}
	publishes, ok := publishesGo(config["publish"])
	if !ok {
		return false
	}
	return publishes || standaloneOption(config["options"])
}

// publishesGo evaluates `(.publish // []) | index("go") != null`. ok is false
// where jq fails: index on a number, a boolean or an object.
func publishesGo(raw json.RawMessage) (publishes, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return false, true
	}
	switch raw[0] {
	case '[':
		var targets []json.RawMessage
		if err := json.Unmarshal(raw, &targets); err != nil {
			return false, false
		}
		for _, target := range targets {
			var text string
			if bytes.HasPrefix(bytes.TrimSpace(target), []byte(`"`)) && json.Unmarshal(target, &text) == nil && text == "go" {
				return true, true
			}
		}
		return false, true
	case '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return false, false
		}
		return strings.Contains(text, "go"), true
	default:
		return false, false
	}
}

// standaloneOption evaluates
// `(.options["@putnami/go"].standalone // false) == true`; a shape jq fails on
// reads as false.
func standaloneOption(raw json.RawMessage) bool {
	var options map[string]json.RawMessage
	if err := json.Unmarshal(raw, &options); err != nil {
		return false
	}
	var goOptions struct {
		Standalone json.RawMessage `json:"standalone"`
	}
	if err := json.Unmarshal(options["@putnami/go"], &goOptions); err != nil {
		return false
	}
	return string(bytes.TrimSpace(goOptions.Standalone)) == "true"
}

// ValidModuleCoordinate reports whether coordinate may be handed to the go
// command as a module argument: it is not empty, holds no whitespace, does not
// start with "-" (which go would read as a flag) and names a version with "@".
func ValidModuleCoordinate(coordinate string) bool {
	if coordinate == "" || strings.HasPrefix(coordinate, "-") {
		return false
	}
	if strings.IndexFunc(coordinate, unicode.IsSpace) >= 0 {
		return false
	}
	return strings.Contains(coordinate, "@")
}

// Snapshot keeps the content and mode of a set of files so they can be put
// back exactly: a file that existed is rewritten with its old bytes and mode,
// and one that did not is removed.
type Snapshot struct {
	entries []snapshotEntry
}

type snapshotEntry struct {
	path    string
	content []byte
	mode    fs.FileMode
	existed bool
}

// Add records the current state of path.
func (s *Snapshot) Add(path string) error {
	content, err := os.ReadFile(path)
	switch {
	case err == nil:
		info, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		s.entries = append(s.entries, snapshotEntry{path: path, content: content, mode: info.Mode().Perm(), existed: true})
	case os.IsNotExist(err):
		s.entries = append(s.entries, snapshotEntry{path: path})
	default:
		// A directory, like a missing file, is not something the script
		// copied: `[ -f path ]` was false for it.
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			s.entries = append(s.entries, snapshotEntry{path: path})
			return nil
		}
		return err
	}
	return nil
}

// Paths returns the recorded paths in the order they were added.
func (s *Snapshot) Paths() []string {
	paths := make([]string, len(s.entries))
	for i, entry := range s.entries {
		paths[i] = entry.path
	}
	return paths
}

// Restore puts every recorded file back. changed is called for each file whose
// content differed and was rewritten, and removed for each file that did not
// exist and was deleted. A file whose content is back but whose mode differs
// gets its recorded mode back without a call. Restore puts back every file it
// can, returns the failures joined, and then forgets the recorded state, so a
// second call does nothing.
func (s *Snapshot) Restore(changed, removed func(path string)) error {
	var errs []error
	for _, entry := range s.entries {
		if entry.existed {
			current, err := os.ReadFile(entry.path)
			if err != nil || !bytes.Equal(current, entry.content) {
				if writeErr := os.WriteFile(entry.path, entry.content, entry.mode); writeErr != nil {
					errs = append(errs, fmt.Errorf("restore %s: %w", entry.path, writeErr))
					continue
				}
				if changed != nil {
					changed(entry.path)
				}
			}
			if info, err := os.Stat(entry.path); err != nil || info.Mode().Perm() != entry.mode {
				if chmodErr := os.Chmod(entry.path, entry.mode); chmodErr != nil {
					errs = append(errs, fmt.Errorf("restore the mode of %s: %w", entry.path, chmodErr))
				}
			}
			continue
		}
		if fileExists(entry.path) {
			if err := os.Remove(entry.path); err != nil {
				errs = append(errs, fmt.Errorf("remove %s: %w", entry.path, err))
			} else if removed != nil {
				removed(entry.path)
			}
		}
	}
	s.entries = nil
	return errors.Join(errs...)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// FileExists reports whether path is a regular file, the script's `[ -f path ]`.
func FileExists(path string) bool { return fileExists(path) }

// IsExecutable reports whether path is a file the job may run: the script's
// `[ -x path ]`.
func IsExecutable(path string) bool { return isExecutable(path) }
