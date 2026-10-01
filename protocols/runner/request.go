package runner

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	protocolcli "go.putnami.dev/protocol/cli"
)

// Execution request version, bounds and vocabulary. The request is the one
// authority a provider executes; raw argv never travels beside it.
const (
	ExecutionRequestVersion = 1
	// ExecutionInputDomain separates the execution-input digest from every
	// other SHA-256 value (source digest, ChangePlan, task keys).
	ExecutionInputDomain = "putnami/runner/execution-input/v1"
	MaxRequestBytes      = MaxManifestBytes
	MaxCommands          = 16
	MaxPlanTasks         = MaxSourceEntries
	MaxSelectionProjects = MaxSourceEntries
	MaxDiagnostics       = 256
	MaxDiagnosticBytes   = 4096
	MaxResourceBudgets   = 64

	SelectionModeAll      = "all"
	SelectionModeImpacted = "impacted"
	SelectionModeProjects = "projects"

	CLISourceWorkspace = "workspace"
	CLISourcePublished = "published"
	CLISourceUnpinned  = "unpinned"

	CallerCLI = "cli"
)

// ExecutionRequest is the versioned, bound portable execution request. Its
// blocks are frozen by the submitting engine; a provider carries them without
// inventing flag semantics, rerunning impact analysis or replacing a baseline.
type ExecutionRequest struct {
	// Version is the request wire version and must equal 1.
	Version int `json:"version"`
	// Protocol names the negotiated provider protocol and capabilities.
	Protocol ProtocolBlock `json:"protocol"`
	// Source binds the immutable snapshot and its credential-free Git context.
	Source SourceBlock `json:"source"`
	// Invocation is the typed projection of the parsed command line.
	Invocation InvocationBlock `json:"invocation"`
	// Selection is the frozen project selection the executing engine must honor.
	Selection SelectionBlock `json:"selection"`
	// Plan is the expected task graph the executing engine validates against.
	Plan PlanBlock `json:"plan"`
	// Environment pins the engine, extensions, toolchains and platform.
	Environment EnvironmentBlock `json:"environment"`
	// Control carries submission identity and bounds; it is outside the input digest.
	Control ControlBlock `json:"control"`
}

// ProtocolBlock records the provider protocol version and the capabilities
// negotiated for this submission.
type ProtocolBlock struct {
	// Version is the provider protocol version this request speaks (1).
	Version int `json:"version"`
	// Capabilities are the sorted capability names negotiated at initialize.
	Capabilities []string `json:"capabilities"`
}

// SourceBlock identifies the executed bytes independently of HEAD and keeps
// the Git and version context those bytes were captured under.
type SourceBlock struct {
	// Digest is the canonical source-manifest digest.
	Digest string `json:"digest"`
	// IndexDigest is the lowercase sha256 of the captured Git index listing.
	IndexDigest string `json:"indexDigest"`
	// Git is the credential-free version-stamp context.
	Git GitContext `json:"git"`
	// Tree is the existing HEAD-bound worktree fingerprint, absent when unknown.
	Tree *TreeIdentity `json:"tree,omitempty"`
	// Versions are the resolved per-line versions the executing engine stamps.
	Versions []LineVersion `json:"versions"`
	// Bound lists, sorted and unique, the workspace-relative paths the
	// submitting engine admitted as required ignored inputs: each is ignored
	// by Git in the source worktree, is selected as an input by a planned
	// task, and has a manifest entry flagged bound. Absent when none.
	Bound []string `json:"bound,omitempty"`
}

// TreeIdentity mirrors the canonical session tree block.
type TreeIdentity struct {
	// Fingerprint is the 64-hex HEAD-bound worktree digest of the session tree contract.
	Fingerprint string `json:"fingerprint"`
	// Dirty reports whether that worktree differed from HeadSHA.
	Dirty bool `json:"dirty"`
	// HeadSHA is the full commit the worktree sat on when captured.
	HeadSHA string `json:"headSHA"`
}

// LineVersion is one resolved version line, exactly as the submitting engine
// stamped it, so the executing engine needs no Git history of its own.
type LineVersion struct {
	// Line is the version line's scope path; empty names the root line.
	Line string `json:"line"`
	// Base is the line's version: the tag's on a tagged commit, else the next one.
	Base string `json:"base"`
	// Full is Base plus the suffix when the commit is not tagged.
	Full string `json:"full"`
	// SHA is the short commit id the version was stamped from.
	SHA string `json:"sha"`
	// Branch is the branch name the version was stamped on, empty when detached.
	Branch string `json:"branch"`
	// Suffix is the ordered pre-release suffix (commit time, sha, dirty hash).
	Suffix string `json:"suffix"`
	// Tag is the line tag at HEAD when Tagged, else empty.
	Tag string `json:"tag"`
	// Tagged reports whether HEAD carries this line's tag.
	Tagged bool `json:"tagged"`
	// Dirty reports whether the stamped tree had uncommitted changes.
	Dirty bool `json:"dirty"`
}

