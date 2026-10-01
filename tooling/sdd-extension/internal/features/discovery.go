package features

import (
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

type Request struct {
	Workspace *workspace.Workspace
	Reader    Reader
	Revision  Revision
	// Scope narrows evaluation to a selected projection. Nil is the historical
	// whole-workspace meaning; see scope.go for the two-tier contract.
	Scope *Scope
}

type discoveryRoot struct {
	path string
}

type discoveredRepository struct {
	manifests    []featureproto.ManifestSource
	evidence     []featureproto.EvidenceSource
	capabilities []capabilityproto.ManifestContainer
	diagnostics  []diag.Diagnostic
	// selected are the authored identities a scoped run owns, sorted. Empty for
	// an unscoped run, which owns every authored identity.
	selected []string
	// external are the workspace paths OUTSIDE the seed roots that were followed
	// because a selected feature's correctness depends on them, sorted. They are
	// what a scope summary means by "external records followed".
	external []string
	// outOfScope names every artifact this run READ that turned out to belong to
	// no selected feature. Diagnostics attributed to one of those artifacts are
	// dropped: an error wholly outside the selection must not fail a scoped run.
	outOfScope map[string]bool
}

// AuthoredManifestResult is the bounded, manifest-only discovery projection
// used by callers that need durable product intent without evaluating evidence
// or capability artifacts. Manifests contains only strictly parsed and locally
// valid documents; Diagnostics names every exact root artifact that was present
// but could not contribute authority.
type AuthoredManifestResult struct {
	Manifests   []featureproto.ManifestSource
	Diagnostics []diag.Diagnostic
}

// EvaluateWorkspace evaluates the current worktree with the contained OS
// reader. Commands may provide the resolved HEAD as Revision metadata without
// allowing it to influence evidence freshness. A nil scope evaluates the whole
// workspace, which is what every unscoped command asks for.
func EvaluateWorkspace(ws *workspace.Workspace, revision Revision, scope *Scope) Result {
	if ws == nil {
		return Result{Diagnostics: []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "workspace is required")}}
	}
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		return Result{Diagnostics: []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "workspace root is unavailable")}}
	}
	return Aggregate(Request{Workspace: ws, Reader: reader, Revision: revision, Scope: scope})
}

// DiscoverAuthoredManifests reads only putnami.features.json at the workspace
// root and exact project roots. It deliberately does not scan source trees,
// specs, fixtures, or generated directories, and it does not read evidence or
// capability manifests. That keeps a durable declaration as the sole
// non-framework authority for this projection.
func DiscoverAuthoredManifests(ws *workspace.Workspace) AuthoredManifestResult {
	if ws == nil {
		return AuthoredManifestResult{Diagnostics: []diag.Diagnostic{
			diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "workspace is required"),
		}}
	}
	reader, err := NewOSReader(ws.Root)
	if err != nil {
		return AuthoredManifestResult{Diagnostics: []diag.Diagnostic{
			diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "workspace root is unavailable"),
		}}
	}
	request := Request{Workspace: ws, Reader: reader}
	roots, diagnostics := discoverRoots(request)
	manifests, findings := discoverManifests(request, roots, make(map[string]bool))
	diagnostics = append(diagnostics, findings...)

	valid := make([]featureproto.ManifestSource, 0, len(manifests))
	for _, source := range manifests {
		local := featureproto.ValidateManifest(source.Manifest)
		var sourced []diag.Diagnostic
		appendPathDiagnostics(&sourced, source.Path, local)
		diagnostics = append(diagnostics, sourced...)
		if !diag.HasErrors(sourced) {
			valid = append(valid, source)
		}
	}
	return AuthoredManifestResult{
		Manifests:   append([]featureproto.ManifestSource(nil), valid...),
		Diagnostics: normalizeDiagnostics(diagnostics),
	}
}

