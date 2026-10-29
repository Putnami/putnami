package collaboration

import "encoding/json"

// ProviderRequest is the single JSON document the orchestrator writes to a
// provider's standard input. The provider answers with one Response document
// on its standard output and nothing else.
type ProviderRequest struct {
	// Arguments is the operation's validated request document.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// WorkspaceRoot and ExtensionRoot let the provider resolve its local
	// configuration and assets.
	WorkspaceRoot string `json:"workspaceRoot"`
	ExtensionRoot string `json:"extensionRoot"`
	// WorkspaceProjects is the COMPLETE resolved workspace membership, in
	// canonical project-id order. Present exactly when the called operation
	// declares workspaceSelection; omitted otherwise.
	//
	// It is the same fact protocols/job publishes under the same member name,
	// for the same reason: what a provider may CLAIM about the workspace is
	// only provable from the whole membership, and a consumer given a subset
	// reports a real member as absent. A provider call has no job context to
	// carry it, so it travels here.
	WorkspaceProjects []ProjectRef `json:"workspaceProjects,omitempty"`
	// Selection is the projection the operation's canonical selection
	// arguments resolved to, produced by the orchestrator's own resolver.
	// Present under the same condition as WorkspaceProjects.
	//
	// The two are not redundant. This says what the CALLER asked for and what
	// it resolved to; the membership says what the workspace CONTAINS. A
	// provider told it covers three projects cannot otherwise tell a
	// deliberate narrowing from a whole-workspace run of a three-project tree.
	Selection *Selection `json:"selection,omitempty"`
	// Provider names the operation the orchestrator routed through the
	// workspace binding. Serve dispatches on it, and refuses a request without
	// it: a provider is called only through a binding.
	Provider *ProviderCall `json:"provider"`
}

// ProviderCall states which collaboration operation a provider request
// performs, under which contract version, and carries the binding's provider
// settings verbatim. The orchestrator writes it from the workspace binding.
type ProviderCall struct {
	// Contract is the contract name: "tasks", "proposals", "memory" or
	// "missions".
	Contract string `json:"contract"`
	// Version is the contract version the workspace bound.
	Version int `json:"version"`
	// Operation is the operation name inside the contract version.
	Operation string `json:"operation"`
	// Settings is the binding's settings object, absent when the binding
	// declares none. It never carries a credential.
	Settings json.RawMessage `json:"settings,omitempty"`
}

// ProjectRef is one workspace member on a provider request: the
// orchestrator's RESOLVED answer for that project, never a re-reading of any
// manifest.
//
// It is a fuller projection than protocols/job's ProjectRef on purpose. A job
// subprocess acts on its own project and receives the authored facts of that
// one project alone; a provider that answers about the workspace reports ON
// other projects, so the facts it reports have to travel. The alternative — a
// provider that opens sibling putnami.json files itself — is the second
// workspace reader this member exists to prevent.
type ProjectRef struct {
	// ID is the project's canonical logical identity, transparent group
	// folders omitted.
	ID string `json:"id,omitempty"`
	// Name is the resolved project name.
	Name string `json:"name"`
	// SourceName is the identity the project DECLARED, before a scope
	// namePattern override. Name is the resolved answer after it.
	SourceName string `json:"sourceName,omitempty"`
	// Version is the RESOLVED effective version: project > nearest scope >
	// workspace. Absent means nobody said, and a consumer must then match by
	// name alone rather than invent one.
	Version string `json:"version,omitempty"`
	// Type is the resolved classification ("library", "application"). Absent
	// means the producer resolved none; a consumer that needs a value applies
	// the workspace protocol's own default rather than a local guess.
	Type string `json:"type,omitempty"`
	// Path is the project directory relative to the workspace root.
	Path string `json:"path"`
	// Tags, Publish, Extensions and RunsWith are the resolved authored
	// members, after the scope chain has contributed its defaults.
	Tags       []string `json:"tags,omitempty"`
	Publish    []string `json:"publish,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	RunsWith   []string `json:"runsWith,omitempty"`
	// Dependencies are the ids of the projects this one depends on DIRECTLY,
	// in the order the orchestrator resolved them. Out-of-workspace edges are
	// omitted: there is no member to point at. Absence means the project
	// declares none.
	Dependencies []string `json:"dependencies,omitempty"`
	// Config is the project's authored putnami.json as the orchestrator
	// parsed it. It stays RAW here: the workspace protocol owns the shape.
	Config json.RawMessage `json:"config,omitempty"`
}

// Selection is the resolved project selection of one provider call.
//
// Each of its members marshals byte-identically to the member of the same
// name in protocols/job's Selection, which marshals byte-identically to the
// CLI's own ResolvedSelection: one resolver answers the planned jobs, the
// interactive commands and the providers, so the surfaces cannot disagree
// about what a narrowing meant. A provider reads no reuse or release-set
// member, so this type carries none. A drift test pins the shared members.
type Selection struct {
	// Mode is how the projection was chosen: one of SelectionModes.
	Mode string `json:"mode"`
	// Scoped distinguishes a narrowed call from the whole-workspace default.
	// A provider that reports a verdict for the workspace may only claim to
	// have covered it when Scoped is false.
	Scoped bool `json:"scoped"`
	// Baseline is the ref `impacted` actually resolved to, empty otherwise.
	Baseline string `json:"baseline,omitempty"`
	// BaselineSource is the resolution tier that produced Baseline, so a
	// consumer can distrust a stale fallback ref the same way the call can.
	BaselineSource string `json:"baselineSource,omitempty"`
	// ProjectIDs are the selected projects' canonical ids, sorted.
	ProjectIDs []string `json:"projects"`
	// EmptyImpact records the legitimate no-op: `impacted` resolved cleanly
	// and nothing changed. It is a success, not a "no projects matched" error.
	EmptyImpact bool `json:"emptyImpact,omitempty"`
}

// Selection modes: the closed vocabulary a resolved Selection.Mode takes,
// identical to protocols/job's.
const (
	// SelectionModeAll is the unscoped whole-workspace projection.
	SelectionModeAll = "all"
	// SelectionModeProjects is an explicit selector, with or without filters.
	SelectionModeProjects = "projects"
	// SelectionModeImpacted is the `impacted` projection.
	SelectionModeImpacted = "impacted"
)

// SelectionModes is the closed selection-mode vocabulary in canonical
// (sorted) order.
var SelectionModes = []string{
	SelectionModeAll,
	SelectionModeImpacted,
	SelectionModeProjects,
}
