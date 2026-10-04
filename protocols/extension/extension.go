// Package extension provides types and loading logic for putnami extension
// manifests (putnami.extension.json).
package extension

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ManifestFilename is the name of the extension manifest file.
const ManifestFilename = "putnami.extension.json"

// Extension-manifest protocol versions. The manifest carries no on-the-wire
// version field — the strict parser accepts a single shape and rejects unknown
// fields — so these constants are the single anchor a bump has to move.
// Manifest.Version is the extension's own semver, not the protocol version.
// ProtocolVersion is pinned by conformance_test.go so any bump is intentional;
// bumping requires a migration story.
const (
	// ProtocolVersionV2 is the inferred-output contract: a task's filesystem
	// footprint is discovered by capturing a directory and subtracting a
	// baseline, and source mutation is a cache-private result marker.
	ProtocolVersionV2 = 2
	// ProtocolVersionV3 adds the task contract — declared outputs, effects, and
	// the source-mutation flag (see task_contract.go) — plus the runtime and
	// workspace lifecycle declarations. It is ADDITIVE: a manifest without
	// this vocabulary behaves exactly as v2 did, and tasks may still migrate
	// their declarations one at a time.
	// ManifestProtocolVersion reports which one a parsed manifest exercises.
	ProtocolVersionV3 = 3
	// ProtocolVersion is the version this package implements.
	ProtocolVersion = ProtocolVersionV3
)

// Manifest is the root of putnami.extension.json.
type Manifest struct {
	Schema  string `json:"$schema,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	// CLIContract is the CLI ↔ extension contract version this manifest was
	// validated against: RequiredCLIContract at package time, the lowest
	// contract whose vocabulary covers the manifest. It is EARNED, not
	// claimed: the package/publish job validates the staged manifest strictly
	// and stamps the field on success. Absent (0) means the pre-registry world,
	// which since contract 3 no longer loads unless the manifest declares no
	// contract surface at all (DeclaresContractSurface). See LoadManifest for
	// the enforce/reject negotiation.
	CLIContract      int               `json:"cliContract,omitempty"`
	ExtensionDeps    Dependencies      `json:"extensionDependencies,omitempty"`
	WorkspaceDevDeps map[string]string `json:"workspaceDevDependencies,omitempty"`
	AutoServe        *bool             `json:"autoServe,omitempty"`
	// Runtime declares the extension's OWN executable and, optionally, how to
	// build it from source. It is the first of the three lifecycle primitives
	// (see runtime.go): the CLI stops classifying extensions by name and asks
	// the manifest instead. Nil means the extension has no prepared runtime and
	// its tasks keep naming their commands explicitly.
	Runtime *RuntimeDefinition `json:"runtime,omitempty"`
	// Workspace declares the workspace adapter: which paths mark a project this
	// extension owns, which files are its metadata inputs, what to skip, and the
	// task that synchronizes native manifests. It is the second lifecycle
	// primitive (see workspace.go). Nil means the extension contributes nothing
	// to project discovery.
	Workspace     *WorkspaceAdapter                 `json:"workspace,omitempty"`
	Hooks         *ManifestHooks                    `json:"hooks,omitempty"`
	Contracts     *ContractsDefinition              `json:"contracts,omitempty"`
	Commands      map[string]CommandDefinition      `json:"commands,omitempty"`
	CommandGroups map[string]CommandGroupDefinition `json:"commandGroups,omitempty"`
	Tasks         map[string]TaskDefinition         `json:"tasks,omitempty"`
	// Tools declares MCP tools contributed by this extension. Each tool runs
	// in an extension-owned subprocess, so the CLI never imports extension
	// code or takes ownership of extension auth/network behavior.
	Tools map[string]ToolDefinition `json:"tools,omitempty"`
	// AgentContent declares the agent instructions this extension ships under
	// its own version: skills, worker profiles, references and helpers with
	// their host adapters (see agent_content.go). Nil means the extension
	// contributes no agent content. Declaring it requires cliContract
	// AgentContentContract, and a workspace receives the content only when it
	// opts in.
	AgentContent *AgentContentContribution `json:"agentContent,omitempty"`
	// Ecosystems lists the ecosystem profiles this extension owns.
	Ecosystems []EcosystemProfile `json:"ecosystems,omitempty"`
	// Uses lists ecosystem ids another extension owns and this extension publishes to.
	Uses []string `json:"uses,omitempty"`
	// OptionNamespaces lists the BARE `options.<name>` blocks of a project
	// config this extension reads for itself — `sdd`, `agent-artifact` — as
	// opposed to the two spellings the CLI already attributes to an extension
	// (`options.<name>` where name is this manifest's name, and the workspace
	// path reference a project declares it by).
	//
	// It exists so a cache key can drop what its task cannot read. A namespace
	// no manifest declares stays in every key, because attributing it would be a
	// guess; a namespace two extensions declare stays in both, because it is an
	// input of both. Declaring a namespace never widens a key — it narrows every
	// OTHER extension's.
	//
	// A command name declared here never narrows the key of an extension that
	// provides the command: the CLI merges `options.<command>` into a task's
	// resolved parameters by the layer contract, so that block stays an input
	// of every provider. Declaring one states a read by an extension that does
	// NOT provide the command — `@putnami/sdd` reads `options.publish`.
	OptionNamespaces []string `json:"optionNamespaces,omitempty"`
}

// ToolDefinition describes an extension-contributed MCP tool. The map key in
// Manifest.Tools is the MCP tool name and must be namespaced (for example,
// "putnami.search") so it cannot collide with core tools such as "impacted".
//
// Command, Args, Cwd, Env, and TimeoutMs follow TaskDefinition's subprocess
// conventions. The CLI writes one ToolCallRequest to the command's stdin; the
// command must write one ToolCallResult to stdout. This narrow request/response
// protocol keeps the extension process boundary intact while letting the CLI
// aggregate every descriptor into its own tools/list response.
type ToolDefinition struct {
	Description string            `json:"description"`
	InputSchema json.RawMessage   `json:"inputSchema"`
	Annotations *ToolAnnotations  `json:"annotations"`
	Meta        map[string]any    `json:"_meta,omitempty"`
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	TimeoutMs   int               `json:"timeoutMs,omitempty"`
	// WorkspaceSelection declares that this tool ANSWERS ABOUT THE WORKSPACE,
	// and asks the orchestrator to resolve the workspace view it needs:
	// ToolCallRequest.WorkspaceProjects (the complete resolved membership) and
	// ToolCallRequest.Selection (the projection the canonical `projects`,
	// `impacted` and `baseline` arguments resolve to).
	//
	// It exists because an extension has no workspace loader and must never
	// grow one. Resolving `impacted` means diffing against a baseline ref,
	// mapping changed paths to owners and walking the dependent graph; resolving
	// `projects` means matching selectors against the workspace's own
	// identities. An extension that re-derived either would be a SECOND
	// definition of what the workspace contains, and the two would diverge on
	// the first scope, alias or transparent group folder. So the orchestrator —
	// which already holds the answer — publishes it, exactly as it does for a
	// job subprocess (`selection` and `workspaceProjects` in protocols/job).
	//
	// It is opt-in rather than implicit for one reason: `projects`, `impacted`
	// and `baseline` are the CLI's vocabulary, not every extension's. A tool
	// that means something else by `projects` must not have its arguments
	// rejected as unknown project selectors, so the resolution happens only
	// where a manifest asks for it. A tool that declares it and takes none of
	// the three arguments still gets the membership, and its selection resolves
	// to the unscoped whole-workspace projection — which is the honest answer
	// for a tool that narrows nothing.
	WorkspaceSelection bool `json:"workspaceSelection,omitempty"`
}

// ToolAnnotations carries the MCP safety hints supplied to agent clients.
// Every extension tool must declare all four hints explicitly; extension code
// may make outbound authenticated requests, but that behavior remains within
// the CLI's existing local trust boundary and must not be implicit to agents.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint"`
	DestructiveHint *bool `json:"destructiveHint"`
	IdempotentHint  *bool `json:"idempotentHint"`
	OpenWorldHint   *bool `json:"openWorldHint"`
}

