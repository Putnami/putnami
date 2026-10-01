// Package agentctx: agent-context pack + freshness gate.
//
// `putnami context pack` deterministically aggregates each targeted project's
// already-committed facts into an agentcontext.Document (protocols/agentcontext)
// and writes it, atomically and in canonical bytes, to
// "<project>/.gen/agent-context.json". Aggregation is strictly BY REFERENCE:
// capabilities, contracts, infra, migrations, and the config schema travel as a
// path plus a sha256 content digest, never as embedded content.
//
// `putnami context pack --check` regenerates every document in memory and diffs
// it byte-for-byte against the on-disk artifact; a missing or differing document
// is drift (exit 2). The artifact is ephemeral/gitignored, so --check is a
// freshness gate: it drifts after any tree change (or a new workspace revision),
// telling the user to re-run `context pack`.
//
// Redaction is fail-closed. Every emit — pack and check alike — runs
// agentcontext.ValidatePublishSafety before any bytes are produced. The
// SensitivePaths set is derived from the FACTS (an infra requirements manifest
// declaring a secret-kind requirement marks its path sensitive), independently
// of the document's own flags, so forgetting to flag a referenced sensitive path
// makes the gate fail and the document is never written and never passes --check.
//
// The document emits identity, compositionRoots, capabilities/contracts/infra/
// migrations, config.schemaRef, and provenance; plus
// deterministic representativeSources (the application main and in-project
// capability-evidence files, as line RANGES with a token estimate, never
// content), adjacent docs (checked/unchecked by a text-walk), and author
// overrides at <project>/schema/agent-context.overrides.json. The tests
// section is populated for every project by aggregating the committed
// conformance-pack metadata — applicable-and-opted-in packs, an
// environment-independent policy, and fixture digests, or a machine-readable
// absence reason (see context_tests.go).
package agentctx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	agentcontext "go.putnami.dev/protocol/agentcontext"
	capabilities "go.putnami.dev/protocol/capabilities"
	protocolcli "go.putnami.dev/protocol/cli"
	contracts "go.putnami.dev/protocol/contracts"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// agentContextSchemaURI is the $schema URI the agent-context protocol fixtures
// pin; every producer stamps the same value so consumers can locate the schema.
const agentContextSchemaURI = "https://putnami.dev/schemas/putnami-agent-context.json"

// Committed artifact paths, project-relative. capabilities and contracts reuse
// the protocol-owned committed paths so a path change on either side is caught
// by a single source of truth; config has no exported protocol constant.
const (
	contextConfigSchemaPath = "schema/config.json"
	contextMigrationsDir    = "migrations"
	// infraSecretKind is the ArtifactRef kind stamped on an infra reference whose
	// manifest declares a secret-kind requirement (see PerProjectManifest.Secrets).
	infraSecretKind = "secret"
	// contextGeneratorName is the provenance generator identity for this producer.
	contextGeneratorName = "putnami"
)

// ContextPackReport is the machine-readable result of `context pack`.
type ContextPackReport struct {
	// Projects lists the documents written, in emit (sorted-by-id) order.
	Projects []ContextPackArtifact `json:"projects"`
}

// ContextPackArtifact records one agent-context document written to disk.
type ContextPackArtifact struct {
	// Project is the workspace project id the document describes.
	Project string `json:"project"`
	// Path is the workspace-relative path of the written document.
	Path string `json:"path"`
}

// The two verdicts `context pack --check` reports.
//
// They were read from internal/commands/sdd's contract-drift vocabulary until
// a later change moved that vertical into the @putnami/sdd extension. The strings
// are unchanged, so no consumer sees a different byte; what changed is that a
// freshness check on agent-context documents no longer depends on the SDD
// surface being compiled into the CLI to name its own answer.
const (
	ContextOutcomeClean = "clean"
	ContextOutcomeDrift = "drift"
)

// ContextCheckReport is the machine-readable result of `context pack --check`.
type ContextCheckReport struct {
	// Outcome is ContextOutcomeClean or ContextOutcomeDrift.
	Outcome string `json:"outcome"`
	// Checked is the number of documents compared.
	Checked int `json:"checked"`
	// Drift lists documents whose on-disk bytes differ from a fresh emit, in
	// emit order.
	Drift []ContextDriftArtifact `json:"drift,omitempty"`
}

// ContextDriftArtifact records one document that no longer matches a fresh emit.
type ContextDriftArtifact struct {
	// Project is the workspace project id the document describes.
	Project string `json:"project"`
	// Path is the workspace-relative path of the drifted document.
	Path string `json:"path"`
	// Reason is "stale" (bytes differ) or "missing" (never written).
	Reason string `json:"reason"`
}

// ContextSafetyReport is the machine-readable payload of a publish-safety
// failure, attached to the exit-2 error for the structured failure envelope.
type ContextSafetyReport struct {
	// Project is the workspace project id whose document failed the gate.
	Project string `json:"project"`
	// Violations lists the fail-closed publish-safety diagnostics.
	Violations []ContextSafetyViolation `json:"violations"`
}

