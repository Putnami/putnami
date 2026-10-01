package capabilities

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// ProtocolVersionV2 is the durable capability-manifest version with uniform
// contribution identities and precise provenance. It is what current producers
// stamp; ProtocolVersion stays frozen at v1 so existing documents keep
// validating and resolving.
const ProtocolVersionV2 = 2

// ContributionKind is the closed v2 vocabulary for concrete contributions.
type ContributionKind string

// Supported v2 contribution kinds form a closed wire vocabulary.
const (
	ContributionKindConfig             ContributionKind = "config"
	ContributionKindSchema             ContributionKind = "schema"
	ContributionKindDiscoverer         ContributionKind = "discoverer"
	ContributionKindMigration          ContributionKind = "migration"
	ContributionKindInfra              ContributionKind = "infra"
	ContributionKindHealth             ContributionKind = "health"
	ContributionKindLifecycle          ContributionKind = "lifecycle"
	ContributionKindPackage            ContributionKind = "package"
	ContributionKindRequiredCapability ContributionKind = "requiredCapability"
	ContributionKindDomainAccess       ContributionKind = "domainAccess"
)

// ValidContributionKinds identifies every supported v2 contribution kind.
var ValidContributionKinds = map[ContributionKind]bool{
	ContributionKindConfig:             true,
	ContributionKindSchema:             true,
	ContributionKindDiscoverer:         true,
	ContributionKindMigration:          true,
	ContributionKindInfra:              true,
	ContributionKindHealth:             true,
	ContributionKindLifecycle:          true,
	ContributionKindPackage:            true,
	ContributionKindRequiredCapability: true,
	ContributionKindDomainAccess:       true,
}

// ContributionIdentity is the timeless, owner-scoped identity shared by all
// v2 contribution kinds. The containing manifest project is intentionally not
// part of this tuple.
type ContributionIdentity struct {
	// OwnerProject is the semantic project that owns the contribution.
	OwnerProject string `json:"ownerProject"`
	// Kind selects the contribution's closed wire category.
	Kind ContributionKind `json:"kind"`
	// Subkind is the kind-specific discriminator when that kind requires one.
	Subkind string `json:"subkind,omitempty"`
	// Key is the stable owner-scoped identity within Kind and Subkind.
	Key string `json:"key"`
}

// ContributionReference is the canonical reference shape used by feature
// evidence. It is exactly a contribution identity.
type ContributionReference = ContributionIdentity

// LocationRoot selects the root against which a provenance path is resolved.
type LocationRoot string

// Supported provenance roots make declaration and artifact paths unambiguous.
const (
	LocationRootWorkspace LocationRoot = "workspace"
	LocationRootProject   LocationRoot = "project"
	LocationRootPackage   LocationRoot = "package"
)

// ValidLocationRoots identifies every supported provenance location root.
var ValidLocationRoots = map[LocationRoot]bool{
	LocationRootWorkspace: true,
	LocationRootProject:   true,
	LocationRootPackage:   true,
}

// DeclarationLocation names the source declaration that caused a contribution.
type DeclarationLocation struct {
	// Root selects the source root against which Path is resolved.
	Root LocationRoot `json:"root"`
	// Path is the canonical relative path to the declaring source file.
	Path string `json:"path"`
	// Symbol names the declaration within Path when one is available.
	Symbol string `json:"symbol,omitempty"`
}

// ArtifactLocation names a generated/lowered output associated with a
// contribution. Digest, when present, is a lower-case SHA-256 content address.
type ArtifactLocation struct {
	// Root selects the source root against which Path is resolved.
	Root LocationRoot `json:"root"`
	// Path is the canonical relative path to the generated artifact.
	Path string `json:"path"`
	// Digest is the artifact's lower-case SHA-256 content address when known.
	Digest string `json:"digest,omitempty"`
}