// discover runs the two tiers scope.go describes: a workspace-wide identity
// read over durable manifests, then a selected evaluation over evidence and
// capability artifacts.
//
// The order matters. Manifests decide which feature identities the selection
// owns, evidence decides which technical contributions those identities reach,
// and only then is a capability manifest outside the seed roots worth opening.
// Nothing here walks a source tree, a generated directory, or a design graph.
func discover(request Request) discoveredRepository {
	result := discoveredRepository{outOfScope: make(map[string]bool)}
	roots, findings := discoverRoots(request)
	result.diagnostics = append(result.diagnostics, findings...)
	if request.Workspace == nil || request.Reader == nil {
		return result
	}
	scope := request.Scope

	seenPaths := make(map[string]bool)
	result.manifests, findings = discoverManifests(request, roots, seenPaths)
	result.diagnostics = append(result.diagnostics, findings...)

	selected := selectedFeatureIdentities(scope, result.manifests)
	result.selected = selected.sorted
	external := newExternalRecords()
	// A manifest outside the seed roots is followed only when it declares one of
	// the selected identities — which is exactly the duplicate declaration whose
	// error must stay visible with its own provenance.
	for _, source := range result.manifests {
		root := featureManifestRoot(source.Path)
		if scope.SeedRoot(root) {
			continue
		}
		if manifestDeclaresSelected(source, selected) {
			external.follow(source.Path)
			continue
		}
		result.outOfScope[source.Path] = true
	}

	evidenceOwners := make(map[string]bool)
	for _, root := range roots {
		result.discoverEvidence(request, root, selected, external, seenPaths, evidenceOwners)
	}
	for _, root := range roots {
		result.discoverCapabilities(request, root, evidenceOwners, external, seenPaths)
	}
	result.external = external.sorted()

	sort.Slice(result.manifests, func(i, j int) bool { return result.manifests[i].Path < result.manifests[j].Path })
	sort.Slice(result.evidence, func(i, j int) bool { return result.evidence[i].Path < result.evidence[j].Path })
	sort.Slice(result.capabilities, func(i, j int) bool { return result.capabilities[i].Path < result.capabilities[j].Path })
	return result
}

// discoverEvidence reads one root's bounded evidence fragments.
//
// Evidence is read at EVERY root, scoped or not, because the project that
// produces a record is not required to be the project that owns the feature:
// dropping a foreign root's evidence would turn a verified requirement into a
// missing-evidence warning, which is a WRONG scoped answer rather than a
// narrower one. What the scope decides is what survives the read — a document
// outside the seed roots is retained only when it carries a record for a
// selected feature, and one that carries none is marked out of scope so its
// findings cannot fail a run it has nothing to do with.
func (result *discoveredRepository) discoverEvidence(
	request Request,
	root discoveryRoot,
	selected selectedFeatures,
	external *externalRecords,
	seenPaths map[string]bool,
	evidenceOwners map[string]bool,
) {
	scope := request.Scope
	seed := scope.SeedRoot(root.path)
	evidenceDir := featureproto.EvidenceDirectory
	directoryPath := joinWorkspacePath(root.path, evidenceDir)
	entries, err := request.Reader.ReadDir(root.path, evidenceDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			result.attribute(seed, directoryPath)
			result.diagnostics = append(result.diagnostics, readDiagnostic(directoryPath, err))
		}
		return
	}
	if len(entries) > MaxEvidenceFiles {
		result.attribute(seed, directoryPath)
		result.diagnostics = append(result.diagnostics, diag.Errorf(featureproto.ErrorCodeSensitiveContent, directoryPath, "evidence directory exceeds the bounded fragment limit"))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for _, entry := range entries {
		if entry.IsDirectory || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		if validateRelativePath(entry.Name, false) != "" || strings.Contains(entry.Name, "/") {
			result.attribute(seed, directoryPath)
			result.diagnostics = append(result.diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidPath, directoryPath, "evidence entry name is not one contained path segment"))
			continue
		}
		relative := path.Join(evidenceDir, entry.Name)
		workspacePath := joinWorkspacePath(root.path, relative)
		if seenPaths[workspacePath] {
			continue
		}
		seenPaths[workspacePath] = true
		var findings []diag.Diagnostic
		data, ok := readDiscoveredFile(request.Reader, root.path, relative, workspacePath, false, &findings)
		if !ok {
			result.attribute(seed, workspacePath)
			result.diagnostics = append(result.diagnostics, findings...)
			continue
		}
		document, parseFindings := featureproto.ParseEvidenceDocument(data)
		appendPathDiagnostics(&findings, workspacePath, parseFindings)
		if document == nil || !seed && !documentCarriesSelected(document, selected) {
			// A document that never mentions a selected feature is outside the
			// selection, and so is a document at a foreign root this run could not
			// parse: nothing here can invalidate a selected feature.
			result.attribute(seed, workspacePath)
			result.diagnostics = append(result.diagnostics, findings...)
			continue
		}
		if !seed {
			external.follow(workspacePath)
		}
		result.diagnostics = append(result.diagnostics, findings...)
		result.evidence = append(result.evidence, featureproto.EvidenceSource{Path: workspacePath, Document: document})
		collectContributionOwners(document, selected, seed, evidenceOwners)
	}
}

