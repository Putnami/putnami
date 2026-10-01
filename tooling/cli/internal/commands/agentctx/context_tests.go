// Tests-section aggregation for the agent-context document.
//
// buildTestsSection populates every project's agentcontext.TestsSection from
// the committed conformance-pack metadata — AGGREGATION ONLY: it never spawns
// a subprocess, provisions runtime infra, reads a live database, or synthesizes a
// runtime test binding (that is internal/jobs' runtime concern). It reads only
// committed facts: each pack's <project>/conformance/pack.json, the project's
// committed schema/capabilities.json, the project's committed test sources, and a
// pack's committed corpus file (referenced by digest, never embedded).
//
// The discovery/match/opt-in convention used to have a twin in internal/jobs —
// the test-infra planner emitted an ADVISORY for each applicable-but-unopted
// pack it found while walking the same closure. A later change
// deleted that planner, and the advisory with it: the nudge relied on
// content-scanning a project's committed test SOURCES for runner tokens, which
// is language knowledge core has no business having, and it rode on a run whose
// only reason to walk the closure was database provisioning. This aggregation
// survives because it is the agent-context DOCUMENT's own content, built from
// committed facts alone — no subprocess, no runtime infra, no live database.
package agentctx

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	agentcontext "go.putnami.dev/protocol/agentcontext"
	capabilities "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

// conformancePackRelPath is the committed pack-manifest location convention:
// each conformance pack lives at <project>/conformance/pack.json.
// It mirrors internal/jobs' constant of the same value.
const conformancePackRelPath = "conformance/pack.json"

// The committed test policy lives in the workspace config at options.test.infra.
// buildTestsSection derives its policy ONLY from this value so the artifact is
// byte-stable at a fixed tree (see testPolicyFromConfig).
const (
	testInfraConfigCommand = "test"
	testInfraConfigKey     = "infra"
)

// conformanceRunnerTokens are the one-line opt-in markers a project uses to
// certify a conformance pack: the Go conformance.Run(t) runner and the TypeScript
// registerConformanceTests() / registerHealthConformanceTests() runners. A test
// source referencing any of them — or the pack id itself — counts as an opt-in.
// This mirrors internal/jobs' identical token set.
var conformanceRunnerTokens = []string{
	"conformance.Run",
	"registerConformanceTests",
	"registerHealthConformanceTests",
}

// conformancePack mirrors the committed conformance/pack.json descriptor: a stable
// id, the capability kinds the pack certifies, an optional committed corpus, and
// the languages that ship a runner. It is decoded leniently (unknown fields
// tolerated) so a forward-compatible pack never breaks aggregation.
type conformancePack struct {
	ID              string   `json:"id"`
	Corpus          string   `json:"corpus,omitempty"`
	CapabilityKinds []string `json:"capabilityKinds"`
	Languages       []string `json:"languages,omitempty"`
}

// discoveredPack pairs a decoded pack with the workspace-relative path of the
// project that OWNS it, so a fixture corpus is referenced relative to the owning
// project's conformance/ directory regardless of which project selected the pack.
type discoveredPack struct {
	pack      conformancePack
	ownerPath string
}

// buildTestsSection aggregates the project's tests section from committed
// conformance-pack metadata ALONE. It reports the environment-independent test
// policy plus, for each applicable-and-opted-in conformance pack reachable through
// the project's dependency closure, a pack reference and (when the pack ships a
// corpus) a fixture digest. When no pack is selected it records a machine-readable
// absence reason so a consumer never has to guess why the section is empty. The
// section is emitted for EVERY project so it is always informative.
func buildTestsSection(ws *workspace.Workspace, proj *workspace.Project, projDir string) (*agentcontext.TestsSection, error) {
	section := &agentcontext.TestsSection{Policy: testPolicyFromConfig(ws)}

	declared := declaredConformanceKinds(projDir)
	selected := selectConformancePacks(ws, proj, projDir, declared)
	if len(selected) == 0 {
		section.AbsenceReason = emptyTestsAbsenceReason(declared)
		return section, nil
	}

	for _, dp := range selected {
		section.Packs = append(section.Packs, agentcontext.PackRef{
			ID:              dp.pack.ID,
			CapabilityKinds: append([]string(nil), dp.pack.CapabilityKinds...),
			Languages:       append([]string(nil), dp.pack.Languages...),
		})
	}
	fixtures, err := conformanceFixtureDigests(ws.Root, selected)
	if err != nil {
		return nil, err
	}
	section.FixtureDigests = fixtures
	return section, nil
}

