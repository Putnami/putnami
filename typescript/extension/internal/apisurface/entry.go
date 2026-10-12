package apisurface

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// ManifestFilename is the file that declares a package's entry points.
const ManifestFilename = "package.json"

// sourceExtensions are the extensions of the files the reader parses.
var sourceExtensions = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// TargetState says what an entry point's target is to the reader.
type TargetState int

const (
	// TargetRead is a source file the side holds: its exports are read.
	TargetRead TargetState = iota
	// TargetAsset is a file with an extension that is not a source one, such as
	// a stylesheet or a JSON document: the entry point is held, and it has no
	// exports to read.
	TargetAsset
	// TargetMissing is a source path the side does not hold, such as build
	// output that is not committed, or a path without an extension that
	// resolves to no source file: its exports cannot be read.
	TargetMissing
)

// EntryPoint is one entry point a package.json declares.
type EntryPoint struct {
	// Key names the entry point: its subpath, then its conditions in
	// parentheses when they are not the default one, such as ". (browser)".
	Key string
	// Target is the slash path, relative to the project, the entry point names.
	Target string
	// State says whether Target is a source file among the side's files.
	State TargetState
}

// IsSource reports whether a slash path, relative to the project, is a file
// the reader parses: a TypeScript or JavaScript file outside node_modules.
func IsSource(name string) bool {
	if !slices.Contains(sourceExtensions, path.Ext(name)) {
		return false
	}
	return !slices.Contains(strings.Split(name, "/"), "node_modules")
}

// EntryPoints returns the entry points a package.json declares, in key order.
// sources are the slash paths of the side's source files: they tell a target
// the reader holds from one it does not, and they expand a subpath pattern.
//
// "exports" is the authority when the package declares it. Each target of a
// subpath is one entry point, named by the conditions that select it; the
// "default" condition adds nothing to the name. A package without "exports"
// declares "." with "main", ". (module)" with "module", and ". (types)" with
// "types" or "typings".
func EntryPoints(manifest []byte, sources []string) ([]EntryPoint, error) {
	var document struct {
		Exports json.RawMessage `json:"exports"`
		Main    json.RawMessage `json:"main"`
		Module  json.RawMessage `json:"module"`
		Types   json.RawMessage `json:"types"`
		Typings json.RawMessage `json:"typings"`
	}
	if err := json.Unmarshal(manifest, &document); err != nil {
		return nil, fmt.Errorf("does not parse (%w)", err)
	}
	held := make(map[string]bool, len(sources))
	for _, source := range sources {
		held[source] = true
	}
	collector := &entryCollector{held: held, sources: sources, seen: map[string]bool{}}
	if present(document.Exports) {
		if err := collector.exports(document.Exports); err != nil {
			return nil, err
		}
	} else {
		types := document.Types
		if !present(types) {
			types = document.Typings
		}
		for _, field := range []struct {
			raw       json.RawMessage
			condition []string
		}{{document.Main, nil}, {document.Module, []string{"module"}}, {types, []string{"types"}}} {
			var target string
			if present(field.raw) && json.Unmarshal(field.raw, &target) == nil && target != "" {
				collector.add(".", field.condition, target)
			}
		}
	}
	sort.Slice(collector.entries, func(i, j int) bool { return collector.entries[i].Key < collector.entries[j].Key })
	return collector.entries, nil
}

// present reports whether a JSON member is set to something other than null.
func present(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

type entryCollector struct {
	held    map[string]bool
	sources []string
	seen    map[string]bool
	entries []EntryPoint
}

// exports reads the "exports" member: one target, a list of fallbacks, a map
// of subpaths, or a map of conditions for the root subpath.
func (c *entryCollector) exports(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return c.target(".", nil, raw)
	}
	subpaths := false
	for key := range members {
		if strings.HasPrefix(key, ".") {
			subpaths = true
			break
		}
	}
	if !subpaths {
		return c.target(".", nil, raw)
	}
	for _, subpath := range sortedKeys(members) {
		if !strings.HasPrefix(subpath, ".") {
			return fmt.Errorf(`mixes the subpath and condition keys of "exports" (%q)`, subpath)
		}
		if err := c.target(subpath, nil, members[subpath]); err != nil {
			return err
		}
	}
	return nil
}

// target reads what a subpath maps to under conditions: a path, null, a list
// of fallbacks, of which the first that names a path counts, or a map of more
// conditions.
func (c *entryCollector) target(subpath string, conditions []string, raw json.RawMessage) error {
	if !present(raw) {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		c.add(subpath, conditions, value)
		return nil
	}
	var fallbacks []json.RawMessage
	if json.Unmarshal(raw, &fallbacks) == nil {
		for _, fallback := range fallbacks {
			if present(fallback) {
				return c.target(subpath, conditions, fallback)
			}
		}
		return nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return fmt.Errorf(`sets the "exports" target of %q to something that is not a path, a list or a map`, subpath)
	}
	for _, condition := range sortedKeys(nested) {
		inner := conditions
		if condition != "default" {
			inner = append(slices.Clone(conditions), condition)
		}
		if err := c.target(subpath, inner, nested[condition]); err != nil {
			return err
		}
	}
	return nil
}

// add records one entry point. A subpath pattern, with one "*" on each side,
// gives one entry point per source file its target matches.
func (c *entryCollector) add(subpath string, conditions []string, target string) {
	target = path.Clean(strings.TrimPrefix(target, "./"))
	if strings.Count(subpath, "*") == 1 && strings.Count(target, "*") == 1 {
		prefix, suffix, _ := strings.Cut(target, "*")
		for _, source := range c.sources {
			if len(source) > len(prefix)+len(suffix) && strings.HasPrefix(source, prefix) && strings.HasSuffix(source, suffix) {
				matched := source[len(prefix) : len(source)-len(suffix)]
				c.record(strings.Replace(subpath, "*", matched, 1), conditions, source)
			}
		}
		return
	}
	c.record(subpath, conditions, target)
}

func (c *entryCollector) record(subpath string, conditions []string, target string) {
	key := subpath
	if len(conditions) > 0 {
		key += " (" + strings.Join(conditions, ", ") + ")"
	}
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	state := TargetAsset
	switch {
	case c.held[target]:
		state = TargetRead
	case slices.Contains(sourceExtensions, path.Ext(target)):
		state = TargetMissing
	case path.Ext(target) == "":
		// A path without an extension names a module or a directory, never an
		// asset: a target the side does not hold is missing, not skipped.
		state = TargetMissing
		if resolved, ok := c.resolve(target); ok {
			target, state = resolved, TargetRead
		}
	}
	c.entries = append(c.entries, EntryPoint{Key: key, Target: target, State: state})
}

// resolve returns the source file a path without an extension names, the way
// Node and TypeScript resolve it: the path plus a source extension, else the
// index module of the directory.
func (c *entryCollector) resolve(target string) (string, bool) {
	for _, base := range []string{target, path.Join(target, "index")} {
		for _, extension := range sourceExtensions {
			if c.held[base+extension] {
				return base + extension, true
			}
		}
	}
	return "", false
}

func sortedKeys(members map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// PackageName returns the name an entry point has for an importer: the package
// name, then the subpath, then the conditions, such as "@scope/lib/hooks" or
// "@scope/lib (browser)".
func PackageName(name, key string) string {
	subpath, conditions, _ := strings.Cut(key, " ")
	display := name + strings.TrimPrefix(subpath, ".")
	if conditions != "" {
		display += " " + conditions
	}
	return display
}
