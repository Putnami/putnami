package workspace

import (
	"encoding/json"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Project represents a discovered project within a workspace.
type Project struct {
	ID         string // canonical logical identity (transparent group folders omitted)
	Name       string
	SourceName string // identity before the scope namePattern override
	// Version is the package version the workspace probe reported for this
	// project — what its own native manifest declares. It is INFORMATIONAL:
	// nothing in the job context, in a cache key, or in a release-set member
	// reads it. The version an artifact is stamped with is derived from the git
	// tags of the project's Line (jobs.RunVersions).
	Version      string
	Type         string // "library" or "application" (empty defaults to "application")
	Path         string // relative to workspace root
	Tags         []string
	Dependencies []string // project names this project depends on
	// DependencySources names, per entry of Dependencies, the manifest family
	// that edge was derived from. An entry Dependencies carries and this map
	// does not is wsproto.DependencySourceDeclared: no provider read it from an
	// import.
	//
	// It never changes WHICH edges the graph carries. Scheduling, the cache
	// keys and ImpactDependentsOf read Dependencies exactly as before; this is
	// the answer to "does the build really read that project", which is the
	// question a visibility boundary is about.
	DependencySources map[string]wsproto.DependencySource
	Publish           []string // publish channels ("npm", "docker", etc.)
	Extensions        []string // explicit extension names
	RunsWith          []string // service dependencies for serve mode
	// Registries is this project's effective registry endpoints per ecosystem:
	// the workspace entries, with each entry the project declares replacing the
	// workspace one WHOLE. The entry's shape belongs to the ecosystem profile.
	Registries map[string]json.RawMessage
	// Visibility is the project's declared import boundary, unresolved: empty
	// means the project declared none. Read it through EffectiveVisibility,
	// which applies wsproto.DefaultVisibility.
	Visibility wsproto.Visibility
	// Line is the scope path of this project's nearest ancestor version line,
	// slash-separated and relative to the workspace root. "" is the implicit
	// root line, which a workspace declaring no line block is entirely made of.
	Line   string
	Config *wsproto.ProjectConfig
	// ConfigDiagnostics are the task-tuning findings produced while reading this
	// project's putnami.json. Discovery carries them to the workspace boundary,
	// which is where an invalid authored deadline becomes a load error.
	ConfigDiagnostics []diag.Diagnostic

	// Metadata is the provider-owned metadata the workspace probe reported for
	// this project, namespaced by the extension that produced it. Core carries
	// it — into the job context, and into the project's metadata digest — and
	// never interprets it for identity.
	Metadata map[string]json.RawMessage

	// GeneratedClient is this project's end of a contract edge, read from the
	// generated client manifest committed at its root. Non-nil exactly for a
	// project that IS a generated client target. BuildGraph resolves its
	// ServiceID against the providers' ContractServiceID to derive the edge.
	GeneratedClient *GeneratedClientBinding
	// ContractServiceID is the provider service identity this project's
	// COMMITTED contract declares. Empty for a project that commits none, which
	// is every project that is not a provider — and also a provider whose
	// contract exists only under .gen, because a cold clone cannot read that
	// and a graph must be the same on both trees.
	ContractServiceID string
	// ContractPath is the committed contract that declares ContractServiceID,
	// workspace-relative in slash form. It is set exactly when
	// ContractServiceID is. Change impact reads it: the provider's contract
	// edge fires only when this file is one of the changed files.
	ContractPath string
	// ContractSHA256 is the lowercase hex sha256 of ContractPath's bytes as
	// the workspace loaded them, the digest a client generated from that
	// contract records as its contractSha256. It is trace evidence only: no
	// identity and no cache key reads it.
	ContractSHA256 string

	// ActivatedScope is true when this project is a scope (has includes) that
	// also opted into self-activation via "activate": true.
	ActivatedScope bool
	// ScopeIncludes lists the canonical IDs of projects directly included by
	// this activated scope. Used to wire implicit scope→child edges in the
	// dependency graph so workspace-level artifacts (infra, etc.) run before
	// their workloads. Empty unless ActivatedScope is true.
	ScopeIncludes []string

	// Scope is the resolved breadcrumb chain contribution, captured once at
	// discovery so applying a probe view stays a pure function of (authored
	// config, scope chain, merged view). Re-reading the chain on every
	// application would make identity depend on how many times a run happened
	// to probe.
	Scope ScopeContribution
}

// GeneratedClientBinding is a generated client target's end of a contract
// edge: the provider service its code was generated from, and the exact
// contract bytes it was generated at.
//
// The contract digest is the WHOLE provider-side identity of a generated
// target. Its provider's sources are read by no action of it, so they never
// reach its cache key; the contract does, and it moves exactly when the
// generated code has to.
type GeneratedClientBinding struct {
	// ServiceID is the provider service identity the manifest names.
	ServiceID string
	// ContractSHA256 is the digest of the contract bytes the target was
	// generated at.
	ContractSHA256 string
	// Language is the target language the manifest declares ("go", "ts").
	Language string
	// ManifestPath is the committed manifest, workspace-relative and
	// slash-separated.
	ManifestPath string
}

// ScopeContribution is what the breadcrumb chain contributes to a project.
type ScopeContribution struct {
	// Name is the namePattern's answer for this directory, used only when the
	// project declares no identity of its own.
	Name string
	// Tags are the scope's tags, applied only when the project has none.
	Tags []string
	// Extensions are the scope's extensions, applied only when the project has
	// none.
	Extensions []string
	// Distribution is the distribution block of the deepest scope that
	// declares one, applied only when the project declares none. Nil when no
	// scope declares one.
	Distribution *wsproto.DistributionConfig
	// ConfigPaths are the scope putnami.json files the chain merged to produce
	// the fields above, workspace-relative and slash-separated, shallowest
	// first. They make the inheritance visible to change impact: a project is
	// selected when one of them changes, the way it is when a declared file
	// input changes. Empty for a project under no scope.
	ConfigPaths []string
}