// ToolCallRequest is the single JSON value written to an extension tool's
// stdin. Arguments is the raw MCP tools/call arguments object. WorkspaceRoot
// and ExtensionRoot let the extension resolve its local configuration and
// assets without the core MCP server taking ownership of either concern.
type ToolCallRequest struct {
	Name          string          `json:"name"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	WorkspaceRoot string          `json:"workspaceRoot"`
	ExtensionRoot string          `json:"extensionRoot"`
	// Agent is present only when the MCP server was explicitly configured to
	// propagate agent provenance. Structured fields are the source of truth;
	// the matching subprocess environment variables are a convenience for
	// extension-owned HTTP clients.
	Agent *AgentIdentity `json:"agent,omitempty"`
	// WorkspaceProjects is the COMPLETE resolved workspace membership, in
	// canonical project-id order. Present exactly when the called tool declares
	// ToolDefinition.WorkspaceSelection; additive and omitted otherwise, so a
	// tool that never asked for it sees the request it always saw.
	//
	// It is the same fact protocols/job publishes under the same member name,
	// for the same reason: what a validator or a catalog may CLAIM about the
	// workspace is only provable from the whole membership, and a consumer given
	// a subset reports a real member as absent. A tool call has no job context to
	// carry it, so it travels here.
	WorkspaceProjects []ToolProjectRef `json:"workspaceProjects,omitempty"`
	// Selection is the projection the tool's canonical selection arguments
	// resolved to, produced by the orchestrator's own resolver. Present under
	// the same condition as WorkspaceProjects.
	//
	// The two are not redundant. This says what the CALLER asked for and what it
	// resolved to; the membership says what the workspace CONTAINS. A catalog
	// told it covers three projects cannot otherwise tell a deliberate narrowing
	// from a whole-workspace run of a three-project tree.
	Selection *ToolSelection `json:"selection,omitempty"`
	// Provider is present exactly when the orchestrator calls this tool as
	// the implementation of a collaboration contract operation the workspace
	// bound to this extension (go.putnami.dev/protocol/collaboration). It is
	// additive and omitted for every other call, so a tool that is not a
	// provider operation sees the request it always saw.
	Provider *ToolProviderCall `json:"provider,omitempty"`
}

// ToolProviderCall states which collaboration operation a routed tool call
// performs, under which contract version, and carries the binding's provider
// settings verbatim. The orchestrator writes it from the workspace binding;
// the arguments member is the operation's validated request document.
type ToolProviderCall struct {
	// Contract is the contract name: "tasks", "proposals" or "memory".
	Contract string `json:"contract"`
	// Version is the contract version the workspace bound.
	Version int `json:"version"`
	// Operation is the operation name inside the contract version.
	Operation string `json:"operation"`
	// Settings is the binding's settings object, absent when the binding
	// declares none. It never carries a credential.
	Settings json.RawMessage `json:"settings,omitempty"`
}

// ToolProjectRef is one workspace member on a tool-call request: the
// orchestrator's RESOLVED answer for that project, never a re-reading of any
// manifest.
//
// It is a fuller projection than protocols/job's ProjectRef on purpose. A job
// subprocess acts on its own project and receives the authored facts of that one
// project alone; a workspace-answering tool reports ON other projects, so the
// facts it reports have to travel. The alternative — a tool that opens sibling
// putnami.json files itself — is the second workspace reader this member exists
// to prevent.
type ToolProjectRef struct {
	// ID is the project's canonical logical identity, transparent group folders
	// omitted.
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
	// Tags, Publish, Extensions and RunsWith are the resolved authored members,
	// after the scope chain has contributed its defaults.
	Tags       []string `json:"tags,omitempty"`
	Publish    []string `json:"publish,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	RunsWith   []string `json:"runsWith,omitempty"`
	// Dependencies are the ids of the projects this one depends on DIRECTLY, in
	// the order the orchestrator resolved them — ids rather than declared names,
	// because an id is the workspace's own stable identity. Out-of-workspace
	// edges are omitted: there is no member to point at.
	//
	// Absence inside this member means the project declares none. The producer
	// of a complete membership has by definition resolved the graph, so there is
	// no third state to read.
	Dependencies []string `json:"dependencies,omitempty"`
	// Config is the project's authored putnami.json as the orchestrator parsed
	// it. It stays RAW here — the workspace protocol owns the shape, and a wire
	// contract that re-declared it would be a second definition of the same
	// document to keep in step.
	//
	// It travels because the members a workspace-answering surface reads out of
	// it (`bin`, `featureAuthority`, `options`) are per-project facts about
	// SOMEBODY ELSE's project. The job wire carries the same raw member on its
	// complete workspace membership.
	Config json.RawMessage `json:"config,omitempty"`
}