// InvocationBlock is the semantic invocation: ordered commands, typed
// parameters, effective execution flags and the workspace-relative cwd.
type InvocationBlock struct {
	// Commands are the ordered job commands, unique identifiers, 1 to 16.
	Commands []string `json:"commands"`
	// Params are the command parameters by name, each with its explicit type.
	Params map[string]ParamValue `json:"params"`
	// Flags are the effective run-shaping flags, every member present.
	Flags ExecutionFlags `json:"flags"`
	// Cwd is the invocation directory relative to the workspace root, "." for the root.
	Cwd string `json:"cwd"`
	// Providers are the sorted, unique invocation providers the caller enabled
	// (InvocationProviderInstall, InvocationProviderPublish); absent when none,
	// never an empty list, so a request that enables none keeps its digest.
	Providers []string `json:"providers,omitempty"`
	// Publication authorizes the plan's publication tasks and names the
	// commands they wait for. ValidatePublication requires it for a task of a
	// publication command; the executing engine, which classifies by declared
	// effects, also refuses it over a plan that publishes nothing.
	Publication *PublicationBlock `json:"publication,omitempty"`
}

// ExecutionFlags are the run-shaping flags with their explicit-versus-default
// distinctions preserved. Placement is deliberately absent: it never reaches
// an executing engine as an instruction.
type ExecutionFlags struct {
	// NoCache disables cache reuse for every task of the run.
	NoCache bool `json:"noCache"`
	// NoCacheExplicit records that the caller typed --no-cache, which is what
	// extensions receive; a host-internal NoCache is not forwarded to them.
	NoCacheExplicit bool `json:"noCacheExplicit"`
	// RetryFailed re-executes tasks whose recorded failure would otherwise replay.
	RetryFailed bool `json:"retryFailed"`
	// ContinueOnError keeps scheduling independent tasks after a failure.
	ContinueOnError bool `json:"continueOnError"`
	// ImpactedStrict makes an unresolved impacted baseline a failure, not a fallback.
	ImpactedStrict bool `json:"impactedStrict"`
	// Verbose enables verbose rendering.
	Verbose bool `json:"verbose"`
	// Debug enables debug diagnostics (implies Verbose).
	Debug bool `json:"debug"`
	// Quiet suppresses informational output.
	Quiet bool `json:"quiet"`
	// Retry is the per-task retry count, zero for none.
	Retry int `json:"retry"`
	// MaxParallel is the explicit worker count, zero when unset.
	MaxParallel int `json:"maxParallel"`
	// MaxParallelMode is the symbolic parallelism mode when no count was given.
	MaxParallelMode string `json:"maxParallelMode"`
	// CPUBudgetPolicy is how per-task CPU ceilings are derived, empty for the default.
	CPUBudgetPolicy string `json:"cpuBudgetPolicy"`
	// CacheTrust is the resolved remote-cache trust policy.
	CacheTrust string `json:"cacheTrust"`
	// Output is the resolved output mode (text, json, jsonl, ...), empty for auto.
	Output string `json:"output"`
	// Profile is the resolved deployment profile (dev, test, production).
	Profile string `json:"profile"`
	// ResourceBudgets maps named external resources to the units this run may use.
	ResourceBudgets map[string]int `json:"resourceBudgets"`
}

// SelectionBlock freezes what the submitting engine resolved. Projects are the
// canonical selected ids; the executing engine plans exactly those and keeps
// Mode as the recorded selection mode.
type SelectionBlock struct {
	// RequestedMode is the selection mode the caller asked for: all, impacted or projects.
	RequestedMode string `json:"requestedMode"`
	// Mode is the effective mode after fallbacks; it is what the session records.
	Mode string `json:"mode"`
	// Scoped is true exactly when Mode is not all.
	Scoped bool `json:"scoped"`
	// Projects are the sorted canonical ids of the selected projects.
	Projects []string `json:"projects"`
	// Baseline is the full commit the impacted baseline ref resolved to; empty otherwise.
	Baseline string `json:"baseline"`
	// BaselineSource is the resolution tier that produced Baseline.
	BaselineSource string `json:"baselineSource"`
	// ChangedPaths are the sorted workspace-relative paths an impacted run measured.
	ChangedPaths []string `json:"changedPaths"`
	// Diagnostics are the selection notices the submitter printed, for the record.
	Diagnostics []string `json:"diagnostics"`
	// NoCacheProjects are the sorted ids whose tasks refuse cache reuse.
	NoCacheProjects []string `json:"noCacheProjects"`
	// TaskScopes maps a selected project id to the sorted tasks of it the
	// change reaches; the executing engine plans those tasks and their
	// predecessors on that project, and nothing else of it. A project absent
	// from the map runs every task. Absent or empty when every selected project
	// runs every task.
	//
	// An entry is `[<extension-project-id>#]<command>~<step>`, the plan name of
	// one task, qualified when only one extension's version of that task is
	// reached; a submitter that resolved no task declarations sends the bare
	// extension project ids of that project's tool scope instead. The values
	// are opaque to this protocol: it validates their shape and their
	// membership in `projects`, never their spelling.
	TaskScopes map[string][]string `json:"taskScopes,omitempty"`
}

