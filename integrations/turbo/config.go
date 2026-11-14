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
	"go.putnami.dev/tooling/stacks"
)

// Adoption modes.
const (
	// ModeImport moves a TypeScript package's task to the TypeScript
	// extension and Putnami's cache when its script already runs what that
	// extension's task runs, and delegates every other task.
	ModeImport = "import"
	// ModeDelegate runs every package through Turborepo, which keeps its
	// cache.
	ModeDelegate = "delegate"
)

// Modes are the adoption modes, the default first.
var Modes = []string{ModeImport, ModeDelegate}

// TypeScriptExtension is the extension that runs the tasks import mode moves.
const TypeScriptExtension = "@putnami/typescript"

// modeKey is the member of options["@putnami/turbo"] in the workspace config
// that holds the mode.
const modeKey = "mode"

// WorkspaceMode reads the mode the workspace config at root records, the
// default when it records none.
func WorkspaceMode(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	if errors.Is(err, os.ErrNotExist) {
		return ModeImport, nil
	}
	if err != nil {
		return "", err
	}
	var cfg struct {
		Options map[string]map[string]any `json:"options"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("%s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	value, set := cfg.Options[ExtensionName][modeKey]
	if !set {
		return ModeImport, nil
	}
	mode, _ := value.(string)
	return checkMode(mode)
}

// WorkspaceImport reads options["@putnami/turbo"].import of the workspace
// config at root; nil when it records none.
func WorkspaceImport(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Options map[string]map[string]any `json:"options"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	return stacks.ImportEntries(ExtensionName, cfg.Options[ExtensionName])
}

func checkMode(mode string) (string, error) {
	if !slices.Contains(Modes, mode) {
		return "", fmt.Errorf("options[%q].mode is %q: choose %s", ExtensionName, mode, strings.Join(Modes, " or "))
	}
	return mode, nil
}

// TaskSet is what a turbo.json declares: its tasks by name, such as build,
// web#build or //#format.
type TaskSet map[string]bool

// readTasks reads the tasks of the turbo.json in dir, or of its pipeline for
// Turborepo 1. A missing file declares none.
func readTasks(dir string) (TaskSet, error) {
	data, err := os.ReadFile(filepath.Join(dir, Marker)) //nolint:gosec // a file under the workspace root
	if errors.Is(err, os.ErrNotExist) {
		return TaskSet{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tasks    map[string]json.RawMessage `json:"tasks"`
		Pipeline map[string]json.RawMessage `json:"pipeline"`
	}
	if err := json.Unmarshal(StripJSONC(data), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, Marker), err)
	}
	out := TaskSet{}
	for name := range doc.Tasks {
		out[name] = true
	}
	for name := range doc.Pipeline {
		out[name] = true
	}
	return out, nil
}

// TaskOrder is what each task of a turbo.json depends on, by task name, such
// as build, web#build or //#format: the dependsOn of its definition.
type TaskOrder map[string][]string

// readTaskOrder reads the dependsOn of each task of the turbo.json in dir, or
// of its pipeline for Turborepo 1. A missing file declares none.
func readTaskOrder(dir string) (TaskOrder, error) {
	data, err := os.ReadFile(filepath.Join(dir, Marker)) //nolint:gosec // a file under the workspace root
	if errors.Is(err, os.ErrNotExist) {
		return TaskOrder{}, nil
	}
	if err != nil {
		return nil, err
	}
	type task struct {
		DependsOn []string `json:"dependsOn"`
	}
	var doc struct {
		Tasks    map[string]task `json:"tasks"`
		Pipeline map[string]task `json:"pipeline"`
	}
	if err := json.Unmarshal(StripJSONC(data), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, Marker), err)
	}
	out := TaskOrder{}
	for name, t := range doc.Pipeline {
		out[name] = t.DependsOn
	}
	for name, t := range doc.Tasks {
		out[name] = t.DependsOn
	}
	return out, nil
}

// StripJSONC removes the comments and the trailing commas Turborepo accepts
// in turbo.json, so the rest decodes as JSON.
func StripJSONC(data []byte) []byte {
	var out bytes.Buffer
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out.WriteByte(c)
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
			out.WriteByte(c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out.WriteByte('\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && (data[i] != '*' || data[i+1] != '/') {
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
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.Bytes()
}

// scripts reads the scripts of the package.json in dir.
func scripts(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a member manifest under the workspace root
	if err != nil {
		return nil, err
	}
	var m struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "package.json"), err)
	}
	return m.Scripts, nil
}

// typeScriptSources are the extensions of a TypeScript source file.
var typeScriptSources = []string{".ts", ".tsx", ".mts", ".cts"}

// isTypeScript reports whether the package in dir is TypeScript: it holds a
// tsconfig.json, or a TypeScript source outside node_modules, dist and the
// hidden directories.
func isTypeScript(dir string) bool {
	if info, err := os.Stat(filepath.Join(dir, "tsconfig.json")); err == nil && info.Mode().IsRegular() {
		return true
	}
	found := errors.New("found")
	err := filepath.WalkDir(dir, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != dir && (strings.HasPrefix(entry.Name(), ".") || skippedDirs[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if slices.Contains(typeScriptSources, filepath.Ext(entry.Name())) && !strings.HasSuffix(entry.Name(), ".d.ts") {
			return found
		}
		return nil
	})
	return errors.Is(err, found)
}