// ToolSelection is the resolved project selection of one tool call.
//
// It marshals BYTE-IDENTICALLY to protocols/job's Selection, which marshals
// byte-identically to the CLI's own ResolvedSelection: one resolver answers the
// planned jobs, the interactive commands and the agent tools, so the three
// surfaces cannot disagree about what a narrowing meant. The type is declared
// here rather than imported because protocols/job already imports this package,
// and the equality is pinned by a drift test — the same arrangement
// protocols/job uses for the staging roots it shares with this contract.
type ToolSelection struct {
	// Mode is how the projection was chosen: one of ToolSelectionModes.
	Mode string `json:"mode"`
	// Scoped distinguishes a narrowed call from the whole-workspace default. A
	// tool that reports a verdict for the workspace may only claim to have
	// covered it when Scoped is false.
	Scoped bool `json:"scoped"`
	// Baseline is the ref `impacted` actually resolved to, empty otherwise.
	Baseline string `json:"baseline,omitempty"`
	// BaselineSource is the resolution tier that produced Baseline, so a
	// consumer can distrust a stale fallback ref the same way the call can.
	BaselineSource string `json:"baselineSource,omitempty"`
	// ProjectIDs are the selected projects' canonical ids, sorted.
	ProjectIDs []string `json:"projects"`
	// EmptyImpact records the legitimate no-op: `impacted` resolved cleanly and
	// nothing changed. It is a success, not a "no projects matched" error.
	EmptyImpact bool `json:"emptyImpact,omitempty"`
}

// Tool selection modes: the closed vocabulary a resolved ToolSelection.Mode
// takes, identical to protocols/job's.
const (
	// ToolSelectionModeAll is the unscoped whole-workspace projection.
	ToolSelectionModeAll = "all"
	// ToolSelectionModeProjects is an explicit selector, with or without filters.
	ToolSelectionModeProjects = "projects"
	// ToolSelectionModeImpacted is the `impacted` projection.
	ToolSelectionModeImpacted = "impacted"
)

// ToolSelectionModes is the closed selection-mode vocabulary in canonical
// (sorted) order.
var ToolSelectionModes = []string{
	ToolSelectionModeAll,
	ToolSelectionModeImpacted,
	ToolSelectionModeProjects,
}

// ToolContent is one text item in an extension tool result. The first version
// deliberately supports text only, matching the core MCP server's existing
// tool-result surface and avoiding an accidental general streaming protocol.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolCallResult is the single JSON value an extension tool writes to stdout.
// Runtime failures use IsError rather than a process/protocol error so agents
// receive the normal MCP tool-result shape. Malformed output and non-zero
// exits are isolated by the CLI and are likewise returned as an MCP tool error.
type ToolCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// CommandGroupDefinition declares a structured command group exposed by an
// extension (e.g. "cloud" with subcommands "login", "logout"). Top-level
// subcommands reference an existing flat command in the same manifest by name,
// so flags, defaults, and pipeline configuration are not duplicated. Nested
// subcommands can inherit that command target and add static completion
// metadata for verbs parsed inside the extension binary.
//
// Flags declares group-level shared flags inherited by every subcommand, so a
// cross-cutting flag (e.g. --env) is written once instead of on every
// subcommand. The effective flag surface of a subcommand merges three layers in
// increasing precedence — the flat command target's flags, these group shared
// flags, then the subcommand's own flags — so the more specific declaration
// wins and a subcommand always overrides the group on a name collision. Names
// that collide with a Putnami global flag (e.g. --output, --dry-run) are
// consumed by the CLI before the extension sees them, so declare only
// extension-owned flag names here.
//
// Default names one of the group's own top-level subcommands. `putnami
// <group>` with no subcommand word runs it, with any flags that follow; `putnami
// <group> --help` still prints the group help. A group without Default prints
// its help when no subcommand is named.
type CommandGroupDefinition struct {
	Description string                          `json:"description,omitempty"`
	Default     string                          `json:"default,omitempty"`
	Flags       map[string]FlagDefinition       `json:"flags,omitempty"`
	Subcommands map[string]SubcommandDefinition `json:"subcommands"`
}

// SubcommandDefinition is one entry in a CommandGroupDefinition.
//
// Command names the flat command in this manifest's "commands" map that the
// subcommand routes to. Nested subcommands may omit Command and inherit the
// nearest ancestor's target. Interactive flags the subprocess as command UX
// rather than job UX: the live renderer is bypassed, stdio is inherited, and
// the extension subprocess receives PUTNAMI_INTERACTIVE=1 in its environment.
//
// Workspace states whether the subcommand needs a workspace:
// SubcommandWorkspaceRequired (the meaning of an absent value) or
// SubcommandWorkspaceOptional. An optional subcommand must be interactive. The
// CLI reads Workspace and Interactive on the top-level subcommand it
// dispatches; on a nested subcommand they are validated the same way and carry
// no dispatch meaning.
type SubcommandDefinition struct {
	Description string                          `json:"description,omitempty"`
	Command     string                          `json:"command,omitempty"`
	Interactive bool                            `json:"interactive,omitempty"`
	Workspace   string                          `json:"workspace,omitempty"`
	Flags       map[string]FlagDefinition       `json:"flags,omitempty"`
	Positionals []PositionalDefinition          `json:"positionals,omitempty"`
	Subcommands map[string]SubcommandDefinition `json:"subcommands,omitempty"`
	Examples    []ExampleDefinition             `json:"examples,omitempty"`
}

// Subcommand workspace requirements, the closed vocabulary of
// SubcommandDefinition.Workspace.
const (
	// SubcommandWorkspaceRequired runs the subcommand only inside a workspace.
	// It is the meaning of an absent value.
	SubcommandWorkspaceRequired = "required"
	// SubcommandWorkspaceOptional also runs the subcommand outside any
	// workspace, from an extension pinned in the user scope. It is valid only
	// on an interactive subcommand.
	SubcommandWorkspaceOptional = "optional"
)