// ContextSafetyViolation is one publish-safety diagnostic.
type ContextSafetyViolation struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// BuildDocument deterministically aggregates project p's committed facts into an
// agent-context Document, strictly by reference. revision and cliVersion are the
// two non-deterministic inputs, passed as parameters so callers (and tests) pin
// them: the command wiring resolves revision via `git rev-parse HEAD` and passes
// the CLI version.
//
// It also returns the PublishSafetyOptions the fail-closed gate must run with.
// The SensitivePaths set is derived from the FACTS (a secret-declaring infra
// manifest), independently of the flags placed on the document, so a future bug
// that references a sensitive path without flagging it makes the gate fail closed
// rather than silently leaking a reference.
func BuildDocument(ws *workspace.Workspace, p *workspace.Project, revision, cliVersion string) (*agentcontext.Document, agentcontext.PublishSafetyOptions, error) {
	projDir := filepath.Join(ws.Root, p.Path)
	sensitive := map[string]bool{}

	doc := &agentcontext.Document{
		Schema:           agentContextSchemaURI,
		ProtocolVersion:  agentcontext.ProtocolVersion,
		Identity:         buildIdentity(ws, p, projDir),
		CompositionRoots: buildCompositionRoots(ws, p, projDir),
	}

	var err error
	if doc.Capabilities, err = artifactRefs(projDir, p.Path, capabilities.CommittedPath); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}
	if doc.Contracts, err = artifactRefs(projDir, p.Path, contracts.CommittedPath); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}
	infraRef, err := buildInfraRef(projDir, p.Path, sensitive)
	if err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}
	if infraRef != nil {
		doc.Infra = []agentcontext.ArtifactRef{*infraRef}
	}
	if doc.Migrations, err = buildMigrationRefs(projDir, p.Path); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}
	if doc.Config, err = buildConfigSection(projDir, p.Path); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}
	if doc.RepresentativeSources, err = buildRepresentativeSources(ws, projDir, p); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}
	if doc.Docs, err = buildDocs(projDir, p.Path); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}
	if doc.Tests, err = buildTestsSection(ws, p, projDir); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}

	doc.Provenance = agentcontext.Provenance{
		WorkspaceRevision: revision,
		Generator:         agentcontext.Generator{Name: contextGeneratorName, Version: cliVersion},
		AggregationMethod: agentcontext.AggregationMethodByReference,
	}

	// Author overrides are applied last — after automatic selection, before the
	// gate — and re-sort every section they touch so output stays byte-stable. A
	// malformed overrides file fails closed (exit 2) rather than being ignored.
	if err := applyContextOverrides(doc, projDir, p, sensitive); err != nil {
		return nil, agentcontext.PublishSafetyOptions{}, err
	}

	return doc, agentcontext.PublishSafetyOptions{SensitivePaths: sensitive}, nil
}

// buildIdentity assembles the identity/graph section. Dependency ids come from
// mapping the project's declared dependency NAMES to ids; dependent ids come
// straight from the dependency graph, which is keyed by and returns project ids.
// Both id lists are deduped and sorted for determinism.
func buildIdentity(ws *workspace.Workspace, p *workspace.Project, projDir string) agentcontext.Identity {
	return agentcontext.Identity{
		ID:           p.ID,
		Name:         p.Name,
		Path:         filepath.ToSlash(p.Path),
		Type:         p.Type,
		Tags:         append([]string(nil), p.Tags...),
		Languages:    detectLanguages(projDir),
		Dependencies: resolveIDs(ws, p.Dependencies),
		Dependents:   resolveIDs(ws, ws.Graph.DependentsOf(p.ID)),
	}
}

// detectLanguages derives the project's languages from on-disk markers: a go.mod
// yields "go", a package.json "typescript", and a pyproject.toml or setup.py
// "python". The result is sorted and deduped so the section is deterministic.
func detectLanguages(projDir string) []string {
	var langs []string
	if shared.FileExists(filepath.Join(projDir, "go.mod")) {
		langs = append(langs, "go")
	}
	if shared.FileExists(filepath.Join(projDir, "package.json")) {
		langs = append(langs, "typescript")
	}
	if shared.FileExists(filepath.Join(projDir, "pyproject.toml")) || shared.FileExists(filepath.Join(projDir, "setup.py")) {
		langs = append(langs, "python")
	}
	sort.Strings(langs)
	return langs
}

