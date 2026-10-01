package features

import (
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// MaxSpecFiles bounds the direct JSON children one specs/ directory may carry.
// A spec is a reviewed prose artifact, so an unbounded directory is a defect
// rather than a scale requirement.
const MaxSpecFiles = 1024

// DiscoveredSpec is one strictly parsed durable specification together with the
// provenance the protocol deliberately does not carry: the exact
// workspace-relative file it was read from and the workspace or project root
// that owns it. The filename never participates in identity — Spec.Feature does
// — so Path locates the document without naming it.
type DiscoveredSpec struct {
	// Path is the canonical workspace-relative spec path ("app/specs/x.json").
	Path string
	// Root is the owning discovery root; "" is the workspace root itself.
	Root string
	// Project is the owning project's name, empty at the workspace root.
	Project string
	// Spec is the strictly parsed document found at Path.
	Spec *featureproto.Spec
}

// SpecDiscoveryResult carries every readable spec plus the sorted diagnostics
// naming the canonical locations that existed but could not contribute one.
type SpecDiscoveryResult struct {
	// Specs are the strictly parsed specs, sorted by canonical path. A scoped
	// discovery still returns every readable document: uniqueness ("one spec per
	// feature") is a workspace-wide fact, and answering it from a subset would
	// weaken it. Selected reports which of them the selection owns.
	Specs []DiscoveredSpec
	// Selected are the specs inside the selected projection, sorted by path. It
	// equals Specs for an unscoped discovery.
	Selected []DiscoveredSpec
	// External are the selected specs that live outside the seed roots, sorted
	// by path — the cross-boundary documents this run followed.
	External []DiscoveredSpec
	// Diagnostics report unreadable directories and unparsable documents that
	// the selection owns. Findings attributed to a document outside it are
	// dropped, so an unrelated broken spec never fails a scoped run.
	Diagnostics []diag.Diagnostic
	// OutOfScope names every document read but attributed to no selected
	// feature, so a caller filtering protocol findings of its own can apply the
	// same attribution this discovery did.
	OutOfScope map[string]bool
}

// InScope reports whether one discovered document belongs to the selection.
func (result SpecDiscoveryResult) InScope(specPath string) bool {
	return !result.OutOfScope[specPath]
}

// Sources projects the discovery result onto the protocol's repository-level
// validation input. Only the path and document cross that boundary: owning
// project provenance is a workspace fact the wire contract does not model.
func (result SpecDiscoveryResult) Sources() []featureproto.SpecSource {
	sources := make([]featureproto.SpecSource, 0, len(result.Specs))
	for _, discovered := range result.Specs {
		sources = append(sources, featureproto.SpecSource{Path: discovered.Path, Spec: discovered.Spec})
	}
	return sources
}

// DiscoverSpecs reads only the direct JSON children of the canonical specs/
// directory at the workspace root and at exact project roots, exactly as
// features.SpecDirectory defines them. It deliberately does not walk source
// trees, generated directories, or nested folders, and it reads no evidence,
// capability, or design-graph artifact: a spec is a reader of authored intent,
// so discovering one must never depend on build output.
//
// A document that fails strict parsing contributes a diagnostic and no spec, so
// callers never hand a half-parsed document to the protocol validator.
//
// A non-nil scope narrows what the result OWNS, never what it reads: every
// canonical location is still listed, because "one spec per feature" is a
// workspace-wide guarantee and a subset cannot prove it. A document is inside
// the selection when it sits at a seed root or details a selected feature; the
// rest is reported through OutOfScope so its findings fail nobody's scoped run.
func DiscoverSpecs(ws *workspace.Workspace, scope *Scope) SpecDiscoveryResult {
	if ws == nil {
		return SpecDiscoveryResult{Diagnostics: []diag.Diagnostic{
			diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "workspace is required"),
		}}
	}
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		return SpecDiscoveryResult{Diagnostics: []diag.Diagnostic{
			diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "workspace root is unavailable"),
		}}
	}
	request := Request{Workspace: ws, Reader: reader, Scope: scope}
	roots, diagnostics := discoverRoots(request)
	projects := projectNamesByRoot(ws)

	specs := make([]DiscoveredSpec, 0)
	outOfScope := make(map[string]bool)
	seenPaths := make(map[string]bool)
	for _, root := range roots {
		seed := scope.SeedRoot(root.path)
		directoryPath := joinWorkspacePath(root.path, featureproto.SpecDirectory)
		entries, err := reader.ReadDir(root.path, featureproto.SpecDirectory)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				markOutOfScopeSpec(outOfScope, seed, directoryPath)
				diagnostics = append(diagnostics, readDiagnostic(directoryPath, err))
			}
			continue
		}
		if len(entries) > MaxSpecFiles {
			markOutOfScopeSpec(outOfScope, seed, directoryPath)
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeSensitiveContent, directoryPath,
				"spec directory exceeds the bounded document limit"))
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		for _, entry := range entries {
			if entry.IsDirectory || !strings.HasSuffix(entry.Name, ".json") {
				continue
			}
			if validateRelativePath(entry.Name, false) != "" || strings.Contains(entry.Name, "/") {
				markOutOfScopeSpec(outOfScope, seed, directoryPath)
				diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidPath, directoryPath,
					"spec entry name is not one contained path segment"))
				continue
			}
			relative := path.Join(featureproto.SpecDirectory, entry.Name)
			workspacePath := joinWorkspacePath(root.path, relative)
			if seenPaths[workspacePath] {
				continue
			}
			seenPaths[workspacePath] = true
			data, ok := readDiscoveredFile(reader, root.path, relative, workspacePath, false, &diagnostics)
			if !ok {
				markOutOfScopeSpec(outOfScope, seed, workspacePath)
				continue
			}
			spec, findings := featureproto.ParseSpec(data)
			appendPathDiagnostics(&diagnostics, workspacePath, findings)
			if spec == nil {
				markOutOfScopeSpec(outOfScope, seed, workspacePath)
				continue
			}
			if !seed && !scope.SelectedFeature(spec.Feature) {
				outOfScope[workspacePath] = true
			}
			specs = append(specs, DiscoveredSpec{
				Path:    workspacePath,
				Root:    root.path,
				Project: projects[root.path],
				Spec:    spec,
			})
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Path < specs[j].Path })

	result := SpecDiscoveryResult{Specs: specs, OutOfScope: outOfScope}
	for _, discovered := range specs {
		if outOfScope[discovered.Path] {
			continue
		}
		result.Selected = append(result.Selected, discovered)
		if !scope.SeedRoot(discovered.Root) {
			result.External = append(result.External, discovered)
		}
	}
	if result.Selected == nil {
		result.Selected = make([]DiscoveredSpec, 0)
	}
	if result.External == nil {
		result.External = make([]DiscoveredSpec, 0)
	}
	kept := make([]diag.Diagnostic, 0, len(diagnostics))
	for _, finding := range diagnostics {
		artifact, _, _ := strings.Cut(finding.Field, "#")
		if outOfScope[artifact] {
			continue
		}
		kept = append(kept, finding)
	}
	result.Diagnostics = normalizeDiagnostics(kept)
	return result
}