// RunsWithoutWorkspace reports whether the subcommand may run outside any
// workspace: it declares SubcommandWorkspaceOptional and is interactive. A
// definition that declares optional without interactive fails validation and
// answers false here, so a manifest the strict parser never saw cannot widen
// where a subcommand runs.
func (s SubcommandDefinition) RunsWithoutWorkspace() bool {
	return s.Workspace == SubcommandWorkspaceOptional && s.Interactive
}

// ExampleDefinition is one runnable example shown under the "Examples:" section
// of `putnami <group> <subcommand> --help`. Command is the runnable command
// line; Description is an optional gloss explaining what it does.
type ExampleDefinition struct {
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
}

// PositionalDefinition describes a positional argument accepted by an
// extension subcommand. Completion is intentionally static for now; dynamic
// value providers can be added later without requiring extension binaries to
// run during shell completion.
type PositionalDefinition struct {
	Name     string `json:"name"`
	Required bool   `json:"required,omitempty"`
}

// Dependencies can be either []string or map[string]string.
// The JSON unmarshalling is handled by a custom decoder.
type Dependencies struct {
	List map[string]string // name → version constraint ("" if no constraint)
}

// CommandDefinition describes a user-facing command (e.g., "build", "test").
type CommandDefinition struct {
	Description          string                          `json:"description,omitempty"`
	Visibility           string                          `json:"visibility,omitempty"`
	ActivationFiles      []string                        `json:"activationFiles,omitempty"`
	Activation           string                          `json:"activation,omitempty"`
	Channel              string                          `json:"channel,omitempty"`
	DependsOn            []string                        `json:"dependsOn,omitempty"`
	SessionPrerequisites []SessionPrerequisiteDefinition `json:"sessionPrerequisites,omitempty"`
	// AlsoRuns widens a REQUEST for this command to also plan the named
	// companion commands, each with its own activation. The expansion happens
	// at command resolution, not job planning: a workspace-once companion
	// still plans exactly once even when this command activates nowhere,
	// which is what a dependsOn edge or a session prerequisite cannot
	// express. Companions must name commands in the same manifest.
	AlsoRuns []string                  `json:"alsoRuns,omitempty"`
	Flags    map[string]FlagDefinition `json:"flags,omitempty"`
	Defaults map[string]any            `json:"defaults,omitempty"`
	Priority int                       `json:"priority,omitempty"`
	Quiet    bool                      `json:"quiet,omitempty"`
	Traits   *CommandTraits            `json:"traits,omitempty"`
	Run      []PipelineStep            `json:"run"`
	Outputs  map[string]OutputBinding  `json:"outputs,omitempty"`
}

// SessionPrerequisiteDefinition plans another command in the same session
// before the command that owns this declaration. Selectors and policy are
// evaluated from the owning command's resolved parameters; the target command
// and its gates may be contributed by any extension.
type SessionPrerequisiteDefinition struct {
	Command           string                                     `json:"command"`
	If                string                                     `json:"if,omitempty"`
	ProjectsFromParam string                                     `json:"projectsFromParam,omitempty"`
	ProjectIf         string                                     `json:"projectIf,omitempty"`
	DependsOn         []string                                   `json:"dependsOn,omitempty"`
	Params            map[string]SessionPrerequisiteParamBinding `json:"params,omitempty"`
}

// SessionPrerequisiteParamBinding supplies an invocation-local parameter to a
// prerequisite command. Value is a constant; FromProjectParam reads a dotted
// path from the owning command's parameters resolved for the target project.
// Exactly one source is required by strict validation.
type SessionPrerequisiteParamBinding struct {
	Value            any    `json:"value,omitempty"`
	FromProjectParam string `json:"fromProjectParam,omitempty"`
}

// Step run conditions: WHEN in a command's lifecycle a step is executed.
//
// This is the single step-gating vocabulary. `if` and `activation` decide
// whether a step is PLANNED at all; RunOn decides what the scheduler does with
// a planned step once its dependencies have finished. The pair is deliberately
// small: contract v3 deleted the `when` field — a runtime expression evaluated
// against step results — because it had zero consumers and zero manifest usage,
// and two overlapping runtime gates is one more than any pipeline needs.
const (
	// StepRunOnSuccess runs the step only when every dependency succeeded. It
	// is the default, and the behavior of every step written before runOn
	// existed.
	StepRunOnSuccess = "success"
	// StepRunOnFinally marks the containing step as the finalizer in an explicit
	// FinalizesRelation. Producer start arms it exactly once and Consumers are
	// the complete terminal frontier it waits for, regardless of whether they
	// succeeded, failed, or were canceled.
	StepRunOnFinally = "finally"
)

// ValidStepRunOn is the closed run-condition vocabulary in canonical (sorted)
// order.
var ValidStepRunOn = []string{StepRunOnFinally, StepRunOnSuccess}

// IsValidStepRunOn reports whether value is part of the closed run-condition
// vocabulary. The empty string is NOT a member: it is the absent field, which
// EffectiveRunOn resolves to StepRunOnSuccess.
func IsValidStepRunOn(value string) bool {
	for _, candidate := range ValidStepRunOn {
		if candidate == value {
			return true
		}
	}
	return false
}