// PlanBlock is the expected plan: canonical identities, edges, contracts and
// declared resources, sorted by identity key. Cache presence is excluded.
type PlanBlock struct {
	// Tasks are the expected plan nodes sorted by identity key, at least one.
	Tasks []PlannedTask `json:"tasks"`
}

// PlannedTask is one expected plan node.
type PlannedTask struct {
	// Identity is the canonical task identity of the CLI contract.
	Identity protocolcli.TaskIdentity `json:"identity"`
	// DependsOn lists the sorted identity keys this task consumes output from.
	DependsOn []string `json:"dependsOn"`
	// SerializeAfter lists the sorted identity keys this task merely orders behind.
	SerializeAfter []string `json:"serializeAfter"`
	// ContractDigest is the task-contract digest ("<format>:<64 hex>") or empty.
	ContractDigest string `json:"contractDigest"`
	// DeadlineMs is the resolved hard subprocess deadline, at least 1.
	DeadlineMs int `json:"deadlineMs"`
	// Cacheable reports whether the task is cacheable at all, a plan property.
	Cacheable bool `json:"cacheable"`
	// Resources is the task's declared scheduling class.
	Resources TaskResources `json:"resources"`
}

// TaskResources is the declared scheduling class of a task.
type TaskResources struct {
	// Heavy marks the task as CPU/memory heavy for worker tuning.
	Heavy bool `json:"heavy"`
	// CPUWeight is the positive relative CPU-demand multiplier, 1 by default.
	CPUWeight float64 `json:"cpuWeight"`
	// Reads are the declared read resources sorted by scope then id.
	Reads []TaskResource `json:"reads"`
	// Writes are the declared write resources sorted by scope then id.
	Writes []TaskResource `json:"writes"`
}

// TaskResource names one declared resource by id and effective scope.
type TaskResource struct {
	// ID is the declared resource identifier.
	ID string `json:"id"`
	// Scope is the resource's effective scope (project or workspace).
	Scope string `json:"scope"`
}

// EnvironmentBlock pins the engine, extensions, toolchains and platform.
type EnvironmentBlock struct {
	// CLI is the engine pin the snapshot declares.
	CLI PinnedCLI `json:"cli"`
	// Extensions are the discovered extensions sorted by unique name.
	Extensions []PinnedComponent `json:"extensions"`
	// Toolchains are the lock-pinned toolchains sorted by unique name.
	Toolchains []PinnedComponent `json:"toolchains"`
	// Platform is the target operating system and architecture.
	Platform Platform `json:"platform"`
}

// PinnedCLI names the engine the snapshot runs: its own source tree, a
// published version, or no pin at all.
type PinnedCLI struct {
	// Source is workspace (built from the snapshot), published or unpinned.
	Source string `json:"source"`
	// Version is the published version; empty for workspace and unpinned.
	Version string `json:"version"`
}

// PinnedComponent is one named, versioned component.
type PinnedComponent struct {
	// Name is the component's name.
	Name string `json:"name"`
	// Version is the resolved version, possibly empty for an unversioned local one.
	Version string `json:"version"`
}

// Platform is the target operating system and architecture.
type Platform struct {
	// OS is the operating system identifier.
	OS string `json:"os"`
	// Arch is the CPU architecture identifier.
	Arch string `json:"arch"`
}

// ControlBlock carries the caller, the idempotency key and the finite deadline.
type ControlBlock struct {
	// Caller identifies the submitting caller; only cli is defined.
	Caller string `json:"caller"`
	// IdempotencyKey is the 32-hex key that identifies one submission attempt.
	IdempotencyKey string `json:"idempotencyKey"`
	// Deadline is the finite RFC 3339 UTC instant the provider enforces.
	Deadline string `json:"deadline"`
}

// CanonicalExecutionRequest returns the compact canonical JSON bytes of a
// validated request. Struct member order and Go JSON escaping define the
// canonical form, exactly as for the source manifest.
func CanonicalExecutionRequest(request ExecutionRequest) ([]byte, error) {
	if err := ValidateExecutionRequest(request); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRequestBytes {
		return nil, fmt.Errorf("runner: request exceeds %d bytes", MaxRequestBytes)
	}
	return data, nil
}

