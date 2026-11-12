package features

import (
	"fmt"
	"path"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// specReaderLookup finds the reader of one format. Production passes
// featureproto.SpecReaderFor; a test passes a reader no release ships.
type specReaderLookup func(featureproto.SpecFormat) (featureproto.SpecReader, bool)

// externalSpecs holds what one pass over the manifests' references found.
type externalSpecs struct {
	specs []DiscoveredSpec
	// diagnostics are the findings the selection owns.
	diagnostics []diag.Diagnostic
	// outOfScope names each reference the selection does not own.
	outOfScope map[string]bool
}

// discoverExternalSpecs projects every external spec the committed manifests
// name. A manifest entry is the only way in: a folder of Markdown never creates
// a spec. A workspace with no reference opens no reader, so it pays nothing and
// its outputs do not change.
func discoverExternalSpecs(ws *workspace.Workspace, scope *Scope, manifests []featureproto.ManifestSource,
	openReader func() (Reader, error), lookup specReaderLookup) externalSpecs {
	result := externalSpecs{outOfScope: make(map[string]bool)}
	var reader Reader
	owners := make(map[string]string)
	projects := projectNamesByRoot(ws)
	for _, source := range manifests {
		root := ManifestRoot(source.Path)
		for index, feature := range source.Manifest.Features {
			if feature.Spec == nil {
				continue
			}
			selected := scope.SeedRoot(root) || scope.SelectedFeature(feature.ID)
			field := source.Path + "#" + source.Field(fmt.Sprintf("features[%d].spec", index))
			report := func(findings ...diag.Diagnostic) {
				if selected {
					result.diagnostics = append(result.diagnostics, findings...)
				}
			}
			// Ownership comes first: a feature that loses a path must not mark
			// the winner's spec as out of scope.
			if first := overlappingSpecOwner(owners, feature.Spec.Path); first != "" {
				report(diag.Errorf(featureproto.ErrorCodeDuplicateSpec, field,
					"feature %q names a spec path that overlaps the one of feature %q; a spec details one feature", feature.ID, first))
				continue
			}
			owners[feature.Spec.Path] = feature.ID
			if !selected {
				result.outOfScope[feature.Spec.Path] = true
			}
			specReader, refusal := resolveSpecReader(ws, root, feature.Spec.Format, field, lookup)
			if refusal != nil {
				report(*refusal)
				continue
			}
			if reader == nil {
				opened, err := openReader()
				if err != nil {
					result.diagnostics = append(result.diagnostics, diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "the candidate files are unavailable"))
					return result
				}
				reader = opened
			}
			files, findings := readSpecFiles(reader, *feature.Spec, field)
			report(findings...)
			if diag.HasErrors(findings) {
				continue
			}
			projection := featureproto.ReadSpecSource(specReader, featureproto.SpecInput{
				Feature: feature.ID, Reference: *feature.Spec, Files: files,
			})
			report(projection.Findings...)
			if projection.Spec == nil {
				continue
			}
			report(featureproto.CompareSpecScenarios(projection, feature)...)
			report(enforceOnlyFindings(ws, root, field, projection)...)
			paths := make([]string, 0, len(files))
			for _, file := range files {
				paths = append(paths, file.Path)
			}
			result.specs = append(result.specs, DiscoveredSpec{
				Path:       feature.Spec.Path,
				Root:       root,
				Project:    projects[root],
				Spec:       projection.Spec,
				Format:     feature.Spec.Format,
				Files:      paths,
				Projection: projection,
			})
		}
	}
	return result
}

// overlappingSpecOwner returns the feature whose path equals, contains or lies
// inside candidate, empty when none does. The sorted walk keeps the answer
// stable when several overlap.
func overlappingSpecOwner(owners map[string]string, candidate string) string {
	taken := make([]string, 0, len(owners))
	for owned := range owners {
		taken = append(taken, owned)
	}
	sort.Strings(taken)
	for _, owned := range taken {
		if owned == candidate || strings.HasPrefix(candidate, owned+"/") || strings.HasPrefix(owned, candidate+"/") {
			return owners[owned]
		}
	}
	return ""
}