// PipelineStep is one node in a command's execution DAG.
type PipelineStep struct {
	ID        string                  `json:"id"`
	Task      string                  `json:"task"`
	DependsOn []string                `json:"dependsOn,omitempty"`
	With      map[string]InputBinding `json:"with,omitempty"`
	Optional  bool                    `json:"optional,omitempty"`
	If        string                  `json:"if,omitempty"`
	// RunOn is the step's run condition: StepRunOnSuccess (default) or
	// StepRunOnFinally. Absent means success, so every manifest written before
	// this field keeps its exact behavior.
	RunOn string `json:"runOn,omitempty"`
	// Finalizes is the explicit invocation-resource relation for a finalizer.
	// Producer arms cleanup when it starts; Consumers are the complete terminal
	// frontier cleanup waits for. It is required exactly when RunOn is finally.
	Finalizes  *FinalizesRelation `json:"finalizes,omitempty"`
	Activation *StepActivation    `json:"activation,omitempty"`
	Cache      *StepCacheOverride `json:"cache,omitempty"`
	TimeoutMs  *int               `json:"timeoutMs,omitempty"`
	// Heavy overrides the command's heavy trait for this step so worker
	// tuning leaves CPU/memory-heavy steps headroom (e.g. golangci-lint,
	// type generation, cross-compilation).
	Heavy *bool `json:"heavy,omitempty"`
	// CPUWeight is a relative multiplier on the step's deterministic,
	// history-derived CPU demand. 1 is the default; a project-level putnami.json
	// tasks entry overrides it. The machine capacity caps the resulting budget.
	// Execution hint only — never affects cache keys.
	CPUWeight *float64 `json:"cpuWeight,omitempty"`
}

// FinalizesRelation makes invocation-resource cleanup derivable from the
// manifest without overloading ordinary DAG dependencies.
type FinalizesRelation struct {
	// Producer is the local step whose start arms the finalizer.
	Producer string `json:"producer"`
	// Consumers is the complete set of local steps allowed to use the
	// invocation artifacts. Cleanup waits until each reaches a terminal state.
	Consumers []string `json:"consumers"`
	// PruneIf is an optional plan-time expression over command params. The
	// relation is eligible for all-hit frontier pruning only when it evaluates
	// true. Empty preserves the default: every all-hit frontier is prunable.
	// A false or malformed expression fails safe by keeping the producer and
	// finalizer, so pruning cannot swallow an explicitly requested side effect.
	PruneIf string `json:"pruneIf,omitempty"`
}

// EffectiveRunOn returns the step's run condition, defaulting to
// StepRunOnSuccess when unset.
func (s PipelineStep) EffectiveRunOn() string {
	if s.RunOn == "" {
		return StepRunOnSuccess
	}
	return s.RunOn
}

// IsFinalizer reports whether the step uses the finalizer run condition.
func (s PipelineStep) IsFinalizer() bool {
	return s.EffectiveRunOn() == StepRunOnFinally
}

// StepActivation declares plan-time conditions that gate whether a pipeline
// step is scheduled at all. Unlike `if` (evaluated against params plus the
// provider/project command set, its resolved command params, and project
// facts), activation probes the target project's files, letting the planner
// drop steps that would deterministically skip at runtime — e.g. a Go
// `describe` step for a project
// that does not import the app framework.
//
// A step is active only when every declared condition holds. An inactive step
// is spliced out of the DAG rather than scheduled-and-skipped: its dependents
// inherit its dependencies, so the ordering and data-flow through the
// remaining steps are preserved exactly (a runtime skip produces no output for
// a dependent to consume). Conditions fail closed — a missing or unreadable
// file makes the step inactive — which mirrors the runtime skip checks, so
// pruning never removes a step that would have done real work.
type StepActivation struct {
	// Files lists glob patterns; the step is active only if at least one
	// project file matches at least one pattern. Empty means no file gate.
	Files []string `json:"files,omitempty"`
	// ClosureFiles lists glob patterns matched against every member of the
	// project's dependency closure (seed included); the step is active only if
	// at least one member has a matching file. Empty means no closure gate.
	//
	// It is the plan-time twin of the `closure` input source, and it exists so a
	// gate can ask the SAME question the step's task will answer at runtime. A
	// step whose task reads the closure but whose gate reads only the project's
	// own files is silently dropped for a project whose need is entirely
	// transitive — the gate and the runtime disagreeing about the same fact.
	ClosureFiles []string `json:"closureFiles,omitempty"`
	// Contains maps a project-relative file path to a required substring; the
	// step is active only if every listed file exists and contains its
	// substring. Empty means no content gate.
	Contains map[string]string `json:"contains,omitempty"`
}

// TaskDefinition describes an atomic executable unit.
type TaskDefinition struct {
	Description string            `json:"description,omitempty"`
	Visibility  string            `json:"visibility,omitempty"`
	Kind        string            `json:"kind"`
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// Toolchains adds lock-resolved runtime requirements for this task. Names
	// reference RuntimeDefinition.Toolchains and are resolved before execution.
	Toolchains      []string                  `json:"toolchains,omitempty"`
	Output          string                    `json:"output,omitempty"`
	TimeoutMs       int                       `json:"timeoutMs,omitempty"`
	InputSchemaRef  string                    `json:"inputSchemaRef,omitempty"`
	OutputSchemaRef string                    `json:"outputSchemaRef,omitempty"`
	Inputs          map[string]TaskInputPort  `json:"inputs,omitempty"`
	Outputs         map[string]TaskOutputPort `json:"outputs,omitempty"`
	Writes          []ResourceRef             `json:"writes,omitempty"`
	Reads           []ResourceRef             `json:"reads,omitempty"`
	// Resources are named BUDGET claims: how many units of a scarce shared
	// resource one execution of this task holds while it runs, e.g.
	// {"db-connections": 230}. It is a divisible quantity, which is what
	// separates it from Writes/Reads: those name a resource two tasks must not
	// touch at once and the planner serializes them, while a claim only has to
	// fit inside what the run says exists (`--resource <name>=<units>`).
	//
	// The scheduler admits a task when the worker count allows it AND every
	// resource it claims still has budget. A resource with no budget declared on
	// the run is unlimited, so a claim never gates a run that never heard of it,
	// and a task that claims nothing is never gated by anyone else's claim.
	// Names are opaque to the CLI — nothing here knows what a database
	// connection is.
	Resources map[string]int   `json:"resources,omitempty"`
	Cache     *TaskCachePolicy `json:"cache,omitempty"`
	Batchable *TaskBatchPolicy `json:"batchable,omitempty"`
	// Declares is the v3 task contract: the exact files and subtrees the task
	// produces, its effects beyond those outputs, and whether it rewrites the
	// sources it reads. Nil means the task uses the v2 contract, where the
	// footprint is inferred — see task_contract.go for what changes and why.
	Declares *TaskDeclaration `json:"declares,omitempty"`
}