// ProvenanceV2 separates producer metadata, the precise declaration, and
// generated artifacts. Consumers that need freshness or integrity guarantees
// compute them from the indexed source revision instead of embedding volatile
// source-state hashes in this durable manifest.
type ProvenanceV2 struct {
	// Project is the semantic project that contributed the entry.
	Project string `json:"project"`
	// Package is the framework or dependency package that declared the entry.
	Package string `json:"package,omitempty"`
	// Version accepts manifests emitted before resolved dependency state moved to
	// build/index snapshots. Canonicalization clears it, so producers never emit
	// it in the durable manifest.
	Version string `json:"version,omitempty"`
	// SourceKind identifies whether the entry is framework, manual, or generated.
	SourceKind SourceKind `json:"sourceKind"`
	// LegacySourceBinding accepts manifests emitted before source integrity moved
	// to indexers. Canonicalization clears it, so producers never emit it again.
	LegacySourceBinding string `json:"sourceBinding,omitempty"`
	// Declaration identifies the precise authored declaration.
	Declaration DeclarationLocation `json:"declaration"`
	// Artifacts lists generated or lowered outputs associated with the entry.
	Artifacts []ArtifactLocation `json:"artifacts,omitempty"`
}

// ManifestV2 is the explicit v2 wire document. Field and collection order are
// deliberate and mirrored by TypeScript.
type ManifestV2 struct {
	// Schema is the optional URI of the JSON schema describing this manifest.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the capabilities wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Project is the semantic Putnami project described by this manifest.
	Project string `json:"project"`
	// ConfigDefinitions lists configuration blocks contributed by the project.
	ConfigDefinitions []ConfigDefinitionV2 `json:"configDefinitions,omitempty"`
	// Schemas lists route, OpenAPI, and proto schema contributions.
	Schemas []SchemaContributionV2 `json:"schemas,omitempty"`
	// Discoverers lists source and configuration discovery contributions.
	Discoverers []DiscovererV2 `json:"discoverers,omitempty"`
	// Migrations lists migration bundles and their logical datasources.
	Migrations []MigrationBundleV2 `json:"migrations,omitempty"`
	// InfraRequirements lists infrastructure resources required by the project.
	InfraRequirements []InfraRequirementV2 `json:"infraRequirements,omitempty"`
	// HealthContributors lists components contributing to platform probes.
	HealthContributors []HealthContributorV2 `json:"healthContributors,omitempty"`
	// LifecycleHooks lists registered startup and shutdown hooks.
	LifecycleHooks []LifecycleHookV2 `json:"lifecycleHooks,omitempty"`
	// Packages lists stable framework or dependency package identities.
	Packages []PackageV2 `json:"packages,omitempty"`
	// PackageVersions accepts manifests emitted before resolved dependency state
	// moved to build/index snapshots. Canonicalization migrates entries to
	// Packages and clears this volatile inventory.
	PackageVersions []PackageVersionV2 `json:"packageVersions,omitempty"`
	// RequiredCapabilities lists logical capabilities and their required providers.
	RequiredCapabilities []RequiredCapabilityV2 `json:"requiredCapabilities,omitempty"`
	// DomainAccess lists the Domain Access & Replication Contracts a framework
	// component enforces at run time. It is the last collection because it is the
	// newest; readers of an older manifest simply see it absent.
	DomainAccess []DomainAccessV2 `json:"domainAccess,omitempty"`
}

