package extension

import (
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// CommandTraits captures the orchestration-relevant semantics of a
// command in machine-readable form. The orchestrator consults traits
// instead of matching command names, so a new extension inherits
// correct scheduling and gating behavior by declaring them (or by using
// a verb whose vocabulary carries default traits).
//
// A manifest-declared traits object replaces the verb's default traits
// wholesale, side effects included.
//
// Traits describe the COMMAND, never the workload the command runs
// against. Contract 3 deleted the one trait that broke that
// rule — `preflight: config-schema`, which asked core to walk a
// project's Go imports and infra markers to decide whether an artifact
// it does not own had to exist. A task that needs another task's output
// says so with a typed input port (`from: "task"`); a port that is not
// `optional` has no producer at plan time is a plan error naming both
// identities. See doc/02-commands-and-tasks.md.
type CommandTraits struct {
	// Heavy marks the command's jobs as CPU/memory heavy so worker
	// tuning leaves them headroom instead of oversubscribing cores.
	// Per-step granularity is available via PipelineStep.Heavy.
	Heavy bool `json:"heavy,omitempty"`
	// RequiresRunnable skips the command for projects without a
	// runnable entrypoint (libraries).
	RequiresRunnable bool `json:"requiresRunnable,omitempty"`
	// InjectPublishChannels auto-activates the project's declared
	// publish channels as boolean params (e.g. publish: ["go","docker"]
	// activates --go and --docker).
	InjectPublishChannels bool `json:"injectPublishChannels,omitempty"`
	// SideEffects classifies what the command mutates outside the
	// workspace: "" (none), "registry", or "cloud".
	SideEffects string `json:"sideEffects,omitempty"`
	// Kind names what the host reads from the command's results, one of
	// ValidCommandKinds: CommandKindVerification, CommandKindCriteria,
	// CommandKindDiagnostics or CommandKindFiles. Empty means the host reads
	// the command's status only. A command that declares it requires CLI
	// contract protocolcli.CommandKindContract.
	Kind string `json:"kind,omitempty"`
	// Workload names the lifecycle of a command that runs the project's
	// workload, one of ValidWorkloads: WorkloadServe streams it until
	// interrupted, WorkloadOnce streams it once and forwards its exit code.
	// It describes how the command runs, never what the workload contains.
	// Empty means the command runs no workload. A command that declares it
	// requires CLI contract protocolcli.CommandKindContract.
	Workload string `json:"workload,omitempty"`
	// RewritesSources declares that the command rewrites the source tree.
	// It is distinct from a task's declares.mutatesSources, which keeps its
	// cache meaning. A command that declares it requires CLI contract
	// protocolcli.CommandKindContract.
	RewritesSources bool `json:"rewritesSources,omitempty"`
	// ConsumesReleaseSet declares that the command binds a release set and
	// an environment. A command that declares it requires CLI contract
	// protocolcli.CommandKindContract.
	ConsumesReleaseSet bool `json:"consumesReleaseSet,omitempty"`
}

// Command kinds, the closed vocabulary of CommandTraits.Kind. Each names a
// record type the host reads from the command's results, never a verb.
const (
	// CommandKindVerification: the command runs the project's verification;
	// its results carry the test counts and coverage of the verification
	// report.
	CommandKindVerification = "verification"
	// CommandKindCriteria: the command checks the project or the workspace
	// against its criteria.
	CommandKindCriteria = "criteria"
	// CommandKindDiagnostics: the command's result is a tally of errors and
	// warnings.
	CommandKindDiagnostics = "diagnostics"
	// CommandKindFiles: the command's result is a count of produced files.
	CommandKindFiles = "files"
)

// ValidCommandKinds is the CommandTraits.Kind vocabulary in canonical order.
var ValidCommandKinds = []string{CommandKindCriteria, CommandKindDiagnostics, CommandKindFiles, CommandKindVerification}

// Workload lifecycles, the closed vocabulary of CommandTraits.Workload.
const (
	// WorkloadServe streams the project's workload until interrupted.
	WorkloadServe = "serve"
	// WorkloadOnce streams the project's workload once and forwards its exit
	// code.
	WorkloadOnce = "once"
)

// ValidWorkloads is the CommandTraits.Workload vocabulary in canonical order.
var ValidWorkloads = []string{WorkloadOnce, WorkloadServe}

// DeclaresCommandKind reports whether traits declare one of the fields
// protocolcli.CommandKindContract adds: a kind, a workload, rewritesSources
// or consumesReleaseSet.
func (t *CommandTraits) DeclaresCommandKind() bool {
	return t != nil && (t.Kind != "" || t.Workload != "" || t.RewritesSources || t.ConsumesReleaseSet)
}

// ValidateTraits checks the closed vocabularies of a command's or a verb's
// traits: a kind from ValidCommandKinds and a workload from ValidWorkloads.
// field is the path of the traits object, for the diagnostics. It is inert
// for nil traits, and it leaves sideEffects to its callers (ValidateVerbs).
func ValidateTraits(traits *CommandTraits, field string) []diag.Diagnostic {
	if traits == nil {
		return nil
	}
	var diags []diag.Diagnostic
	if traits.Kind != "" && !slices.Contains(ValidCommandKinds, traits.Kind) {
		diags = append(diags, diag.Errorf("invalid-enum", field+".kind",
			"invalid command kind %q; must be one of: %s", traits.Kind, strings.Join(ValidCommandKinds, ", ")))
	}
	if traits.Workload != "" && !slices.Contains(ValidWorkloads, traits.Workload) {
		diags = append(diags, diag.Errorf("invalid-enum", field+".workload",
			"invalid workload %q; must be one of: %s", traits.Workload, strings.Join(ValidWorkloads, ", ")))
	}
	return diags
}

// SideEffects classifications.
const (
	SideEffectsNone     = ""
	SideEffectsRegistry = "registry"
	SideEffectsCloud    = "cloud"

	// CloudTokenEnv is the process-level transport for an already resource-scoped
	// cloud capability. With a non-empty CloudCapabilityAfterEnv, the CLI captures
	// it before repository-controlled work; token-only local use stays ambient for
	// backward-compatible publish and upgrade commands.
	CloudTokenEnv = "PUTNAMI_CLOUD_TOKEN"
	// CloudCapabilityAfterEnv is runner-owned control data naming the commands
	// whose complete functional leaf set must successfully dominate a cloud or
	// registry job before the scheduler may re-deliver CloudTokenEnv. It is never
	// delivered to extension subprocesses or represented in manifests.
	CloudCapabilityAfterEnv = "PUTNAMI_CLOUD_CAPABILITY_AFTER"
)

// VerbSpec documents a lifecycle verb: its purpose and the traits it carries
// by default.
type VerbSpec struct {
	Description string
	// LongRunning marks a verb that applies only to workloads with a
	// run-forever lifecycle.
	//
	// Deprecated: no lifecycle verb sets it. A command declares
	// `workload: "serve"` instead (CommandTraits.Workload).
	LongRunning bool
	Traits      CommandTraits
}

// WellKnownVerbs is the registry of the lifecycle verbs: the commands a
// language extension declares to prepare a workspace or a project's config
// (workspace-fetch, workspace-install, deps-upgrade, config-extract and
// config-merge). It lists no development verb. A verb vocabulary
// (Manifest.Verbs) declares the others, such as build or publish, with their
// default traits. See doc/06-verbs.md.
var WellKnownVerbs = map[string]VerbSpec{
	"workspace-fetch": {
		Description: "Download the language ecosystem's dependencies for the workspace, running none of their code.",
	},
	"workspace-install": {
		Description: "Install the language ecosystem's dependencies for the workspace.",
	},
	"deps-upgrade": {
		Description: "Upgrade framework and tooling dependencies.",
	},
	"config-extract": {
		Description: "Extract the project's config schema manifest.",
	},
	"config-merge": {
		Description: "Merge config defaults from workspace dependencies.",
	},
}

// BaseCommandName strips a pipeline step suffix from a job name
// (e.g. "build~transpile" → "build").
func BaseCommandName(name string) string {
	if i := strings.IndexByte(name, '~'); i >= 0 {
		return name[:i]
	}
	return name
}

// EffectiveTraits resolves the traits for a command: the traits its manifest
// declares, or the zero traits when it declares none. The registry adds no
// default, so a verb's default traits come from the vocabulary that declares
// the verb, which the caller applies before it asks.
func EffectiveTraits(commandName string, declared *CommandTraits) CommandTraits {
	if declared == nil {
		return CommandTraits{}
	}
	return *declared
}