// TaskBatchPolicy opts a cacheable per-project task into opportunistic
// same-key batching. ConfigFiles is an ordered list of template-expanded
// candidates; the first existing regular file supplies the source-qualified
// configuration digest. Projects with identical task/tool identity, effective
// parameters, toolchain version, and resolved config may share one subprocess.
type TaskBatchPolicy struct {
	Tool        string   `json:"tool"`
	ConfigFiles []string `json:"configFiles,omitempty"`
	// MaxProjects caps projects in one shared invocation. Zero is unbounded.
	MaxProjects int `json:"maxProjects,omitempty"`
	// MaxProjectsParam names a parameter that lets a workspace or project set
	// the cap instead of MaxProjects, for example through
	// options["@putnami/go:test"]. The scheduler reads it from the job's
	// resolved parameters. When the parameter is absent, MaxProjects applies
	// unchanged. A positive integer replaces it, and 1 runs every project
	// alone. Any other value fails the plan. The parameter only decides how
	// jobs are grouped, so it must never key a task: the task may not declare
	// it as an input or cache key param, a command that runs the task may not
	// declare it as a flag, and a step may not bind it, because the CLI keys a
	// task on each of those.
	MaxProjectsParam string `json:"maxProjectsParam,omitempty"`
	// MaxWorkers disables batching above this resolved scheduler concurrency.
	// Zero enables batching at every worker count.
	MaxWorkers int `json:"maxWorkers,omitempty"`
}

// Resource scope values for ResourceRef.Scope.
const (
	// ResourceScopeProject confines a resource to the owning project, so two
	// accesses conflict only when they belong to the same project.
	ResourceScopeProject = "project"
	// ResourceScopeWorkspace makes a resource shared across the whole workspace,
	// so any two accesses to the same id conflict regardless of project.
	ResourceScopeWorkspace = "workspace"
)

// ResourceRef names a write/read resource a task touches — a generated tree, a
// shared output directory, or any path that must not be written by two jobs at
// once. It expresses write serialization independently of functional
// dependencies: the planner orders jobs whose resource accesses conflict
// (writer/writer or writer/reader) without adding a data dependency between
// them.
//
// A bare JSON string is shorthand for a project-scoped resource, so "gen" is
// equivalent to {"id": "gen", "scope": "project"}.
type ResourceRef struct {
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"`
}

// EffectiveScope returns the resource scope, defaulting to project scope when
// unset.
func (r ResourceRef) EffectiveScope() string {
	if r.Scope == "" {
		return ResourceScopeProject
	}
	return r.Scope
}

// UnmarshalJSON accepts either a bare string (project-scoped shorthand) or a
// full {"id", "scope"} object.
func (r *ResourceRef) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		r.ID = s
		r.Scope = ""
		return nil
	}

	type alias ResourceRef
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("resource ref must be a string or object: %w", err)
	}
	*r = ResourceRef(a)
	return nil
}

// MarshalJSON emits the bare-string shorthand for project-scoped resources and
// the full object form otherwise, so round-tripping a manifest is stable.
func (r ResourceRef) MarshalJSON() ([]byte, error) {
	if r.EffectiveScope() == ResourceScopeProject {
		return json.Marshal(r.ID)
	}
	type alias ResourceRef
	return json.Marshal(alias(r))
}

// Task input sources: where a declared input port gets its value.
const (
	// TaskInputFromProject reads project-relative files matched by Files.
	TaskInputFromProject = "project"
	// TaskInputFromWorkspace reads workspace-relative files matched by Files.
	TaskInputFromWorkspace = "workspace"
	// TaskInputFromClosure reads the files matched by Files in EVERY member of
	// the project's dependency closure — the same set the job context delivers
	// as project.dependencyClosure, the seed project included.
	//
	// It exists because a task can legitimately read its dependencies' COMMITTED
	// manifests: an aggregation over a workload's graph, or the provisioning of a
	// resource the whole closure declares a need for. A `project` port cannot
	// state that. Declaring it as a closure port is what makes such a task's
	// cache key move when a DEPENDENCY's manifest changes — without it, the key
	// is blind to the very inputs the task read, and a stored verdict is served
	// against a different world.
	TaskInputFromClosure = "closure"
	// TaskInputFromTask consumes another task's output — the typed
	// producer/consumer edge. The consuming pipeline step supplies the
	// producer with a `with` binding; a port that is not Optional and has no
	// producer in the plan is a plan-time error, not a runtime surprise.
	TaskInputFromTask = "task"
	// TaskInputFromParams reads the resolved command parameter of the same name.
	TaskInputFromParams = "params"
	// TaskInputFromEnv reads the environment variable of the same name.
	TaskInputFromEnv = "env"
	// TaskInputFromRuntime reads a runtime identity value (toolchain version).
	TaskInputFromRuntime = "runtime"
)

// Recognized `from: "runtime"` input names.
//
// A runtime input names an AMBIENT fact — something true of the machine or the
// running toolchain rather than of the checked-out sources — that a task's
// output depends on. Declaring one is a statement with two consequences: the
// value enters the task's cache key, and the plan's keys stop being recomputable
// from a revision alone (CacheKeysRecomputableAtRevision).
const (
	// RuntimeInputHostPlatform is the GOOS/GOARCH of the machine running the
	// CLI. A task declares it when its output or its VERDICT is host-specific:
	// a host-platform compile emits Mach-O on darwin and ELF on linux, and a
	// host-platform compile check accepts sources on one OS that do not build
	// on another (`//go:build linux` files are compiled only on linux). Other
	// cache key fields differ between platforms only as a consequence of what
	// they identify: the runtime toolchain identity of a task that uses a
	// runtime toolchain, and the implementation digest of an installed
	// extension that ships platform executables. A task with neither shares
	// cache entries between developer laptops and CI, so without this
	// declaration a darwin verdict can be served to linux.
	RuntimeInputHostPlatform = "hostPlatform"
	// RuntimeInputExtensionVersion is the resolved extension version. It is
	// accepted for documentation value only and adds no key material: every
	// cache key already names the extension's implementation, by its version
	// or by the implementation digest that replaces the version.
	RuntimeInputExtensionVersion = "extensionVersion"
)