// resolveSpecReader refuses a format the owning project did not opt into and a
// format no release can read yet. Neither depends on the verification mode:
// either way the gate would pass over prose nobody read.
func resolveSpecReader(ws *workspace.Workspace, root string, format featureproto.SpecFormat, field string,
	lookup specReaderLookup) (featureproto.SpecReader, *diag.Diagnostic) {
	enabled, err := resolveProjectSpecFormats(ws, root)
	if err != nil {
		finding := diag.Errorf(featureproto.ErrorCodeParseError, field+".format", "%v", err)
		return nil, &finding
	}
	opted := false
	for _, candidate := range enabled {
		opted = opted || candidate == format
	}
	if !opted {
		finding := diag.Errorf(featureproto.ErrorCodeSpecFormatNotEnabled, field+".format",
			"spec format %q is not in options.%s.%s of the owning project: add it, or remove the reference; the file was not read",
			format, featureproto.VerificationOptionsBlock, featureproto.SpecFormatsOptionsMember)
		return nil, &finding
	}
	reader, exists := lookup(format)
	if !exists {
		finding := diag.Errorf(featureproto.ErrorCodeSpecFormatUnavailable, field+".format",
			"this release ships no reader for spec format %q", format)
		return nil, &finding
	}
	return reader, nil
}

func projectOptionsAt(ws *workspace.Workspace, root string) (string, map[string]map[string]any) {
	if root == "" {
		return "workspace", nil
	}
	for _, project := range ws.Projects {
		if project == nil || project.Path != root {
			continue
		}
		if project.Config == nil {
			return project.Path, nil
		}
		return project.Path, project.Config.Options
	}
	return root, nil
}

func workspaceOptionsOf(ws *workspace.Workspace) map[string]map[string]any {
	if ws.Config == nil {
		return nil
	}
	return ws.Config.Options
}

func resolveProjectSpecFormats(ws *workspace.Workspace, root string) ([]featureproto.SpecFormat, error) {
	scope, options := projectOptionsAt(ws, root)
	return featureproto.ResolveSpecFormats("workspace", workspaceOptionsOf(ws), scope, options)
}

// enforceOnlyFindings applies the owning project's verification mode to what
// the projection left mode-dependent.
func enforceOnlyFindings(ws *workspace.Workspace, root, field string, projection featureproto.SpecProjection) []diag.Diagnostic {
	scope, options := projectOptionsAt(ws, root)
	mode, _, err := featureproto.ResolveVerificationMode(featureproto.VerificationDomainSpecs, "workspace", workspaceOptionsOf(ws), scope, options)
	if err != nil {
		return []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, field, "%v", err)}
	}
	return projection.EnforceOnly(mode)
}

// readSpecFiles reads the one file a reference names, or the files with the
// format's extensions below its directory.
func readSpecFiles(reader Reader, reference featureproto.SpecReference, field string) ([]featureproto.SpecFile, []diag.Diagnostic) {
	extensions := featureproto.SpecFileExtensions(reference.Format)
	var paths []string
	entries, err := reader.ReadDir("", reference.Path)
	if err != nil {
		if !hasSpecExtension(reference.Path, extensions) {
			return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeInvalidSpecReference, field+".path",
				"spec path is neither a directory nor a %s file", strings.Join(extensions, " or "))}
		}
		paths = []string{reference.Path}
	} else {
		var findings []diag.Diagnostic
		if paths, findings = walkSpecDirectory(reader, reference.Path, entries, extensions, field); len(findings) > 0 {
			return nil, findings
		}
		if len(paths) == 0 {
			return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeSpecRead, field+".path",
				"spec path holds no %s file", strings.Join(extensions, " or "))}
		}
	}
	files := make([]featureproto.SpecFile, 0, len(paths))
	for _, file := range paths {
		data, err := reader.ReadFile("", file)
		if err != nil {
			return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeSpecRead, field+".path", "spec file %s cannot be read", file)}
		}
		files = append(files, featureproto.SpecFile{Path: file, Data: data})
	}
	return files, nil
}

func walkSpecDirectory(reader Reader, directory string, entries []DirEntry, extensions []string, field string) ([]string, []diag.Diagnostic) {
	var paths []string
	pending := []string{directory}
	listed := map[string][]DirEntry{directory: entries}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		children, known := listed[current]
		if !known {
			found, err := reader.ReadDir("", current)
			if err != nil {
				return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeSpecRead, field+".path", "spec directory %s cannot be listed", current)}
			}
			children = found
		}
		for _, entry := range children {
			child := path.Join(current, entry.Name)
			switch {
			case entry.IsDirectory:
				pending = append(pending, child)
			case hasSpecExtension(child, extensions):
				paths = append(paths, child)
			}
		}
		if len(paths) > MaxSpecFiles {
			return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeSpecRead, field+".path", "spec path exceeds the bounded document limit")}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func hasSpecExtension(file string, extensions []string) bool {
	extension := path.Ext(file)
	for _, candidate := range extensions {
		if extension == candidate {
			return true
		}
	}
	return false
}
