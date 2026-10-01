package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ParseProbeRequest strict-decodes a probe request. Unknown fields are
// rejected: a provider that silently ignores a member core added would answer a
// question it did not understand.
func ParseProbeRequest(data []byte) (*ProbeRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var r ProbeRequest
	if err := dec.Decode(&r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse probe request: %v", err),
		}
	}
	return &r, nil
}

// ValidateProbeRequest checks the request's structural invariants.
func ValidateProbeRequest(r *ProbeRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf("nil-request", "", "probe request is nil")}
	}
	var diags []diag.Diagnostic
	if r.Version != ProbeProtocolVersion {
		diags = append(diags, diag.Errorf("invalid-probe-version", "version",
			"probe request version %d is not supported (want %d)", r.Version, ProbeProtocolVersion))
	}
	if strings.TrimSpace(r.Extension) == "" {
		diags = append(diags, diag.Errorf("required-field", "extension", "probe request must name the target extension"))
	}
	if r.Reason != "" && r.Reason != ProbeReasonLoad && r.Reason != ProbeReasonRefresh && r.Reason != ProbeReasonPlan {
		diags = append(diags, diag.Errorf("invalid-probe-reason", "reason", "unknown probe reason %q", r.Reason))
	}
	diags = append(diags, validateProbePathList("paths", r.Paths)...)
	diags = append(diags, validateProbePathList("files", r.Files)...)
	return diags
}

// ParseAndValidateProbeRequest combines strict parsing and validation.
func ParseAndValidateProbeRequest(data []byte) (*ProbeRequest, []diag.Diagnostic) {
	r, diags := ParseProbeRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateProbeRequest(r)...)
}

// ParseProbeResult strict-decodes a probe result.
func ParseProbeResult(data []byte) (*ProbeResult, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var r ProbeResult
	if err := dec.Decode(&r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse probe result: %v", err),
		}
	}
	return &r, nil
}

// ValidateProbeResult checks a result's structural invariants. Everything it
// rejects is something that would otherwise poison a digest or a merge:
// a version core cannot interpret, an unattributable result, a path that
// depends on the checkout location, a project reported twice by one provider,
// or metadata that is not a JSON object.
func ValidateProbeResult(r *ProbeResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf("nil-result", "", "probe result is nil")}
	}

	var diags []diag.Diagnostic
	if r.Version != ProbeProtocolVersion {
		diags = append(diags, diag.Errorf("invalid-probe-version", "version",
			"probe result version %d is not supported (want %d)", r.Version, ProbeProtocolVersion))
	}
	if strings.TrimSpace(r.Extension) == "" {
		diags = append(diags, diag.Errorf("required-field", "extension",
			"probe result must name the answering extension; it keys the provider-owned metadata bucket"))
	}
	diags = append(diags, validateProbePathList("watchedFiles", r.WatchedFiles)...)

	seen := make(map[string]bool, len(r.Projects))
	for i := range r.Projects {
		project := &r.Projects[i]
		field := fmt.Sprintf("projects[%d]", i)

		if strings.TrimSpace(project.Path) == "" {
			diags = append(diags, diag.Errorf("required-field", field+".path",
				"probe project must carry a repo-relative path; core joins providers by path"))
			continue
		}
		cleaned, ok := NormalizeProbePath(project.Path)
		if !ok {
			diags = append(diags, diag.Errorf("invalid-path", field+".path",
				"probe project path %q must be repo-relative, slash-separated, and inside the workspace", project.Path))
			continue
		}
		if seen[cleaned] {
			diags = append(diags, diag.Errorf("duplicate-project", field+".path",
				"probe result reports project path %q more than once", cleaned))
			continue
		}
		seen[cleaned] = true

		if project.SourceFile != "" {
			if _, ok := NormalizeProbePath(project.SourceFile); !ok {
				diags = append(diags, diag.Errorf("invalid-path", field+".sourceFile",
					"probe source file %q must be repo-relative and inside the workspace", project.SourceFile))
			}
		}
		diags = append(diags, validateProbePathList(field+".dependencies", project.Dependencies)...)
		diags = append(diags, validateDependencySources(field+".dependencySources", project)...)
		diags = append(diags, validateProbePathList(field+".watchedFiles", project.WatchedFiles)...)

		if len(bytes.TrimSpace(project.Metadata)) > 0 {
			if _, ok := CanonicalMetadata(project.Metadata); !ok {
				diags = append(diags, diag.Errorf("invalid-metadata", field+".metadata",
					"provider metadata must be a JSON object so it can be carried under project.metadata[%q]", r.Extension))
			}
		}
	}

	return diags
}