// discoverCapabilities reads one root's committed capability manifest. Unlike
// evidence, this artifact is opened only for a seed root or for a root whose
// project OWNS a technical contribution the retained evidence references — the
// exact cross-boundary follow, and nothing wider.
func (result *discoveredRepository) discoverCapabilities(
	request Request,
	root discoveryRoot,
	evidenceOwners map[string]bool,
	external *externalRecords,
	seenPaths map[string]bool,
) {
	scope := request.Scope
	seed := scope.SeedRoot(root.path)
	if !seed && !rootOwnsContribution(request.Workspace, root.path, evidenceOwners) {
		return
	}
	capabilityPath := joinWorkspacePath(root.path, capabilityproto.CommittedPath)
	if seenPaths[capabilityPath] {
		return
	}
	seenPaths[capabilityPath] = true
	data, ok := readDiscoveredFile(request.Reader, root.path, capabilityproto.CommittedPath, capabilityPath, true, &result.diagnostics)
	if !ok {
		return
	}
	if !seed {
		external.follow(capabilityPath)
	}
	document, findings := capabilityproto.ParseManifestDocument(data)
	appendPathDiagnostics(&result.diagnostics, capabilityPath, findings)
	if document == nil {
		return
	}
	if document.V1 != nil {
		appendPathDiagnostics(&result.diagnostics, capabilityPath, capabilityproto.ValidateManifest(document.V1))
	} else {
		appendPathDiagnostics(&result.diagnostics, capabilityPath, capabilityproto.ValidateManifestV2(document.V2))
	}
	result.capabilities = append(result.capabilities, capabilityproto.ManifestContainer{Path: capabilityPath, Manifest: document})
}

// attribute records an artifact as out of scope when it belongs to no seed
// root, so scopeDiagnostics can drop the findings it produced.
func (result *discoveredRepository) attribute(seed bool, workspacePath string) {
	if !seed {
		result.outOfScope[workspacePath] = true
	}
}

// selectedFeatures is the identity tier's answer: which authored identities the
// seed roots declare. An inactive scope selects everything, which is what makes
// an unscoped run byte-identical to the pre-selection behavior.
type selectedFeatures struct {
	active bool
	ids    map[string]bool
	sorted []string
}

func (features selectedFeatures) has(id string) bool {
	return !features.active || features.ids[id]
}