// TaskInputPort declares a task input source.
type TaskInputPort struct {
	From     string   `json:"from"`               // "project", "workspace", "closure", "task", "params", "env", "runtime"
	Files    []string `json:"files,omitempty"`    // file globs (for "project", "workspace" or "closure")
	Optional bool     `json:"optional,omitempty"` // only for "task" inputs
}

// RequiresProducer reports whether the port states a HARD dependency on
// another task's output: a `from: "task"` port that is not optional.
//
// This is the typed replacement for the orchestrator-side preflight trait epic
// contract v3 deleted. Before it, "this command needs an artifact somebody
// else produces" was expressed by core walking the project's sources and
// looking for the file; now the consuming task states it, the producing task
// is named by the step binding, and a missing producer is a plan error citing
// both identities. An optional port keeps the opposite meaning it always had:
// the consumer handles absence itself.
func (p TaskInputPort) RequiresProducer() bool {
	return p.From == TaskInputFromTask && !p.Optional
}

// TaskOutputPort declares a task output.
type TaskOutputPort struct {
	Kind        string `json:"kind,omitempty"`        // "file", "directory", or empty for data
	Path        string `json:"path,omitempty"`        // output path (for file/directory)
	Description string `json:"description,omitempty"` // human-readable description
}

// TaskCachePolicy controls caching for a task. The JSON "cache" field
// can be a boolean (false = disabled) or an object. Custom unmarshalling
// handles both.
type TaskCachePolicy struct {
	Enabled       *bool `json:"enabled,omitempty"`
	Deterministic bool  `json:"deterministic,omitempty"`
	// VersionAware marks a task whose output embeds the full publish version
	// (e.g. an npm package.json version, a Go module zip path, or a
	// version-stamped archive). Such tasks must vary their cache key with the
	// per-commit version suffix so that repackaging at a new release id is a
	// cache miss even when the upstream build content is unchanged. Distinct
	// from the Go `version-var` param signal, which only covers binaries that
	// inject the version via ldflags.
	VersionAware bool                 `json:"versionAware,omitempty"`
	Key          *TaskCacheKey        `json:"key,omitempty"`
	Outputs      []TaskOutputArtifact `json:"outputs,omitempty"`
	// RestoreMode is accepted and ignored.
	//
	// Deprecated: it was the opt-in a `package` task had to declare to earn
	// declared capture while the command's shared channel index had no owner.
	// Every package output now has an owner, so the opt-in decides nothing:
	// eligibility is the declaration itself, in every command. The field is
	// RETAINED rather than deleted because ParseManifest rejects unknown fields,
	// and manifests published before that change still carry it — deleting it
	// would make a CLI one release ahead of its extensions refuse to load them.
	// Remove it once no pinned manifest declares it.
	RestoreMode string `json:"restoreMode,omitempty"`
	AtomicWrite bool   `json:"atomicWrite,omitempty"`
	// NoOutput declares that the task never writes output files — its result
	// is pure data (a go mod tidy, a lint, a format check). Two consumers:
	// the store path keeps successful results status-only instead of
	// capturing the shared per-command output directory (which may hold
	// SIBLING steps' files, making the entry both wrong — a hit restores
	// stale sibling artifacts — and heavy: tens of MB keyed to a sub-second
	// task, permanently excluded by the remote break-even guard); and the
	// remote required-input guard treats the task's files-less hits as
	// legitimate instead of rebuilding them locally on every run.
	NoOutput bool `json:"noOutput,omitempty"`
}

// IsEnabled returns whether caching is enabled for this task.
func (p *TaskCachePolicy) IsEnabled() bool {
	if p == nil {
		return true // default enabled
	}
	if p.Enabled != nil {
		return *p.Enabled
	}
	return true
}

// TaskCacheKey defines what contributes to a cache key.
type TaskCacheKey struct {
	Files          []string `json:"files,omitempty"`
	WorkspaceFiles []string `json:"workspaceFiles,omitempty"`
	// ClosureFiles are project-relative globs hashed across EVERY member of the
	// project's dependency closure, seed included. A task that reads its
	// dependencies' committed manifests keys on them here; a `files` entry would
	// cover only the task's own project and leave the key blind to the rest.
	ClosureFiles []string `json:"closureFiles,omitempty"`
	Inputs       []string `json:"inputs,omitempty"`
	Params       []string `json:"params,omitempty"`
	Env          []string `json:"env,omitempty"`
	Runtime      []string `json:"runtime,omitempty"`
}

