// Package workspace provides Python workspace discovery, pyproject.toml sync,
// and file utilities for the Putnami Python extension.
package workspace

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/sdk/extension/uvtoolchain"
)

var nameRe = regexp.MustCompile(`^name\s*=\s*"([^"]+)"\s*$`)

var skipDirs = map[string]bool{
	".git":          true,
	".putnami":      true,
	"node_modules":  true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	"dist":          true,
	"build":         true,
	".pytest_cache": true,
	".ruff_cache":   true,
}

// Project represents a discovered Python project.
type Project struct {
	Name         string
	RelativePath string
	FullPath     string
}

// ParsePyprojectName extracts the [project] name from a pyproject.toml file.
func ParsePyprojectName(pyprojectPath string) (string, error) {
	data, err := os.ReadFile(pyprojectPath)
	if err != nil {
		return "", err
	}

	content := string(data)
	inProject := false

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inProject = line == "[project]"
			continue
		}
		if !inProject {
			continue
		}
		if m := nameRe.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("no [project] name found in %s", pyprojectPath)
}

// putnamiRCConfig is the minimal structure of the workspace config we need.
type putnamiRCConfig struct {
	Includes []string `json:"includes"`
	Projects []string `json:"projects"`
}

// parseWorkspaceConfigProjects reads project paths from the workspace config.
func parseWorkspaceConfigProjects(workspaceRoot string) []string {
	path := filepath.Join(workspaceRoot, "putnami.workspace.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// Fallback to legacy filename
		data, err = os.ReadFile(filepath.Join(workspaceRoot, ".putnamirc.json"))
		if err != nil {
			return nil
		}
	}
	var config putnamiRCConfig
	if json.Unmarshal(data, &config) != nil {
		return nil
	}
	return expandWorkspaceEntries(workspaceRoot, "", configEntries(config), make(map[string]bool))
}

func configEntries(config putnamiRCConfig) []string {
	seen := make(map[string]bool, len(config.Includes)+len(config.Projects))
	entries := make([]string, 0, len(config.Includes)+len(config.Projects))
	for _, entry := range append(config.Includes, config.Projects...) {
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		entries = append(entries, entry)
	}
	return entries
}

func expandWorkspaceEntries(workspaceRoot, base string, entries []string, seen map[string]bool) []string {
	projects := make([]string, 0, len(entries))
	for _, entry := range entries {
		rel := filepath.Clean(filepath.Join(base, entry))
		if rel == "." || rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true

		scopeCfg := readPutnamiConfig(filepath.Join(workspaceRoot, rel, "putnami.json"))
		scopeEntries := configEntries(scopeCfg)
		if len(scopeEntries) > 0 {
			projects = append(projects, expandWorkspaceEntries(workspaceRoot, rel, scopeEntries, seen)...)
			continue
		}
		projects = append(projects, rel)
	}
	return projects
}

func readPutnamiConfig(path string) putnamiRCConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return putnamiRCConfig{}
	}
	var config putnamiRCConfig
	_ = json.Unmarshal(data, &config)
	return config
}

// DiscoverPythonProjects finds Python projects by filtering workspace config entries.
func DiscoverPythonProjects(workspaceRoot string) []string {
	rels := parseWorkspaceConfigProjects(workspaceRoot)
	members := make([]string, 0, len(rels))
	for _, rel := range rels {
		projectDir := filepath.Join(workspaceRoot, rel)
		if projectDir == workspaceRoot {
			continue
		}
		pyproject := filepath.Join(projectDir, "pyproject.toml")
		data, err := os.ReadFile(pyproject)
		if err != nil {
			continue
		}
		if !strings.Contains(string(data), "[project]") {
			continue
		}
		// uv reads members as slash-separated globs, and a backslash in the
		// TOML basic string they are written to is an escape: on Windows
		// "packages\mypkg" would not even parse.
		members = append(members, filepath.ToSlash(rel))
	}
	sort.Strings(members)
	return dedupe(members)
}