type executionInput struct {
	Domain      string           `json:"domain"`
	Protocol    int              `json:"protocolVersion"`
	Source      SourceBlock      `json:"source"`
	Invocation  InvocationBlock  `json:"invocation"`
	Selection   selectionInput   `json:"selection"`
	Environment EnvironmentBlock `json:"environment"`
}

// selectionInput is the selection block without its human notices: the
// diagnostics explain a selection to a reader and never decide it.
type selectionInput struct {
	RequestedMode   string   `json:"requestedMode"`
	Mode            string   `json:"mode"`
	Scoped          bool     `json:"scoped"`
	Projects        []string `json:"projects"`
	Baseline        string   `json:"baseline"`
	BaselineSource  string   `json:"baselineSource"`
	ChangedPaths    []string `json:"changedPaths"`
	NoCacheProjects []string `json:"noCacheProjects"`
	// TaskScopes is omitted when empty, so a request that scopes nothing keeps
	// the digest it had before the member existed.
	TaskScopes map[string][]string `json:"taskScopes,omitempty"`
}

// ExecutionInputDigest binds source, semantic invocation, frozen selection,
// Git/version inputs and pinned environment. It excludes the expected plan
// (derived), the control block (per-submission), the negotiated capabilities
// and the selection diagnostics (free-text notices), and is never a task key.
func ExecutionInputDigest(request ExecutionRequest) (string, error) {
	if err := ValidateExecutionRequest(request); err != nil {
		return "", err
	}
	selection := request.Selection
	data, err := json.Marshal(executionInput{
		Domain: ExecutionInputDomain, Protocol: request.Protocol.Version,
		Source: request.Source, Invocation: request.Invocation,
		Selection: selectionInput{
			RequestedMode: selection.RequestedMode, Mode: selection.Mode, Scoped: selection.Scoped,
			Projects: selection.Projects, Baseline: selection.Baseline, BaselineSource: selection.BaselineSource,
			ChangedPaths: selection.ChangedPaths, NoCacheProjects: selection.NoCacheProjects,
			TaskScopes: selection.TaskScopes,
		},
		Environment: request.Environment,
	})
	if err != nil {
		return "", err
	}
	return BlobDigest(data), nil
}

