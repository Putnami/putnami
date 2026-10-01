// Workspace probe protocol v1. The reasoning is recorded in
// doc/adr/0001-core-owns-identity-providers-own-language.md.
//
// The probe is how a provider (an extension) tells core what it knows about a
// directory: which project lives there, what identity its own manifest gives
// it, which other projects it depends on, and any provider-owned metadata core
// must carry but never interpret. Core keeps everything that is identity —
// canonical paths, canonical IDs, the merge rules, the digest — and providers
// keep everything that is language knowledge.
//
// Three properties are load-bearing and every helper here exists to hold one of
// them:
//
//   - KEY ON PATHS. A probe result addresses projects by their repo-relative
//     directory path, never by a name and never by a module path. The accepted
//     contract is explicit that a project root is NOT
//     assumed to be a Go module root (or a package.json root, or any other
//     provider's unit): a provider may report a SourceFile nested arbitrarily
//     deep below the project directory, and dependency edges are project PATHS
//     that core resolves, not module identifiers the provider resolved.
//
//   - DETERMINISTIC DIGEST. NormalizeProbeResult produces one canonical form
//     for any authoring order, and ProbeResultDigest hashes that form. Nothing
//     that varies between two runs over the same tree may enter it: no
//     timestamps, no absolute paths, no map iteration order, no host identity.
//     The digest is what feeds workspace/plan identity and the cache keys that
//     observe project metadata or dependency edges, so a non-deterministic byte
//     in it is a cache-correctness bug, not a cosmetic one (the incidents this
//     rule exists for: a build timestamp stamped into a generated file, and
//     source-blind Go cache keys).
//
//   - CONTENT IS THE ORACLE. Nothing in this protocol carries a modification
//     time or a file size. Consumers may use stat data to PRIORITIZE work, but
//     validity is decided by content digests alone: two rewrites within the
//     same second, to the same length, must invalidate.

package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ProbeProtocolVersion is the wire version of ProbeRequest/ProbeResult. Unlike
// the workspace config (which has no version field), the probe IS a negotiated
// wire between core and out-of-tree providers, so the version travels in the
// payload and a mismatch is rejected rather than guessed at. Pinned by
// conformance_test.go: bumping it requires a migration story on both sides.
const ProbeProtocolVersion = 1

// ProbeRootPath is the canonical spelling of the workspace root as a probe
// project path. Probe paths are required and non-empty, so the root cannot be
// spelled "" — consumers whose internal representation uses the empty string
// for the root translate at the boundary.
const ProbeRootPath = "."

// ProbeReason tells a provider why core is probing. It is advisory: a provider
// may use it to pick a cheaper strategy, but it must return the same facts for
// the same tree regardless, and it is NOT part of the result digest.
type ProbeReason string

// Probe reasons.
const (
	// ProbeReasonLoad is an ordinary workspace load.
	ProbeReasonLoad ProbeReason = "load"
	// ProbeReasonRefresh is a load whose persisted snapshot was rejected.
	ProbeReasonRefresh ProbeReason = "refresh"
	// ProbeReasonPlan is a `--plan`/`--dry-run` probe. Results computed under
	// this reason are held in memory and MUST NOT be persisted: a planning run
	// is allowed to be speculative, and persisting its view would let a
	// hypothetical selection become the workspace's recorded identity.
	ProbeReasonPlan ProbeReason = "plan"
)

