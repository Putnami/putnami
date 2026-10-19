package features

import (
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
	extensionproto "go.putnami.dev/protocol/extension"
	featureproto "go.putnami.dev/protocol/features"
)

// servedSurface is what one project's own extension manifest lets a user or an
// agent reach: the command paths typed after `putnami` and the MCP tool names
// the tool listing advertises.
type servedSurface struct {
	commands map[string]bool
	tools    map[string]bool
}

// surfaceManifest holds the members of an extension manifest that name what
// it serves. It decodes leniently: a field a newer extension protocol adds must
// not make a feature's claims unresolvable, and the CLI loader already
// validates the whole manifest strictly.
type surfaceManifest struct {
	Commands map[string]struct {
		Visibility string `json:"visibility"`
	} `json:"commands"`
	CommandGroups map[string]surfaceGroup `json:"commandGroups"`
	Verbs         []struct {
		Name string `json:"name"`
	} `json:"verbs"`
	Tools map[string]json.RawMessage `json:"tools"`
}

// surfaceGroup is a command group or a subcommand, reduced to its children.
type surfaceGroup struct {
	Subcommands map[string]surfaceGroup `json:"subcommands"`
}

// servedSurfaceOf derives the served set from an extension manifest:
//
//   - a command key, except one whose visibility is internal, because the
//     flat key behind a command group (sdd-features-validate) is a wire key
//     nobody types;
//   - the path of every subcommand of a command group, intermediate paths
//     included (`cloud config`, `cloud config list`);
//   - the name of every verb of the vocabulary;
//   - the key of every tool.
func servedSurfaceOf(manifest surfaceManifest) servedSurface {
	served := servedSurface{commands: make(map[string]bool), tools: make(map[string]bool)}
	for name, command := range manifest.Commands {
		if command.Visibility != "internal" {
			served.commands[name] = true
		}
	}
	for group, definition := range manifest.CommandGroups {
		addSubcommandPaths(served.commands, group, definition.Subcommands)
	}
	for _, verb := range manifest.Verbs {
		served.commands[verb.Name] = true
	}
	for name := range manifest.Tools {
		served.tools[name] = true
	}
	return served
}

func addSubcommandPaths(into map[string]bool, prefix string, subcommands map[string]surfaceGroup) {
	for name, subcommand := range subcommands {
		path := prefix + " " + name
		into[path] = true
		addSubcommandPaths(into, path, subcommand.Subcommands)
	}
}

// resolveFeatureSurfaces checks every command path and tool name a feature
// lists against the extension manifest that sits at the same project root as
// the declaring putnami.features.json, and nothing else. A root without an
// extension manifest is not resolved: it serves no extension surface this
// check can read. No other extension, lock or installed state is consulted, so
// the verdict is a function of the worktree and holds in every installation.
//
// The extension manifest is read through the evaluation's own reader, so a
// cached validation step reads it from the same candidate cut that keys it.
func resolveFeatureSurfaces(reader Reader, manifests []featureproto.ManifestSource) []diag.Diagnostic {
	if reader == nil {
		return nil
	}
	var diagnostics []diag.Diagnostic
	for _, source := range manifests {
		if source.Manifest == nil || !declaresSurface(source.Manifest) {
			continue
		}
		root := featureManifestRoot(source.Path)
		manifestPath := joinWorkspacePath(root, extensionproto.ManifestFilename)
		data, err := reader.ReadFile(root, extensionproto.ManifestFilename)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeUnresolvedSurface, source.Path,
				"a feature lists a surface, but %s could not be read", manifestPath))
			continue
		}
		var manifest surfaceManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeUnresolvedSurface, source.Path,
				"a feature lists a surface, but %s could not be parsed: %v", manifestPath, err))
			continue
		}
		served := servedSurfaceOf(manifest)
		for index, feature := range source.Manifest.Features {
			if feature.Surface == nil {
				continue
			}
			field := source.Path + "#" + "features[" + strconv.Itoa(index) + "]" + ".surface"
			diagnostics = append(diagnostics, unservedNames(field+".commands", "command", feature.Surface.Commands, served.commands, manifestPath)...)
			diagnostics = append(diagnostics, unservedNames(field+".tools", "tool", feature.Surface.Tools, served.tools, manifestPath)...)
		}
	}
	sort.SliceStable(diagnostics, func(i, j int) bool { return diagnostics[i].Field < diagnostics[j].Field })
	return diagnostics
}

func declaresSurface(manifest *featureproto.Manifest) bool {
	for _, feature := range manifest.Features {
		if feature.Surface != nil {
			return true
		}
	}
	return false
}

func unservedNames(field, noun string, names []string, served map[string]bool, manifestPath string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	for index, name := range names {
		if served[name] {
			continue
		}
		diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeUnresolvedSurface,
			field+"["+strconv.Itoa(index)+"]", "%s %q is not served by %s", noun, name, manifestPath))
	}
	return diagnostics
}