// resolveIDs maps a list of project references (names or ids) to canonical
// project ids, skipping references that do not resolve to a workspace project,
// deduping, and sorting. It handles both the name-valued Project.Dependencies
// and the id-valued dependency-graph output through one path.
func resolveIDs(ws *workspace.Workspace, refs []string) []string {
	seen := make(map[string]bool, len(refs))
	var ids []string
	for _, ref := range refs {
		id, ok := resolveRefID(ws, ref)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// resolveRefID resolves a single project reference (a name, or an already-"/"-
// prefixed id) to its canonical id.
func resolveRefID(ws *workspace.Workspace, ref string) (string, bool) {
	if p := ws.ProjectByName(ref); p != nil {
		return p.ID, true
	}
	if strings.HasPrefix(ref, "/") {
		if p := ws.ProjectByID(ref); p != nil {
			return p.ID, true
		}
	}
	return "", false
}

// buildCompositionRoots aggregates the project's entrypoints. An application (an
// empty or "application" type) contributes an applicationMain root (see
// applicationMainRoot); the first conventional describe entrypoint that exists
// on disk contributes a describeEntrypoint root. Roots whose file is absent are
// omitted.
func buildCompositionRoots(ws *workspace.Workspace, p *workspace.Project, projDir string) []agentcontext.CompositionRoot {
	var roots []agentcontext.CompositionRoot
	if p.Type == "" || p.Type == "application" {
		if rel, prov := applicationMainRoot(ws, p, projDir); rel != "" {
			roots = append(roots, agentcontext.CompositionRoot{
				Kind:       agentcontext.RootKindApplicationMain,
				Path:       wsRel(p.Path, rel),
				Provenance: prov,
			})
		}
	}
	for _, candidate := range []string{filepath.Join("cmd", "describe", "main.go"), "describe.go"} {
		if shared.FileExists(filepath.Join(projDir, candidate)) {
			roots = append(roots, agentcontext.CompositionRoot{
				Kind:       agentcontext.RootKindDescribeEntrypoint,
				Path:       wsRel(p.Path, candidate),
				Provenance: "describe-hook",
			})
			break
		}
	}
	return roots
}

// applicationMainRoot returns the project-relative application entrypoint and
// its provenance tag, or ("", "") when none is found.
//
// The declared entrypoint is looked up as a CONVENTION rather than as identity:
// the project's own putnami.json wins, and otherwise the workspace probe's
// provider-owned metadata is consulted for a `main` key (the TypeScript adapter
// publishes package.json's). An earlier change deleted core's package.json
// parser, so this is the only remaining path to a TypeScript entrypoint — and it
// is deliberately confined to this human-facing document, never to identity or a
// cache key. When nothing declares one — as for every Go project — it falls back
// to a conventional root main.go that declares `package main`, so a Go
// application's composition root is not silently absent. The `package main`
// check keeps a Go library that happens to have a main.go from being misreported
// as an app.
func applicationMainRoot(ws *workspace.Workspace, p *workspace.Project, projDir string) (rel, provenance string) {
	declared := ""
	if p.Config != nil {
		declared = p.Config.Main
	}
	if declared == "" {
		declared = ws.ProviderMetadataValue(p, "main")
	}
	if main := strings.TrimSpace(declared); main != "" {
		if shared.FileExists(filepath.Join(projDir, main)) {
			return main, "type=application"
		}
	}
	const goMain = "main.go"
	if declaresGoMainPackage(filepath.Join(projDir, goMain)) {
		return goMain, "convention=main.go"
	}
	return "", ""
}

// declaresGoMainPackage reports whether path is a Go source file whose package
// clause is `package main`. It parses only the package clause (stopping there),
// so it is robust against leading comments and build-constraint lines.
func declaresGoMainPackage(path string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
	if err != nil {
		return false
	}
	return f.Name != nil && f.Name.Name == "main"
}

// artifactRefs returns a single-element reference slice for the committed
// project-relative artifact at rel when it exists, or nil when it does not.
func artifactRefs(projDir, projPath, rel string) ([]agentcontext.ArtifactRef, error) {
	ref, err := artifactRef(projDir, projPath, rel)
	if err != nil || ref == nil {
		return nil, err
	}
	return []agentcontext.ArtifactRef{*ref}, nil
}

// artifactRef references the committed project-relative artifact at rel by
// workspace-relative path plus sha256 content digest, or returns (nil, nil) when
// the file does not exist.
func artifactRef(projDir, projPath, rel string) (*agentcontext.ArtifactRef, error) {
	abs := filepath.Join(projDir, rel)
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read artifact %s: %w", rel, err)
	}
	return &agentcontext.ArtifactRef{Path: wsRel(projPath, rel), Digest: digestBytes(data)}, nil
}

// buildInfraRef references the committed infra/requirements.json (if present)
// and classifies its sensitivity. It parses the manifest to decide: any declared
// secret-kind requirement marks the reference kind "secret", flags it sensitive,
// AND records the reference path in the caller-owned sensitive set so the
// fail-closed publish-safety gate passes BECAUSE the reference is flagged. A
// present-but-unparseable manifest is an exit-2 error: sensitivity cannot be
// determined, so the aggregator fails closed rather than emit an unflagged
// reference to a manifest that may declare secrets.
func buildInfraRef(projDir, projPath string, sensitive map[string]bool) (*agentcontext.ArtifactRef, error) {
	rel := filepath.Join(infra.PerProjectManifestDir, infra.PerProjectManifestFilename)
	abs := filepath.Join(projDir, rel)
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read infra requirements %s: %w", rel, err)
	}
	path := wsRel(projPath, rel)
	ref := &agentcontext.ArtifactRef{Path: path, Digest: digestBytes(data)}

	manifest, diags := infra.ParsePerProjectManifest(data)
	if manifest == nil || diag.HasErrors(diags) {
		return nil, protocolcli.Classify(
			fmt.Errorf("infra requirements at %s could not be parsed; cannot determine secret sensitivity", path),
			protocolcli.ErrInvalidConfig)
	}
	if len(manifest.Secrets) > 0 {
		ref.Kind = infraSecretKind
		ref.Sensitive = true
		sensitive[path] = true
	}
	return ref, nil
}

// buildMigrationRefs references every committed migration bundle (migrations/
// *.json), sorted, by path plus content digest.
func buildMigrationRefs(projDir, projPath string) ([]agentcontext.ArtifactRef, error) {
	matches, err := filepath.Glob(filepath.Join(projDir, contextMigrationsDir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("scan migration bundles: %w", err)
	}
	sort.Strings(matches)
	var refs []agentcontext.ArtifactRef
	for _, abs := range matches {
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read migration bundle %s: %w", abs, err)
		}
		rel := filepath.Join(contextMigrationsDir, filepath.Base(abs))
		refs = append(refs, agentcontext.ArtifactRef{Path: wsRel(projPath, rel), Digest: digestBytes(data)})
	}
	return refs, nil
}

// buildConfigSection references the committed config schema (schema/config.json)
// when present. Operational hints (endpoint detection) are deferred to a later
// slice.
func buildConfigSection(projDir, projPath string) (*agentcontext.ConfigSection, error) {
	ref, err := artifactRef(projDir, projPath, contextConfigSchemaPath)
	if err != nil || ref == nil {
		return nil, err
	}
	return &agentcontext.ConfigSection{SchemaRef: ref}, nil
}