// testPolicyFromConfig derives the DETERMINISM-CRITICAL test policy from committed
// config alone: options.test.infra when it names a valid policy (auto/require/skip),
// else the auto default. It deliberately does NOT consult the CI environment (as
// the runtime planner's resolveTestMode does), so the artifact is byte-identical
// in CI and locally at a fixed tree. A nil config is tolerated (auto).
func testPolicyFromConfig(ws *workspace.Workspace) agentcontext.TestPolicy {
	if ws != nil && ws.Config != nil && ws.Config.Options != nil {
		if opts, ok := ws.Config.Options[testInfraConfigCommand]; ok {
			if s, ok := opts[testInfraConfigKey].(string); ok {
				policy := agentcontext.TestPolicy(strings.TrimSpace(s))
				if agentcontext.ValidTestPolicies[policy] {
					return policy
				}
			}
		}
	}
	return agentcontext.TestPolicyAuto
}

// declaredConformanceKinds reads the project's committed capability manifest
// (schema/capabilities.json) and returns the set of capability-kind strings it
// declares, via the protocol's canonical version-specific provider projection
// so the aggregator can never drift from the validator's notion of
// which kinds a manifest provides. An absent or malformed manifest yields an empty
// set (never an error): aggregation is best-effort over committed facts.
func declaredConformanceKinds(projDir string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(projDir, filepath.FromSlash(capabilities.CommittedPath)))
	if err != nil {
		return nil
	}
	document, diags := capabilities.ParseManifestDocument(data)
	if document == nil || diag.HasErrors(diags) {
		return nil
	}
	var available map[capabilities.CapabilityKind]bool
	if document.V1 != nil {
		available = capabilities.AvailableProviderKinds(document.V1)
	} else {
		available = capabilities.AvailableProviderKindsV2(document.V2)
	}
	kinds := map[string]bool{}
	for kind, present := range available {
		if present {
			kinds[string(kind)] = true
		}
	}
	return kinds
}

// selectConformancePacks returns the applicable-and-opted-in conformance packs for
// the project, sorted by id. A pack is APPLICABLE when its capabilityKinds
// intersect the project's declared kinds, and OPTED-IN when a committed test
// source references the pack id or a known conformance runner token; the
// intersection of the two is the selection. It short-circuits when the project
// declares no kinds or the closure ships no pack, and only reads test sources once
// a matching candidate exists.
func selectConformancePacks(ws *workspace.Workspace, proj *workspace.Project, projDir string, declared map[string]bool) []discoveredPack {
	if len(declared) == 0 {
		return nil
	}
	packs := discoverConformancePacks(ws, proj)
	if len(packs) == 0 {
		return nil
	}
	var applicable []discoveredPack
	for _, dp := range packs {
		if conformancePackApplicable(dp.pack, declared) {
			applicable = append(applicable, dp)
		}
	}
	if len(applicable) == 0 {
		return nil
	}
	testSources := readProjectTestSources(projDir)
	var selected []discoveredPack
	for _, dp := range applicable {
		if conformanceOptedIn(testSources, dp.pack) {
			selected = append(selected, dp)
		}
	}
	return selected
}

// discoverConformancePacks walks the project's dependency closure — the project
// itself plus its transitive dependencies — and returns the committed
// conformance/pack.json descriptors found there, each paired with its owning
// project's path, deduped by pack id and sorted by id. Missing or malformed
// pack.json files are skipped silently (the lenient decode tolerates unknown
// fields): discovery aggregates committed facts and never fails over a
// forward-compatible or absent pack.
func discoverConformancePacks(ws *workspace.Workspace, proj *workspace.Project) []discoveredPack {
	var packs []discoveredPack
	seen := map[string]bool{}
	for _, p := range conformanceClosure(ws, proj) {
		abs := filepath.Join(ws.Root, p.Path, filepath.FromSlash(conformancePackRelPath))
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		var pk conformancePack
		if err := json.Unmarshal(data, &pk); err != nil {
			continue
		}
		id := strings.TrimSpace(pk.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		packs = append(packs, discoveredPack{pack: pk, ownerPath: p.Path})
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].pack.ID < packs[j].pack.ID })
	return packs
}