// SyncUVWorkspace updates the root pyproject.toml with [tool.uv.workspace]
// members and [tool.uv.sources] entries for all discovered Python projects.
// Returns (changed, members, error).
func SyncUVWorkspace(workspaceRoot string) (bool, []string, error) {
	members := DiscoverPythonProjects(workspaceRoot)
	if len(members) == 0 {
		return false, nil, nil
	}

	// Collect project names for sources
	var projectNames []string
	seen := make(map[string]bool)
	for _, member := range members {
		name, err := ParsePyprojectName(filepath.Join(workspaceRoot, member, "pyproject.toml"))
		if err != nil {
			continue
		}
		if !seen[name] {
			projectNames = append(projectNames, name)
			seen[name] = true
		}
	}
	sort.Strings(projectNames)

	workspaceSection := fmt.Sprintf("[tool.uv.workspace]\nmembers = [%s]", formatTOMLArray(members))

	sourcesSection := "[tool.uv.sources]"
	for _, name := range projectNames {
		sourcesSection += fmt.Sprintf("\n%s = { workspace = true }", name)
	}

	pyprojectPath := filepath.Join(workspaceRoot, "pyproject.toml")

	data, err := os.ReadFile(pyprojectPath)
	if err != nil {
		// File doesn't exist — create it
		content := strings.Join([]string{
			"[project]",
			`name = "putnami-workspace"`,
			`version = "0.0.1"`,
			`description = "Putnami monorepo workspace"`,
			`requires-python = ">=3.11"`,
			"",
			workspaceSection,
			"",
			"[tool.uv]",
			"",
			sourcesSection,
			"",
		}, "\n")
		if err := os.WriteFile(pyprojectPath, []byte(content), 0644); err != nil {
			return false, nil, err
		}
		return true, members, nil
	}

	content := string(data)

	workspacePattern := regexp.MustCompile(`\[tool\.uv\.workspace\][\s\S]*?members\s*=\s*\[[^\]]*\]`)
	updated, membersChanged := replaceOrAppendSection(content, workspacePattern, workspaceSection)

	// The table ends where the next table starts, or at the end of the file.
	// The white space and the "[" of the next header are captured and written
	// back, so a table after [tool.uv.sources], such as a [tool.uv] that holds
	// required-version, keeps its header.
	sourcesPattern := regexp.MustCompile(`\[tool\.uv\.sources\][\s\S]*?(\s*(?:\n\[|$))`)
	updated, sourcesChanged := replaceOrAppendSection(updated, sourcesPattern, sourcesSection)

	changed := membersChanged || sourcesChanged
	if changed {
		if err := os.WriteFile(pyprojectPath, []byte(updated), 0644); err != nil {
			return false, nil, err
		}
	}

	return changed, members, nil
}