func selectedFeatureIdentities(scope *Scope, manifests []featureproto.ManifestSource) selectedFeatures {
	if !scope.Active() {
		return selectedFeatures{}
	}
	features := selectedFeatures{active: true, ids: make(map[string]bool)}
	for _, source := range manifests {
		if !scope.SeedRoot(featureManifestRoot(source.Path)) {
			continue
		}
		if source.Manifest == nil {
			continue
		}
		for _, feature := range featureproto.CanonicalManifest(source.Manifest).Features {
			if feature.ID != "" {
				features.ids[feature.ID] = true
			}
		}
	}
	// Recorded on the scope so every later tier — specs, decisions, the command
	// surfaces — resolves the same identities this evaluation did.
	scope.SelectFeatures(sortedKeys(features.ids))
	features.sorted = sortedKeys(features.ids)
	return features
}

func manifestDeclaresSelected(source featureproto.ManifestSource, selected selectedFeatures) bool {
	if source.Manifest == nil {
		return false
	}
	for _, feature := range featureproto.CanonicalManifest(source.Manifest).Features {
		if selected.has(feature.ID) {
			return true
		}
	}
	return false
}

func documentCarriesSelected(document *featureproto.EvidenceDocument, selected selectedFeatures) bool {
	for _, record := range featureproto.CanonicalEvidenceDocument(document).Evidence {
		if selected.has(record.Feature) {
			return true
		}
	}
	return false
}

// collectContributionOwners names the projects whose capability manifest a
// retained record forces this run to open. Only records for selected features
// count, so a seed root's evidence for a foreign feature never drags an
// unrelated project's manifest in.
func collectContributionOwners(document *featureproto.EvidenceDocument, selected selectedFeatures, seed bool, owners map[string]bool) {
	for _, record := range featureproto.CanonicalEvidenceDocument(document).Evidence {
		if !selected.has(record.Feature) {
			continue
		}
		if reference := record.Subject.Contribution; reference != nil && reference.OwnerProject != "" {
			owners[reference.OwnerProject] = true
		}
		if record.Source.OwnerProject != "" && seed {
			owners[record.Source.OwnerProject] = true
		}
	}
}

func rootOwnsContribution(ws *workspace.Workspace, root string, owners map[string]bool) bool {
	if ws == nil || len(owners) == 0 {
		return false
	}
	for _, project := range ws.Projects {
		if project == nil || project.Path != root {
			continue
		}
		for _, identity := range []string{project.Name, project.ID, project.SourceName} {
			if identity != "" && owners[identity] {
				return true
			}
		}
	}
	return false
}

// externalRecords accumulates the followed cross-boundary artifacts. It is a
// set so a path followed for two reasons is still one record, and it sorts so
// the reported count and list are the same bytes on every run.
type externalRecords struct {
	paths map[string]bool
}

func newExternalRecords() *externalRecords {
	return &externalRecords{paths: make(map[string]bool)}
}

func (records *externalRecords) follow(workspacePath string) {
	records.paths[workspacePath] = true
}

func (records *externalRecords) sorted() []string {
	return sortedKeys(records.paths)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// featureManifestRoot recovers the discovery root that owns one durable
// feature manifest. This is exact string arithmetic against its canonical
// location rather than a guess about directory layout.
func featureManifestRoot(workspacePath string) string {
	if workspacePath == featureproto.ManifestFilename {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(workspacePath, featureproto.ManifestFilename), "/")
}

func discoverRoots(request Request) ([]discoveryRoot, []diag.Diagnostic) {
	if request.Workspace == nil {
		return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "workspace", "workspace is required")}
	}
	if request.Reader == nil {
		return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "reader", "repository reader is required")}
	}
	roots := []discoveryRoot{{path: ""}}
	seenRoots := map[string]bool{"": true}
	var diagnostics []diag.Diagnostic
	for _, project := range request.Workspace.Projects {
		if project == nil {
			continue
		}
		if code := validateRelativePath(project.Path, true); code != "" {
			diagnostics = append(diagnostics, pathDiagnostic(code, "projects", "project root is not a canonical contained workspace-relative path"))
			continue
		}
		if seenRoots[project.Path] {
			continue
		}
		seenRoots[project.Path] = true
		roots = append(roots, discoveryRoot{path: project.Path})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].path < roots[j].path })
	return roots, diagnostics
}