// representativeSourceCap bounds the number of capability-evidence source
// ranges the aggregator emits, so the artifact stays small and O(referenced
// files) regardless of how many contributions a manifest carries. Selection is
// deterministic (sorted by path) before the cap, so the retained set is stable.
const representativeSourceCap = 25

// buildRepresentativeSources selects the project's representative source RANGES
// (never content), deterministically: the application main (why=main) first,
// then the in-project capability-evidence files (why=capability-evidence), each
// sorted by path, every range carrying a bytes/4 token estimate. The final order
// is (reason-rank, path) via sortSourceRanges, so a later author override
// (why=override) has a defined slot too.
func buildRepresentativeSources(ws *workspace.Workspace, projDir string, p *workspace.Project) ([]agentcontext.SourceRange, error) {
	wsRoot := ws.Root
	var sources []agentcontext.SourceRange
	added := map[string]bool{}

	// main: the application entrypoint, when the project has one. Gated on the
	// same application-type check buildCompositionRoots uses, so the representative
	// "main" and the applicationMain composition root always agree.
	if p.Type == "" || p.Type == "application" {
		if rel, _ := applicationMainRoot(ws, p, projDir); rel != "" {
			wsPath := wsRel(p.Path, rel)
			sr, err := fileSourceRange(filepath.Join(projDir, rel), wsPath, agentcontext.SourceReasonMain)
			if err != nil {
				return nil, err
			}
			if sr != nil {
				sources = append(sources, *sr)
				added[wsPath] = true
			}
		}
	}

	// capability-evidence: the in-project source files a committed capability
	// manifest points its contribution provenance at.
	evidence, err := capabilityEvidenceSources(wsRoot, projDir, p, added)
	if err != nil {
		return nil, err
	}
	sources = append(sources, evidence...)

	sortSourceRanges(sources)
	return sources, nil
}

// capabilityEvidenceSources reads the committed capability manifest and returns
// a SourceRange for every v1 EvidencePath or v2 declaration/artifact path that
// is (a) inside this project, (b) a .go/.ts/.tsx source file, (c) present on disk, and (d) not the
// already-selected main file. Results are deduped, sorted by path, and capped at
// representativeSourceCap. A missing or unparseable manifest yields no evidence
// (the manifest is still referenced by digest in doc.Capabilities); evidence
// selection is a convenience, not a security boundary, so it never fails the
// build over a manifest that only affects which ranges we surface.
func capabilityEvidenceSources(wsRoot, projDir string, p *workspace.Project, added map[string]bool) ([]agentcontext.SourceRange, error) {
	data, err := os.ReadFile(filepath.Join(projDir, filepath.FromSlash(capabilities.CommittedPath)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read capability manifest %s: %w", capabilities.CommittedPath, err)
	}
	document, diags := capabilities.ParseManifestDocument(data)
	if document == nil || diag.HasErrors(diags) {
		return nil, nil
	}

	projPrefix := filepath.ToSlash(p.Path) + "/"
	seen := map[string]bool{}
	var candidates []string
	for _, sourcePath := range manifestSourcePaths(document, p) {
		ep := filepath.ToSlash(strings.TrimSpace(sourcePath))
		if ep == "" || seen[ep] || added[ep] {
			continue
		}
		if !strings.HasPrefix(ep, projPrefix) || !hasSourceExtension(ep) {
			continue
		}
		if !shared.FileExists(filepath.Join(wsRoot, filepath.FromSlash(ep))) {
			continue
		}
		seen[ep] = true
		candidates = append(candidates, ep)
	}
	sort.Strings(candidates)
	if len(candidates) > representativeSourceCap {
		candidates = candidates[:representativeSourceCap]
	}

	var sources []agentcontext.SourceRange
	for _, ep := range candidates {
		sr, err := fileSourceRange(filepath.Join(wsRoot, filepath.FromSlash(ep)), ep, agentcontext.SourceReasonCapabilityEvidence)
		if err != nil {
			return nil, err
		}
		if sr != nil {
			sources = append(sources, *sr)
		}
	}
	return sources, nil
}

// manifestSourcePaths projects both wire versions into workspace-relative
// source candidates. V2 workspace and current-owner project roots are directly
// resolvable; a package root is resolvable only when it names this project
// package. Dependency package locations are deliberately skipped rather than
// guessed from machine-global caches.
func manifestSourcePaths(document *capabilities.ManifestDocument, project *workspace.Project) []string {
	if document.V1 != nil {
		var out []string
		for _, provenance := range manifestProvenances(document.V1) {
			out = append(out, provenance.EvidencePath)
		}
		return out
	}
	var out []string
	for _, provenance := range manifestProvenancesV2(document.V2) {
		add := func(root capabilities.LocationRoot, value string) {
			value = strings.TrimSpace(value)
			if value == "" {
				return
			}
			switch root {
			case capabilities.LocationRootWorkspace:
				out = append(out, filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))))
			case capabilities.LocationRootProject:
				if provenance.Project == project.Name {
					out = append(out, filepath.ToSlash(filepath.Join(project.Path, filepath.FromSlash(value))))
				}
			case capabilities.LocationRootPackage:
				if provenance.Package == project.Name {
					out = append(out, filepath.ToSlash(filepath.Join(project.Path, filepath.FromSlash(value))))
				}
			}
		}
		add(provenance.Declaration.Root, provenance.Declaration.Path)
		for _, artifact := range provenance.Artifacts {
			add(artifact.Root, artifact.Path)
		}
	}
	return out
}