// EnsureUVRequiredVersion declares version as the uv release of the workspace,
// as required-version = "==<version>" in the [tool.uv] table of the root
// pyproject.toml, when that file exists and its [tool.uv] table sets no
// required-version yet. The CLI pins the lock's uv toolchain from this
// declaration, and uv itself refuses to run the workspace with another
// release. It reports whether it wrote the file.
//
// The declaration is read as the CLI reads it (uvtoolchain.ReadDeclaration).
// An exact one is never changed. One that names no single release, such as a
// range, is an error: the CLI pins nothing from it. So are uv settings written
// as a dotted key or an inline table, such as uv.managed in the [tool] table:
// a [tool.uv] table added beside them would define the table twice.
//
// The key goes on the line after the [tool.uv] header, or in a [tool.uv] table
// appended to the file when it has none. The rest of the file is kept byte for
// byte, and the new file replaces the old one in one rename.
func EnsureUVRequiredVersion(workspaceRoot, version string) (bool, error) {
	path := filepath.Join(workspaceRoot, "pyproject.toml")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	content := string(data)
	current := uvtoolchain.ReadDeclaration(content)
	switch {
	case current.Present && current.Version != "":
		return false, nil
	case current.Present:
		return false, current.Problem()
	case current.Outside != "":
		return false, fmt.Errorf("it writes uv settings outside a [tool.uv] table header (%s), "+
			"so a [tool.uv] table added for required-version would define the table twice: "+
			"move the uv settings into a [tool.uv] table, with required-version = \"==%s\"", current.Outside, version)
	}
	declaration := `required-version = "==` + version + `"`
	lines := strings.SplitAfter(content, "\n")
	inserted := false
	for i, line := range lines {
		if uvTableHeader(line) {
			newline := "\n"
			if strings.HasSuffix(line, "\r\n") {
				newline = "\r\n"
			}
			if !strings.HasSuffix(line, "\n") {
				lines[i] = line + newline
			}
			lines = append(lines[:i+1], append([]string{declaration + newline}, lines[i+1:]...)...)
			inserted = true
			break
		}
	}
	updated := strings.Join(lines, "")
	if !inserted {
		if updated != "" && !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		updated += "\n[tool.uv]\n" + declaration + "\n"
	}
	if err := writeFileAtomic(path, []byte(updated), info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

// writeFileAtomic replaces path with data, with the permissions perm. The
// bytes go to a temporary file in the same directory, which is then renamed
// over path, so a reader finds the old file or the new one, never a part of
// one. robustio retries the rename while Windows reports the file busy.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".putnami-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	if err := writeAndClose(temp, data, perm); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := robustio.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

// writeAndClose sets the permissions of file, writes data to it, flushes it to
// disk and closes it.
func writeAndClose(file *os.File, data []byte, perm fs.FileMode) error {
	err := file.Chmod(perm)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// uvTableHeader reports whether line opens the [tool.uv] table, with any white
// space around the name parts and an optional trailing comment.
func uvTableHeader(line string) bool {
	header, _, _ := strings.Cut(strings.TrimSpace(line), "#")
	header = strings.TrimSpace(header)
	if strings.HasPrefix(header, "[[") || !strings.HasPrefix(header, "[") || !strings.HasSuffix(header, "]") {
		return false
	}
	return strings.Join(strings.Fields(strings.Trim(header, "[]")), "") == "tool.uv"
}

// DiscoverTestFiles walks the project tree finding test_*.py and *_test.py files.
func DiscoverTestFiles(projectRoot string) ([]string, error) {
	var tests []string
	err := filepath.Walk(projectRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if (strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py")) ||
			strings.HasSuffix(name, "_test.py") {
			tests = append(tests, path)
		}
		return nil
	})
	return tests, err
}

// FileSnapshot creates a map of {relative_path: mtime_ns} for .py and .toml files.
// Used for hot-reload detection in the serve job.
func FileSnapshot(projectRoot string) (map[string]int64, error) {
	snapshot := make(map[string]int64)
	err := filepath.Walk(projectRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".py") && !strings.HasSuffix(name, ".toml") {
			return nil
		}
		rel, err := filepath.Rel(projectRoot, path)
		if err != nil {
			return nil
		}
		snapshot[rel] = info.ModTime().UnixNano()
		return nil
	})
	return snapshot, err
}

// formatTOMLArray formats a list of strings as a TOML inline array.
func formatTOMLArray(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = fmt.Sprintf(`"%s"`, item)
	}
	return strings.Join(parts, ", ")
}

// replaceOrAppendSection replaces a regex match in content, or appends the
// section. When pattern has a capture group, the text it captures is the end of
// the match that does not belong to the section, and is kept after it.
func replaceOrAppendSection(content string, pattern *regexp.Regexp, section string) (string, bool) {
	if pattern.MatchString(content) {
		updated := pattern.ReplaceAllStringFunc(content, func(match string) string {
			if sub := pattern.FindStringSubmatch(match); len(sub) > 1 {
				return section + sub[1]
			}
			return section
		})
		return updated, updated != content
	}

	suffix := ""
	if !strings.HasSuffix(content, "\n") {
		suffix = "\n"
	}
	return content + suffix + "\n" + section + "\n", true
}

// dedupe removes duplicates from a sorted slice.
func dedupe(items []string) []string {
	if len(items) <= 1 {
		return items
	}
	result := []string{items[0]}
	for _, item := range items[1:] {
		if item != result[len(result)-1] {
			result = append(result, item)
		}
	}
	return result
}