// ConfigDefinitionV2 is a v2 configuration contribution.
type ConfigDefinitionV2 struct {
	// Identity is the exact owner-scoped identity of the configuration block.
	Identity ContributionIdentity `json:"identity"`
	// Path is the canonical dotted configuration-block path.
	Path string `json:"path"`
	// Fields describes the configuration fields declared by the block.
	Fields []ConfigField `json:"fields,omitempty"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// SchemaContributionV2 is a v2 schema contribution.
type SchemaContributionV2 struct {
	// Identity is the exact owner-scoped identity of the schema contribution.
	Identity ContributionIdentity `json:"identity"`
	// Name is the stable schema or module-registry identity.
	Name string `json:"name"`
	// Kind identifies whether this is a route, OpenAPI, or proto schema.
	Kind SchemaKind `json:"kind"`
	// Path is the project-relative schema or generated activation-module path.
	Path string `json:"path,omitempty"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// DiscovererV2 is a v2 discoverer contribution.
type DiscovererV2 struct {
	// Identity is the exact owner-scoped identity of the discoverer.
	Identity ContributionIdentity `json:"identity"`
	// Name is the stable discoverer or module-registry identity.
	Name string `json:"name"`
	// Kind identifies whether the discoverer scans source or configuration.
	Kind DiscovererKind `json:"kind"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// MigrationBundleV2 is a v2 migration contribution with an explicit wire kind.
type MigrationBundleV2 struct {
	// Identity is the exact owner-scoped identity of the migration bundle.
	Identity ContributionIdentity `json:"identity"`
	// Name is the migration bundle namespace.
	Name string `json:"name"`
	// Kind is the producer-defined migration format discriminator.
	Kind string `json:"kind"`
	// Datasource is the logical datasource targeted by the bundle.
	Datasource string `json:"datasource,omitempty"`
	// Digest is the bundle content address when the producer can compute it.
	Digest string `json:"digest,omitempty"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// InfraRequirementV2 is a v2 infrastructure contribution.
type InfraRequirementV2 struct {
	// Identity is the exact owner-scoped identity of the infra requirement.
	Identity ContributionIdentity `json:"identity"`
	// Name is the logical infrastructure resource name.
	Name string `json:"name"`
	// Kind identifies the required infrastructure resource class.
	Kind InfraKind `json:"kind"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// HealthContributorV2 is a v2 health-probe contribution.
type HealthContributorV2 struct {
	// Identity is the exact owner-scoped identity of the probe contributor.
	Identity ContributionIdentity `json:"identity"`
	// Name is the stable contributor identity within the probe.
	Name string `json:"name"`
	// Probe identifies whether the contributor feeds health, liveness, or readiness.
	Probe ProbeKind `json:"probe"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// LifecycleHookV2 is a v2 lifecycle contribution.
type LifecycleHookV2 struct {
	// Identity is the exact owner-scoped identity of the lifecycle hook.
	Identity ContributionIdentity `json:"identity"`
	// Name is the stable lifecycle-hook identity.
	Name string `json:"name"`
	// Phase identifies whether the hook starts or stops a component.
	Phase LifecyclePhase `json:"phase"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// PackageV2 is a stable package-identity contribution. Exact resolved versions
// belong to build/index snapshots rather than the durable capability manifest.
type PackageV2 struct {
	// Identity is the exact owner-scoped identity of the package contribution.
	Identity ContributionIdentity `json:"identity"`
	// Package is the canonical framework or dependency package name.
	Package string `json:"package"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// PackageVersionV2 is the historical v2 package-version contribution retained
// for strict reads of manifests emitted before versions moved to snapshots.
type PackageVersionV2 struct {
	// Identity is the exact owner-scoped identity of the package contribution.
	Identity ContributionIdentity `json:"identity"`
	// Package is the canonical framework or dependency package name.
	Package string `json:"package"`
	// Version is the resolved package version recorded by historical producers.
	Version string `json:"version"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// RequiredCapabilityV2 is a v2 capability-requirement contribution.
type RequiredCapabilityV2 struct {
	// Identity is the exact owner-scoped identity of the logical capability.
	Identity ContributionIdentity `json:"identity"`
	// Name is the logical capability being declared.
	Name string `json:"name"`
	// Requires lists the non-empty provider-kind set the capability depends on.
	Requires []CapabilityKind `json:"requires"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// DomainAccessV2 is one Domain Access & Replication Contract a framework
// component enforces at run time.
//
// It is EVIDENCE, not authority. The contract itself is declared and reviewed in
// a `putnami.architecture.json`; this row states that a running component was
// configured with it, so a checker can tell a declared import that nothing
// implements from an implemented one nobody declared. It never creates a
// permission (protocols/architecture, ADR 0001).
//
// The vocabulary members below — Mode, Status, transport kinds and
// availabilities, and every enforced parameter — are carried VERBATIM from the
// architecture contract and are opaque here. This protocol deliberately does not
// restate what a mode or a failure behavior means: `protocols/architecture` owns
// that, and a second copy of the vocabulary is exactly the drift a checker
// comparing these two documents exists to catch.
type DomainAccessV2 struct {
	// Identity is the exact owner-scoped identity of the enforced contract; Key
	// is the import ID.
	Identity ContributionIdentity `json:"identity"`
	// Import is the architecture import ID the component enforces.
	Import string `json:"import"`
	// Mode is the declared access mode, carried verbatim.
	Mode string `json:"mode"`
	// Status is the declared lifecycle status of the import, carried verbatim.
	Status string `json:"status"`
	// Transports lists the carriers the contract declares, each with the role it
	// plays: a projection has bootstrap and updates, the other modes have one.
	Transports []DomainAccessTransportV2 `json:"transports,omitempty"`
	// Enforced records the contract parameters the component actually applies.
	Enforced *DomainAccessEnforcementV2 `json:"enforced,omitempty"`
	// Provenance binds the contribution to its source declaration.
	Provenance ProvenanceV2 `json:"provenance"`
}

// DomainAccessTransportV2 is one declared carrier of an enforced contract.
type DomainAccessTransportV2 struct {
	// Role is the carrier's part in the contract: transport, bootstrap, updates.
	Role string `json:"role"`
	// Kind is the carrier category, carried verbatim from the contract.
	Kind string `json:"kind"`
	// Contract names the concrete API, event, file, or in-process contract.
	Contract string `json:"contract,omitempty"`
	// Availability states whether that carrier is usable yet, carried verbatim.
	Availability string `json:"availability"`
}

// DomainAccessEnforcementV2 records the contract parameters a component applies.
// Every member is optional because the modes carry different halves of the
// contract: a reference declares none of them, a projection declares them all.
type DomainAccessEnforcementV2 struct {
	// MaxStaleness is the declared freshness bound.
	MaxStaleness string `json:"maxStaleness,omitempty"`
	// OnMissing is the declared behavior for an absent fact.
	OnMissing string `json:"onMissing,omitempty"`
	// OnStale is the declared behavior past the freshness bound.
	OnStale string `json:"onStale,omitempty"`
	// Ordering is the declared update-ordering strategy.
	Ordering string `json:"ordering,omitempty"`
	// LateEvents is the declared handling of an out-of-order update.
	LateEvents string `json:"lateEvents,omitempty"`
	// Deletion is the declared deletion strategy.
	Deletion string `json:"deletion,omitempty"`
	// Writer is the sole component the local model allows to update copied facts.
	Writer string `json:"writer,omitempty"`
	// Rebuild is the declared reconstruction strategy.
	Rebuild string `json:"rebuild,omitempty"`
}

// ManifestDocument preserves the exact source protocol version while exposing
// the corresponding strict wire type.
type ManifestDocument struct {
	// ProtocolVersion is the exact source protocol version.
	ProtocolVersion int
	// V1 holds the strict v1 manifest when ProtocolVersion is one.
	V1 *Manifest
	// V2 holds the strict v2 manifest when ProtocolVersion is two.
	V2 *ManifestV2
}

// CanonicalManifestV2 returns a deep-enough sorted copy suitable for canonical
// serialization. Caller-owned slices are never reordered.
//
// Canonicalization is the emission projection, and it is deliberately lossy in
// exactly one direction: content that is not a durable property of THIS
// manifest's project is dropped rather than emitted. It already drops resolved
// dependency versions and legacy source bindings; it also scopes Packages to
// the project's direct capability surface (see ScopePackagesToContributionsV2
// and doc/adr/0001-committed-manifest-scope.md). A producer therefore cannot
// emit — and a committed manifest cannot carry — a reachable-module closure
// that turns an unrelated dependency edit into a re-stamp of this file.
func CanonicalManifestV2(in *ManifestV2) *ManifestV2 {
	return canonicalManifestV2(in, true, true, true)
}

// canonicalManifestV2 canonicalizes in. The three switches separate EMISSION
// (all true, via CanonicalManifestV2) from the read paths that must observe the
// document as written: validation reports diagnostics for every entry a
// producer actually emitted, and reference resolution must still resolve
// historical entries, so neither drops content.
func canonicalManifestV2(in *ManifestV2, stripLegacyBinding, stripLegacyVersions, scopePackages bool) *ManifestV2 {
	if in == nil {
		return nil
	}
	out := *in
	out.ConfigDefinitions = canonicalV2Slice(in.ConfigDefinitions, func(v ConfigDefinitionV2) ContributionIdentity { return v.Identity })
	out.Schemas = canonicalV2Slice(in.Schemas, func(v SchemaContributionV2) ContributionIdentity { return v.Identity })
	out.Discoverers = canonicalV2Slice(in.Discoverers, func(v DiscovererV2) ContributionIdentity { return v.Identity })
	out.Migrations = canonicalV2Slice(in.Migrations, func(v MigrationBundleV2) ContributionIdentity { return v.Identity })
	out.InfraRequirements = canonicalV2Slice(in.InfraRequirements, func(v InfraRequirementV2) ContributionIdentity { return v.Identity })
	out.HealthContributors = canonicalV2Slice(in.HealthContributors, func(v HealthContributorV2) ContributionIdentity { return v.Identity })
	out.LifecycleHooks = canonicalV2Slice(in.LifecycleHooks, func(v LifecycleHookV2) ContributionIdentity { return v.Identity })
	packages := append([]PackageV2(nil), in.Packages...)
	if stripLegacyVersions {
		for _, legacy := range in.PackageVersions {
			packages = append(packages, PackageV2{Identity: legacy.Identity, Package: legacy.Package, Provenance: legacy.Provenance})
		}
		out.PackageVersions = nil
	} else {
		out.PackageVersions = canonicalV2Slice(in.PackageVersions, func(v PackageVersionV2) ContributionIdentity { return v.Identity })
	}
	if scopePackages {
		packages = scopePackagesToContributions(in, packages)
	}
	out.Packages = canonicalV2Slice(packages, func(v PackageV2) ContributionIdentity { return v.Identity })
	out.RequiredCapabilities = canonicalV2Slice(in.RequiredCapabilities, func(v RequiredCapabilityV2) ContributionIdentity { return v.Identity })
	out.DomainAccess = canonicalV2Slice(in.DomainAccess, func(v DomainAccessV2) ContributionIdentity { return v.Identity })

	canonicalize := func(p *ProvenanceV2) {
		if stripLegacyBinding {
			p.LegacySourceBinding = ""
		}
		if stripLegacyVersions {
			p.Version = ""
		}
		p.Artifacts = append([]ArtifactLocation(nil), p.Artifacts...)
		sort.Slice(p.Artifacts, func(i, j int) bool {
			if p.Artifacts[i].Root != p.Artifacts[j].Root {
				return p.Artifacts[i].Root < p.Artifacts[j].Root
			}
			if p.Artifacts[i].Path != p.Artifacts[j].Path {
				return p.Artifacts[i].Path < p.Artifacts[j].Path
			}
			return p.Artifacts[i].Digest < p.Artifacts[j].Digest
		})
	}
	for i := range out.ConfigDefinitions {
		canonicalize(&out.ConfigDefinitions[i].Provenance)
	}
	for i := range out.Schemas {
		canonicalize(&out.Schemas[i].Provenance)
	}
	for i := range out.Discoverers {
		canonicalize(&out.Discoverers[i].Provenance)
	}
	for i := range out.Migrations {
		canonicalize(&out.Migrations[i].Provenance)
	}
	for i := range out.InfraRequirements {
		canonicalize(&out.InfraRequirements[i].Provenance)
	}
	for i := range out.HealthContributors {
		canonicalize(&out.HealthContributors[i].Provenance)
	}
	for i := range out.LifecycleHooks {
		canonicalize(&out.LifecycleHooks[i].Provenance)
	}
	for i := range out.Packages {
		canonicalize(&out.Packages[i].Provenance)
	}
	for i := range out.PackageVersions {
		canonicalize(&out.PackageVersions[i].Provenance)
	}
	for i := range out.RequiredCapabilities {
		canonicalize(&out.RequiredCapabilities[i].Provenance)
		out.RequiredCapabilities[i].Requires = append([]CapabilityKind(nil), out.RequiredCapabilities[i].Requires...)
		sort.Slice(out.RequiredCapabilities[i].Requires, func(a, b int) bool {
			return out.RequiredCapabilities[i].Requires[a] < out.RequiredCapabilities[i].Requires[b]
		})
	}
	for i := range out.DomainAccess {
		canonicalize(&out.DomainAccess[i].Provenance)
		// Transports are sorted by ROLE, which is their identity within one
		// contract: a projection has exactly one bootstrap and one updates
		// carrier, so the order they were listed in carries no meaning.
		out.DomainAccess[i].Transports = append([]DomainAccessTransportV2(nil), out.DomainAccess[i].Transports...)
		sort.Slice(out.DomainAccess[i].Transports, func(a, b int) bool {
			return out.DomainAccess[i].Transports[a].Role < out.DomainAccess[i].Transports[b].Role
		})
		// Deep-copy through the SORTED element: out was already reordered, so
		// indexing the input here would attach another entry's parameters.
		if out.DomainAccess[i].Enforced != nil {
			enforced := *out.DomainAccess[i].Enforced
			out.DomainAccess[i].Enforced = &enforced
		}
	}
	return &out
}

func canonicalV2Slice[T any](in []T, identity func(T) ContributionIdentity) []T {
	out := append([]T(nil), in...)
	sort.Slice(out, func(i, j int) bool { return CompareContributionIdentity(identity(out[i]), identity(out[j])) < 0 })
	return out
}

// CapabilitySurfaceV2 returns the direct capability surface of m: the project
// itself, every owner project that declares a contribution in m, and every
// package those contributions were declared by. It is derived from m alone, so
// any reader can recompute it and check a manifest against it.
//
// This is the set a COMMITTED manifest may name in packages[]. The alternative
// — the reachable module closure a workload links — is workspace state: adding a
// dependency edge anywhere in the graph rewrites it in every workload's tracked
// manifest at once. The closure is still recorded, but as ephemeral build
// evidence (the scheduler's .gen stamp), never as committed content.
//
// Package contributions are excluded from the derivation on purpose: a package
// entry may not justify itself, or the closure would be self-certifying.
func CapabilitySurfaceV2(m *ManifestV2) map[string]bool {
	surface := map[string]bool{}
	if m == nil {
		return surface
	}
	add := func(values ...string) {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				surface[value] = true
			}
		}
	}
	add(m.Project)
	for _, v := range m.ConfigDefinitions {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.Schemas {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.Discoverers {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.Migrations {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.InfraRequirements {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.HealthContributors {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.LifecycleHooks {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.RequiredCapabilities {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	for _, v := range m.DomainAccess {
		add(v.Identity.OwnerProject, v.Provenance.Project, v.Provenance.Package)
	}
	return surface
}

// PackageWithinSurfaceV2 reports whether one package contribution belongs to
// surface — true when either the owner project or the named package is part of
// it. Both are checked because producers label package entries either way: the
// workload may own an entry naming a dependency package, or the dependency may
// own the entry naming itself.
func PackageWithinSurfaceV2(surface map[string]bool, entry PackageV2) bool {
	return surface[entry.Identity.OwnerProject] || surface[entry.Package] || surface[entry.Provenance.Project]
}

// ScopePackagesToContributionsV2 returns the package contributions of m that
// fall within its capability surface, in input order. It is the projection
// canonical emission applies, exposed so a checker (`putnami doctor`) can report
// the same subset decision on a committed manifest instead of re-deriving it.
//
// The rule is a SUBSET rule: emission never invents a package entry, it only
// declines to carry one whose owner contributes nothing to this manifest.
func ScopePackagesToContributionsV2(m *ManifestV2) []PackageV2 {
	if m == nil {
		return nil
	}
	return scopePackagesToContributions(m, m.Packages)
}

func scopePackagesToContributions(m *ManifestV2, packages []PackageV2) []PackageV2 {
	surface := CapabilitySurfaceV2(m)
	out := make([]PackageV2, 0, len(packages))
	for _, entry := range packages {
		if PackageWithinSurfaceV2(surface, entry) {
			out = append(out, entry)
		}
	}
	return out
}

// CompareContributionIdentity compares the complete tuple bytewise. Go string
// comparison is bytewise for UTF-8 strings, matching the protocol contract.
func CompareContributionIdentity(a, b ContributionIdentity) int {
	for _, pair := range [][2]string{{a.OwnerProject, b.OwnerProject}, {string(a.Kind), string(b.Kind)}, {a.Subkind, b.Subkind}, {a.Key, b.Key}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// MarshalManifestV2 renders canonical v2 JSON with two-space indentation and
// one trailing newline.
func MarshalManifestV2(m *ManifestV2) ([]byte, error) {
	data, err := json.MarshalIndent(CanonicalManifestV2(m), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func canonicalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func equalCanonical(a, b []byte) bool { return bytes.Equal(a, b) }