func discoverManifests(request Request, roots []discoveryRoot, seenPaths map[string]bool) ([]featureproto.ManifestSource, []diag.Diagnostic) {
	var manifests []featureproto.ManifestSource
	var diagnostics []diag.Diagnostic
	for _, root := range roots {
		manifestPath := joinWorkspacePath(root.path, featureproto.ManifestFilename)
		if seenPaths[manifestPath] {
			continue
		}
		seenPaths[manifestPath] = true
		data, ok := readDiscoveredFile(request.Reader, root.path, featureproto.ManifestFilename, manifestPath, true, &diagnostics)
		if !ok {
			continue
		}
		manifest, findings := featureproto.ParseManifest(data)
		appendPathDiagnostics(&diagnostics, manifestPath, findings)
		if manifest != nil {
			manifests = append(manifests, featureproto.ManifestSource{Path: manifestPath, Manifest: manifest})
		}
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Path < manifests[j].Path })
	return manifests, diagnostics
}

func readDiscoveredFile(reader Reader, root, relative, workspacePath string, optional bool, diagnostics *[]diag.Diagnostic) ([]byte, bool) {
	data, err := reader.ReadFile(root, relative)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			return nil, false
		}
		*diagnostics = append(*diagnostics, readDiagnostic(workspacePath, err))
		return nil, false
	}
	if len(data) > MaxReadBytes {
		*diagnostics = append(*diagnostics, diag.Errorf(featureproto.ErrorCodeSensitiveContent, workspacePath, "feature artifact exceeds the bounded read limit"))
		return nil, false
	}
	return data, true
}

func readDiagnostic(workspacePath string, err error) diag.Diagnostic {
	if kind, ok := readerErrorKind(err); ok {
		switch kind {
		case ReaderErrorPathEscape:
			return diag.Errorf(featureproto.ErrorCodePathEscape, workspacePath, "artifact path escapes its declared discovery root")
		case ReaderErrorSymlinkEscape:
			return diag.Errorf(featureproto.ErrorCodeSymlinkEscape, workspacePath, "artifact symlink escapes its declared discovery root")
		case ReaderErrorOutsideWorkspace:
			return diag.Errorf(featureproto.ErrorCodeOutsideDiscoveryRoot, workspacePath, "artifact resolves outside the selected workspace")
		case ReaderErrorReadLimit, ReaderErrorEvidenceFileLimit:
			return diag.Errorf(featureproto.ErrorCodeSensitiveContent, workspacePath, "artifact exceeds a bounded feature-reader limit")
		case ReaderErrorInvalidPath, ReaderErrorUnsupportedFile:
			return diag.Errorf(featureproto.ErrorCodeInvalidPath, workspacePath, "artifact is not one contained regular file")
		}
	}
	return diag.Errorf(featureproto.ErrorCodeParseError, workspacePath, "feature artifact could not be read")
}

func pathDiagnostic(kind ReaderErrorKind, field, message string) diag.Diagnostic {
	code := featureproto.ErrorCodeInvalidPath
	if kind == ReaderErrorPathEscape {
		code = featureproto.ErrorCodePathEscape
	}
	return diag.Errorf(code, field, "%s", message)
}

func appendPathDiagnostics(target *[]diag.Diagnostic, workspacePath string, findings []diag.Diagnostic) {
	for _, finding := range findings {
		if finding.Field == "" {
			finding.Field = workspacePath
		} else {
			finding.Field = workspacePath + "#" + finding.Field
		}
		*target = append(*target, finding)
	}
}

func joinWorkspacePath(root, relative string) string {
	if root == "" {
		return relative
	}
	return path.Join(root, relative)
}