// conformanceClosure returns the project plus every project reachable through its
// dependency edges, resolved to in-workspace projects. It mirrors the closure the
// infra/test planners walk (proj itself + its transitive dependencies) so the
// aggregator and the runtime advisory agree on which packs are "available" to a
// project. A nil dependency graph degrades to the project alone.
func conformanceClosure(ws *workspace.Workspace, proj *workspace.Project) []*workspace.Project {
	closure := []*workspace.Project{proj}
	seen := map[string]bool{proj.ID: true}
	if ws.Graph != nil {
		for _, id := range ws.Graph.TransitiveDependenciesOf([]string{proj.ID}) {
			if seen[id] {
				continue
			}
			seen[id] = true
			if p := ws.ProjectByID(id); p != nil {
				closure = append(closure, p)
			}
		}
	}
	return closure
}

// conformancePackApplicable reports whether any capability kind the pack certifies
// is one the project declares — the applicability rule the runtime advisory's
// firstDeclaredMatch encodes, reduced to a boolean.
func conformancePackApplicable(pack conformancePack, declared map[string]bool) bool {
	for _, k := range pack.CapabilityKinds {
		if declared[k] {
			return true
		}
	}
	return false
}

// conformanceOptedIn reports whether the project's committed test sources certify
// the pack — by referencing the pack id or any known conformance runner token. It
// biases toward reporting an opt-in so a certified project's section is populated.
func conformanceOptedIn(testSources string, pack conformancePack) bool {
	if testSources == "" {
		return false
	}
	if id := strings.TrimSpace(pack.ID); id != "" && strings.Contains(testSources, id) {
		return true
	}
	for _, tok := range conformanceRunnerTokens {
		if strings.Contains(testSources, tok) {
			return true
		}
	}
	return false
}

// readProjectTestSources concatenates the project's committed test-file contents
// for opt-in detection, skipping vendored and generated trees. Best-effort: an
// unreadable file is skipped and a project with no test files yields "".
func readProjectTestSources(projDir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(projDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".gen", ".git", "dist", "build", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !isTestSourceFile(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		b.Write(data)
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}

// isTestSourceFile reports whether name looks like a committed test source: a Go
// *_test.go file, or a TypeScript/JavaScript *.test.* / *.spec.* file.
func isTestSourceFile(name string) bool {
	if strings.HasSuffix(name, "_test.go") {
		return true
	}
	if !strings.Contains(name, ".test.") && !strings.Contains(name, ".spec.") {
		return false
	}
	switch {
	case strings.HasSuffix(name, ".ts"), strings.HasSuffix(name, ".tsx"),
		strings.HasSuffix(name, ".mts"), strings.HasSuffix(name, ".cts"),
		strings.HasSuffix(name, ".js"), strings.HasSuffix(name, ".jsx"),
		strings.HasSuffix(name, ".mjs"), strings.HasSuffix(name, ".cjs"):
		return true
	}
	return false
}

// emptyTestsAbsenceReason classifies WHY a project selected no conformance pack.
// The aggregator DID run, so it never reports not-collected. A project that
// declares no capability kinds (no manifest, or one that declares nothing) has
// nothing to conformance-certify → unsupported-project-type; a project that
// declares kinds but matched no opted-in pack → no-packs.
func emptyTestsAbsenceReason(declared map[string]bool) agentcontext.AbsenceReason {
	if len(declared) == 0 {
		return agentcontext.AbsenceReasonUnsupportedProjectType
	}
	return agentcontext.AbsenceReasonNoPacks
}

// conformanceFixtureDigests references each selected pack's committed corpus file
// (conformance/<corpus>, owned by the pack's project) by workspace-relative path +
// sha256 content digest, but only when the pack declares a corpus AND the file
// exists on disk. Results are deduped and sorted by path so the section is
// byte-stable.
func conformanceFixtureDigests(wsRoot string, selected []discoveredPack) ([]agentcontext.ArtifactRef, error) {
	seen := map[string]bool{}
	var refs []agentcontext.ArtifactRef
	for _, dp := range selected {
		corpus := strings.TrimSpace(dp.pack.Corpus)
		if corpus == "" {
			continue
		}
		rel := "conformance/" + corpus
		abs := filepath.Join(wsRoot, filepath.FromSlash(dp.ownerPath), filepath.FromSlash(rel))
		if !shared.FileExists(abs) {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read conformance corpus %s: %w", rel, err)
		}
		path := wsRel(dp.ownerPath, rel)
		if seen[path] {
			continue
		}
		seen[path] = true
		refs = append(refs, agentcontext.ArtifactRef{Path: path, Digest: digestBytes(data)})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })
	return refs, nil
}