// ParseAndValidateProbeResult combines strict parsing and validation.
func ParseAndValidateProbeResult(data []byte) (*ProbeResult, []diag.Diagnostic) {
	r, diags := ParseProbeResult(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateProbeResult(r)...)
}

// validateDependencySources checks one project's edge provenance: every key
// names a dependency the same answer reports, and every value is a source a
// provider may claim.
//
// Both are errors rather than silent drops. A provenance for an edge the answer
// does not carry means the provider computed the two from different sets, and a
// consumer acting on real imports would then see a boundary the graph does not
// have; a source outside the closed set — including the core-derived contract
// source — means the provider is claiming a derivation this protocol does not
// define. Diagnostics are emitted in sorted key order so one answer produces
// one report.
func validateDependencySources(field string, project *ProbeProject) []diag.Diagnostic {
	if len(project.DependencySources) == 0 {
		return nil
	}
	reported := make(map[string]bool, len(project.Dependencies))
	for _, dependency := range project.Dependencies {
		if cleaned, ok := NormalizeProbePath(dependency); ok {
			reported[cleaned] = true
		}
	}
	keys := make([]string, 0, len(project.DependencySources))
	for key := range project.DependencySources {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var diags []diag.Diagnostic
	for _, key := range keys {
		source := project.DependencySources[key]
		cleaned, ok := NormalizeProbePath(key)
		if !ok {
			diags = append(diags, diag.Errorf("invalid-path", fmt.Sprintf("%s[%q]", field, key),
				"dependency source path %q must be repo-relative, slash-separated, and inside the workspace", key))
			continue
		}
		if !reported[cleaned] {
			diags = append(diags, diag.Errorf("unknown-dependency", fmt.Sprintf("%s[%q]", field, key),
				"dependency source names %q, which this answer does not report as a dependency", cleaned))
		}
		if !source.ReportableByProvider() {
			diags = append(diags, diag.Errorf("invalid-dependency-source", fmt.Sprintf("%s[%q]", field, key),
				"dependency source %q is not one a provider may report (want one of %s)",
				source, providerReportableSources()))
		}
	}
	return diags
}

// providerReportableSources renders the closed set a provider may claim, for
// the diagnostic above.
func providerReportableSources() string {
	names := make([]string, 0, len(ValidDependencySources))
	for _, source := range ValidDependencySources {
		if source.ReportableByProvider() {
			names = append(names, string(source))
		}
	}
	return strings.Join(names, ", ")
}

func validateProbePathList(field string, paths []string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, p := range paths {
		if strings.TrimSpace(p) == "" {
			diags = append(diags, diag.Errorf("required-field", fmt.Sprintf("%s[%d]", field, i),
				"probe path entries must not be empty"))
			continue
		}
		if _, ok := NormalizeProbePath(p); !ok {
			diags = append(diags, diag.Errorf("invalid-path", fmt.Sprintf("%s[%d]", field, i),
				"probe path %q must be repo-relative, slash-separated, and inside the workspace", p))
		}
	}
	return diags
}

// --- merge ------------------------------------------------------------------

// ExplicitProject is the project's own explicitly authored identity — what
// putnami.json (or an equivalently explicit core-owned source) declared. It is
// the tiebreaker in every merge rule below: a conflict between two providers is
// a hard error UNLESS the author already said which value wins.
type ExplicitProject struct {
	Name         string
	Type         string
	Tags         []string
	Dependencies []string
	Publish      []string
	RunsWith     []string
	Extensions   []string
}

// ExplicitFromProjectConfig projects a parsed putnami.json onto the merge
// input. A nil config yields the zero value ("nothing was declared").
func ExplicitFromProjectConfig(c *ProjectConfig) ExplicitProject {
	if c == nil {
		return ExplicitProject{}
	}
	return ExplicitProject{
		Name:         c.Name,
		Type:         c.Type,
		Tags:         c.Tags,
		Dependencies: c.Dependencies,
		Publish:      c.Publish,
		RunsWith:     c.RunsWith,
		Extensions:   c.Extensions,
	}
}

// MergedProject is the merged, core-owned view of one project directory. Its
// Path is canonical and assigned by core; nothing a provider says can move it.
type MergedProject struct {
	Path         string   `json:"path"`
	SourceName   string   `json:"sourceName,omitempty"`
	SourceFile   string   `json:"sourceFile,omitempty"`
	Version      string   `json:"version,omitempty"`
	Type         string   `json:"type,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	// DependencySources is the union of the providers' edge provenance, keyed
	// by the same paths Dependencies carries. An edge only the authored
	// putnami.json declares is absent from it, which is what
	// DependencySourceDeclared means for a consumer.
	DependencySources map[string]DependencySource `json:"dependencySources,omitempty"`
	Publish           []string                    `json:"publish,omitempty"`
	RunsWith          []string                    `json:"runsWith,omitempty"`
	Extensions        []string                    `json:"extensions,omitempty"`
	WatchedFiles      []string                    `json:"watchedFiles,omitempty"`
	Metadata          map[string]json.RawMessage  `json:"metadata,omitempty"`
}

// MergeProbeResults folds every provider's answer, plus the explicitly authored
// config, into one view per project path. The v1 merge rules, in force order:
//
//  1. Core alone assigns canonical paths. A provider's path is normalized and
//     used as the join key; providers cannot rename or relocate a project.
//  2. Explicit putnami.json overrides probe, for every scalar and as the
//     resolver of provider conflicts.
//  3. Conflicting non-empty scalars from two providers are a HARD ERROR unless
//     rule 2 already decided the field. Silently picking one provider is how a
//     workspace starts building differently depending on extension load order.
//  4. Explicit and probe list members are UNIONED and normalized (sorted,
//     deduped) — never replaced. Dependencies are unioned as paths.
//  5. Provider-owned metadata lands under metadata[<extension>], so two
//     providers can never collide.
//
// Results are digested per provider before merging is attempted, so an invalid
// result never reaches here: callers validate first. Diagnostics returned here
// are merge conflicts (rule 3) and carry field-level attribution.
func MergeProbeResults(results []ProbeResult, explicit map[string]ExplicitProject) (map[string]MergedProject, []diag.Diagnostic) {
	// Normalize private copies so the caller's values are never mutated and the
	// merge sees canonical paths, sorted lists, and canonical metadata.
	normalized := make([]ProbeResult, len(results))
	for i, r := range results {
		copied := r
		copied.Projects = append([]ProbeProject(nil), r.Projects...)
		NormalizeProbeResult(&copied)
		normalized[i] = copied
	}
	// Provider order must not affect the outcome, including the order
	// conflicts are reported in.
	sort.SliceStable(normalized, func(i, j int) bool { return normalized[i].Extension < normalized[j].Extension })

	merged := make(map[string]MergedProject)
	// scalars[path][field] collects each provider's non-empty value so rule 3
	// can tell "two providers agree" from "two providers disagree".
	scalars := make(map[string]map[string][]scalarClaim)

	for _, result := range normalized {
		for _, project := range result.Projects {
			view, ok := merged[project.Path]
			if !ok {
				view = MergedProject{Path: project.Path}
			}
			view.Tags = normalizeTokens(append(view.Tags, project.Tags...))
			view.Dependencies = normalizeProbePaths(append(view.Dependencies, project.Dependencies...))
			view.DependencySources = mergeDependencySources(view.DependencySources, project.DependencySources)
			view.Publish = normalizeTokens(append(view.Publish, project.Publish...))
			view.RunsWith = normalizeTokens(append(view.RunsWith, project.RunsWith...))
			view.Extensions = normalizeTokens(append(view.Extensions, project.Extensions...))
			view.WatchedFiles = normalizeProbePaths(append(view.WatchedFiles, project.WatchedFiles...))
			if len(project.Metadata) > 0 {
				if view.Metadata == nil {
					view.Metadata = make(map[string]json.RawMessage, 1)
				}
				view.Metadata[result.Extension] = project.Metadata
			}
			merged[project.Path] = view

			if scalars[project.Path] == nil {
				scalars[project.Path] = make(map[string][]scalarClaim, 4)
			}
			claimScalar(scalars[project.Path], "sourceName", result.Extension, project.SourceName)
			claimScalar(scalars[project.Path], "sourceFile", result.Extension, project.SourceFile)
			claimScalar(scalars[project.Path], "version", result.Extension, project.Version)
			claimScalar(scalars[project.Path], "type", result.Extension, project.Type)
		}
	}

	// Fold the explicit config in and resolve scalars. Paths are walked in
	// sorted order so conflict diagnostics are emitted deterministically.
	paths := make([]string, 0, len(merged))
	for p := range merged {
		paths = append(paths, p)
	}
	for p := range explicit {
		if _, ok := merged[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	diags := make([]diag.Diagnostic, 0, len(paths))
	for _, p := range paths {
		view, ok := merged[p]
		if !ok {
			view = MergedProject{Path: p}
		}
		declared := explicit[p]

		view.Tags = normalizeTokens(append(view.Tags, declared.Tags...))
		view.Dependencies = normalizeProbePaths(append(view.Dependencies, declared.Dependencies...))
		view.Publish = normalizeTokens(append(view.Publish, declared.Publish...))
		view.RunsWith = normalizeTokens(append(view.RunsWith, declared.RunsWith...))
		view.Extensions = normalizeTokens(append(view.Extensions, declared.Extensions...))

		var fieldDiags []diag.Diagnostic
		view.SourceName, fieldDiags = resolveScalar(p, "sourceName", declared.Name, scalars[p])
		diags = append(diags, fieldDiags...)
		view.SourceFile = resolveProvenance(scalars[p], view.SourceName)
		view.Version, fieldDiags = resolveScalar(p, "version", "", scalars[p])
		diags = append(diags, fieldDiags...)
		view.Type, fieldDiags = resolveScalar(p, "type", declared.Type, scalars[p])
		diags = append(diags, fieldDiags...)

		merged[p] = view
	}

	return merged, diags
}

// mergeDependencySources unions two providers' edge provenance.
//
// It is rule 4 (list members are unioned, never replaced) applied to a map: two
// providers describing one project both tell the truth about their own
// manifest, so a Go service that also carries a package.json contributes both
// families. One edge claimed twice resolves through StrongerDependencySource,
// which is order-independent — the merge sorts providers by name, and the
// answer must not depend on that order either.
func mergeDependencySources(into, from map[string]DependencySource) map[string]DependencySource {
	if len(from) == 0 {
		return into
	}
	if into == nil {
		into = make(map[string]DependencySource, len(from))
	}
	for path, source := range from {
		into[path] = StrongerDependencySource(into[path], source)
	}
	return into
}

// resolveProvenance resolves `sourceFile`, which is PROVENANCE rather than
// identity: it records which manifest the winning source name was read from.
//
// It is deliberately NOT run through resolveScalar. Two providers reporting two
// different source FILES for one directory is an ordinary, correct state the
// moment a second language provider ships — a Go service that also carries a
// package.json for its front-end tooling has both a go.mod and a package.json,
// and both providers are telling the truth about their own manifest. Treating
// that as a hard conflict would fail the whole workspace load with a diagnostic
// whose advertised escape hatch ("set it explicitly in putnami.json") does not
// exist for this field, since nothing authors a sourceFile.
//
// So provenance FOLLOWS the resolved name: whichever provider's sourceName won
// contributes the file it read that name from. When the name was decided by
// explicit configuration (or no provider claimed one), the lexicographically
// smallest claim wins — arbitrary, but deterministic, which is the property
// that actually matters because this value reaches the workspace snapshot.
func resolveProvenance(claims map[string][]scalarClaim, resolvedName string) string {
	entries := claims["sourceFile"]
	if len(entries) == 0 {
		return ""
	}

	files := make([]string, 0, len(entries))
	byExtension := make(map[string]string, len(entries))
	for _, entry := range entries {
		files = append(files, entry.value)
		if _, seen := byExtension[entry.extension]; !seen {
			byExtension[entry.extension] = entry.value
		}
	}
	sort.Strings(files)

	if resolvedName != "" {
		owners := make([]string, 0, 1)
		for _, entry := range claims["sourceName"] {
			if entry.value == resolvedName {
				owners = append(owners, entry.extension)
			}
		}
		sort.Strings(owners)
		for _, owner := range owners {
			if file, ok := byExtension[owner]; ok {
				return file
			}
		}
	}
	return files[0]
}

// scalarClaim is one provider's non-empty value for one scalar field.
type scalarClaim struct {
	extension string
	value     string
}

func claimScalar(into map[string][]scalarClaim, field, extension, value string) {
	if value == "" {
		return
	}
	into[field] = append(into[field], scalarClaim{extension: extension, value: value})
}

// resolveScalar applies merge rules 2 and 3 to one field. An explicit value
// short-circuits: the author already decided, so provider disagreement is not
// an error. Otherwise a single distinct value wins and two distinct values are
// a hard error naming both claimants.
func resolveScalar(projectPath, field, explicitValue string, claims map[string][]scalarClaim) (string, []diag.Diagnostic) {
	explicitValue = strings.TrimSpace(explicitValue)
	if explicitValue != "" {
		return explicitValue, nil
	}
	entries := claims[field]
	if len(entries) == 0 {
		return "", nil
	}

	distinct := make([]string, 0, 2)
	byValue := make(map[string][]string, 2)
	for _, entry := range entries {
		if _, seen := byValue[entry.value]; !seen {
			distinct = append(distinct, entry.value)
		}
		byValue[entry.value] = append(byValue[entry.value], entry.extension)
	}
	if len(distinct) == 1 {
		return distinct[0], nil
	}

	sort.Strings(distinct)
	claimants := make([]string, 0, len(distinct))
	for _, value := range distinct {
		providers := byValue[value]
		sort.Strings(providers)
		claimants = append(claimants, fmt.Sprintf("%q (%s)", value, strings.Join(providers, ", ")))
	}
	return "", []diag.Diagnostic{diag.Errorf("probe-conflict", projectPath+"."+field,
		"providers disagree on %s: %s — set it explicitly in putnami.json to resolve",
		field, strings.Join(claimants, " vs "))}
}

// ResolveProjectName applies the v1 identity precedence for a project's name:
//
//	explicit putnami.json name > probe source identity > scope namePattern > directory basename
//
// The probe-over-namePattern rule is the one that is easy to get backwards: a
// self-identifying nested module (a generated cross-language client, a Go
// module inside a TypeScript scope) must keep the build-addressable name its
// own manifest declares, or the scope convention silently renames it and every
// dependency edge pointing at it breaks.
func ResolveProjectName(explicitName, probeSourceName, scopePatternName, basename string) string {
	for _, candidate := range []string{explicitName, probeSourceName, scopePatternName, basename} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// FillScopeTags applies the "scope tags fill but never overwrite" rule: a
// project that carries any tag of its own keeps exactly those, and a project
// with none inherits the scope's. Scope tags are NOT unioned into a non-empty
// set, because a scope-wide tag silently added to a project that curated its
// own tags changes which jobs that project runs.
func FillScopeTags(projectTags, scopeTags []string) []string {
	if normalized := normalizeTokens(projectTags); len(normalized) > 0 {
		return normalized
	}
	return normalizeTokens(scopeTags)
}