// TaskOutputArtifact describes an output produced by a task.
type TaskOutputArtifact struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Path     string         `json:"path"`
	Required bool           `json:"required,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// DeriveTaskCacheKey builds a TaskCacheKey from task input ports.
// Output slices are sorted for deterministic results regardless of map iteration order.
func DeriveTaskCacheKey(inputs map[string]TaskInputPort) TaskCacheKey {
	// Iterate in sorted key order for determinism.
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)

	var files, workspaceFiles, closureFiles, params, env, runtime []string
	for _, name := range names {
		input := inputs[name]
		switch input.From {
		case TaskInputFromProject:
			files = append(files, input.Files...)
		case TaskInputFromWorkspace:
			workspaceFiles = append(workspaceFiles, input.Files...)
		case TaskInputFromClosure:
			closureFiles = append(closureFiles, input.Files...)
		case TaskInputFromParams:
			params = append(params, name)
		case TaskInputFromEnv:
			env = append(env, name)
		case TaskInputFromRuntime:
			runtime = append(runtime, name)
		case TaskInputFromTask:
			// Task inputs contribute via dependency hashes
		}
	}

	sort.Strings(files)
	sort.Strings(workspaceFiles)
	sort.Strings(closureFiles)
	sort.Strings(params)
	sort.Strings(env)
	sort.Strings(runtime)

	return TaskCacheKey{
		Files:          files,
		WorkspaceFiles: workspaceFiles,
		ClosureFiles:   closureFiles,
		Params:         params,
		Env:            env,
		Runtime:        runtime,
	}
}

// FlagDefinition describes a CLI flag declared in a manifest.
type FlagDefinition struct {
	Type        string   `json:"type"`
	Short       string   `json:"short,omitempty"`
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

// InputBinding is a union type for step input wiring.
// Exactly one of Value, From, or FromStep should be set.
type InputBinding struct {
	// Literal value binding
	Value    any  `json:"value,omitempty"`
	HasValue bool `json:"-"` // set by custom unmarshaller

	// Context/command binding
	From    string `json:"from,omitempty"`
	Path    string `json:"path,omitempty"`
	Default any    `json:"default,omitempty"`

	// Step result binding
	FromStep string `json:"fromStep,omitempty"`
	Output   string `json:"output,omitempty"` // v2: named output port (replaces Path for step bindings)
	Required bool   `json:"required,omitempty"`
}

// OutputBinding maps a command output to a step result.
type OutputBinding struct {
	FromStep string `json:"fromStep"`
	Path     string `json:"path"`
	Required bool   `json:"required,omitempty"`
}

// StepCacheOverride allows a pipeline step to override task cache behavior.
type StepCacheOverride struct {
	Enabled *bool    `json:"enabled,omitempty"`
	BustOn  []string `json:"bustOn,omitempty"`
}

// ContractsDefinition holds JSON schemas for task I/O validation.
type ContractsDefinition struct {
	Schemas map[string]map[string]any `json:"schemas,omitempty"`
}

// ManifestHooks defines extension lifecycle hooks.
//
// The cache lifecycle is NOT here. `cacheClean` / `cacheGC` were hooks until
// contract v3 moved them to the reserved cache COMMANDS (cache.go): a
// hook is an untyped side vocabulary — no declared task, no contract digest, no
// effects — and cache collection is exactly the kind of provider behavior the
// epic expresses as a typed task behind a hidden command. A manifest that still
// declares them fails strict parsing rather than silently never being asked.
type ManifestHooks struct {
	PreBuild  *HookDefinition `json:"preBuild,omitempty"`
	OnInstall *HookDefinition `json:"onInstall,omitempty"`
}

// HookDefinition describes a subprocess-based hook.
//
// Order is the invocation rank of this hook among all extensions that declare
// the same hook kind for one project. Hooks of a kind run in ascending Order,
// ties broken by extension name in byte order, so the sequence is a pure
// function of the manifests and never depends on map iteration or filesystem
// order. Absent (0) is the default "producer" rank: a hook that only writes
// generated sources. A hook that must observe what the other hooks generated —
// e.g. one that imports the workload entry point, or inventories the whole
// generated tree — declares a higher Order and runs last.
type HookDefinition struct {
	Kind      string            `json:"kind"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Output    string            `json:"output,omitempty"`
	TimeoutMs int               `json:"timeoutMs,omitempty"`
	Cache     *HookCacheConfig  `json:"cache,omitempty"`
	DependsOn []string          `json:"dependsOn,omitempty"`
	Order     int               `json:"order,omitempty"`
}

// HookCacheConfig defines incremental build cache for hooks.
type HookCacheConfig struct {
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
}

// --- Custom JSON unmarshalling ---

// UnmarshalJSON handles the extensionDependencies field which can be
// either a JSON array of strings or a JSON object of name→constraint.
func (d *Dependencies) UnmarshalJSON(data []byte) error {
	d.List = make(map[string]string)

	// Try array first
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		for _, name := range arr {
			d.List[name] = ""
		}
		return nil
	}

	// Try object
	var obj map[string]string
	if err := json.Unmarshal(data, &obj); err == nil {
		d.List = obj
		return nil
	}

	return fmt.Errorf("extensionDependencies must be an array or object")
}

// MarshalJSON serializes Dependencies back to JSON.
func (d Dependencies) MarshalJSON() ([]byte, error) {
	if d.List == nil {
		return []byte("null"), nil
	}
	return json.Marshal(d.List)
}

// UnmarshalJSON handles the task cache field which can be a boolean or
// a TaskCachePolicy object.
func (t *TaskDefinition) UnmarshalJSON(data []byte) error {
	// Use an alias to prevent infinite recursion.
	type Alias TaskDefinition
	type Raw struct {
		Alias
		RawCache json.RawMessage `json:"cache,omitempty"`
	}

	var raw Raw
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*t = TaskDefinition(raw.Alias)

	if raw.RawCache != nil {
		// Try boolean first
		var b bool
		if err := json.Unmarshal(raw.RawCache, &b); err == nil {
			t.Cache = &TaskCachePolicy{Enabled: &b}
			return nil
		}
		// Try object
		var policy TaskCachePolicy
		if err := json.Unmarshal(raw.RawCache, &policy); err != nil {
			return fmt.Errorf("task cache must be boolean or object: %w", err)
		}
		t.Cache = &policy
	}

	return nil
}

// UnmarshalJSON handles the InputBinding union type.
func (b *InputBinding) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if v, ok := raw["value"]; ok {
		b.HasValue = true
		if err := json.Unmarshal(v, &b.Value); err != nil {
			return err
		}
	}
	if v, ok := raw["from"]; ok {
		if err := json.Unmarshal(v, &b.From); err != nil {
			return fmt.Errorf("unmarshal input binding 'from': %w", err)
		}
	}
	if v, ok := raw["path"]; ok {
		if err := json.Unmarshal(v, &b.Path); err != nil {
			return fmt.Errorf("unmarshal input binding 'path': %w", err)
		}
	}
	if v, ok := raw["default"]; ok {
		if err := json.Unmarshal(v, &b.Default); err != nil {
			return fmt.Errorf("unmarshal input binding 'default': %w", err)
		}
	}
	if v, ok := raw["fromStep"]; ok {
		if err := json.Unmarshal(v, &b.FromStep); err != nil {
			return fmt.Errorf("unmarshal input binding 'fromStep': %w", err)
		}
	}
	if v, ok := raw["output"]; ok {
		if err := json.Unmarshal(v, &b.Output); err != nil {
			return fmt.Errorf("unmarshal input binding 'output': %w", err)
		}
	}
	if v, ok := raw["required"]; ok {
		if err := json.Unmarshal(v, &b.Required); err != nil {
			return fmt.Errorf("unmarshal input binding 'required': %w", err)
		}
	}
	if b.FromStep != "" {
		if _, hasPath := raw["path"]; hasPath {
			return fmt.Errorf("unmarshal input binding: 'path' is not valid with 'fromStep'; use the named 'output' port")
		}
	}

	return nil
}