// ParseExecutionRequest strictly decodes and validates a request document.
func ParseExecutionRequest(data []byte) (ExecutionRequest, error) {
	var request ExecutionRequest
	if len(data) > MaxRequestBytes {
		return request, fmt.Errorf("runner: request exceeds %d bytes", MaxRequestBytes)
	}
	if err := strictJSON(data); err != nil {
		return request, err
	}
	if err := strictRequestShape(data); err != nil {
		return request, err
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return request, fmt.Errorf("runner: request: %w", err)
	}
	if err := ValidateExecutionRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

// strictRequestShape rejects unknown or missing members at every object level
// before the lenient struct decoder runs.
func strictRequestShape(data []byte) error {
	root, err := strictObject(data, []string{"version", "protocol", "source", "invocation", "selection", "plan", "environment", "control"}, nil)
	if err != nil {
		return err
	}
	if _, err := strictObject(root["protocol"], []string{"version", "capabilities"}, nil); err != nil {
		return fmt.Errorf("runner: protocol: %w", err)
	}
	source, err := strictObject(root["source"], []string{"digest", "indexDigest", "git", "versions"}, []string{"tree", "bound"})
	if err != nil {
		return fmt.Errorf("runner: source: %w", err)
	}
	if _, err := strictObject(source["git"], []string{"head", "branch", "dirty"}, nil); err != nil {
		return fmt.Errorf("runner: source.git: %w", err)
	}
	if tree, ok := source["tree"]; ok {
		if _, err := strictObject(tree, []string{"fingerprint", "dirty", "headSHA"}, nil); err != nil {
			return fmt.Errorf("runner: source.tree: %w", err)
		}
	}
	if bound, ok := source["bound"]; ok {
		// The canonical form omits an empty list, so its explicit presence is a
		// second spelling of the same request and is refused.
		if items, err := strictArray(bound, MaxSourceEntries); err != nil || len(items) == 0 {
			return fmt.Errorf("runner: source.bound must be a non-empty array when present")
		}
	}
	if err := strictEach(source["versions"], MaxSourceEntries, []string{"line", "base", "full", "sha", "branch", "suffix", "tag", "tagged", "dirty"}); err != nil {
		return fmt.Errorf("runner: source.versions: %w", err)
	}
	invocation, err := strictObject(root["invocation"], []string{"commands", "params", "flags", "cwd"}, []string{"providers", "publication"})
	if err != nil {
		return fmt.Errorf("runner: invocation: %w", err)
	}
	if err := strictInvocationExtensions(invocation); err != nil {
		return err
	}
	if _, err := strictObject(invocation["flags"], []string{"noCache", "noCacheExplicit", "retryFailed", "continueOnError", "impactedStrict", "verbose", "debug", "quiet", "retry", "maxParallel", "maxParallelMode", "cpuBudgetPolicy", "cacheTrust", "output", "profile", "resourceBudgets"}, nil); err != nil {
		return fmt.Errorf("runner: invocation.flags: %w", err)
	}
	if _, err := strictObject(root["selection"], []string{"requestedMode", "mode", "scoped", "projects", "baseline", "baselineSource", "changedPaths", "diagnostics", "noCacheProjects"}, []string{"taskScopes"}); err != nil {
		return fmt.Errorf("runner: selection: %w", err)
	}
	plan, err := strictObject(root["plan"], []string{"tasks"}, nil)
	if err != nil {
		return fmt.Errorf("runner: plan: %w", err)
	}
	tasks, err := strictArray(plan["tasks"], MaxPlanTasks)
	if err != nil {
		return fmt.Errorf("runner: plan.tasks: %w", err)
	}
	for index, raw := range tasks {
		if err := strictTaskShape(raw); err != nil {
			return fmt.Errorf("runner: plan.tasks[%d]: %w", index, err)
		}
	}
	environment, err := strictObject(root["environment"], []string{"cli", "extensions", "toolchains", "platform"}, nil)
	if err != nil {
		return fmt.Errorf("runner: environment: %w", err)
	}
	if _, err := strictObject(environment["cli"], []string{"source", "version"}, nil); err != nil {
		return fmt.Errorf("runner: environment.cli: %w", err)
	}
	if _, err := strictObject(environment["platform"], []string{"os", "arch"}, nil); err != nil {
		return fmt.Errorf("runner: environment.platform: %w", err)
	}
	for _, key := range []string{"extensions", "toolchains"} {
		if err := strictEach(environment[key], MaxSourceEntries, []string{"name", "version"}); err != nil {
			return fmt.Errorf("runner: environment.%s: %w", key, err)
		}
	}
	if _, err := strictObject(root["control"], []string{"caller", "idempotencyKey", "deadline"}, nil); err != nil {
		return fmt.Errorf("runner: control: %w", err)
	}
	return nil
}

func strictTaskShape(raw json.RawMessage) error {
	task, err := strictObject(raw, []string{"identity", "dependsOn", "serializeAfter", "contractDigest", "deadlineMs", "cacheable", "resources"}, nil)
	if err != nil {
		return err
	}
	identity, err := strictObject(task["identity"], []string{"key", "scope", "project", "task", "provider"}, nil)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	if _, err := strictObject(identity["project"], []string{"id", "name"}, nil); err != nil {
		return fmt.Errorf("identity.project: %w", err)
	}
	if _, err := strictObject(identity["task"], []string{"name", "command", "kind"}, []string{"step"}); err != nil {
		return fmt.Errorf("identity.task: %w", err)
	}
	if _, err := strictObject(identity["provider"], []string{"extension"}, []string{"version"}); err != nil {
		return fmt.Errorf("identity.provider: %w", err)
	}
	resources, err := strictObject(task["resources"], []string{"heavy", "cpuWeight", "reads", "writes"}, nil)
	if err != nil {
		return fmt.Errorf("resources: %w", err)
	}
	for _, key := range []string{"reads", "writes"} {
		if err := strictEach(resources[key], MaxSourceEntries, []string{"id", "scope"}); err != nil {
			return fmt.Errorf("resources.%s: %w", key, err)
		}
	}
	return nil
}

func strictEach(raw json.RawMessage, limit int, required []string) error {
	items, err := strictArray(raw, limit)
	if err != nil {
		return err
	}
	for index, item := range items {
		if _, err := strictObject(item, required, nil); err != nil {
			return fmt.Errorf("[%d]: %w", index, err)
		}
	}
	return nil
}

// ValidateExecutionRequest checks the semantic rules every block must satisfy.
func ValidateExecutionRequest(request ExecutionRequest) error {
	if request.Version != ExecutionRequestVersion {
		return fmt.Errorf("runner: unsupported execution request version %d", request.Version)
	}
	if request.Protocol.Version != ProviderProtocolVersion {
		return fmt.Errorf("runner: unsupported provider protocol version %d", request.Protocol.Version)
	}
	if err := sortedUnique(request.Protocol.Capabilities, MaxDiagnostics); err != nil {
		return fmt.Errorf("runner: protocol.capabilities: %w", err)
	}
	if err := validateSourceBlock(request.Source); err != nil {
		return err
	}
	if err := validateInvocation(request.Invocation); err != nil {
		return err
	}
	if err := validateSelection(request.Selection); err != nil {
		return err
	}
	if err := ValidatePlan(request.Plan); err != nil {
		return err
	}
	if err := ValidatePublication(request.Invocation, request.Plan, IsPublicationTask); err != nil {
		return err
	}
	if err := validateEnvironment(request.Environment); err != nil {
		return err
	}
	return validateControl(request.Control)
}

func validateSourceBlock(source SourceBlock) error {
	if !validDigest(source.Digest) {
		return fmt.Errorf("runner: source.digest must be a lowercase sha256 digest")
	}
	if !validHex(source.IndexDigest, 64) {
		return fmt.Errorf("runner: source.indexDigest must be 64 lowercase hex digits")
	}
	if err := ValidateGitContext(source.Git); err != nil {
		return err
	}
	if tree := source.Tree; tree != nil {
		if !validHex(tree.Fingerprint, 64) || !(validHex(tree.HeadSHA, 40) || validHex(tree.HeadSHA, 64)) {
			return fmt.Errorf("runner: source.tree must carry a 64-hex fingerprint and a full head SHA")
		}
	}
	if source.Versions == nil {
		return fmt.Errorf("runner: source.versions must be an array")
	}
	if len(source.Bound) > 0 {
		if err := sortedUnique(source.Bound, MaxSourceEntries); err != nil {
			return fmt.Errorf("runner: source.bound: %w", err)
		}
		for _, name := range source.Bound {
			if err := ValidateSourcePath(name); err != nil {
				return fmt.Errorf("runner: source.bound %q: %w", name, err)
			}
		}
	}
	for index, version := range source.Versions {
		if index > 0 && source.Versions[index-1].Line >= version.Line {
			return fmt.Errorf("runner: source.versions must be sorted by unique line")
		}
		for _, text := range []string{version.Line, version.Base, version.Full, version.SHA, version.Branch, version.Suffix, version.Tag} {
			if err := boundedText(text); err != nil {
				return fmt.Errorf("runner: source.versions[%d]: %w", index, err)
			}
		}
		if version.Base == "" || version.Full == "" {
			return fmt.Errorf("runner: source.versions[%d] must name a base and full version", index)
		}
	}
	return nil
}

func validateInvocation(invocation InvocationBlock) error {
	if len(invocation.Commands) == 0 || len(invocation.Commands) > MaxCommands {
		return fmt.Errorf("runner: invocation.commands must hold 1 to %d commands", MaxCommands)
	}
	seen := make(map[string]bool, len(invocation.Commands))
	for _, command := range invocation.Commands {
		if !validIdentifier(command) || seen[command] {
			return fmt.Errorf("runner: invocation.commands must be unique identifiers")
		}
		seen[command] = true
	}
	if err := validateParams(invocation.Params); err != nil {
		return err
	}
	flags := invocation.Flags
	if flags.Retry < 0 || flags.MaxParallel < 0 {
		return fmt.Errorf("runner: invocation.flags counts must be non-negative")
	}
	for _, text := range []string{flags.MaxParallelMode, flags.CPUBudgetPolicy, flags.CacheTrust, flags.Output, flags.Profile} {
		if err := boundedText(text); err != nil {
			return fmt.Errorf("runner: invocation.flags: %w", err)
		}
	}
	if flags.ResourceBudgets == nil {
		return fmt.Errorf("runner: invocation.flags.resourceBudgets must be an object")
	}
	if len(flags.ResourceBudgets) > MaxResourceBudgets {
		return fmt.Errorf("runner: invocation.flags.resourceBudgets exceeds %d entries", MaxResourceBudgets)
	}
	for name, units := range flags.ResourceBudgets {
		if !validIdentifier(name) || units < 0 {
			return fmt.Errorf("runner: invocation.flags.resourceBudgets has an invalid entry")
		}
	}
	if err := validatePath(invocation.Cwd, false); invocation.Cwd != "." && err != nil {
		return fmt.Errorf("runner: invocation.cwd: %w", err)
	}
	if err := validateProviders(invocation.Providers); err != nil {
		return err
	}
	return validatePublicationBlock(invocation)
}

func validateSelection(selection SelectionBlock) error {
	for _, mode := range []string{selection.RequestedMode, selection.Mode} {
		if mode != SelectionModeAll && mode != SelectionModeImpacted && mode != SelectionModeProjects {
			return fmt.Errorf("runner: selection mode %q is not all, impacted or projects", mode)
		}
	}
	if selection.Scoped != (selection.Mode != SelectionModeAll) {
		return fmt.Errorf("runner: selection.scoped disagrees with selection.mode")
	}
	if err := sortedUnique(selection.Projects, MaxSelectionProjects); err != nil || len(selection.Projects) == 0 {
		return fmt.Errorf("runner: selection.projects must be a sorted non-empty list of unique ids")
	}
	if selection.Mode == SelectionModeImpacted {
		if !(validHex(selection.Baseline, 40) || validHex(selection.Baseline, 64)) || selection.BaselineSource == "" {
			return fmt.Errorf("runner: an impacted selection must carry a full baseline commit and its source")
		}
	} else if selection.Baseline != "" || selection.BaselineSource != "" {
		return fmt.Errorf("runner: only an impacted selection carries a baseline")
	}
	if err := boundedText(selection.BaselineSource); err != nil {
		return fmt.Errorf("runner: selection.baselineSource: %w", err)
	}
	if err := sortedUnique(selection.ChangedPaths, MaxSourceEntries); err != nil {
		return fmt.Errorf("runner: selection.changedPaths: %w", err)
	}
	if selection.Diagnostics == nil || len(selection.Diagnostics) > MaxDiagnostics {
		return fmt.Errorf("runner: selection.diagnostics must be an array of at most %d entries", MaxDiagnostics)
	}
	for _, diagnostic := range selection.Diagnostics {
		if diagnostic == "" || len(diagnostic) > MaxDiagnosticBytes || !utf8.ValidString(diagnostic) {
			return fmt.Errorf("runner: selection.diagnostics entries must be bounded non-empty UTF-8")
		}
	}
	if err := sortedUnique(selection.NoCacheProjects, MaxSelectionProjects); err != nil {
		return fmt.Errorf("runner: selection.noCacheProjects: %w", err)
	}
	return validateTaskScopes(selection)
}

// validateTaskScopes checks that every task scope names a selected project
// and lists at least one scope entry, sorted and unique. A scope that
// names an unselected project would describe a narrowing of nothing.
func validateTaskScopes(selection SelectionBlock) error {
	if len(selection.TaskScopes) > MaxSelectionProjects {
		return fmt.Errorf("runner: selection.taskScopes has more than %d entries", MaxSelectionProjects)
	}
	for project, extensions := range selection.TaskScopes {
		if _, found := slices.BinarySearch(selection.Projects, project); !found {
			return fmt.Errorf("runner: selection.taskScopes names %q, which selection.projects does not list", project)
		}
		if len(extensions) == 0 {
			return fmt.Errorf("runner: selection.taskScopes[%q] must list at least one extension project", project)
		}
		if err := sortedUnique(extensions, MaxSelectionProjects); err != nil {
			return fmt.Errorf("runner: selection.taskScopes[%q]: %w", project, err)
		}
	}
	return nil
}

// ValidatePlan checks the expected plan's canonical ordering, identities and
// edges. Every edge must name another task of the same plan.
func ValidatePlan(plan PlanBlock) error {
	if len(plan.Tasks) == 0 || len(plan.Tasks) > MaxPlanTasks {
		return fmt.Errorf("runner: plan.tasks must hold 1 to %d tasks; an empty plan is a local no-op, never a submission", MaxPlanTasks)
	}
	keys := make(map[string]bool, len(plan.Tasks))
	for index, task := range plan.Tasks {
		if err := validateIdentity(task.Identity); err != nil {
			return fmt.Errorf("runner: plan.tasks[%d]: %w", index, err)
		}
		if index > 0 && plan.Tasks[index-1].Identity.Key >= task.Identity.Key {
			return fmt.Errorf("runner: plan.tasks must be sorted by unique identity key")
		}
		keys[task.Identity.Key] = true
		if task.DeadlineMs < 1 {
			return fmt.Errorf("runner: plan.tasks[%d] has no finite deadline", index)
		}
		if task.ContractDigest != "" && !validPrefixedDigest(task.ContractDigest) {
			return fmt.Errorf("runner: plan.tasks[%d] contract digest is malformed", index)
		}
		if task.Resources.CPUWeight <= 0 || math.IsNaN(task.Resources.CPUWeight) || math.IsInf(task.Resources.CPUWeight, 0) {
			return fmt.Errorf("runner: plan.tasks[%d] cpuWeight must be a positive finite number", index)
		}
		for _, refs := range [][]TaskResource{task.Resources.Reads, task.Resources.Writes} {
			if refs == nil {
				return fmt.Errorf("runner: plan.tasks[%d] resources must be arrays", index)
			}
			for position, ref := range refs {
				if ref.ID == "" || ref.Scope == "" {
					return fmt.Errorf("runner: plan.tasks[%d] resource is incomplete", index)
				}
				if position > 0 && !(refs[position-1].Scope < ref.Scope || refs[position-1].Scope == ref.Scope && refs[position-1].ID < ref.ID) {
					return fmt.Errorf("runner: plan.tasks[%d] resources must be sorted by scope then id", index)
				}
			}
		}
	}
	for index, task := range plan.Tasks {
		for _, edges := range [][]string{task.DependsOn, task.SerializeAfter} {
			if err := sortedUnique(edges, MaxPlanTasks); err != nil {
				return fmt.Errorf("runner: plan.tasks[%d] edges: %w", index, err)
			}
			for _, edge := range edges {
				if !keys[edge] || edge == task.Identity.Key {
					return fmt.Errorf("runner: plan.tasks[%d] edge %q does not name another planned task", index, edge)
				}
			}
		}
	}
	return nil
}

func validateIdentity(identity protocolcli.TaskIdentity) error {
	if identity.Scope != protocolcli.TaskScopeProject && identity.Scope != protocolcli.TaskScopeWorkspace {
		return fmt.Errorf("identity scope %q is invalid", identity.Scope)
	}
	if identity.Project.ID == "" || identity.Project.Name == "" || identity.Task.Name == "" || identity.Task.Command == "" || identity.Task.Kind == "" || identity.Provider.Extension == "" {
		return fmt.Errorf("identity is incomplete")
	}
	if identity.Key != identity.DerivedKey() {
		return fmt.Errorf("identity key %q disagrees with its structured fields", identity.Key)
	}
	for _, text := range []string{identity.Key, identity.Project.Name, identity.Task.Step, identity.Provider.Version} {
		if err := boundedText(text); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvironment(environment EnvironmentBlock) error {
	switch environment.CLI.Source {
	case CLISourceWorkspace, CLISourceUnpinned:
		if environment.CLI.Version != "" {
			return fmt.Errorf("runner: environment.cli %s pins no version", environment.CLI.Source)
		}
	case CLISourcePublished:
		if environment.CLI.Version == "" {
			return fmt.Errorf("runner: environment.cli published pin needs a version")
		}
	default:
		return fmt.Errorf("runner: environment.cli.source %q is invalid", environment.CLI.Source)
	}
	if err := boundedText(environment.CLI.Version); err != nil {
		return err
	}
	for _, components := range []struct {
		name string
		list []PinnedComponent
	}{{"extensions", environment.Extensions}, {"toolchains", environment.Toolchains}} {
		if components.list == nil {
			return fmt.Errorf("runner: environment.%s must be an array", components.name)
		}
		for index, component := range components.list {
			if component.Name == "" || boundedText(component.Name) != nil || boundedText(component.Version) != nil {
				return fmt.Errorf("runner: environment.%s[%d] is invalid", components.name, index)
			}
			if index > 0 && components.list[index-1].Name >= component.Name {
				return fmt.Errorf("runner: environment.%s must be sorted by unique name", components.name)
			}
		}
	}
	if !validIdentifier(environment.Platform.OS) || !validIdentifier(environment.Platform.Arch) {
		return fmt.Errorf("runner: environment.platform must name an os and an arch")
	}
	return nil
}

func validateControl(control ControlBlock) error {
	if control.Caller != CallerCLI {
		return fmt.Errorf("runner: control.caller %q is not supported", control.Caller)
	}
	if !validHex(control.IdempotencyKey, 32) {
		return fmt.Errorf("runner: control.idempotencyKey must be 32 lowercase hex digits")
	}
	deadline, err := time.Parse(time.RFC3339, control.Deadline)
	if err != nil || deadline.Location() != time.UTC || !strings.HasSuffix(control.Deadline, "Z") {
		return fmt.Errorf("runner: control.deadline must be an RFC 3339 UTC instant")
	}
	return nil
}

func validDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHex(strings.TrimPrefix(value, "sha256:"), 64)
}

// validPrefixedDigest accepts "<format>:<64 hex>" for the task-contract digest,
// whose format tag is owned by the extension protocol ("tc1" today).
func validPrefixedDigest(value string) bool {
	format, hex, ok := strings.Cut(value, ":")
	if !ok || format == "" || len(format) > 16 || !validHex(hex, 64) {
		return false
	}
	for _, char := range format {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' || char == '@' || char == '/' || char == '~') {
			return false
		}
	}
	return true
}

func boundedText(value string) error {
	if len(value) > MaxSourcePathBytes || !utf8.ValidString(value) || strings.ContainsRune(value, utf8.RuneError) {
		return fmt.Errorf("text must be bounded valid UTF-8")
	}
	for _, char := range value {
		if char < ' ' || char == 0x7f {
			return fmt.Errorf("text must not contain control characters")
		}
	}
	return nil
}

func sortedUnique(values []string, limit int) error {
	if values == nil {
		return fmt.Errorf("must be an array")
	}
	if len(values) > limit {
		return fmt.Errorf("exceeds %d entries", limit)
	}
	for index, value := range values {
		if value == "" || boundedText(value) != nil {
			return fmt.Errorf("entry %d is empty or unbounded", index)
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("entries must be sorted and unique")
		}
	}
	return nil
}

// SortStrings returns a sorted, deduplicated copy with empty entries removed.
func SortStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
