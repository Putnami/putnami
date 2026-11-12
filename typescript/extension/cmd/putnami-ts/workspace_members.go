package main

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// The member list a package manager reads belongs to the repository: its
// globs and its exclusions say which directories are packages, and a fixed
// list of paths would drop what they encode. So the workspace sync edits the
// list rather than writing it: it keeps every entry, removes only a path that
// names a directory without a package manifest, and adds a member only when no
// entry names it. An exclusion names a member too: the repository excluded it,
// and listing it again would undo that.

// packageManifests are the files that make a directory a package: pnpm reads
// package.yaml and package.json5 as well as package.json.
var packageManifests = []string{workspaceMarkerFile, "package.yaml", "package.json5"}

// memberListEdit is what the member list needs: the entries to remove and the
// member paths to add, sorted.
type memberListEdit struct {
	drop  func(string) bool
	added []string
}

// changes reports whether the edit changes current.
func (e memberListEdit) changes(current []string) bool {
	if len(e.added) > 0 {
		return true
	}
	for _, entry := range current {
		if e.drop(entry) {
			return true
		}
	}
	return false
}

// planMemberList returns the edit that lists every member of members in
// current, the entries of the member list as the repository wrote them.
func planMemberList(root string, current []string, members []typeScriptMember) memberListEdit {
	drop := func(entry string) bool {
		pattern, negated := memberPattern(entry)
		if negated || !literalMemberPattern(pattern) {
			return false
		}
		dir := filepath.Join(root, filepath.FromSlash(pattern))
		for _, manifest := range packageManifests {
			if isRegularFile(filepath.Join(dir, manifest)) {
				return false
			}
		}
		return true
	}
	var kept []string
	for _, entry := range current {
		if !drop(entry) {
			kept = append(kept, entry)
		}
	}
	var added []string
	for _, member := range members {
		if memberListNames(kept, member.Path) || templateMember(root, member.Path) {
			continue
		}
		added = append(added, member.Path)
	}
	return memberListEdit{drop: drop, added: added}
}

// memberListNames reports whether an entry of entries matches the member at
// rel, as a path or a glob it includes or as an exclusion.
func memberListNames(entries []string, rel string) bool {
	for _, entry := range entries {
		pattern, _ := memberPattern(entry)
		if matchMemberPattern(pattern, rel) {
			return true
		}
	}
	return false
}

// memberPattern returns entry as a slash-separated pattern relative to the
// root, and whether it is an exclusion ("!" ahead of it).
func memberPattern(entry string) (string, bool) {
	entry = strings.TrimSpace(entry)
	negated := strings.HasPrefix(entry, "!")
	entry = strings.TrimPrefix(entry, "!")
	entry = strings.TrimPrefix(filepath.ToSlash(entry), "./")
	entry = strings.TrimSuffix(entry, "/")
	return entry, negated
}

// literalMemberPattern reports whether pattern names one directory, with no
// glob character.
func literalMemberPattern(pattern string) bool {
	return !strings.ContainsAny(pattern, "*?[{")
}

// matchMemberPattern reports whether rel matches pattern the way the glob
// library of pnpm, npm and Yarn does: {a,b} alternatives, [!x] classes, a "**"
// segment that matches any number of segments, and no wildcard that matches a
// segment starting with a dot.
func matchMemberPattern(pattern, rel string) bool {
	segments := strings.Split(rel, "/")
	for _, alternative := range expandBraces(pattern) {
		if matchSegments(strings.Split(alternative, "/"), segments) {
			return true
		}
	}
	return false
}

// expandBraces returns the patterns the {a,b} alternatives of pattern stand
// for, pattern itself when it has none or an unbalanced brace.
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	depth, start := 0, open+1
	var options []string
	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case ',':
			if depth == 1 {
				options = append(options, pattern[start:i])
				start = i + 1
			}
		case '}':
			depth--
			if depth > 0 {
				continue
			}
			options = append(options, pattern[start:i])
			var expanded []string
			for _, option := range options {
				expanded = append(expanded, expandBraces(pattern[:open]+option+pattern[i+1:])...)
			}
			return expanded
		}
	}
	return []string{pattern}
}

func matchSegments(pattern, rel []string) bool {
	if len(pattern) == 0 {
		return len(rel) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(rel); i++ {
			if matchSegments(pattern[1:], rel[i:]) {
				return true
			}
			if i < len(rel) && strings.HasPrefix(rel[i], ".") {
				return false
			}
		}
		return false
	}
	if len(rel) == 0 {
		return false
	}
	if strings.HasPrefix(rel[0], ".") && !strings.HasPrefix(pattern[0], ".") {
		return false
	}
	segment := strings.ReplaceAll(pattern[0], "[!", "[^")
	if ok, err := path.Match(segment, rel[0]); err != nil || !ok {
		return false
	}
	return matchSegments(pattern[1:], rel[1:])
}

// packageNamePattern is the name a package manager accepts: an optional
// @scope/ and a name of URL-safe characters.
var packageNamePattern = regexp.MustCompile(`^(@[A-Za-z0-9._~-]+/)?[A-Za-z0-9._~-]+$`)

// templateMember reports whether the package.json of the member at rel is a
// template's: one that is no JSON object, or whose name is a placeholder such
// as {{ packageName }} that no package manager accepts. Listing it would fail
// the install of every other member.
func templateMember(root, rel string) bool {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel), workspaceMarkerFile)) //nolint:gosec // a member manifest under the workspace root
	if err != nil {
		return false
	}
	var pkg struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return true
	}
	return pkg.Name != nil && !packageNamePattern.MatchString(*pkg.Name)
}

// adoptionModeOption is the member of an extension's workspace options in
// which putnami init --from records how the workspace adopts the repository
// (D-064).
const adoptionModeOption = "mode"

// adoptedRepository reports whether the workspace at root adopts a repository
// with putnami init --from: an extension's options in putnami.workspace.json
// record an adoption mode. The member list of an adopted repository is the
// repository's: one it does not keep, the extension does not add.
func adoptedRepository(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json")) //nolint:gosec // the workspace config at the root
	if err != nil {
		return false
	}
	var config struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if json.Unmarshal(data, &config) != nil {
		return false
	}
	for _, raw := range config.Options {
		var option map[string]json.RawMessage
		if json.Unmarshal(raw, &option) != nil {
			continue
		}
		var mode string
		if json.Unmarshal(option[adoptionModeOption], &mode) == nil && (mode == "import" || mode == "delegate") {
			return true
		}
	}
	return false
}