// ProbeRequest is core's question to one provider.
type ProbeRequest struct {
	// Version is ProbeProtocolVersion.
	Version int `json:"version"`
	// Extension is the provider's extension name, echoed back in the result so
	// a misrouted response is detectable.
	Extension string `json:"extension"`
	// Reason is why core is probing. Advisory; never digested.
	Reason ProbeReason `json:"reason,omitempty"`
	// Paths are the repo-relative candidate project directories core already
	// knows about, sorted. A provider may report a subset; it may not invent a
	// project outside this set, because core alone assigns canonical paths.
	Paths []string `json:"paths,omitempty"`
	// Files are repo-relative files core has already observed under Paths, as a
	// hint so a provider can avoid re-walking the tree. Hints only: a provider
	// that ignores them must still produce the same result.
	Files []string `json:"files,omitempty"`
	// DependencySources asks the provider to attribute every edge it reports
	// (ProbeProject.DependencySources).
	//
	// It is how a result member was added without a protocol version: both
	// sides strict-decode, so neither may meet a member it does not know. Core
	// sets this only for a provider whose manifest declares the capability
	// (extension WorkspaceAdapter.DependencySources) — a provider built before
	// the member would reject the request — and ServeProbe strips the
	// attribution from every answer to a request that did not ask, so a core
	// built before the member never receives it.
	DependencySources bool `json:"dependencySources,omitempty"`
}

