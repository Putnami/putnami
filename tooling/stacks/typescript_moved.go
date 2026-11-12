package stacks

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// A moved TypeScript task does what the script it replaces did (ADR 0017):
// a test runs no type check its script did not run, and a Bun build moves
// only when it writes the entries its package.json names.

// SetTypeScriptTestTypesOff writes options["@putnami/typescript:test"].types
// false into a putnami.json document when off is set, so the test that moved
// runs no type check its script did not run, and otherwise removes a false
// types, with every object it empties.
func SetTypeScriptTestTypesOff(doc map[string]any, off bool) {
	setFalse(doc, TypeScriptRunnerLayer, "types", off)
}

// setFalse sets options[layer][member] false when set, and otherwise
// removes a false value there and every object it empties. A true value is
// the project's own choice and stays.
func setFalse(doc map[string]any, layer, member string, set bool) {
	if set {
		ensureObject(ensureObject(doc, "options"), layer)[member] = false
		return
	}
	options, _ := doc["options"].(map[string]any)
	values, _ := options[layer].(map[string]any)
	if values == nil || values[member] != false {
		return
	}
	delete(values, member)
	pruneEmpty(options, layer)
	pruneEmpty(doc, "options")
}

// LintDependentsNote is the note an import prints when a lint moves to the
// TypeScript extension: that lint reads the project's own files alone, so
// putnami lint --impacted no longer lints the dependents of a changed
// project, as the tool's lint did.
const LintDependentsNote = "putnami lint --impacted lints the projects a change touches, not their dependents: the lint of " +
	TypeScriptExtension + " reads only the project's own files, where the tool's lint ran on every dependent"

// BunBuildKeptReason answers why a build script that runs bun build keeps
// running through the tool, ending with keeps, or "" when it moves. The Bun
// build writes under Putnami's output, nothing in the project, so it moves
// only when the package.json of the project in dir names no entry outside
// src/, or when the import rewrites that entry (D-066): nothing publishes the
// project (publishedBy is empty), it declares no exports, and it holds
// src/index.ts. Otherwise a dependent that reads the entry finds no file.
func BunBuildKeptReason(root, dir, publishedBy, keeps string) string {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	data, err := os.ReadFile(filepath.Join(abs, "package.json")) //nolint:gosec // a project manifest under the workspace root
	if err != nil {
		return ""
	}
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	entry := builtEntry(manifest)
	if entry == "" {
		return ""
	}
	why := ""
	switch {
	case publishedBy != "":
		why = publishedBy + " publishes it, so that entry stays"
	case manifest["exports"] != nil:
		why = "it declares exports, which the import does not rewrite"
	default:
		after, reason, _ := withSourceExports(abs, data)
		switch {
		case after != nil:
			return ""
		case reason != "":
			why = "the import cannot add its source entry: " + reason
		default:
			why = "it holds no " + strings.TrimPrefix(SourceEntry, "./") + " to name instead"
		}
	}
	return fmt.Sprintf("its package.json names %q, which the Bun build of %s does not write, and %s: %s", entry, TypeScriptExtension, why, keeps)
}

// builtEntry answers the first entry the manifest names outside src/: a
// path its exports name, package.json aside, or, when it declares no
// exports, its main, types or typings. A resolver that reads exports reads
// nothing else, so the main an import leaves beside the exports it adds
// (D-066) names no entry.
func builtEntry(manifest map[string]any) string {
	entries := exportPaths(manifest["exports"])
	if manifest["exports"] == nil {
		for _, key := range entryKeys {
			if entry, _ := manifest[key].(string); entry != "" {
				entries = append(entries, entry)
			}
		}
	}
	for _, entry := range entries {
		clean := path.Clean(entry)
		if clean != "package.json" && !strings.HasPrefix(clean, "src/") {
			return entry
		}
	}
	return ""
}

// exportPaths answers every path an exports value names, in a stable order.
func exportPaths(exports any) []string {
	switch v := exports.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, exportPaths(item)...)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		var out []string
		for _, key := range keys {
			out = append(out, exportPaths(v[key])...)
		}
		return out
	}
	return nil
}