// manifestProvenances returns the Provenance block of every contribution slice
// in the manifest, in manifest field order, so evidence-path selection walks the
// full contribution surface (config, schemas, discoverers, migrations, infra,
// health, lifecycle, packages, and required capabilities).
func manifestProvenances(m *capabilities.Manifest) []capabilities.Provenance {
	var out []capabilities.Provenance
	for _, c := range m.ConfigDefinitions {
		out = append(out, c.Provenance)
	}
	for _, s := range m.Schemas {
		out = append(out, s.Provenance)
	}
	for _, d := range m.Discoverers {
		out = append(out, d.Provenance)
	}
	for _, mb := range m.Migrations {
		out = append(out, mb.Provenance)
	}
	for _, r := range m.InfraRequirements {
		out = append(out, r.Provenance)
	}
	for _, h := range m.HealthContributors {
		out = append(out, h.Provenance)
	}
	for _, l := range m.LifecycleHooks {
		out = append(out, l.Provenance)
	}
	for _, pv := range m.PackageVersions {
		out = append(out, pv.Provenance)
	}
	for _, rc := range m.RequiredCapabilities {
		out = append(out, rc.Provenance)
	}
	return out
}

func manifestProvenancesV2(m *capabilities.ManifestV2) []capabilities.ProvenanceV2 {
	var out []capabilities.ProvenanceV2
	for _, contribution := range m.ConfigDefinitions {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.Schemas {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.Discoverers {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.Migrations {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.InfraRequirements {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.HealthContributors {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.LifecycleHooks {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.PackageVersions {
		out = append(out, contribution.Provenance)
	}
	for _, contribution := range m.RequiredCapabilities {
		out = append(out, contribution.Provenance)
	}
	return out
}

// fileSourceRange builds a whole-file SourceRange (never content) for the file at
// abs, addressed by its workspace-relative path wsPath. It returns (nil, nil)
// when the file does not exist. StartLine is 1; EndLine is the file's
// newline-delimited line count (>= 1), so validateSourceRange's 1 <= start <= end
// holds even for an empty file. Tokens is the bytes/4 estimate over the whole file.
func fileSourceRange(abs, wsPath string, why agentcontext.SourceReason) (*agentcontext.SourceRange, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read representative source %s: %w", wsPath, err)
	}
	return &agentcontext.SourceRange{
		Path:      wsPath,
		StartLine: 1,
		EndLine:   fileLineCount(data),
		Why:       why,
		Tokens:    tokenEstimate(data),
	}, nil
}

// fileLineCount returns the number of newline-delimited lines in data, with a
// floor of 1 so an empty or newline-free file still yields a valid [1,EndLine]
// range. A file that does not end in a newline still counts its final partial
// line.
func fileLineCount(data []byte) int {
	if len(data) == 0 {
		return 1
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	if n < 1 {
		return 1
	}
	return n
}

// tokenEstimate returns the dependency-free ceil(bytes/4) token estimate over
// data — computed as (n+3)/4 — tagged with the bytes/4 method so the number is
// auditable.
func tokenEstimate(data []byte) agentcontext.TokenEstimate {
	return agentcontext.TokenEstimate{
		Estimated: (len(data) + 3) / 4,
		Method:    agentcontext.TokenMethodBytesDiv4,
	}
}

// hasSourceExtension reports whether p names a first-class source file the
// aggregator surfaces as a representative range: .go, .ts, or .tsx. Committed
// artifact extensions (.mod, .json, .sum) are deliberately excluded — an
// evidence path pointing at go.mod is a build fact, not representative source.
func hasSourceExtension(p string) bool {
	return strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".ts") || strings.HasSuffix(p, ".tsx")
}

// sourceReasonRank orders representative-source reasons for a deterministic
// section: main first, then capability-evidence, then author overrides.
func sourceReasonRank(why agentcontext.SourceReason) int {
	switch why {
	case agentcontext.SourceReasonMain:
		return 0
	case agentcontext.SourceReasonCapabilityEvidence:
		return 1
	case agentcontext.SourceReasonOverride:
		return 2
	default:
		return 3
	}
}

// sortSourceRanges orders representative sources by (reason-rank, path) in place —
// the single canonical ordering both automatic selection and override
// application reproduce, so the section is byte-stable.
func sortSourceRanges(sources []agentcontext.SourceRange) {
	sort.SliceStable(sources, func(i, j int) bool {
		if ri, rj := sourceReasonRank(sources[i].Why), sourceReasonRank(sources[j].Why); ri != rj {
			return ri < rj
		}
		return sources[i].Path < sources[j].Path
	})
}

// buildDocs collects the project's adjacent documentation as DocRefs, sorted by
// path. It surfaces the root README.md/AI.md/AGENTS.md/CLAUDE.md plus every
// doc/*.md and docs/*.md under the project. Each doc is classified checked when
// its text mentions the basename of an existing project source file, else
// unchecked — a lightweight text-walk (never importing the doc or the source),
// borrowing protocols/doccov's read-as-text spirit.
func buildDocs(projDir, projPath string) ([]agentcontext.DocRef, error) {
	rels, err := docCandidatePaths(projDir)
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, nil
	}
	basenames, err := projectSourceBasenames(projDir)
	if err != nil {
		return nil, err
	}
	var docs []agentcontext.DocRef
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(projDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("read doc %s: %w", rel, err)
		}
		relationship := agentcontext.DocRelationshipUnchecked
		if docMentionsSource(data, basenames) {
			relationship = agentcontext.DocRelationshipChecked
		}
		docs = append(docs, agentcontext.DocRef{Path: wsRel(projPath, rel), Relationship: relationship})
	}
	sortDocRefs(docs)
	return docs, nil
}

// docCandidatePaths returns the project-relative paths of the adjacent docs the
// aggregator considers, deduped and deterministic: the conventional root docs
// that exist, plus every doc/*.md and docs/*.md match (sorted).
func docCandidatePaths(projDir string) ([]string, error) {
	seen := map[string]bool{}
	var rels []string
	add := func(rel string) {
		rel = filepath.ToSlash(rel)
		if !seen[rel] {
			seen[rel] = true
			rels = append(rels, rel)
		}
	}
	// AGENTS.md is where Putnami's guidance block lives; a CLAUDE.md it writes
	// only imports it.
	for _, name := range []string{"README.md", "AI.md", "AGENTS.md", "CLAUDE.md"} {
		if shared.FileExists(filepath.Join(projDir, name)) {
			add(name)
		}
	}
	for _, dir := range []string{"doc", "docs"} {
		matches, err := filepath.Glob(filepath.Join(projDir, dir, "*.md"))
		if err != nil {
			return nil, fmt.Errorf("scan %s docs: %w", dir, err)
		}
		sort.Strings(matches)
		for _, abs := range matches {
			add(filepath.Join(dir, filepath.Base(abs)))
		}
	}
	return rels, nil
}

// projectSourceBasenames returns the set of .go/.ts/.tsx file base names under
// projDir, skipping hidden, node_modules, and vendor directories. It is the
// text-walk's notion of "a source file that exists in the project": a doc that
// names one of these by basename is treated as checked.
func projectSourceBasenames(projDir string) (map[string]bool, error) {
	names := map[string]bool{}
	err := filepath.WalkDir(projDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != projDir {
				name := d.Name()
				if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if hasSourceExtension(d.Name()) {
			names[d.Name()] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan project sources: %w", err)
	}
	return names, nil
}

// docMentionsSource reports whether the doc text names any project source file by
// basename. The check is a plain substring scan (order-independent, so the
// boolean is deterministic regardless of map iteration order).
func docMentionsSource(data []byte, basenames map[string]bool) bool {
	text := string(data)
	for name := range basenames {
		if strings.Contains(text, name) {
			return true
		}
	}
	return false
}

// sortDocRefs orders docs by path in place — the canonical ordering both
// automatic collection and override application reproduce.
func sortDocRefs(docs []agentcontext.DocRef) {
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
}

// applyContextOverrides applies the optional author-owned overrides file at
// <project>/schema/agent-context.overrides.json to an already-built document,
// then re-sorts the affected sections so output stays byte-stable. A missing file
// is a no-op; a present-but-malformed file fails closed with an exit-2 classified
// error, so a broken overrides file can never be silently ignored.
//
// Overrides are strictly narrow: they add/remove representative sources and docs
// and force-flag paths sensitive. Sensitive marking runs last, so a path an
// author both adds and flags is redacted on the entry it just added.
func applyContextOverrides(doc *agentcontext.Document, projDir string, p *workspace.Project, sensitive map[string]bool) error {
	abs := filepath.Join(projDir, filepath.FromSlash(agentcontext.OverridesPath))
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read agent-context overrides %s: %w", agentcontext.OverridesPath, err)
	}
	overrides, diags := agentcontext.ParseAndValidateOverrides(data)
	if overrides == nil || diag.HasErrors(diags) {
		return contextDocumentError(p.ID, "has an invalid overrides file", diags)
	}

	doc.RepresentativeSources = applySourceOverrides(doc.RepresentativeSources, overrides)
	doc.Docs = applyDocOverrides(doc.Docs, overrides)
	markSensitivePaths(doc, sensitive, overrides.Sensitive)
	return nil
}

// applySourceOverrides drops the removed representative-source paths, appends the
// author-added (already-validated) ranges, and re-sorts by (reason-rank, path).
func applySourceOverrides(sources []agentcontext.SourceRange, o *agentcontext.OverridesFile) []agentcontext.SourceRange {
	remove := sliceToSet(o.RemoveSources)
	kept := make([]agentcontext.SourceRange, 0, len(sources)+len(o.AddSources))
	for _, s := range sources {
		if !remove[s.Path] {
			kept = append(kept, s)
		}
	}
	kept = append(kept, o.AddSources...)
	sortSourceRanges(kept)
	return kept
}

// applyDocOverrides drops the removed doc paths, appends the author-added docs,
// and re-sorts by path.
func applyDocOverrides(docs []agentcontext.DocRef, o *agentcontext.OverridesFile) []agentcontext.DocRef {
	remove := sliceToSet(o.RemoveDocs)
	kept := make([]agentcontext.DocRef, 0, len(docs)+len(o.AddDocs))
	for _, d := range docs {
		if !remove[d.Path] {
			kept = append(kept, d)
		}
	}
	kept = append(kept, o.AddDocs...)
	sortDocRefs(kept)
	return kept
}

// markSensitivePaths force-flags every document entry whose path an author listed
// as sensitive and records each path in the caller-owned sensitive set, so the
// fail-closed publish-safety gate passes BECAUSE the entry is flagged. It is the
// author's escape hatch for a path the automatic rules did not catch.
func markSensitivePaths(doc *agentcontext.Document, sensitive map[string]bool, paths []string) {
	if len(paths) == 0 {
		return
	}
	set := sliceToSet(paths)
	for p := range set {
		sensitive[p] = true
	}
	for i := range doc.CompositionRoots {
		if set[doc.CompositionRoots[i].Path] {
			doc.CompositionRoots[i].Sensitive = true
		}
	}
	markRefsSensitive(doc.Capabilities, set)
	markRefsSensitive(doc.Contracts, set)
	markRefsSensitive(doc.Infra, set)
	markRefsSensitive(doc.Migrations, set)
	for i := range doc.RepresentativeSources {
		if set[doc.RepresentativeSources[i].Path] {
			doc.RepresentativeSources[i].Sensitive = true
		}
	}
	for i := range doc.Docs {
		if set[doc.Docs[i].Path] {
			doc.Docs[i].Sensitive = true
		}
	}
	if doc.Config != nil && doc.Config.SchemaRef != nil && set[doc.Config.SchemaRef.Path] {
		doc.Config.SchemaRef.Sensitive = true
	}
}

// markRefsSensitive flags every artifact reference whose path is in set.
func markRefsSensitive(refs []agentcontext.ArtifactRef, set map[string]bool) {
	for i := range refs {
		if set[refs[i].Path] {
			refs[i].Sensitive = true
		}
	}
}

// sliceToSet returns the set of the given strings.
func sliceToSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, it := range items {
		set[it] = true
	}
	return set
}

// digestBytes returns the canonical "sha256:<64hex>" content address of data.
func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// wsRel joins a workspace-relative project path and a project-relative artifact
// path into the canonical forward-slash workspace-relative path the wire uses.
func wsRel(projPath, rel string) string {
	return filepath.ToSlash(filepath.Join(projPath, rel))
}

// canonicalDocumentBytes runs the fail-closed gates on doc and, only when they
// pass, returns its canonical serialization: json.MarshalIndent with two-space
// indentation plus a trailing newline (byte-identical to the protocol's own
// canonical form). A structural failure or a publish-safety violation is an
// exit-2 classified error and NO bytes are produced, so an invalid or unsafe
// document is never written and never passes --check. label is the project id
// recorded in the failure report.
func canonicalDocumentBytes(doc *agentcontext.Document, opts agentcontext.PublishSafetyOptions, label string) ([]byte, error) {
	if diags := agentcontext.ValidateDocument(doc); diag.HasErrors(diags) {
		return nil, contextDocumentError(label, "is invalid", diags)
	}
	if diags := agentcontext.ValidatePublishSafety(doc, opts); diag.HasErrors(diags) {
		return nil, contextDocumentError(label, "failed the publish-safety gate", diags)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("serialize agent-context document: %w", err)
	}
	return append(data, '\n'), nil
}

// contextDocumentError classifies a fail-closed diagnostic set as an exit-2
// configuration error and attaches a machine-readable report for the structured
// failure envelope.
func contextDocumentError(label, what string, diags []diag.Diagnostic) error {
	errs := diag.Errors(diags)
	report := ContextSafetyReport{Project: label}
	for _, d := range errs {
		report.Violations = append(report.Violations, ContextSafetyViolation{Code: d.Code, Field: d.Field, Message: d.Message})
	}
	msg := fmt.Sprintf("agent-context document for %s %s", label, what)
	if len(errs) > 0 {
		msg = fmt.Sprintf("%s: %s", msg, errs[0].Message)
	}
	return shared.WithResultData(protocolcli.Classify(errors.New(msg), protocolcli.ErrInvalidConfig), report)
}

// buildPackedDocument builds, gates, and serializes one project's document,
// returning the document, its workspace-relative target path, and the canonical
// bytes. It is the single build+gate path context pack (write), context check
// (compare), and the MCP agent_context tool (freshness) all share, so none of
// them can disagree about what "current" means. A gate failure returns the same
// exit-2 classified error for every caller — no unsafe or invalid document is
// ever produced.
func buildPackedDocument(ws *workspace.Workspace, p *workspace.Project, revision, cliVersion string) (*agentcontext.Document, string, []byte, error) {
	doc, opts, err := BuildDocument(ws, p, revision, cliVersion)
	if err != nil {
		return nil, "", nil, err
	}
	data, err := canonicalDocumentBytes(doc, opts, p.ID)
	if err != nil {
		return nil, "", nil, err
	}
	rel := filepath.ToSlash(filepath.Join(p.Path, agentcontext.DocumentEmitDir, agentcontext.DocumentFilename))
	return doc, rel, data, nil
}

// packDocument builds, gates, and serializes one project's document, returning
// the workspace-relative target path and the canonical bytes. It is the single
// emit path both pack (which writes it) and check (which compares it) call, so
// the two can never disagree about what "current" means.
func packDocument(ws *workspace.Workspace, p *workspace.Project, revision, cliVersion string) (string, []byte, error) {
	_, rel, data, err := buildPackedDocument(ws, p, revision, cliVersion)
	return rel, data, err
}

// ContextPack aggregates and atomically writes an agent-context document for
// each targeted project. Every emit runs the fail-closed publish-safety gate;
// an unsafe document aborts the run (exit 2) before any file is touched for it.
func ContextPack(ws *workspace.Workspace, projects []*workspace.Project, revision, cliVersion string) (ContextPackReport, error) {
	var report ContextPackReport
	for _, p := range projects {
		rel, data, err := packDocument(ws, p, revision, cliVersion)
		if err != nil {
			return ContextPackReport{}, err
		}
		if err := shared.AtomicWriteFile(filepath.Join(ws.Root, filepath.FromSlash(rel)), data); err != nil {
			return ContextPackReport{}, err
		}
		report.Projects = append(report.Projects, ContextPackArtifact{Project: p.ID, Path: rel})
	}
	return report, nil
}

// ContextCheck regenerates each targeted document in memory and diffs it
// byte-for-byte against the on-disk artifact. A missing or differing document is
// drift. It writes nothing and returns an exit-2 error on drift (with the report
// attached for the failure envelope); a publish-safety failure surfaces as its
// own exit-2 error, so an unsafe document can never pass the freshness gate.
func ContextCheck(ws *workspace.Workspace, projects []*workspace.Project, revision, cliVersion string) (ContextCheckReport, error) {
	report := ContextCheckReport{Outcome: ContextOutcomeClean, Checked: len(projects)}
	for _, p := range projects {
		rel, want, err := packDocument(ws, p, revision, cliVersion)
		if err != nil {
			return ContextCheckReport{}, err
		}
		got, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				report.Drift = append(report.Drift, ContextDriftArtifact{Project: p.ID, Path: rel, Reason: "missing"})
				continue
			}
			return ContextCheckReport{}, fmt.Errorf("read agent-context document %s: %w", rel, err)
		}
		if !bytes.Equal(got, want) {
			report.Drift = append(report.Drift, ContextDriftArtifact{Project: p.ID, Path: rel, Reason: "stale"})
		}
	}
	if len(report.Drift) > 0 {
		report.Outcome = ContextOutcomeDrift
	}
	return report, contextCheckError(report)
}

// contextCheckError maps a drifted report to an exit-2 configuration error with
// the report attached for the structured failure envelope. A clean report is a
// nil error (exit 0).
func contextCheckError(r ContextCheckReport) error {
	if r.Outcome == ContextOutcomeDrift {
		err := protocolcli.Classify(
			fmt.Errorf("context check found %d drifted agent-context document(s); run `putnami context pack`", len(r.Drift)),
			protocolcli.ErrInvalidConfig)
		return shared.WithResultData(err, r)
	}
	return nil
}

// ContextPackCommand is the `putnami context pack [--project <id|name>] [--check]`
// entrypoint: it resolves the target projects, resolves the workspace revision,
// and either writes (pack) or diffs (check) their documents. cliVersion is the
// running CLI version, stamped into every document's provenance.
func ContextPackCommand(wsRoot string, args []string, outputFormat, cliVersion string) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	projects, err := selectContextProjects(ws, args)
	if err != nil {
		return err
	}
	revision := resolveWorkspaceRevision(wsRoot)

	if hasCheckFlag(args) {
		report, checkErr := ContextCheck(ws, projects, revision, cliVersion)
		return renderContextCheck(outputFormat, report, checkErr)
	}
	report, err := ContextPack(ws, projects, revision, cliVersion)
	if err != nil {
		return err
	}
	return renderContextPack(outputFormat, report)
}

// selectContextProjects resolves the projects to operate on. With no --project
// flag, every workspace project is targeted (sorted by id for a deterministic
// emit order); with --project, only the resolved project is. A --project flag
// with a missing value is a usage error (not the flag-absent "all projects"
// default), so `context pack --project --check` never silently packs everything.
func selectContextProjects(ws *workspace.Workspace, args []string) ([]*workspace.Project, error) {
	selector, err := shared.ParseProjectFlag(args)
	if err != nil {
		return nil, err
	}
	if selector == "" {
		projects := append([]*workspace.Project(nil), ws.Projects...)
		sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
		return projects, nil
	}
	proj := shared.ResolveProjectSelector(ws, selector)
	if proj == nil {
		return nil, cmderr.NotFoundf("project not found: %s", selector)
	}
	return []*workspace.Project{proj}, nil
}

// hasCheckFlag reports whether --check is present in args.
func hasCheckFlag(args []string) bool {
	return slices.Contains(args, "--check")
}

// resolveWorkspaceRevision returns the workspace revision recorded in provenance:
// the HEAD commit sha when git can resolve it, or "unknown" otherwise (no
// commits, or not a git checkout). The value is stable across a fixed tree, so
// --check drifts precisely when the revision changes — the intended freshness
// signal — while still working outside a repository.
func resolveWorkspaceRevision(wsRoot string) string {
	out, err := exec.Command("git", "-C", wsRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	rev := strings.TrimSpace(string(out))
	if rev == "" {
		return "unknown"
	}
	return rev
}

// renderContextPack writes the pack result: a success v2 envelope in
// structured mode, or a human summary of the documents written otherwise.
func renderContextPack(outputFormat string, report ContextPackReport) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		_, err := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
			protocolcli.NewResultV2("context pack", report, nil))
		return err
	}
	iox.Fprintf(os.Stdout, "\n  Packed %d agent-context document(s):\n", len(report.Projects))
	for _, a := range report.Projects {
		iox.Fprintf(os.Stdout, "    %s → %s\n", a.Project, a.Path)
	}
	iox.Fprintln(os.Stdout)
	return nil
}

// renderContextCheck writes the check result. In structured mode it emits the
// success v2 envelope on a clean report and emits nothing on drift (the
// dispatcher writes the failure envelope with the report attached via
// WithResultData). In human mode it prints a summary either way. checkErr is
// returned unchanged so the dispatcher maps the process exit code.
func renderContextCheck(outputFormat string, report ContextCheckReport, checkErr error) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		if checkErr == nil {
			_, _ = protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
				protocolcli.NewResultV2("context pack --check", report, nil))
		}
		return checkErr
	}
	if report.Outcome == ContextOutcomeClean {
		iox.Fprintf(os.Stdout, "\n  Agent-context documents are fresh (%d checked, no drift).\n\n", report.Checked)
		return checkErr
	}
	iox.Fprintf(os.Stdout, "\n  Agent-context documents are stale (%d drifted); run `putnami context pack`:\n", len(report.Drift))
	for _, d := range report.Drift {
		iox.Fprintf(os.Stdout, "    drift: %s (%s)\n", d.Path, d.Reason)
	}
	iox.Fprintln(os.Stdout)
	return checkErr
}