// ProbeResult is one provider's answer.
type ProbeResult struct {
	// Version is ProbeProtocolVersion.
	Version int `json:"version"`
	// Extension is the answering provider's extension name. It keys the
	// provider-owned metadata bucket in the merged view, so it is required.
	Extension string `json:"extension"`
	// Projects are the provider's per-directory findings, keyed by Path.
	Projects []ProbeProject `json:"projects,omitempty"`
	// WatchedFiles are repo-relative files whose content changes the provider's
	// own answer independently of any single project (a workspace lockfile, a
	// root toolchain config). Per-project files belong on the project.
	WatchedFiles []string `json:"watchedFiles,omitempty"`
	// Diagnostics carry provider-side findings. They are advisory prose and are
	// deliberately EXCLUDED from the digest: a reworded warning must not cold
	// every cache key that observes this result.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// ProbeProject is what one provider knows about one project directory.
//
// Every path member is repo-relative and slash-separated. SourceFile may sit
// arbitrarily deep below Path: the project root is not assumed to be the
// provider's own unit root (a Go module, an npm package) — that is the accepted
// contract amendment for this slice, and it is why Dependencies are paths.
type ProbeProject struct {
	// Path is the repo-relative project directory. Required; it is the join key
	// across providers and against core's own discovery.
	Path string `json:"path"`
	// SourceName is the identity the provider's own manifest declares
	// (go.mod module, package.json name, pyproject name). It outranks a scope
	// namePattern but never an explicit putnami.json name.
	SourceName string `json:"sourceName,omitempty"`
	// SourceFile is the repo-relative file SourceName was read from.
	SourceFile string `json:"sourceFile,omitempty"`
	// Version is the version the provider's manifest declares.
	Version string `json:"version,omitempty"`
	// Type is the provider's project classification ("library",
	// "application").
	Type string `json:"type,omitempty"`
	// Tags are provider-derived tags.
	Tags []string `json:"tags,omitempty"`
	// Dependencies are repo-relative PATHS of workspace projects this project
	// depends on. Providers resolve their own module graph to paths; core
	// resolves paths to projects. A dependency naming a path core did not
	// probe is dropped by the consumer, not by this protocol.
	Dependencies []string `json:"dependencies,omitempty"`
	// DependencySources names, per dependency PATH in Dependencies, the
	// manifest family the provider derived that edge from. A path Dependencies
	// carries and this map does not resolves to DependencySourceDeclared: the
	// provider read it from a declaration rather than from an import.
	//
	// It never changes WHICH edges a provider reports — it says where each one
	// came from. A provider whose answer folds a declared list into the same
	// list as its real imports (a `putnami` block inside package.json beside
	// the package's own dependencies) reports both here, so a consumer that
	// must act on real imports alone can tell them apart without re-reading the
	// provider's manifests.
	//
	// DependencySourceContract is core's own derivation and is not reportable
	// here.
	//
	// It is a NEGOTIATED member: present only in an answer to a request that
	// set ProbeRequest.DependencySources, so the v1 result shape a strict older
	// core decodes is unchanged.
	DependencySources map[string]DependencySource `json:"dependencySources,omitempty"`
	// Publish are publish channels the provider derives.
	Publish []string `json:"publish,omitempty"`
	// RunsWith are serve-mode service dependencies the provider derives.
	RunsWith []string `json:"runsWith,omitempty"`
	// Extensions are extension references the project requests.
	Extensions []string `json:"extensions,omitempty"`
	// WatchedFiles are repo-relative files whose content changes this project's
	// probe answer or invalidates work for this project. Entries at the workspace
	// root also let core map an otherwise-unowned root change back to the projects
	// whose provider declared it, without learning provider-specific filenames.
	WatchedFiles []string `json:"watchedFiles,omitempty"`
	// Metadata is provider-owned opaque data. Core never interprets it; it is
	// carried under project.metadata[<extension>] and hashed canonically, so
	// two providers can never collide and a provider's own re-ordering of its
	// object keys cannot move the digest. Must be a JSON object.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// ProbeFailureKind classifies why a probe did not produce a usable result.
// Consumers fail graph-dependent commands with the kind attached so the cause
// is machine-readable rather than a flattened string.
type ProbeFailureKind string

// Probe failure kinds.
const (
	// ProbeFailureUnavailable means the provider could not be started or
	// resolved at all (missing extension, missing runtime).
	ProbeFailureUnavailable ProbeFailureKind = "provider-unavailable"
	// ProbeFailureTransport means the provider ran but the exchange failed
	// (broken pipe, unparseable payload).
	ProbeFailureTransport ProbeFailureKind = "transport"
	// ProbeFailureTimeout means the provider did not answer in time.
	ProbeFailureTimeout ProbeFailureKind = "timeout"
	// ProbeFailureInvalidResult means the provider answered with a payload the
	// protocol rejects (wrong version, absolute path, duplicate project).
	ProbeFailureInvalidResult ProbeFailureKind = "invalid-result"
	// ProbeFailureConflict means two providers reported irreconcilable
	// non-empty scalars for one project and no explicit config resolved it.
	ProbeFailureConflict ProbeFailureKind = "merge-conflict"
	// ProbeFailureVisibility means the resolved graph crosses an import
	// boundary a project declared. The answer is usable; the graph it describes
	// is one the workspace refuses to plan over, and it travels on this channel
	// because that refusal is exactly the policy this type carries: every
	// graph-dependent command fails, and the commands that repair a workspace
	// stay reachable.
	ProbeFailureVisibility ProbeFailureKind = "visibility-violation"
	// ProbeFailureDeclaredEdge means the resolved graph carries a dependency
	// edge a manifest declares and no import backs. It is raised only when the
	// workspace asks for it (`options.workspace.declaredEdges: enforce`).
	ProbeFailureDeclaredEdge ProbeFailureKind = "declared-edge"
)

// ValidProbeFailureKinds is the closed set, in diagnostic order.
var ValidProbeFailureKinds = []ProbeFailureKind{
	ProbeFailureUnavailable,
	ProbeFailureTransport,
	ProbeFailureTimeout,
	ProbeFailureInvalidResult,
	ProbeFailureConflict,
	ProbeFailureVisibility,
	ProbeFailureDeclaredEdge,
}

// Valid reports whether k is a member of the closed set.
func (k ProbeFailureKind) Valid() bool {
	for _, known := range ValidProbeFailureKinds {
		if k == known {
			return true
		}
	}
	return false
}

// ProbeFailure is a typed, causal probe failure. It implements error so a
// consumer can return it directly, and Diagnostic() renders it into the shared
// diagnostic shape.
type ProbeFailure struct {
	// Kind is the machine-readable cause.
	Kind ProbeFailureKind `json:"kind"`
	// Extension names the provider that failed, when known.
	Extension string `json:"extension,omitempty"`
	// Message is the human-readable cause.
	Message string `json:"message"`
	// Diagnostics are the underlying validation findings, when the failure came
	// from rejecting a payload.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// Error renders the failure as "<kind>: <extension>: <message>".
func (f *ProbeFailure) Error() string {
	parts := make([]string, 0, 3)
	parts = append(parts, string(f.Kind))
	if f.Extension != "" {
		parts = append(parts, f.Extension)
	}
	parts = append(parts, f.Message)
	return strings.Join(parts, ": ")
}

// Diagnostic renders the failure as an error diagnostic whose code is the
// failure kind, so a caller that only speaks diagnostics keeps the cause.
func (f *ProbeFailure) Diagnostic() diag.Diagnostic {
	return diag.Errorf(string(f.Kind), f.Extension, "%s", f.Message)
}

// NewProbeFailure builds a typed failure. An unknown kind is coerced to
// ProbeFailureTransport rather than traveling as an unclassified string.
func NewProbeFailure(kind ProbeFailureKind, extension, format string, args ...any) *ProbeFailure {
	if !kind.Valid() {
		kind = ProbeFailureTransport
	}
	return &ProbeFailure{Kind: kind, Extension: extension, Message: fmt.Sprintf(format, args...)}
}

// --- normalization ----------------------------------------------------------

// NormalizeProbePath canonicalizes one repo-relative probe path: slashes
// forward, "." and "" collapsed to ProbeRootPath, redundant segments removed.
// It reports ok=false for a path this protocol refuses to carry — absolute
// paths and any path escaping the workspace root — because such a path would
// make a digest depend on where the repository is checked out.
func NormalizeProbePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return ProbeRootPath, true
	}
	if strings.ContainsRune(p, '\\') {
		// Backslashes are not a path separator on the wire. Accepting them
		// would make the same tree digest differently per host OS.
		return "", false
	}
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return ProbeRootPath, true
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

// normalizeProbePaths cleans, drops unrepresentable entries, sorts and dedupes
// a path list. Invalid entries are dropped here and REPORTED by validation;
// normalization never fails, so a caller that normalizes without validating
// still gets a canonical (if lossy) value rather than a poisoned digest.
func normalizeProbePaths(in []string) []string {
	return normalizeStrings(in, NormalizeProbePath)
}

// normalizeDependencySources canonicalizes an edge-provenance map: keys are
// normalized as repo-relative paths, and an entry that names no reported
// dependency, carries no path, or claims a source outside the closed set is
// dropped.
//
// Dropping rather than keeping is what makes the digest a function of the facts
// the answer actually carries: a provenance for an edge the answer does not
// report describes nothing, and two providers spelling one path differently
// must not produce two digests. The dropped entries are reported by
// ValidateProbeResult, which runs on the raw answer before any merge.
func normalizeDependencySources(in map[string]DependencySource, dependencies []string) map[string]DependencySource {
	if len(in) == 0 {
		return nil
	}
	reported := make(map[string]bool, len(dependencies))
	for _, dependency := range dependencies {
		reported[dependency] = true
	}
	out := make(map[string]DependencySource, len(in))
	for rawPath, source := range in {
		path, ok := NormalizeProbePath(rawPath)
		if !ok || !reported[path] || !source.ReportableByProvider() {
			continue
		}
		out[path] = StrongerDependencySource(out[path], source)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeTokens trims, drops empties, sorts and dedupes a plain string list.
func normalizeTokens(in []string) []string {
	return normalizeStrings(in, func(s string) (string, bool) {
		s = strings.TrimSpace(s)
		return s, s != ""
	})
}

func normalizeStrings(in []string, clean func(string) (string, bool)) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		value, ok := clean(raw)
		if !ok || value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// CanonicalMetadata re-encodes provider-owned metadata into its canonical form:
// decoded and re-marshaled, so object keys land in encoding/json's sorted order
// and insignificant whitespace disappears. A provider that reorders its own
// keys therefore cannot move a digest. Returns (nil, false) when the value is
// not a JSON object.
func CanonicalMetadata(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true
	}
	var decoded map[string]any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return nil, false
	}
	if len(decoded) == 0 {
		return nil, true
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// NormalizeProbeProject canonicalizes one project entry in place.
func NormalizeProbeProject(p *ProbeProject) {
	if p == nil {
		return
	}
	if cleaned, ok := NormalizeProbePath(p.Path); ok {
		p.Path = cleaned
	}
	p.SourceName = strings.TrimSpace(p.SourceName)
	if p.SourceFile != "" {
		if cleaned, ok := NormalizeProbePath(p.SourceFile); ok {
			p.SourceFile = cleaned
		}
	}
	p.Version = strings.TrimSpace(p.Version)
	p.Type = strings.TrimSpace(p.Type)
	p.Tags = normalizeTokens(p.Tags)
	p.Dependencies = normalizeProbePaths(p.Dependencies)
	p.DependencySources = normalizeDependencySources(p.DependencySources, p.Dependencies)
	p.Publish = normalizeTokens(p.Publish)
	p.RunsWith = normalizeTokens(p.RunsWith)
	p.Extensions = normalizeTokens(p.Extensions)
	p.WatchedFiles = normalizeProbePaths(p.WatchedFiles)
	if canonical, ok := CanonicalMetadata(p.Metadata); ok {
		p.Metadata = canonical
	}
}

// NormalizeProbeResult canonicalizes a result in place: every project is
// normalized and the project list is sorted by path. Authoring order, key
// order and path spelling all collapse, which is what makes ProbeResultDigest
// a function of the FACTS rather than of the serialization.
func NormalizeProbeResult(r *ProbeResult) {
	if r == nil {
		return
	}
	r.Extension = strings.TrimSpace(r.Extension)
	for i := range r.Projects {
		NormalizeProbeProject(&r.Projects[i])
	}
	sort.SliceStable(r.Projects, func(i, j int) bool { return r.Projects[i].Path < r.Projects[j].Path })
	r.WatchedFiles = normalizeProbePaths(r.WatchedFiles)
}

// --- digest -----------------------------------------------------------------

// probeDigestFormat names the digest serialization. It is hashed with the
// payload and prefixes every digest, so bumping it moves every digest at once —
// the reviewed lever for changing what the digest covers.
const probeDigestFormat = "wp1"

// ProbeProjectDigest is the canonical digest of one project's probe view. The
// input is normalized into a private copy first, so the caller's value is never
// mutated and an un-normalized value digests identically to a normalized one.
func ProbeProjectDigest(p ProbeProject) string {
	NormalizeProbeProject(&p)
	return hashProbeView(struct {
		Format  string       `json:"format"`
		Kind    string       `json:"kind"`
		Project ProbeProject `json:"project"`
	}{probeDigestFormat, "project", p})
}

// ProbeResultDigest is the canonical digest of one provider's whole result.
// Diagnostics are cleared before hashing: they are advisory prose, and a
// reworded warning must not invalidate caches.
func ProbeResultDigest(r ProbeResult) string {
	copied := ProbeResult{
		Version:      r.Version,
		Extension:    r.Extension,
		Projects:     append([]ProbeProject(nil), r.Projects...),
		WatchedFiles: append([]string(nil), r.WatchedFiles...),
	}
	NormalizeProbeResult(&copied)
	return hashProbeView(struct {
		Format string      `json:"format"`
		Kind   string      `json:"kind"`
		Result ProbeResult `json:"result"`
	}{probeDigestFormat, "result", copied})
}

// ProbeWorkspaceDigest is the aggregate digest over every provider's result.
// Results are digested individually and then folded in sorted digest order, so
// the aggregate does not depend on the order core happened to run providers in.
func ProbeWorkspaceDigest(results []ProbeResult) string {
	digests := make([]string, 0, len(results))
	for _, r := range results {
		digests = append(digests, ProbeResultDigest(r))
	}
	sort.Strings(digests)
	return hashProbeView(struct {
		Format  string   `json:"format"`
		Kind    string   `json:"kind"`
		Results []string `json:"results"`
	}{probeDigestFormat, "workspace", digests})
}

// hashProbeView marshals a digest view and returns "<format>:<sha256>". The
// view types are JSON-native by construction; a marshal failure would mean a
// future field broke that property, and failing loudly beats returning a shared
// sentinel two different inputs could collide on.
func hashProbeView(view any) string {
	encoded, err := json.Marshal(view)
	if err != nil {
		panic(fmt.Sprintf("workspace: probe digest view is not serializable: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return probeDigestFormat + ":" + hex.EncodeToString(sum[:])
}