// markOutOfScopeSpec attributes a location a scoped run could not classify.
// Only a location outside the seed roots is dropped: a broken document at a
// seed root is exactly the failure the caller asked about.
func markOutOfScopeSpec(outOfScope map[string]bool, seed bool, workspacePath string) {
	if !seed {
		outOfScope[workspacePath] = true
	}
}

// ResolveDecisionRecords reports, for each workspace-relative decision link,
// whether a contained regular record exists in the current worktree.
//
// The wire contract deliberately stops at "the link is well-formed and inside
// the workspace" (ADR 0001 §Consequences): a moved or deleted record is a
// filesystem fact only a tree reader can see. Resolution goes through the same
// contained reader discovery uses, so a symlinked escape resolves to "absent"
// rather than reaching outside the workspace. Directory listings are read once
// per directory, so a spec linking many records in one doc/adr costs one read.
func ResolveDecisionRecords(ws *workspace.Workspace, links []string) map[string]bool {
	resolved := make(map[string]bool, len(links))
	if ws == nil || len(links) == 0 {
		return resolved
	}
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		return resolved
	}
	listings := make(map[string]map[string]bool)
	for _, link := range links {
		if _, seen := resolved[link]; seen {
			continue
		}
		resolved[link] = false
		if validateRelativePath(link, false) != "" {
			continue
		}
		directory, name := path.Split(link)
		directory = strings.TrimSuffix(directory, "/")
		if directory == "" || name == "" {
			continue
		}
		entries, listed := listings[directory]
		if !listed {
			entries = make(map[string]bool)
			if found, err := reader.ReadDir("", directory); err == nil {
				for _, entry := range found {
					if !entry.IsDirectory {
						entries[entry.Name] = true
					}
				}
			}
			listings[directory] = entries
		}
		if !entries[name] {
			continue
		}
		// A directory listing only proves that a leaf with this name exists.
		// Read it through the contained reader before reporting the record as
		// present so an escaping or broken leaf symlink cannot satisfy the link.
		if _, err := reader.ReadFile("", link); err == nil {
			resolved[link] = true
		}
	}
	return resolved
}

// projectNamesByRoot maps every exact project root onto the project name that
// owns it, so a spec keeps its owning project as provenance without discovery
// having to re-resolve paths later.
func projectNamesByRoot(ws *workspace.Workspace) map[string]string {
	names := make(map[string]string)
	for _, project := range ws.Projects {
		if project == nil || project.Path == "" {
			continue
		}
		name := project.Name
		if name == "" {
			name = project.ID
		}
		names[project.Path] = name
	}
	return names
}
